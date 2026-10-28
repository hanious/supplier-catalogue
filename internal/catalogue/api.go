package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// API serves staff operations; wakeups never replace durable job state.
type API struct {
	Store  *Store
	Origin string
	Wake   func(context.Context) error
	Static http.Handler
}

// Handler validates demo operations and serves bounded catalogue views.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.Store.Control(r.Context()); err != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("POST /api/updates/start", a.start)
	mux.HandleFunc("POST /api/updates/{id}/resume", a.resume)
	mux.HandleFunc("POST /api/demo/publish", a.publish)
	mux.HandleFunc("POST /api/demo/restore", a.restore)
	mux.HandleFunc("POST /api/demo/reset", a.reset)
	mux.HandleFunc("GET /api/products", a.products)
	mux.HandleFunc("GET /api/history", a.history)
	mux.HandleFunc("GET /api/changes", a.changes)
	mux.HandleFunc("GET /api/issues", a.issues)
	if a.Static != nil {
		mux.Handle("/", a.Static)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != a.Origin {
			http.Error(w, "origin not allowed", 403)
			return
		}
		if origin == a.Origin && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, If-None-Match")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "ETag")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		mux.ServeHTTP(w, r)
	})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	status := 503
	if errors.Is(err, ErrInvalid) {
		status = 400
	}
	if errors.Is(err, ErrConflict) {
		status = 409
	}
	if errors.Is(err, ErrNotFound) {
		status = 404
	}
	// Raw storage errors stay out of public responses.
	if status == 503 {
		slog.Error("request failed", "error", err)
		writeJSON(w, 503, map[string]string{"error": "Storage unavailable. Retry; saved catalogue changes are preserved."})
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *API) notify(ctx context.Context) {
	if a.Wake != nil {
		if err := a.Wake(ctx); err != nil {
			slog.Warn("work saved; scheduled dispatcher will recover queue publication", "error", err)
		}
	}
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	c, err := a.Store.Control(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	var job *Job
	if c.Current != "" && c.Current != "baseline" {
		j, e := a.Store.Job(r.Context(), c.Current)
		if e != nil {
			a.fail(w, e)
			return
		}
		job = &j
	}
	writeJSON(w, 200, map[string]any{"job": job, "generation": c.Generation, "epoch": c.Epoch, "latest": c.Latest, "scenarios": Scenarios(), "baseline_ready": c.Current != "", "cleanup_pending": len(c.Retired) > 0})
}

type startRequest struct {
	Scenario string `json:"scenario"`
	UpdateID string `json:"update_id"`
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("invalid request body")
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid("invalid trailing request body")
	}
	return nil
}

func (a *API) start(w http.ResponseWriter, r *http.Request) {
	var input startRequest
	if err := decode(r, &input); err != nil {
		a.fail(w, err)
		return
	}
	if input.Scenario == "baseline" {
		a.fail(w, invalid("invalid scenario; use Reset demo"))
		return
	}
	if input.UpdateID != "" && !ValidID(input.UpdateID) {
		a.fail(w, invalid("invalid update ID"))
		return
	}
	j, err := a.Store.Start(r.Context(), input.Scenario, r.Header.Get("Idempotency-Key"), input.UpdateID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.notify(r.Context())
	writeJSON(w, 202, j)
}

func (a *API) resume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidID(id) {
		a.fail(w, invalid("invalid update ID"))
		return
	}
	j, err := a.Store.Resume(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.notify(r.Context())
	writeJSON(w, 202, j)
}

func (a *API) publish(w http.ResponseWriter, r *http.Request) {
	var input startRequest
	if err := decode(r, &input); err != nil {
		a.fail(w, err)
		return
	}
	m, err := a.Store.Publish(r.Context(), input.Scenario, r.Header.Get("Idempotency-Key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 201, m)
}

func (a *API) restore(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UpdateID string `json:"update_id"`
	}
	if err := decode(r, &input); err != nil {
		a.fail(w, err)
		return
	}
	if !ValidID(input.UpdateID) {
		a.fail(w, invalid("invalid update ID"))
		return
	}
	if err := a.Store.Restore(r.Context(), input.UpdateID); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"message": "Supplier restored. Resume update to continue."})
}

