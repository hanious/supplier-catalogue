package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	endpoint := os.Getenv("INTEGRATION_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("set INTEGRATION_DYNAMODB_ENDPOINT to run against real DynamoDB Local")
	}
	db := dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
	s := &Store{DB: db, Table: fmt.Sprintf("catalogue-test-%d", time.Now().UnixNano())}
	ctx := context.Background()
	if err := s.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(s.Table)}); err != nil {
			t.Errorf("delete isolated test table: %v", err)
		}
	})
	c, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// These tests need only the first 100 baseline products. The full 10,000
	// baseline and import are exercised by the actual Compose walkthrough.
	for i := 1; i <= 100; i++ {
		p := BaselineProduct(i)
		attrs, e := encode(c.Epoch+"|PRODUCTS", p.ID, p, 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.Table), Item: attrs}); e != nil {
			t.Fatal(e)
		}
	}
	c.Current = "baseline"
	c.Generation = 1
	cw, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.transaction(ctx, cw); err != nil {
		t.Fatal(err)
	}
	return s
}

func integrationWorker(t *testing.T, s *Store) *Worker {
	t.Helper()
	server := httptest.NewServer(SupplierHandler(s))
	t.Cleanup(server.Close)
	return &Worker{Store: s, Supplier: &SupplierClient{BaseURL: server.URL, HTTP: server.Client()}, Lease: time.Minute, Slice: 30 * time.Second}
}

