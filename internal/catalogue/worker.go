package catalogue

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SupplierClient fetches only a configured, trusted supplier origin.
type SupplierClient struct {
	BaseURL string
	HTTP    *http.Client
}

// Fetch requests the exact retained supplier update and stable cursor.
func (s *SupplierClient) Fetch(ctx context.Context, id string, cursor int) (Page, error) {
	var page Page
	if !ValidID(id) {
		return page, fmt.Errorf("invalid update ID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/updates/"+url.PathEscape(id)+"?cursor="+strconv.Itoa(cursor), nil)
	if err != nil {
		return page, err
	}
	response, err := s.HTTP.Do(req)
	if err != nil {
		return page, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return page, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return page, fmt.Errorf("supplier HTTP %d", response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 256*1024)).Decode(&page); err != nil {
		return page, err
	}
	if page.UpdateID != id || page.Cursor != cursor || page.Next != cursor+1 || len(page.Records) == 0 || len(page.Records) > 100 || page.Total < 1 || page.Total > 10000 {
		return page, fmt.Errorf("invalid immutable supplier page")
	}
	for _, record := range page.Records {
		if !ValidID(record.SourceID) {
			return page, fmt.Errorf("supplier record lacks a stable source identity")
		}
	}
	return page, nil
}

// Worker processes bounded slices; durable state is authoritative over queue delivery.
type Worker struct {
	Store       *Store
	Supplier    *SupplierClient
	Lease       time.Duration
	Slice       time.Duration
	Interval    time.Duration
	RecordLimit int
	// Fault is integration-only fault injection, never exposed by the HTTP API.
	Fault func(string) error
}

// Tick processes current work synchronously and returns whether another wakeup is useful.
func (w *Worker) Tick(ctx context.Context) (bool, error) {
	c, err := w.Store.Control(ctx)
	if err != nil {
		return false, err
	}
	if c.Active == "" {
		return w.Store.Cleanup(ctx)
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return false, err
	}
	j, err := w.Store.Acquire(ctx, c.Active, fmt.Sprintf("%x", token), w.Lease)
	if errors.Is(err, ErrConflict) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	deadline := time.Now().Add(w.Slice)
	processedInSlice := 0
	for time.Now().Before(deadline) {
		page, e := w.Supplier.Fetch(ctx, j.ID, j.Cursor)
		if e != nil {
			j.Attempts++
			j.Message = "Supplier unavailable. Restore supplier, then resume this update."
			j.NextAttempt = time.Now().Add(time.Duration(1<<min(j.Attempts-1, 3)) * time.Second).UnixMilli()
			if j.Attempts >= 3 {
				j.Status = "interrupted"
				j.NextAttempt = 0
			}
			if errors.Is(e, ErrNotFound) {
				j.Status = "unavailable"
				j.Message = "This supplier update is no longer retained and cannot be resumed. Existing catalogue preserved."
			}
			return j.Status == "running", w.Store.Release(ctx, &j)
		}
		if page.Total != j.Total {
			j.Status = "interrupted"
			j.Message = "Supplier total changed; original update preserved."
			return false, w.Store.Release(ctx, &j)
		}
		for index, record := range page.Records {
			if index < j.Offset {
				continue
			}
			if !time.Now().Before(deadline) {
				return true, w.Store.Release(ctx, &j)
			}
			if w.Fault != nil {
				if e = w.Fault("before_record"); e != nil {
					return true, e
				}
			}
			processed := j.Processed
			if e = w.Store.Commit(ctx, &j, record); e != nil {
				return true, e
			}
			if w.Fault != nil {
				if e = w.Fault("after_record"); e != nil {
					return true, e
				}
			}
			if j.Processed != processed {
				processedInSlice++
				if e = wait(ctx, w.Interval); e != nil {
					return true, e
				}
			}
			if w.RecordLimit > 0 && processedInSlice >= w.RecordLimit && index < len(page.Records)-1 {
				return true, w.Store.Release(ctx, &j)
			}
		}
		if w.Fault != nil {
			if e = w.Fault("before_checkpoint"); e != nil {
				return true, e
			}
		}
		if e = w.Store.Checkpoint(ctx, &j, page); e != nil {
			return true, e
		}
		if w.Fault != nil {
			if e = w.Fault("after_checkpoint"); e != nil {
				return true, e
			}
		}
		if !j.Unfinished() {
			return false, nil
		}
		if w.RecordLimit > 0 && processedInSlice >= w.RecordLimit {
			return true, w.Store.Release(ctx, &j)
		}
	}
	return true, w.Store.Release(ctx, &j)
}

// SupplierHandler exposes deterministic fixtures through a separate HTTP service.
func SupplierHandler(store *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /updates/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("id")
		if !ValidID(id) {
			http.Error(w, "invalid ID", 400)
			return
		}
		cursor, err := strconv.Atoi(r.URL.Query().Get("cursor"))
		if err != nil {
			http.Error(w, "invalid cursor", 400)
			return
		}
		m, err := store.Manifest(r.Context(), id)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "update no longer retained", 404)
			return
		}
		if err != nil {
			http.Error(w, "supplier storage unavailable", 503)
			return
		}
		if m.Scenario == "outage" && cursor >= 3 && !m.Restored {
			http.Error(w, "simulated supplier outage", 503)
			return
		}
		if m.Scenario == "slow" {
			if err = wait(r.Context(), time.Second); err != nil {
				return
			}
		}
		page, err := FixturePage(m, cursor)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, 200, page)
	})
	return mux
}
