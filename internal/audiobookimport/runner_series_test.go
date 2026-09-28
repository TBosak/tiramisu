package audiobookimport

import (
	"context"
	"testing"
)

// Requirement A2: owned AudioSilo series produce missing-volume candidates
// only when a stable series id and volume/position prove a real gap. Owned
// volumes, ambiguous/non-numeric positions, duplicate work ids, and
// unrelated series are skipped with safe reason codes. Candidate ordering is
// series id then numeric volume then work id.

func TestRunner_SeriesGap_OnlyQueriesOwnedSeries(t *testing.T) {
	deps, _, inventory, _, _, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = nil

	if _, err := Run(context.Background(), deps, RunRequest{}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(inventory.gapCalls) != 1 || inventory.gapCalls[0] != "series-a" {
		t.Fatalf("gapCalls = %v, want exactly [series-a]", inventory.gapCalls)
	}
}

func TestRunner_SeriesGap_MissingVolumeBecomesCandidate(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "vol-3", Volume: "3", Owned: false},
	}
	provider.details["vol-3"] = sampleWorkDetail("vol-3", "Book Three", "a1", "Author", "rec-3")
	work := WorkFacts{WorkID: "vol-3", Title: "Book Three", Authors: []string{"Author"}}
	selector.byWork["vol-3"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 || result.Rows[0].WorkID != "vol-3" {
		t.Fatalf("Rows = %+v, want one row for vol-3", result.Rows)
	}
	if got := result.Rows[0].Reasons; len(got) != 1 || got[0] != DiscoveryReasonSeriesGap {
		t.Errorf("Reasons = %v, want exactly [series_gap]", got)
	}
}

func TestRunner_SeriesGap_SkipsOwnedVolume(t *testing.T) {
	deps, _, inventory, _, selector, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "owned-vol", Volume: "1", Owned: true},
	}

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(result.Rows))
	}
	if row := result.Rows[0]; row.WorkID != "owned-vol" || row.Status != StatusSkipped || row.Reason != RunReasonOwnedVolume {
		t.Errorf("owned-vol row = %+v, want Skipped/%v", row, RunReasonOwnedVolume)
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called for an owned-only run, want zero calls")
	}
}

// TestRunner_SeriesGap_SkipsNonNumericVolume pins RunReasonNonNumericVolume
// specifically, distinct from RunReasonAmbiguousVolume: a position that
// cannot be parsed as a number at all (e.g. spelled out, or a stray label)
// is a different, more certain failure than two positions genuinely
// competing for the same slot.
func TestRunner_SeriesGap_SkipsNonNumericVolume(t *testing.T) {
	deps, _, inventory, _, selector, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "text-vol", Volume: "two", Owned: false},
	}

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(result.Rows))
	}
	if row := result.Rows[0]; row.WorkID != "text-vol" || row.Status != StatusSkipped || row.Reason != RunReasonNonNumericVolume {
		t.Errorf("text-vol row = %+v, want Skipped/%v", row, RunReasonNonNumericVolume)
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called for a skipped-only run, want zero calls")
	}
}

// TestRunner_SeriesGap_SameWorkClaimsTwoVolumes_IsAmbiguous pins
// RunReasonAmbiguousVolume for the case the brief calls "duplicate work ids":
// the same work id appears twice in one series' gap candidates with two
// different declared positions, so which position is real is genuinely
// unknown from the provider data alone.
func TestRunner_SeriesGap_SameWorkClaimsTwoVolumes_IsAmbiguous(t *testing.T) {
	deps, _, inventory, _, selector, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "dup-vol", Volume: "4", Owned: false},
		{SeriesID: "series-a", WorkID: "dup-vol", Volume: "5", Owned: false},
	}

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want exactly 1 (the same work id collapses to one row, not two)", len(result.Rows))
	}
	if row := result.Rows[0]; row.WorkID != "dup-vol" || row.Status != StatusSkipped || row.Reason != RunReasonAmbiguousVolume {
		t.Errorf("dup-vol row = %+v, want Skipped/%v", row, RunReasonAmbiguousVolume)
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called for a skipped-only run, want zero calls")
	}
}

