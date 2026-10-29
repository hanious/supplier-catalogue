// Command catalogue runs the local supplier catalogue services.
package main

import (
	"context"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/hanious/supplier-catalogue/internal/catalogue"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("process stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: catalogue <init|api|supplier|worker>")
	}
	mode := args[0]
	switch mode {
	case "init", "api", "supplier", "worker":
	default:
		return fmt.Errorf("unknown process mode %q", mode)
	}
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		return errors.New("DYNAMODB_ENDPOINT is required for DynamoDB Local")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	awsConfig := aws.Config{
		Region:           env("AWS_REGION", "us-east-1"),
		Credentials:      credentials.NewStaticCredentialsProvider("local", "local", ""),
		RetryMaxAttempts: 3,
	}
	db := dynamodb.NewFromConfig(awsConfig, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	store := &catalogue.Store{DB: db, Table: env("TABLE_NAME", "supplier-catalogue-local")}
	worker := &catalogue.Worker{Store: store, Supplier: &catalogue.SupplierClient{BaseURL: env("SUPPLIER_URL", "http://supplier:8081"), HTTP: &http.Client{Timeout: 4 * time.Second}}, Lease: time.Duration(number("LEASE_SECONDS", 20)) * time.Second, Slice: time.Duration(number("WORK_SLICE_SECONDS", 10)) * time.Second, Interval: time.Duration(number("RECORD_INTERVAL_MS", 0)) * time.Millisecond}
	worker.RecordLimit = number("RECORDS_PER_SLICE", 0)
	if worker.Lease < worker.Slice+5*time.Second || worker.Slice <= 0 || worker.Interval < 0 || worker.RecordLimit < 0 {
		return fmt.Errorf("lease must exceed a positive work slice by at least five seconds")
	}
	if mode == "init" {
		if err := store.EnsureTable(ctx); err != nil {
			return err
		}
		return store.SeedLocal(ctx)
	}
	if mode == "worker" {
		for ctx.Err() == nil {
			more, err := worker.Tick(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("worker will recover from durable progress", "error", err)
			}
			if err == nil && more {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
		}
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
		handler = (&catalogue.API{Store: store, Origin: env("ALLOWED_ORIGIN", "http://localhost:55801"), Static: static}).Handler()
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("HTTP service ready", "mode", mode, "address", address)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
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
