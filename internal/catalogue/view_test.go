package catalogue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func viewResponse(t *testing.T, handler http.Handler, path string) struct {
	Items []json.RawMessage `json:"items"`
	Page  int               `json:"page"`
	Pages int               `json:"total_pages"`
	Total int               `json:"total_items"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("%s returned %d: %s", path, rec.Code, rec.Body.String())
	}
	var result struct {
		Items []json.RawMessage `json:"items"`
		Page  int               `json:"page"`
		Pages int               `json:"total_pages"`
		Total int               `json:"total_items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIntegrationGlobalSortFiltersAndExactPageTotals(t *testing.T) {
	s := integrationStore(t)
	worker := integrationWorker(t, s)
	api := (&API{Store: s}).Handler()
	first := viewResponse(t, api, "/api/products?limit=25&sort=price&order=desc")
	if first.Total != 100 || first.Pages != 4 || first.Page != 1 || len(first.Items) != 25 {
		t.Fatalf("initial pages: %+v", first)
	}
	var product productView
	if err := json.Unmarshal(first.Items[0], &product); err != nil {
		t.Fatal(err)
	}
	if product.ID != "P00027" || product.Price != 999 {
		t.Fatalf("products(price descending) first = %+v, want P00027 at 999 cents", product)
	}
	last := viewResponse(t, api, "/api/products?limit=25&page=4&sort=price&order=desc")
	if err := json.Unmarshal(last.Items[24], &product); err != nil {
		t.Fatal(err)
	}
	if product.ID != "P00095" || product.Price != 515 {
		t.Fatalf("products(price descending) last = %+v, want P00095 at 515 cents", product)
	}
	for _, tt := range []struct {
		query  string
		wantID string
	}{
		{query: "", wantID: "P00001"},
		{query: "?sort=id&order=desc", wantID: "P00100"},
		{query: "?sort=price&order=asc", wantID: "P00095"},
	} {
		result := viewResponse(t, api, "/api/products"+tt.query)
		if err := json.Unmarshal(result.Items[0], &product); err != nil {
			t.Fatal(err)
		}
		if product.ID != tt.wantID {
			t.Errorf("products(%q) first ID = %q, want %q", tt.query, product.ID, tt.wantID)
		}
	}
	search := viewResponse(t, api, "/api/products?q=P00100")
	if search.Total != 1 || search.Pages != 1 {
		t.Fatalf("search missed a later page: %+v", search)
	}
	j := startTest(t, s, "normal")
	finish(t, worker, j.ID)
	withdrawn := viewResponse(t, api, "/api/products?status=withdrawn&limit=2")
	if withdrawn.Total != 5 || withdrawn.Pages != 3 {
		t.Fatalf("withdrawn totals = %+v", withdrawn)
	}
	all := viewResponse(t, api, "/api/products?page=3")
	if all.Total != 110 || len(all.Items) != 10 || all.Pages != 3 {
		t.Fatalf("added page = %+v", all)
	}
	changes := viewResponse(t, api, "/api/changes?update_id="+j.ID+"&status=changed&limit=7")
	if changes.Total != 20 || changes.Pages != 3 {
		t.Fatalf("filtered changes = %+v", changes)
	}
	empty := viewResponse(t, api, "/api/products?q=absent")
	if empty.Total != 0 || empty.Pages != 0 || empty.Page != 0 || len(empty.Items) != 0 {
		t.Fatalf("empty view = %+v", empty)
	}
	invalid := startTest(t, s, "invalid")
	finish(t, worker, invalid.ID)
	issues := viewResponse(t, api, "/api/issues?status=open")
	if issues.Total != 2 {
		t.Fatalf("open issues = %+v", issues)
	}
	correction := startTest(t, s, "correction")
	finish(t, worker, correction.ID)
	resolved := viewResponse(t, api, "/api/issues?status=resolved")
	if resolved.Total != 1 {
		t.Fatalf("resolved filter = %+v", resolved)
	}
}

func TestIntegrationReadProjectionsSurviveReplayAndColdCache(t *testing.T) {
	s := integrationStore(t)
	worker := integrationWorker(t, s)
	api := (&API{Store: s}).Handler()
	viewResponse(t, api, "/api/products")
	j := startTest(t, s, "duplicates")
	finish(t, worker, j.ID)
	for _, handler := range []http.Handler{api, (&API{Store: s}).Handler()} {
		products := viewResponse(t, handler, "/api/products")
		if products.Total != 110 {
			t.Fatalf("projection product total = %d", products.Total)
		}
		changes := viewResponse(t, handler, "/api/changes?update_id="+j.ID)
		if changes.Total != 35 {
			t.Fatalf("duplicate changes = %d", changes.Total)
		}
	}
	control, err := s.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.rangeRows(context.Background(), control.Epoch+"|FEED", 0, control.DataRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 35 {
		t.Fatalf("replay inflated product deltas: %d", len(rows))
	}
}

func TestSmallDemoFixturesKeepThePerformanceImportSeparate(t *testing.T) {
	for _, scenario := range []string{"baseline", "outage", "slow"} {
		m, err := NewManifest("small", scenario, 1)
		if err != nil {
			t.Fatal(err)
		}
		if m.Total != 100 {
			t.Fatalf("%s total = %d", scenario, m.Total)
		}
		page, err := FixturePage(m, 0)
		if err != nil {
			t.Fatal(err)
		}
		if scenario != "baseline" && len(page.Records) != 10 {
			t.Fatalf("%s batch = %d", scenario, len(page.Records))
		}
	}
	full, err := NewManifest("performance", "full", 1)
	if err != nil {
		t.Fatal(err)
	}
	if full.Total != 10000 || full.Pages != 100 {
		t.Fatalf("performance fixture = %+v", full)
	}
}

func TestIntegrationChangeCacheHandlesAQueuedOlderRequest(t *testing.T) {
	s := integrationStore(t)
	worker := integrationWorker(t, s)
	worker.RecordLimit = 4
	j := startTest(t, s, "normal")
	if _, err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	older, err := s.Job(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.RecordLimit = 0
	newer := finish(t, worker, j.ID)
	var cache changeCache
	latest, err := cache.snapshot(context.Background(), s, newer)
	if err != nil || len(latest) != 35 {
		t.Fatalf("latest changes = %d, %v", len(latest), err)
	}
	queued, err := cache.snapshot(context.Background(), s, older)
	if err != nil || len(queued) != 4 {
		t.Fatalf("queued changes = %d, %v", len(queued), err)
	}
}
