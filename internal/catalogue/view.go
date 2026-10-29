package catalogue

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type viewQuery struct {
	page, limit                 int
	search, status, sort, order string
}

func parseView(r *http.Request, sorts, statuses []string, defaultOrder string) (viewQuery, error) {
	v := r.URL.Query()
	q := viewQuery{page: 1, limit: 50, search: strings.ToLower(strings.TrimSpace(v.Get("q"))), status: v.Get("status"), sort: v.Get("sort"), order: v.Get("order")}
	if q.sort == "" {
		q.sort = sorts[0]
	}
	if q.order == "" {
		q.order = defaultOrder
	}
	if len(q.search) > 80 || !slices.Contains(sorts, q.sort) || !slices.Contains(statuses, q.status) || q.order != "asc" && q.order != "desc" {
		return q, invalid("invalid table sort or filter")
	}
	for _, field := range []struct {
		name   string
		target *int
		max    int
	}{{name: "page", target: &q.page, max: 1000000}, {name: "limit", target: &q.limit, max: 100}} {
		if raw := v.Get(field.name); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || n > field.max {
				return q, invalid("invalid " + field.name)
			}
			*field.target = n
		}
	}
	return q, nil
}

func (q viewQuery) matches(value string) bool {
	return q.search == "" || strings.Contains(strings.ToLower(value), q.search)
}
func (q viewQuery) less(comparison int) bool {
	if q.order == "desc" {
		return comparison > 0
	}
	return comparison < 0
}
func (q viewQuery) bounds(total int) (start, end, page, pages int) {
	pages = (total + q.limit - 1) / q.limit
	if pages == 0 {
		return 0, 0, 0, 0
	}
	page = min(q.page, pages)
	start = (page - 1) * q.limit
	end = min(start+q.limit, total)
	return start, end, page, pages
}

func (a *API) products(w http.ResponseWriter, r *http.Request) {
	q, err := parseView(r, []string{"id", "name", "price", "status"}, []string{"", "active", "withdrawn"}, "asc")
	if err != nil {
		a.fail(w, err)
		return
	}
	snapshot, err := a.catalogueView.snapshot(r.Context(), a.Store)
	if err != nil {
		a.fail(w, err)
		return
	}
	products := make([]productView, 0, len(snapshot.Products))
	for _, p := range snapshot.Products {
		if !q.matches(p.ID+" "+p.Name) || q.status == "active" && !p.Active || q.status == "withdrawn" && p.Active {
			continue
		}
		products = append(products, productView{ID: p.ID, Name: p.Name, Price: p.Price, Active: p.Active})
	}
	sort.Slice(products, func(i, j int) bool {
		left, right := products[i], products[j]
		comparison := strings.Compare(left.ID, right.ID)
		switch q.sort {
		case "name":
			comparison = strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
		case "price":
			comparison = cmp.Compare(left.Price, right.Price)
		case "status":
			comparison = strings.Compare(productStatus(left.Active), productStatus(right.Active))
		}
		if comparison == 0 {
			comparison = strings.Compare(left.ID, right.ID)
		}
		return q.less(comparison)
	})
	start, end, page, pages := q.bounds(len(products))
	etag := `"` + digest(fmt.Sprintf("%s/%d/%d/%d/%s/%s/%s/%s", snapshot.Epoch, snapshot.Generation, page, q.limit, q.search, q.status, q.sort, q.order)) + `"`
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	writeJSON(w, 200, map[string]any{"items": products[start:end], "page": page, "page_size": q.limit, "total_pages": pages, "total_items": len(products), "catalogue_total": len(snapshot.Products), "generation": snapshot.Generation})
}

func productStatus(active bool) string {
	if active {
		return "active"
	}
	return "withdrawn"
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	q, err := parseView(r, []string{"created_at", "status", "processed"}, []string{"", "running", "interrupted", "completed", "finished_with_issues", "unavailable", "abandoned"}, "desc")
	if err != nil {
		a.fail(w, err)
		return
	}
	c, err := a.Store.Control(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	rows, err := a.Store.allRows(r.Context(), "JOBS")
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
		if (j.Epoch == c.Epoch || j.ID == c.Current) && q.matches(j.ID+" "+j.Scenario) && (q.status == "" || q.status == j.Status) {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		left, right := jobs[i], jobs[j]
		comparison := strings.Compare(left.CreatedAt, right.CreatedAt)
		switch q.sort {
		case "status":
			comparison = strings.Compare(left.Status, right.Status)
		case "processed":
			comparison = cmp.Compare(left.Processed, right.Processed)
		}
		if comparison == 0 {
			comparison = strings.Compare(left.ID, right.ID)
		}
		return q.less(comparison)
	})
	start, end, page, pages := q.bounds(len(jobs))
	writeJSON(w, 200, map[string]any{"items": jobs[start:end], "page": page, "page_size": q.limit, "total_pages": pages, "total_items": len(jobs)})
}

func (a *API) changes(w http.ResponseWriter, r *http.Request) {
	q, err := parseView(r, []string{"source_id", "name", "kind"}, []string{"", "added", "changed", "withdrawn"}, "asc")
	if err != nil {
		a.fail(w, err)
		return
	}
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
	results, err := a.changeView.snapshot(r.Context(), a.Store, j)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeResults(w, q, results, false)
}

func (a *API) issues(w http.ResponseWriter, r *http.Request) {
	q, err := parseView(r, []string{"source_id", "name", "status"}, []string{"", "open", "resolved"}, "asc")
	if err != nil {
		a.fail(w, err)
		return
	}
	snapshot, err := a.catalogueView.snapshot(r.Context(), a.Store)
	if err != nil {
		a.fail(w, err)
		return
	}
	products := make(map[string]Product, len(snapshot.Products))
	for _, p := range snapshot.Products {
		products[p.ID] = p
	}
	rows, err := a.Store.allRows(r.Context(), snapshot.Epoch+"|ISSUES")
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
		p, ok := products[result.After.ID]
		result.Resolved = ok && p.Version > result.After.Version
		results = append(results, result)
	}
	a.writeResults(w, q, results, true)
}

func (a *API) writeResults(w http.ResponseWriter, q viewQuery, results []Result, issues bool) {
	filtered := make([]Result, 0, len(results))
	for _, r := range results {
		status := r.Kind
		if issues {
			status = issueStatus(r.Resolved)
		}
		if q.matches(r.After.Name+" "+r.After.ID+" "+r.SourceID+" "+r.UpdateID+" "+r.Problem) && (q.status == "" || q.status == status) {
			filtered = append(filtered, r)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		left, right := filtered[i], filtered[j]
		comparison := strings.Compare(left.SourceID, right.SourceID)
		switch q.sort {
		case "name":
			comparison = strings.Compare(strings.ToLower(left.After.Name), strings.ToLower(right.After.Name))
		case "kind":
			comparison = strings.Compare(left.Kind, right.Kind)
		case "status":
			comparison = strings.Compare(issueStatus(left.Resolved), issueStatus(right.Resolved))
		}
		if comparison == 0 {
			comparison = strings.Compare(left.UpdateID+left.SourceID, right.UpdateID+right.SourceID)
		}
		return q.less(comparison)
	})
	start, end, page, pages := q.bounds(len(filtered))
	writeJSON(w, 200, map[string]any{"items": filtered[start:end], "page": page, "page_size": q.limit, "total_pages": pages, "total_items": len(filtered)})
}

func issueStatus(resolved bool) string {
	if resolved {
		return "resolved"
	}
	return "open"
}
