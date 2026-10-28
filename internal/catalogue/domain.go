// Package catalogue implements a durable, replay-safe supplier catalogue demo.
package catalogue

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Product retains supplier ordering metadata even after withdrawal.
type Product struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Price   int64  `json:"price"`
	Active  bool   `json:"active"`
	Version int64  `json:"version"`
}

// Record identifies one immutable supplier event independently of delivery.
type Record struct {
	SourceID string  `json:"source_id"`
	Product  Product `json:"product"`
}

// Counts summarize durable outcomes, never worker attempts.
type Counts struct {
	Added     int `json:"added"`
	Changed   int `json:"changed"`
	Withdrawn int `json:"withdrawn"`
	Ignored   int `json:"ignored"`
	Issues    int `json:"issues"`
}

// Result is the immutable outcome for a single source record.
type Result struct {
	SourceID string   `json:"source_id"`
	UpdateID string   `json:"update_id"`
	Kind     string   `json:"kind"`
	Before   *Product `json:"before,omitempty"`
	After    Product  `json:"after"`
	Problem  string   `json:"problem,omitempty"`
	Resolved bool     `json:"resolved"`
}

// Job is durable progress through one immutable supplier update.
type Job struct {
	ID          string `json:"id"`
	Epoch       string `json:"epoch"`
	Scenario    string `json:"scenario"`
	Status      string `json:"status"`
	Cursor      int    `json:"cursor"`
	Offset      int    `json:"offset"`
	Processed   int    `json:"processed"`
	LastChange  int    `json:"last_change"`
	Total       int    `json:"total"`
	Counts      Counts `json:"counts"`
	Token       string `json:"-"`
	LeaseUntil  int64  `json:"lease_until"`
	Revision    int64  `json:"-"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt"`
	Message     string `json:"message"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// Manifest describes an immutable supplier update; Restored only controls availability.
type Manifest struct {
	Epoch    string `json:"epoch"`
	ID       string `json:"id"`
	Scenario string `json:"scenario"`
	Sequence int64  `json:"sequence"`
	Total    int    `json:"total"`
	Pages    int    `json:"pages"`
	Restored bool   `json:"restored"`
}

// Page is the supplier's stable pagination contract.
type Page struct {
	UpdateID string   `json:"update_id"`
	Cursor   int      `json:"cursor"`
	Next     int      `json:"next"`
	Total    int      `json:"total"`
	End      bool     `json:"end"`
	Records  []Record `json:"records"`
}

// ErrConflict means another owner changed the durable state.
var ErrConflict = errors.New("state changed; retry the operation")

// ErrNotFound identifies a missing or no longer retained update.
var ErrNotFound = errors.New("not found")

// ErrInvalid identifies a rejected staff request without exposing storage errors.
var ErrInvalid = errors.New("invalid request")

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

// ValidID accepts bounded identifiers used in paths and database keys.
func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 80 {
		return false
	}
	for _, c := range []byte(id) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Decide applies supplier chronology and normalized business-content equality.
func Decide(old *Product, record Record) Result {
	p := record.Product
	p.Name = strings.TrimSpace(p.Name)
	r := Result{SourceID: record.SourceID, Before: old, After: p}
	switch {
	case !ValidID(p.ID):
		r.Problem = "Missing or invalid product ID; supplier correction must reference this source record."
	case p.Version < 1:
		r.Problem = "Supplier version must be positive."
	case p.Name == "" || len(p.Name) > 160:
		r.Problem = "Product name must contain 1–160 bytes."
	case p.Price < 0 || p.Price > 100_000_000:
		r.Problem = "Price must be between 0 and 100000000 cents."
	}
	if r.Problem != "" {
		r.Kind = "issue"
		return r
	}
	if old != nil && p.Version < old.Version {
		r.Kind = "ignored"
		return r
	}
	if old != nil && p.Version == old.Version {
		r.Kind = "ignored"
		if p != *old {
			r.Kind = "issue"
			r.Problem = "Inconsistent supplier contents at the same version; accepted value preserved."
		}
		return r
	}
	r.Kind = "changed"
	if old != nil && p.ID == old.ID && p.Name == old.Name && p.Price == old.Price && p.Active == old.Active {
		r.Kind = "ignored"
	}
	if old == nil {
		r.Kind = "added"
	}
	if !p.Active && (old == nil || old.Active) {
		r.Kind = "withdrawn"
	}
	return r
}

// Add accounts for an outcome once, inside its persistence transaction.
func (c *Counts) Add(kind string) {
	switch kind {
	case "added":
		c.Added++
	case "changed":
		c.Changed++
	case "withdrawn":
		c.Withdrawn++
	case "ignored":
		c.Ignored++
	case "issue":
		c.Issues++
	}
}

// Unfinished reports whether a supplier lock must remain held.
func (j Job) Unfinished() bool {
	return j.Status == "running" || j.Status == "interrupted"
}

// Completion refuses partial or inconsistent terminal checkpoints.
func (j Job) Completion(end bool) (string, error) {
	if !end || j.Processed != j.Total {
		return "", fmt.Errorf("incomplete supplier update: saved %d of %d records", j.Processed, j.Total)
	}
	if j.Counts.Issues > 0 {
		return "finished_with_issues", nil
	}
	return "completed", nil
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:16])
}