// TestRunner_SeriesGap_TwoWorksClaimSameVolume_IsAmbiguous pins
// RunReasonAmbiguousVolume for a position collision between two distinct
// works: the runner cannot know which one actually fills the missing slot,
// so both are skipped rather than one being arbitrarily chosen.
func TestRunner_SeriesGap_TwoWorksClaimSameVolume_IsAmbiguous(t *testing.T) {
	deps, _, inventory, _, selector, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "cross-a", Volume: "6", Owned: false},
		{SeriesID: "series-a", WorkID: "cross-b", Volume: "6", Owned: false},
	}

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	byWork := map[string]ResultRow{}
	for _, row := range result.Rows {
		byWork[row.WorkID] = row
	}
	for _, id := range []string{"cross-a", "cross-b"} {
		row, ok := byWork[id]
		if !ok || row.Status != StatusSkipped || row.Reason != RunReasonAmbiguousVolume {
			t.Errorf("%s row = %+v, want Skipped/%v", id, row, RunReasonAmbiguousVolume)
		}
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called for a skipped-only run, want zero calls")
	}
}

// TestRunner_SeriesGap_ExactDuplicateEntry_CollapsesSilently distinguishes a
// harmless exact duplicate (same work id, same position, e.g. reported by
// two overlapping inventory pages) from the genuine ambiguity above: it
// collapses to one admitted candidate with no skip reason at all.
func TestRunner_SeriesGap_ExactDuplicateEntry_CollapsesSilently(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "exact-dup", Volume: "7", Owned: false},
		{SeriesID: "series-a", WorkID: "exact-dup", Volume: "7", Owned: false},
	}
	provider.details["exact-dup"] = sampleWorkDetail("exact-dup", "Exact Dup", "a1", "Author", "rec-exact-dup")
	work := WorkFacts{WorkID: "exact-dup", Title: "Exact Dup", Authors: []string{"Author"}}
	selector.byWork["exact-dup"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want exactly 1", len(result.Rows))
	}
	if row := result.Rows[0]; row.WorkID != "exact-dup" || row.Status == StatusSkipped {
		t.Errorf("exact-dup row = %+v, want an admitted candidate, not a skip", row)
	}
	if calls := selector.callList(); len(calls) != 1 {
		t.Errorf("selector called %d times, want exactly 1", len(calls))
	}
}

func TestRunner_SeriesGap_DeterministicOrdering(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-b": true, "series-a": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{
		{SeriesID: "series-a", WorkID: "a-10", Volume: "10", Owned: false},
		{SeriesID: "series-a", WorkID: "a-2", Volume: "2", Owned: false},
	}
	inventory.seriesGaps["series-b"] = []SeriesGapCandidate{
		{SeriesID: "series-b", WorkID: "b-1", Volume: "1", Owned: false},
	}
	for _, id := range []string{"a-10", "a-2", "b-1"} {
		provider.details[id] = sampleWorkDetail(id, id, "a1", "Author", "rec-"+id)
		work := WorkFacts{WorkID: id, Title: id, Authors: []string{"Author"}}
		func(id string, work WorkFacts) {
			selector.byWork[id] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
				return sampleEligibleSelection(work), nil
			}
		}(id, work)
		configurePublishSuccess(publisher, work)
	}

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	var order []string
	for _, row := range result.Rows {
		order = append(order, row.WorkID)
	}
	want := []string{"a-2", "a-10", "b-1"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order = %v, want series id then numeric volume then work id: %v", order, want)
			break
		}
	}
}

func TestRunner_SeriesGap_InventoryErrorIsSafeNotFatal(t *testing.T) {
	deps, _, inventory, _, _, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true, "series-b": true}
	inventory.gapErrFor["series-a"] = context.DeadlineExceeded
	inventory.seriesGaps["series-b"] = []SeriesGapCandidate{{SeriesID: "series-b", WorkID: "b-1", Volume: "1", Owned: false}}

	// A non-context error for one series branch must not erase progress on
	// the others (E2: partial discovery-source failure is safe).
	inventory.gapErrFor["series-a"] = errNonContextGapFailure

	result, err := Run(context.Background(), deps, RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a partial, non-context series failure", err)
	}
	found := false
	for _, row := range result.Rows {
		if row.WorkID == "b-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("Rows = %+v, want the healthy series-b branch represented despite series-a failing", result.Rows)
	}
}

var errNonContextGapFailure = &nonContextError{"series gap lookup failed"}

type nonContextError struct{ msg string }

func (e *nonContextError) Error() string { return e.msg }