func (a *API) reset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := decode(r, &input); err != nil {
		a.fail(w, err)
		return
	}
	if input.Confirm != "reset synthetic demo" {
		a.fail(w, invalid("required reset confirmation"))
		return
	}
	j, err := a.Store.Start(r.Context(), "baseline", r.Header.Get("Idempotency-Key"), "")
	if err != nil {
		a.fail(w, err)
		return
	}
	a.notify(r.Context())
	writeJSON(w, 202, j)
}

func pagination(r *http.Request) (string, int, error) {
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 180 {
		return "", 0, invalid("invalid cursor")
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return "", 0, invalid("invalid limit")
		}
		limit = n
	}
	return cursor, limit, nil
}

func (a *API) products(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pagination(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	filter := r.URL.Query().Get("status")
	if len(search) > 80 || (filter != "" && filter != "active" && filter != "withdrawn") {
		a.fail(w, invalid("invalid product filter"))
		return
	}
	c, err := a.Store.Control(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	etag := `"` + digest(fmt.Sprintf("%s/%d/%s/%d/%s/%s", c.Epoch, c.Generation, cursor, limit, search, filter)) + `"`
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	// At most one bounded key page is examined. Filters may return an empty page
	// with a continuation; the UI offers the next page rather than scanning 10k rows.
	rows, next, err := a.Store.Rows(r.Context(), c.Epoch+"|PRODUCTS", cursor, limit, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	products := make([]Product, 0, len(rows))
	for _, row := range rows {
		var p Product
		if err = json.Unmarshal([]byte(row.Data), &p); err != nil {
			a.fail(w, err)
			return
		}
		if search != "" && !strings.Contains(strings.ToLower(p.Name+" "+p.ID), search) {
			continue
		}
		if filter == "active" && !p.Active || filter == "withdrawn" && p.Active {
			continue
		}
		products = append(products, p)
	}
	writeJSON(w, 200, map[string]any{"items": products, "next_cursor": next, "generation": c.Generation})
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := pagination(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	c, err := a.Store.Control(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	// Publications are bounded to 50 per epoch. Include the retiring epoch in
	// this bounded key query, then page the current epoch in actual time order.
	rows, _, err := a.Store.Rows(r.Context(), "JOBS", "", 200, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	jobs := make([]Job, 0, len(rows))
	for _, row := range rows {
		var j Job
		if err = json.Unmarshal([]byte(row.Data), &j); err != nil {
			a.fail(w, err)
			return
		}
		if j.Epoch == c.Epoch || j.ID == c.Current {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt > jobs[j].CreatedAt })
	start := 0
	if cursor != "" {
		found := false
		for i, j := range jobs {
			if j.ID == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			a.fail(w, invalid("history cursor is no longer retained"))
			return
		}
	}
	end := min(start+limit, len(jobs))
	next := ""
	if end < len(jobs) {
		next = jobs[end-1].ID
	}
	jobs = jobs[start:end]
	writeJSON(w, 200, map[string]any{"items": jobs, "next_cursor": next})
}

func (a *API) changes(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("update_id")
	if !ValidID(id) {
		a.fail(w, invalid("invalid update ID"))
		return
	}
	j, err := a.Store.Job(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.results(w, r, j.Epoch+"|RESULTS|"+id, false)
}

func (a *API) issues(w http.ResponseWriter, r *http.Request) {
	c, err := a.Store.Control(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	a.results(w, r, c.Epoch+"|ISSUES", true)
}

func (a *API) results(w http.ResponseWriter, r *http.Request, pk string, issues bool) {
	cursor, limit, err := pagination(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	rows, next, err := a.Store.Rows(r.Context(), pk, cursor, limit, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	results := make([]Result, 0, len(rows))
	for _, row := range rows {
		var result Result
		if err = json.Unmarshal([]byte(row.Data), &result); err != nil {
			a.fail(w, err)
			return
		}
		if !issues && (result.Kind == "ignored" || result.Kind == "issue") {
			continue
		}
		if issues && ValidID(result.After.ID) {
			epoch := strings.SplitN(pk, "|", 2)[0]
			p, e := a.Store.Product(r.Context(), epoch, result.After.ID)
			if e != nil {
				a.fail(w, e)
				return
			}
			result.Resolved = p != nil && p.Version > result.After.Version
		}
		results = append(results, result)
	}
	writeJSON(w, 200, map[string]any{"items": results, "next_cursor": next})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("response ended", "error", err)
	}
}