func finish(t *testing.T, w *Worker, id string) Job {
	t.Helper()
	for i := 0; i < 20; i++ {
		j, err := w.Store.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != "running" {
			return j
		}
		if _, err = w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if j.NextAttempt > time.Now().UnixMilli() {
			if err = wait(context.Background(), time.Until(time.UnixMilli(j.NextAttempt))); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("worker did not reach a terminal or interrupted status")
	return Job{}
}

func startTest(t *testing.T, s *Store, scenario string) Job {
	t.Helper()
	j, err := s.Start(context.Background(), scenario, fmt.Sprintf("request-%d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestIntegrationNormalAndDuplicateDelivery(t *testing.T) {
	for _, scenario := range []string{"normal", "duplicates", "out_of_order"} {
		t.Run(scenario, func(t *testing.T) {
			s := integrationStore(t)
			w := integrationWorker(t, s)
			j := startTest(t, s, scenario)
			done := finish(t, w, j.ID)
			if done.Status != "completed" {
				t.Fatalf("status = %s: %s", done.Status, done.Message)
			}
			want := Counts{Added: 10, Changed: 20, Withdrawn: 5}
			if scenario == "duplicates" {
				want.Ignored = 35
			}
			if scenario == "out_of_order" {
				want = Counts{Changed: 1, Withdrawn: 1, Ignored: 2}
			}
			if done.Counts != want {
				t.Fatalf("counts = %+v, want %+v", done.Counts, want)
			}
			replayed, err := s.Start(context.Background(), scenario, "replay-request", j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if replayed.Counts != want || replayed.ID != j.ID {
				t.Fatal("whole update replay changed its result")
			}
			if scenario == "out_of_order" {
				p, err := s.Product(context.Background(), j.Epoch, "P00002")
				if err != nil || p == nil || p.Active {
					t.Fatalf("stale active record restored withdrawal: %+v, %v", p, err)
				}
			}
		})
	}
}

func expireLease(t *testing.T, s *Store, id string) {
	t.Helper()
	j, err := s.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	j.LeaseUntil = 0
	j.NextAttempt = 0
	w, err := s.put("JOBS", id, j, j.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.transaction(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationCrashWindowsRecoverDurableOutcomes(t *testing.T) {
	for _, boundary := range []string{"before_record", "after_record", "before_checkpoint", "after_checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			s := integrationStore(t)
			w := integrationWorker(t, s)
			j := startTest(t, s, "normal")
			crash := errors.New("simulated abrupt process loss")
			fired := false
			w.Fault = func(point string) error {
				if point == boundary && !fired {
					fired = true
					return crash
				}
				return nil
			}
			if _, err := w.Tick(context.Background()); !errors.Is(err, crash) {
				t.Fatalf("fault = %v", err)
			}
			expireLease(t, s, j.ID)
			// A fresh store and worker use only persisted DynamoDB state.
			restarted := integrationWorker(t, &Store{DB: s.DB, Table: s.Table})
			done := finish(t, restarted, j.ID)
			if done.Processed != 35 || done.Counts != (Counts{Added: 10, Changed: 20, Withdrawn: 5}) {
				t.Fatalf("recovery inflated or lost outcomes: %+v", done)
			}
		})
	}
}

func TestIntegrationLeaseRacesAndObsoleteWorkerFencing(t *testing.T) {
	s := integrationStore(t)
	j := startTest(t, s, "normal")
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan Job, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			owner, err := s.Acquire(ctx, j.ID, fmt.Sprintf("owner-%d", i), time.Minute)
			results <- owner
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("lease winners = %d, want 1", winners)
	}
	old, err := s.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	expireLease(t, s, j.ID)
	current, err := s.Acquire(ctx, j.ID, "new-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Manifest(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := FixturePage(m, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(ctx, &old, page.Records[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("obsolete commit = %v", err)
	}
	if err = s.Commit(ctx, &current, page.Records[0]); err != nil {
		t.Fatal(err)
	}
	if err = s.Checkpoint(ctx, &current, page); err == nil {
		t.Fatal("checkpoint advanced past incomplete records")
	}
	if err = s.Release(ctx, &old); !errors.Is(err, ErrConflict) {
		t.Fatalf("obsolete release = %v", err)
	}
}

func TestIntegrationOutageResumeRetainsU1WhileU2IsPublished(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	j := startTest(t, s, "outage")
	ctx := context.Background()
	// Use a four-page fixture to exercise the same durable outage boundary quickly.
	var m Manifest
	rev, err := s.read(ctx, "UPDATES", j.ID, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Total = 400
	m.Pages = 4
	mw, err := s.put("UPDATES", j.ID, m, rev)
	if err != nil {
		t.Fatal(err)
	}
	j.Total = 400
	jw, err := s.put("JOBS", j.ID, j, j.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.transaction(ctx, mw, jw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		expireLease(t, s, j.ID)
	}
	interrupted, err := s.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != "interrupted" || interrupted.Processed != 300 || interrupted.Cursor != 3 {
		t.Fatalf("outage state = %+v", interrupted)
	}
	u2, err := s.Publish(ctx, "normal", "publish-u2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, "normal", "overlap", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping start = %v", err)
	}
	if err = s.Restore(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	still, err := s.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != "interrupted" {
		t.Fatal("restore resumed processing")
	}
	if _, err = s.Resume(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	done := finish(t, w, j.ID)
	if done.ID != j.ID || done.Processed != 400 || done.Counts.Changed != 100 || done.Counts.Added != 300 {
		t.Fatalf("U1 resume = %+v", done)
	}
	second, err := s.Start(ctx, "normal", "start-u2", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != u2.ID {
		t.Fatalf("started %s, want published %s", second.ID, u2.ID)
	}
}

func TestIntegrationIssuesResolveWithoutRewritingHistory(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	j := startTest(t, s, "invalid")
	bad := finish(t, w, j.ID)
	if bad.Status != "finished_with_issues" || bad.Counts.Issues != 2 || bad.Counts.Changed != 2 {
		t.Fatalf("invalid update = %+v", bad)
	}
	p, err := s.Product(context.Background(), j.Epoch, "P00012")
	if err != nil || p.Price != BaselineProduct(12).Price {
		t.Fatalf("bad price replaced existing product: %+v, %v", p, err)
	}
	correction := startTest(t, s, "correction")
	finish(t, w, correction.ID)
	api := (&API{Store: s}).Handler()
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/api/issues", nil))
	var response struct {
		Items []Result `json:"items"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	resolved := 0
	unidentified := 0
	for _, issue := range response.Items {
		if issue.Resolved {
			resolved++
		}
		if issue.After.ID == "" && !issue.Resolved {
			unidentified++
		}
	}
	if resolved != 1 || unidentified != 1 {
		t.Fatalf("issue resolution = %+v", response.Items)
	}
	historical, err := s.Job(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Status != "finished_with_issues" || historical.Counts != bad.Counts {
		t.Fatal("correction rewrote original job outcome")
	}
}

func TestIntegrationQueuePublicationCrashGapAndCacheGeneration(t *testing.T) {
	for _, afterDispatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("dispatched-%t", afterDispatch), func(t *testing.T) {
			s := integrationStore(t)
			w := integrationWorker(t, s)
			wakeups := 0
			api := (&API{Store: s, Wake: func(context.Context) error {
				if afterDispatch {
					wakeups++
				}
				return errors.New("process lost before acknowledgement")
			}}).Handler()
			get := httptest.NewRecorder()
			api.ServeHTTP(get, httptest.NewRequest("GET", "/api/products", nil))
			etag := get.Header().Get("ETag")
			req := httptest.NewRequest("POST", "/api/updates/start", strings.NewReader(`{"scenario":"normal"}`))
			req.Header.Set("Idempotency-Key", "queue-crash")
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, req)
			if rec.Code != 202 {
				t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
			}
			var j Job
			if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
				t.Fatal(err)
			}
			// Recovery dispatch reads this same durable coordinator after either gap.
			c, err := s.Control(context.Background())
			if err != nil || c.Active != j.ID {
				t.Fatalf("lost durable work: %+v, %v", c, err)
			}
			finish(t, w, j.ID)
			cached := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/api/products", nil)
			request.Header.Set("If-None-Match", etag)
			api.ServeHTTP(cached, request)
			if cached.Code != 200 || cached.Header().Get("ETag") == etag {
				t.Fatal("committed changes left a stale validator")
			}
			fresh := cached.Header().Get("ETag")
			again := httptest.NewRecorder()
			request = httptest.NewRequest("GET", "/api/products", nil)
			request.Header.Set("If-None-Match", fresh)
			api.ServeHTTP(again, request)
			if again.Code != 304 {
				t.Fatalf("unchanged catalogue = %d, want 304", again.Code)
			}
			status := httptest.NewRecorder()
			api.ServeHTTP(status, httptest.NewRequest("GET", "/api/status", nil))
			if status.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("progress can be cached")
			}
			if afterDispatch && wakeups != 1 {
				t.Fatal("dispatch fixture not exercised")
			}
		})
	}
}

func TestSupplierContractStableReplay(t *testing.T) {
	for _, scenario := range append(Scenarios(), "baseline") {
		m, err := NewManifest("U1", scenario, 1)
		if err != nil {
			t.Fatal(err)
		}
		unique := make(map[string]bool)
		for cursor := 0; cursor < m.Pages; cursor++ {
			a, err := FixturePage(m, cursor)
			if err != nil {
				t.Fatal(err)
			}
			b, err := FixturePage(m, cursor)
			if err != nil {
				t.Fatal(err)
			}
			first, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			second, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) || a.End != (cursor == m.Pages-1) {
				t.Fatal("supplier stream is unstable")
			}
			for _, record := range a.Records {
				unique[record.SourceID] = true
			}
		}
		if len(unique) != m.Total {
			t.Fatalf("%s declares %d records but has %d identities", scenario, m.Total, len(unique))
		}
	}
}

func TestHTTPRejectsUnsafeAndMalformedRequests(t *testing.T) {
	handler := (&API{Origin: "https://staff.example"}).Handler()
	tests := []struct {
		method, path, body, origin string
		code                       int
	}{
		{"POST", "/api/updates/start", `{"scenario":"normal","supplier_url":"http://bad"}`, "", 400},
		{"POST", "/api/updates/start", strings.Repeat("x", 5000), "", 400},
		{"POST", "/api/updates/start", `{} {}`, "", 400},
		{"GET", "/api/products?limit=10001", "", "", 400},
		{"GET", "/api/products?status=unknown", "", "", 400},
		{"GET", "/api/status", "", "https://untrusted.example", 403},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
		req.Header.Set("Origin", tt.origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tt.code {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.code)
		}
	}
}

func TestIntegrationResetPublishesBaselineAtomicallyAndCleansOnlyRetiredEpoch(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	ctx := context.Background()
	old, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := startTest(t, s, "baseline")
	var m Manifest
	rev, err := s.read(ctx, "UPDATES", j.ID, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Total = 100
	m.Pages = 1
	mw, err := s.put("UPDATES", j.ID, m, rev)
	if err != nil {
		t.Fatal(err)
	}
	j.Total = 100
	jw, err := s.put("JOBS", j.ID, j, j.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.transaction(ctx, mw, jw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, "normal", "reset-race", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset race = %v", err)
	}
	fired := false
	w.Fault = func(point string) error {
		if point == "after_record" && !fired {
			fired = true
			return errors.New("reset worker interrupted")
		}
		return nil
	}
	if _, err = w.Tick(ctx); err == nil {
		t.Fatal("reset interruption did not fire")
	}
	during, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if during.Epoch != old.Epoch {
		t.Fatal("partial reset replaced the visible baseline")
	}
	if during.Generation != old.Generation {
		t.Fatal("hidden reset records changed the visible catalogue validator")
	}
	rows, _, err := s.Rows(ctx, old.Epoch+"|PRODUCTS", "", 100, false)
	if err != nil || len(rows) != 100 {
		t.Fatal("reset interruption damaged existing catalogue")
	}
	expireLease(t, s, j.ID)
	w.Fault = nil
	done := finish(t, w, j.ID)
	if done.Status != "completed" {
		t.Fatalf("reset = %+v", done)
	}
	after, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Epoch == old.Epoch {
		t.Fatal("completed baseline not published")
	}
	for i := 0; i < 100; i++ {
		more, e := s.Cleanup(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if !more {
			break
		}
	}
	rows, _, err = s.Rows(ctx, old.Epoch+"|PRODUCTS", "", 100, false)
	if err != nil || len(rows) != 0 {
		t.Fatal("retired demo products retained")
	}
	rows, _, err = s.Rows(ctx, after.Epoch+"|PRODUCTS", "", 100, false)
	if err != nil || len(rows) != 100 {
		t.Fatal("cleanup removed current baseline")
	}
}

func TestIntegrationMissingRetainedUpdateAllowsSeparateNewUpdate(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	j := startTest(t, s, "normal")
	ctx := context.Background()
	if _, err := s.DB.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(s.Table), Key: key("UPDATES", j.ID)}); err != nil {
		t.Fatal(err)
	}
	done := finish(t, w, j.ID)
	if done.Status != "unavailable" || done.Processed != 0 {
		t.Fatalf("missing retained source = %+v", done)
	}
	fresh := startTest(t, s, "normal")
	if fresh.ID == done.ID {
		t.Fatal("new update replaced missing original")
	}
}

func TestIntegrationBoundedSlicesKeepRecordOffsetsDurable(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	w.RecordLimit = 4
	j := startTest(t, s, "normal")
	ctx := context.Background()
	previous := 0
	for slice := 0; slice < 12; slice++ {
		if _, err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		saved, err := s.Job(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Processed-previous > 4 {
			t.Fatal("slice exceeded its write budget")
		}
		previous = saved.Processed
		if saved.Status == "completed" {
			if saved.Counts != (Counts{Added: 10, Changed: 20, Withdrawn: 5}) {
				t.Fatalf("sliced counts = %+v", saved.Counts)
			}
			return
		}
		if saved.Offset != saved.Processed {
			t.Fatalf("offset = %d, processed = %d", saved.Offset, saved.Processed)
		}
		w.Store = &Store{DB: s.DB, Table: s.Table}
	}
	t.Fatal("bounded invocations did not finish")
}

func TestIntegrationHistoryPagesUseSavedChronology(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	first := startTest(t, s, "normal")
	finish(t, w, first.ID)
	second := startTest(t, s, "invalid")
	finish(t, w, second.ID)
	api := (&API{Store: s}).Handler()
	get := func(path string) struct {
		Items []Job  `json:"items"`
		Next  string `json:"next_cursor"`
	} {
		t.Helper()
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("history = %d: %s", rec.Code, rec.Body.String())
		}
		var response struct {
			Items []Job  `json:"items"`
			Next  string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	newest := get("/api/history?limit=1")
	if len(newest.Items) != 1 || newest.Items[0].ID != second.ID || newest.Next == "" {
		t.Fatalf("newest history = %+v", newest)
	}
	older := get("/api/history?limit=1&cursor=" + newest.Next)
	if len(older.Items) != 1 || older.Items[0].ID != first.ID || older.Next != "" {
		t.Fatalf("older history = %+v", older)
	}
}
