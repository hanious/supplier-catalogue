package catalogue

import "testing"

func TestOrderingAndValidation(t *testing.T) {
	old := Product{ID: "P00001", Name: "Canvas tote", Price: 1200, Active: true, Version: 3}
	withdrawn := old
	withdrawn.Active = false
	withdrawn.Version = 4
	tests := []struct {
		name     string
		old      *Product
		incoming Product
		kind     string
	}{
		{"addition", nil, old, "added"},
		{"duplicate", &old, old, "ignored"},
		{"stale", &withdrawn, old, "ignored"},
		{"withdrawal", &old, withdrawn, "withdrawn"},
		{"newer reactivation", &withdrawn, Product{ID: old.ID, Name: old.Name, Price: 1300, Active: true, Version: 5}, "changed"},
		{"same version conflict", &old, Product{ID: old.ID, Name: old.Name, Price: 999, Active: true, Version: 3}, "issue"},
		{"negative price", &old, Product{ID: old.ID, Name: old.Name, Price: -1, Active: true, Version: 4}, "issue"},
		{"missing id", nil, Product{Name: "Unknown", Version: 1}, "issue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.old, Record{SourceID: "event-1", Product: tt.incoming})
			if got.Kind != tt.kind {
				t.Fatalf("kind = %q, want %q", got.Kind, tt.kind)
			}
			if tt.kind == "issue" && got.Problem == "" {
				t.Fatal("rejected record must explain correction")
			}
		})
	}
}

func TestCompletionRequiresTerminalMarkerAndEveryOutcome(t *testing.T) {
	j := Job{Total: 100, Processed: 99}
	if _, err := j.Completion(true); err == nil {
		t.Fatal("partial update completed")
	}
	j.Processed = 100
	if _, err := j.Completion(false); err == nil {
		t.Fatal("missing terminal marker accepted")
	}
	j.Counts.Issues = 1
	if got, err := j.Completion(true); err != nil || got != "finished_with_issues" {
		t.Fatalf("completion = %q, %v", got, err)
	}
}
