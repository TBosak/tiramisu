package audiobookimport

import (
	"context"
	"errors"
	"math"
	"testing"
)

// Requirement A7: every considered work yields a deterministic result row
// containing work id, ordered discovery reasons, terminal status, and a
// canonical safe reason code. Rows and errors never contain tokens, magnets,
// torrent bytes, raw download URLs, or provider payloads.
//
// Requirement I1: all limits are positive and have hard upper bounds;
// arithmetic for caps/volume positions/result counts cannot overflow;
// zero-value/typed-nil dependencies and empty required ids fail before
// calls.

func TestRunner_ResultRow_HasCanonicalStatusAndReason(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "w1")

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(result.Rows))
	}
	row := result.Rows[0]
	switch row.Status {
	case StatusPlanned, StatusImported, StatusExisting, StatusSkipped, StatusFailed:
	default:
		t.Errorf("Status = %q, want one of the five canonical terminal statuses", row.Status)
	}
	if row.WorkID != "w1" {
		t.Errorf("WorkID = %q, want %q", row.WorkID, "w1")
	}
	// A7 requires "a canonical safe reason code" for every row, not just a
	// canonical status: a successfully planned candidate must still carry a
	// nonempty, canonical Reason (RunReasonOK) rather than leaving the zero
	// value, which would be indistinguishable from "the runner forgot to set
	// a reason."
	if row.Reason != RunReasonOK {
		t.Errorf("Reason = %q, want %q for a successfully planned candidate", row.Reason, RunReasonOK)
	}
}

// TestRunner_Result_NeverLeaksSecretsOrPayloads first pins the nonvacuous
// shape of the result (exactly one row, StatusFailed, RunReasonSourceError)
// before checking for leaked text: a version of this test that only looped
// over result.Rows would pass trivially if the runner silently dropped the
// failed candidate instead of recording it, which proves nothing about
// redaction.
func TestRunner_Result_NeverLeaksSecretsOrPayloads(t *testing.T) {
	deps, provider, _, _, selector, _, _ := newRunnerDeps()
	provider.details["leaky"] = sampleWorkDetail("leaky", "Leaky", "a1", "Author", "rec-leaky")
	leakedMagnet := "magnet:?xt=urn:btih:deadbeef&tr=http://tracker.example/announce?apikey=SECRETVALUE"
	selector.byWork["leaky"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return SelectionResult{}, errors.New("fetch failed for " + leakedMagnet)
	}

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"leaky"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (one candidate's source error is a row, not fatal)", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want exactly 1 (the leaky candidate must still be recorded, not dropped)", len(result.Rows))
	}
	row := result.Rows[0]
	if row.WorkID != "leaky" || row.Status != StatusFailed || row.Reason != RunReasonSourceError {
		t.Fatalf("row = %+v, want WorkID=leaky Status=Failed Reason=%v", row, RunReasonSourceError)
	}
	if containsSubstring(string(row.Reason), "SECRETVALUE") || containsSubstring(string(row.Reason), "magnet:") {
		t.Errorf("row %+v leaks raw error text in Reason", row)
	}
	for _, reason := range row.Reasons {
		if containsSubstring(string(reason), "SECRETVALUE") {
			t.Errorf("row %+v leaks raw error text in Reasons", row)
		}
	}
	if err != nil && (containsSubstring(err.Error(), "SECRETVALUE") || containsSubstring(err.Error(), leakedMagnet)) {
		t.Fatalf("Run() error leaks raw provider text: %v", err)
	}
}

func TestRunner_InvalidLimits_FailBeforeCalls(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DiscoveryLimits)
	}{
		{"zero MaxCandidates", func(l *DiscoveryLimits) { l.MaxCandidates = 0 }},
		{"negative MaxCandidates", func(l *DiscoveryLimits) { l.MaxCandidates = -1 }},
		{"zero MaxImports", func(l *DiscoveryLimits) { l.MaxImports = 0 }},
		{"negative MaxImports", func(l *DiscoveryLimits) { l.MaxImports = -1 }},
		{"extreme MaxCandidates near int overflow", func(l *DiscoveryLimits) { l.MaxCandidates = math.MaxInt64 }},
		{"extreme MaxLatestExamined near int overflow", func(l *DiscoveryLimits) { l.MaxLatestExamined = math.MaxInt64 }},
		{"negative SearchLimit", func(l *DiscoveryLimits) { l.SearchLimit = -5 }},
		{"negative LatestLimit", func(l *DiscoveryLimits) { l.LatestLimit = -5 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
			tt.mutate(&deps.Limits)
			setUpTwoExplicitCandidates(deps, provider, selector, publisher, "w1")

			_, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}})
			if err == nil {
				t.Fatalf("Run() error = nil, want an error for invalid limits %+v", deps.Limits)
			}
			if len(selector.callList()) != 0 {
				t.Errorf("selector called, want 0 before limits are validated")
			}
		})
	}
}

func TestRunner_TypedNilDependencies_FailBeforeCalls(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RunnerDeps)
	}{
		{"nil Provider", func(d *RunnerDeps) { d.Provider = nil }},
		{"nil Inventory", func(d *RunnerDeps) { d.Inventory = nil }},
		{"nil Identities", func(d *RunnerDeps) { d.Identities = nil }},
		{"nil Selector", func(d *RunnerDeps) { d.Selector = nil }},
		{"nil Publisher", func(d *RunnerDeps) { d.Publisher = nil }},
		{"nil Pacer", func(d *RunnerDeps) { d.Pacer = nil }},
		{"typed-nil Provider", func(d *RunnerDeps) { d.Provider = (*fakeDiscoveryProvider)(nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
			tt.mutate(&deps)
			setUpTwoExplicitCandidates(deps, provider, selector, publisher, "w1")

			_, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}})
			if err == nil {
				t.Fatalf("Run() error = nil, want an error for a nil/typed-nil required dependency")
			}
		})
	}
}
