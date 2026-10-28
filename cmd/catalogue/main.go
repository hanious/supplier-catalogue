// Command catalogue runs the local services and their equivalent Lambda handlers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"

	"github.com/hanious/supplier-catalogue/internal/catalogue"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("process stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mode := os.Getenv("MODE")
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "" {
		return fmt.Errorf("mode required: init, api, supplier, worker, dispatcher")
	}
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	opts := []func(*config.LoadOptions) error{config.WithRegion(env("AWS_REGION", "us-east-1")), config.WithRetryMaxAttempts(3)}
	if endpoint != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "")))
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	db := dynamodb.NewFromConfig(awsConfig, func(o *dynamodb.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	store := &catalogue.Store{DB: db, Table: env("TABLE_NAME", "supplier-catalogue-local")}
	queueURL := os.Getenv("QUEUE_URL")
	queue := sqs.NewFromConfig(awsConfig)
	wake := func(ctx context.Context) error {
		if queueURL == "" {
			return nil
		}
		_, err := queue.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(`{"wake":true}`), DelaySeconds: 2})
		return err
	}
	worker := &catalogue.Worker{Store: store, Supplier: &catalogue.SupplierClient{BaseURL: env("SUPPLIER_URL", "http://supplier:8081"), HTTP: &http.Client{Timeout: 4 * time.Second}}, Lease: time.Duration(number("LEASE_SECONDS", 20)) * time.Second, Slice: time.Duration(number("WORK_SLICE_SECONDS", 10)) * time.Second, Interval: time.Duration(number("RECORD_INTERVAL_MS", 0)) * time.Millisecond}
	worker.RecordLimit = number("RECORDS_PER_SLICE", 0)
	if worker.Lease < worker.Slice+5*time.Second || worker.Slice <= 0 || worker.Interval < 0 || worker.RecordLimit < 0 {
		return fmt.Errorf("lease must exceed a positive work slice by at least five seconds")
	}
	if mode == "init" {
		if endpoint == "" {
			return fmt.Errorf("init is local only; DYNAMODB_ENDPOINT is required")
		}
		if err = store.EnsureTable(ctx); err != nil {
			return err
		}
		return store.SeedLocal(ctx)
	}
	if mode == "worker" {
		if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
			lambda.Start(func(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
				response := events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{}}
				for _, message := range event.Records {
					more, e := worker.Tick(ctx)
					if e == nil && more {
						e = wake(ctx)
					}
					if e != nil {
						slog.Error("worker slice failed", "error", e)
						response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: message.MessageId})
					}
				}
				return response, nil
			})
			return nil
		}
		for ctx.Err() == nil {
			_, err := worker.Tick(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("worker will recover from durable progress", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
		}
		return nil
	}
	if mode == "dispatcher" {
		lambda.Start(func(ctx context.Context, _ json.RawMessage) error {
			c, e := store.Control(ctx)
			if e != nil {
				return e
			}
			if c.Active == "" {
				if len(c.Retired) > 0 {
					return wake(ctx)
				}
				return nil
			}
			j, e := store.Job(ctx, c.Active)
			if e != nil {
				return e
			}
			if j.Status == "running" && j.LeaseUntil < time.Now().UnixMilli() {
				return wake(ctx)
			}
			return nil
		})
		return nil
	}
	var handler http.Handler
	address := env("HTTP_ADDR", ":8080")
	switch mode {
	case "supplier":
		handler = catalogue.SupplierHandler(store)
		address = env("HTTP_ADDR", ":8081")
	case "api":
		static := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") || strings.HasSuffix(r.URL.Path, "config.js") {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.FileServer(http.Dir("web")).ServeHTTP(w, r)
		})
		handler = (&catalogue.API{Store: store, Origin: env("ALLOWED_ORIGIN", "http://localhost:55801"), Wake: wake, Static: static}).Handler()
	default:
		return fmt.Errorf("unknown process mode %q", mode)
	}
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(httpadapter.NewV2(handler).ProxyWithContext)
		return nil
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("HTTP service ready", "mode", mode, "address", address)
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			return err
		}
		<-done
		return nil
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func number(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return n
}
