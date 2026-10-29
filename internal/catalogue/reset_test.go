package catalogue

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationResetRejectsPublicationAndPreservesVisibleProducts(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	ctx := t.Context()
	api := (&API{Store: s}).Handler()
	before := viewResponse(t, api, "/api/products")
	reset := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/demo/reset", strings.NewReader(`{"confirm":"reset synthetic demo"}`))
	request.Header.Set("Idempotency-Key", "reset-publication")
	api.ServeHTTP(reset, request)
	if reset.Code != http.StatusAccepted {
		t.Fatalf("POST /api/demo/reset = %d: %s, want 202", reset.Code, reset.Body.String())
	}
	var job Job
	if err := json.Unmarshal(reset.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "interrupted"} {
		if status == "interrupted" {
			pauseAfterRecord(t, w, job.ID)
		}
		publish := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/demo/publish", strings.NewReader(`{"scenario":"normal"}`))
		request.Header.Set("Idempotency-Key", "publish-during-reset")
		api.ServeHTTP(publish, request)
		if publish.Code != http.StatusConflict {
			t.Errorf("publish during %s reset = %d: %s, want 409", status, publish.Code, publish.Body.String())
		}
		visible := viewResponse(t, api, "/api/products")
		if visible.Total != before.Total {
			t.Errorf("products during %s reset = %d, want %d", status, visible.Total, before.Total)
		}
	}
	replayed, err := s.Reset(ctx, "reset-publication")
	if err != nil || replayed.ID != job.ID || replayed.Status != "interrupted" {
		t.Fatalf("Reset(replayed key) = %+v, %v, want original interrupted job %s", replayed, err, job.ID)
	}
	if _, err = s.Resume(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	w.RecordLimit = 0
	if done := finish(t, w, job.ID); done.Status != "completed" {
		t.Fatalf("resumed reset status = %q, want completed", done.Status)
	}
	cleanupAll(t, s)
	m, err := s.Publish(ctx, "normal", "publish-during-reset")
	if err != nil {
		t.Fatalf("Publish(after reset) = %v, want success", err)
	}
	j, err := s.Start(ctx, "normal", "start-after-reset", "")
	if err != nil || j.ID != m.ID {
		t.Fatalf("Start(after reset) = %+v, %v, want published update %s", j, err, m.ID)
	}
	if done := finish(t, w, j.ID); done.Status != "completed" {
		t.Errorf("post-reset update status = %q, want completed", done.Status)
	}
}

func TestIntegrationRepeatedInterruptedResetsRetireEveryStagingEpoch(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	ctx := t.Context()
	initial, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var abandoned []Job
	for _, requestID := range []string{"first-reset", "second-reset"} {
		job, err := s.Reset(ctx, requestID)
		if err != nil {
			t.Fatalf("Reset(%q) = %v, want success", requestID, err)
		}
		abandoned = append(abandoned, pauseAfterRecord(t, w, job.ID))
	}
	job, err := s.Reset(ctx, "final-reset")
	if err != nil {
		t.Fatalf("Reset(final) = %v, want success with earlier retired staging data", err)
	}
	for _, old := range abandoned {
		j, err := s.Job(ctx, old.ID)
		if err != nil || j.Status != "abandoned" {
			t.Errorf("Job(%s) = %+v, %v, want abandoned", old.ID, j, err)
		}
		if err := s.Commit(ctx, &old, Record{SourceID: "stale-event", Product: BaselineProduct(2)}); !errors.Is(err, ErrConflict) {
			t.Errorf("Commit(abandoned reset %s) = %v, want conflict", old.ID, err)
		}
	}
	visible := viewResponse(t, (&API{Store: s}).Handler(), "/api/products")
	if visible.Total != DemoProductCount {
		t.Errorf("products before final reset completion = %d, want %d", visible.Total, DemoProductCount)
	}
	w.RecordLimit = 0
	if done := finish(t, w, job.ID); done.Status != "completed" {
		t.Fatalf("final reset status = %q, want completed", done.Status)
	}
	cleanupAll(t, s)
	for _, old := range abandoned {
		for _, pk := range []string{old.Epoch + "|PRODUCTS", old.Epoch + "|RESULTS|" + old.ID, old.Epoch + "|CHANGES|" + old.ID} {
			rows, err := s.allRows(ctx, pk)
			if err != nil || len(rows) != 0 {
				t.Errorf("retired partition %q = %d rows, %v, want empty", pk, len(rows), err)
			}
		}
		if _, err := s.Job(ctx, old.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Job(abandoned %s) after cleanup = %v, want not found", old.ID, err)
		}
		if _, err := s.Manifest(ctx, old.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Manifest(abandoned %s) after cleanup = %v, want not found", old.ID, err)
		}
	}
	oldProducts, err := s.allRows(ctx, initial.Epoch+"|PRODUCTS")
	if err != nil || len(oldProducts) != 0 {
		t.Errorf("original visible epoch after cleanup = %d products, %v, want empty", len(oldProducts), err)
	}
	current := viewResponse(t, (&API{Store: s}).Handler(), "/api/products")
	if current.Total != DemoProductCount {
		t.Errorf("products after cleanup = %d, want %d", current.Total, DemoProductCount)
	}
	for _, requestID := range []string{"first-reset", "second-reset"} {
		var request struct{ JobID string }
		if _, err := s.read(ctx, "REQUESTS", requestID, &request); !errors.Is(err, ErrNotFound) {
			t.Errorf("request key %q after cleanup = %v, want not found", requestID, err)
		}
	}
}

func TestIntegrationResetAbandonsAnInterruptedOrdinaryImport(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	ctx := t.Context()
	old := startTest(t, s, "normal")
	pauseAfterRecord(t, w, old.ID)
	job, err := s.Reset(ctx, "replace-normal")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Control(ctx)
	if err != nil || len(c.Retired) != 0 {
		t.Fatalf("Control(during reset) = %+v, %v, want visible epoch retained", c, err)
	}
	w.RecordLimit = 0
	if done := finish(t, w, job.ID); done.Status != "completed" {
		t.Fatalf("reset status = %q, want completed", done.Status)
	}
	cleanupAll(t, s)
	if _, err = s.Job(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Job(previous import) after cleanup = %v, want not found", err)
	}
	current := viewResponse(t, (&API{Store: s}).Handler(), "/api/products")
	if current.Total != DemoProductCount {
		t.Errorf("reset products = %d, want %d", current.Total, DemoProductCount)
	}
}

func TestIntegrationControlRejectsAnInvalidEpoch(t *testing.T) {
	s := integrationStore(t)
	c, err := s.Control(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.Epoch = ""
	write, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(t.Context(), write); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Control(t.Context()); err == nil {
		t.Error("Control(empty epoch) succeeded, want storage validation error")
	}
}

func TestIntegrationResetRejectsAPublicationFromARetiredEpoch(t *testing.T) {
	s := integrationStore(t)
	w := integrationWorker(t, s)
	ctx := t.Context()
	old, err := s.Publish(ctx, "normal", "old-publication")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := s.Reset(ctx, "retire-publication")
	if err != nil {
		t.Fatal(err)
	}
	if done := finish(t, w, baseline.ID); done.Status != "completed" {
		t.Fatalf("reset status = %q, want completed", done.Status)
	}
	// Retired manifests still exist until cleanup, but cannot create new jobs.
	if _, err := s.Start(ctx, "normal", "start-old-publication", old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Start(retired publication %s) = %v, want not found", old.ID, err)
	}
	fresh, err := s.Publish(ctx, "normal", "current-publication")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Start(ctx, "normal", "start-current-publication", fresh.ID)
	if err != nil || job.Epoch != baseline.Epoch {
		t.Fatalf("Start(current publication) = %+v, %v, want visible epoch %s", job, err, baseline.Epoch)
	}
	if done := finish(t, w, job.ID); done.Status != "completed" {
		t.Fatalf("current publication status = %q, want completed", done.Status)
	}
	products := viewResponse(t, (&API{Store: s}).Handler(), "/api/products")
	if products.Total != DemoProductCount+10 {
		t.Errorf("products after current publication = %d, want %d", products.Total, DemoProductCount+10)
	}
	cleanupAll(t, s)
}

func pauseAfterRecord(t *testing.T, w *Worker, id string) Job {
	t.Helper()
	w.RecordLimit = 1
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	j, err := w.Store.Acquire(t.Context(), id, "interrupted-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	j.Status = "interrupted"
	if err := w.Store.Release(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func cleanupAll(t *testing.T, s *Store) {
	t.Helper()
	for range 200 {
		more, err := s.Cleanup(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("Cleanup() exceeded 200 calls, want all retired demo data removed")
}
