package audiobookimport

import (
	"context"
	"reflect"
	"testing"
)

// Requirement A3: latest AudioSilo works are eligible only when related to an
// owned author id or owned series id. Latest retrieval and examined rows are
// bounded; unrelated works are skipped. Stable work-id dedup merges explicit,
// series-gap, and latest paths with precedence explicit > series-gap >
// latest and preserves all safe discovery reasons.
//
// Requirement I2: ordering/dedup must be stable and independent of map
// iteration order, and dedup keys use provider work/recording ids only.

func latestCard(id, title, authorID string, seriesID string) AudioSiloWorkCard {
	card := AudioSiloWorkCard{ID: id, Title: title, Authors: []AudioSiloPerson{{ID: authorID, Name: "Some Author"}}}
	if seriesID != "" {
		card.Series = &AudioSiloSeriesRef{ID: seriesID, Name: "Some Series", Position: "1"}
	}
	return card
}

func TestRunner_Latest_RelatedByAuthorBySeriesByBothAndUnrelated(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.authors = map[string]bool{"owned-author": true}
	inventory.series = map[string]bool{"owned-series": true}
	provider.latest = []AudioSiloWorkCard{
		latestCard("by-author", "By Author", "owned-author", ""),
		latestCard("by-series", "By Series", "unowned-author", "owned-series"),
		latestCard("by-both", "By Both", "owned-author", "owned-series"),
		latestCard("unrelated", "Unrelated", "unowned-author", "unowned-series"),
	}
	for _, id := range []string{"by-author", "by-series", "by-both"} {
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
	byWork := map[string]ResultRow{}
	for _, row := range result.Rows {
		byWork[row.WorkID] = row
	}
	for _, want := range []string{"by-author", "by-series", "by-both"} {
		if _, ok := byWork[want]; !ok {
			t.Errorf("Rows = %+v, want a row for related work %q", result.Rows, want)
		}
	}
	if _, ok := byWork["unrelated"]; ok {
		t.Errorf("Rows = %+v, want no row for an unrelated latest work", result.Rows)
	}
	// A double relation (owned by both author and series) must still yield
	// exactly one DiscoveryReasonLatest entry, not two: relation is a
	// boolean admission test, not a count of matching criteria.
	if row := byWork["by-both"]; len(row.Reasons) != 1 || row.Reasons[0] != DiscoveryReasonLatest {
		t.Errorf("by-both Reasons = %v, want exactly one [latest] entry even though both author and series match", row.Reasons)
	}
	if _, _, detail := provider.callCounts(); detail != 3 {
		t.Errorf("WorkDetail called %d times, want exactly 3 for the related works only", detail)
	}
}

func TestRunner_Latest_RetrievalIsBounded(t *testing.T) {
	deps, provider, inventory, _, _, _, _ := newRunnerDeps()
	deps.Limits.LatestLimit = 7
	inventory.authors = map[string]bool{}
	inventory.series = map[string]bool{}

	if _, err := Run(context.Background(), deps, RunRequest{}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if _, latestCalls, _ := provider.callCounts(); latestCalls != 1 {
		t.Fatalf("LatestWorks called %d times, want exactly 1", latestCalls)
	}
	if provider.latestCalls[0] != 7 {
		t.Errorf("LatestWorks called with limit %d, want the configured LatestLimit 7", provider.latestCalls[0])
	}
}

func TestRunner_Latest_ExaminedRowsAreBounded(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	deps.Limits.MaxLatestExamined = 2
	inventory.authors = map[string]bool{"owned-author": true}
	provider.latest = []AudioSiloWorkCard{
		latestCard("first", "First", "owned-author", ""),
		latestCard("second", "Second", "owned-author", ""),
		// beyond the examined bound: must never be looked at even though it
		// is related, proving the bound applies before relation is checked.
		latestCard("third", "Third", "owned-author", ""),
	}
	for _, id := range []string{"first", "second"} {
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
	for _, row := range result.Rows {
		if row.WorkID == "third" {
			t.Fatalf("Rows contain %q, want it excluded because MaxLatestExamined=2 bounds examination before relation is even evaluated", row.WorkID)
		}
	}
	if _, _, detail := provider.callCounts(); detail != 2 {
		t.Errorf("WorkDetail called %d times, want exactly 2 (bounded to MaxLatestExamined)", detail)
	}
}

func TestRunner_Dedup_ExplicitBeatsSeriesGapBeatsLatest(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.series = map[string]bool{"series-a": true}
	inventory.authors = map[string]bool{"owned-author": true}
	inventory.seriesGaps["series-a"] = []SeriesGapCandidate{{SeriesID: "series-a", WorkID: "shared", Volume: "1", Owned: false}}
	provider.latest = []AudioSiloWorkCard{latestCard("shared", "Shared", "owned-author", "")}
	provider.details["shared"] = sampleWorkDetail("shared", "Shared", "a1", "Author", "rec-shared")
	work := WorkFacts{WorkID: "shared", Title: "Shared", Authors: []string{"Author"}}
	selector.byWork["shared"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"shared"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 (work id dedup across explicit/series-gap/latest)", len(result.Rows))
	}
	row := result.Rows[0]
	if len(row.Reasons) == 0 || row.Reasons[0] != DiscoveryReasonExplicit {
		t.Errorf("Reasons[0] = %v, want DiscoveryReasonExplicit to take precedence", row.Reasons)
	}
	foundSeriesGap, foundLatest := false, false
	for _, reason := range row.Reasons {
		if reason == DiscoveryReasonSeriesGap {
			foundSeriesGap = true
		}
		if reason == DiscoveryReasonLatest {
			foundLatest = true
		}
	}
	if !foundSeriesGap || !foundLatest {
		t.Errorf("Reasons = %v, want all three safe discovery reasons preserved", row.Reasons)
	}
	if calls := selector.callList(); len(calls) != 1 {
		t.Errorf("selector called %d times, want exactly 1 for the deduplicated candidate", len(calls))
	}
}

// TestRunner_Order_StableAcrossRepeatedRunsDespiteMapIteration proves I2's
// "independent of map iteration order" requirement empirically: Go
// deliberately randomizes map iteration order per run, so OwnedAuthorIDs/
// OwnedSeriesIDs/committed-identity maps returned by the fakes would surface
// any accidental map-order dependency as a flaky row order across repeated
// invocations with byte-identical input. A single run cannot show this; many
// runs of the same fixed scenario can, deterministically and without sleeps.
func TestRunner_Order_StableAcrossRepeatedRunsDespiteMapIteration(t *testing.T) {
	build := func() RunnerDeps {
		deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
		inventory.series = map[string]bool{"series-a": true, "series-b": true, "series-c": true}
		inventory.authors = map[string]bool{"author-x": true, "author-y": true, "author-z": true}
		inventory.seriesGaps["series-a"] = []SeriesGapCandidate{{SeriesID: "series-a", WorkID: "sa-1", Volume: "1", Owned: false}}
		inventory.seriesGaps["series-b"] = []SeriesGapCandidate{{SeriesID: "series-b", WorkID: "sb-1", Volume: "1", Owned: false}}
		inventory.seriesGaps["series-c"] = []SeriesGapCandidate{{SeriesID: "series-c", WorkID: "sc-1", Volume: "1", Owned: false}}
		provider.latest = []AudioSiloWorkCard{
			latestCard("lx", "LX", "author-x", ""),
			latestCard("ly", "LY", "author-y", ""),
			latestCard("lz", "LZ", "author-z", ""),
		}
		for _, id := range []string{"sa-1", "sb-1", "sc-1", "lx", "ly", "lz"} {
			provider.details[id] = sampleWorkDetail(id, id, "a1", "Author", "rec-"+id)
			work := WorkFacts{WorkID: id, Title: id, Authors: []string{"Author"}}
			func(id string, work WorkFacts) {
				selector.byWork[id] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
					return sampleEligibleSelection(work), nil
				}
			}(id, work)
			configurePublishSuccess(publisher, work)
		}
		return deps
	}

	var firstOrder []string
	for iteration := 0; iteration < 25; iteration++ {
		result, err := Run(context.Background(), build(), RunRequest{DryRun: true})
		if err != nil {
			t.Fatalf("iteration %d: Run() error = %v, want nil", iteration, err)
		}
		order := make([]string, len(result.Rows))
		for i, row := range result.Rows {
			order[i] = row.WorkID
		}
		if iteration == 0 {
			firstOrder = order
			if len(firstOrder) != 6 {
				t.Fatalf("iteration 0: len(order) = %d, want 6", len(firstOrder))
			}
			continue
		}
		if len(order) != len(firstOrder) {
			t.Fatalf("iteration %d: order = %v, want same length as iteration 0 order %v", iteration, order, firstOrder)
		}
		for i := range order {
			if order[i] != firstOrder[i] {
				t.Fatalf("iteration %d: order = %v, want identical to iteration 0 order %v (map iteration must not leak into result order)", iteration, order, firstOrder)
			}
		}
	}
}

// TestRunner_Order_IndependentOfProviderAndInventoryResponseOrder is the
// direct proof review-2 asked for: two scenarios that are logically
// identical (same series, same owned author, same works) but where the
// LatestWorks slice and the per-series SeriesGapCandidates slice are
// physically reversed between them. The earlier repeated-run test could not
// distinguish "sorts by a canonical key" from "faithfully preserves whatever
// order the provider/inventory happened to return" — reversing the input
// and requiring byte-identical output does distinguish them, because an
// implementation that merely preserved arrival order would reverse its
// output here too.
func TestRunner_Order_IndependentOfProviderAndInventoryResponseOrder(t *testing.T) {
	buildScenario := func(reversedGaps, reversedLatest bool) RunnerDeps {
		deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
		inventory.series = map[string]bool{"series-a": true}
		inventory.authors = map[string]bool{"owned-author": true}

		gaps := []SeriesGapCandidate{
			{SeriesID: "series-a", WorkID: "sa-2", Volume: "2", Owned: false},
			{SeriesID: "series-a", WorkID: "sa-1", Volume: "1", Owned: false},
		}
		if reversedGaps {
			gaps = []SeriesGapCandidate{gaps[1], gaps[0]}
		}
		inventory.seriesGaps["series-a"] = gaps

		latest := []AudioSiloWorkCard{
			latestCard("lw-b", "LW B", "owned-author", ""),
			latestCard("lw-a", "LW A", "owned-author", ""),
		}
		if reversedLatest {
			latest = []AudioSiloWorkCard{latest[1], latest[0]}
		}
		provider.latest = latest

		for _, id := range []string{"sa-1", "sa-2", "lw-a", "lw-b"} {
			provider.details[id] = sampleWorkDetail(id, id, "a1", "Author", "rec-"+id)
			work := WorkFacts{WorkID: id, Title: id, Authors: []string{"Author"}}
			func(id string, work WorkFacts) {
				selector.byWork[id] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
					return sampleEligibleSelection(work), nil
				}
			}(id, work)
			configurePublishSuccess(publisher, work)
		}
		return deps
	}

	baseline, err := Run(context.Background(), buildScenario(false, false), RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run(baseline) error = %v, want nil", err)
	}
	reversed, err := Run(context.Background(), buildScenario(true, true), RunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Run(reversed) error = %v, want nil", err)
	}

	if len(baseline.Rows) != 4 {
		t.Fatalf("baseline len(Rows) = %d, want 4", len(baseline.Rows))
	}
	if !reflect.DeepEqual(baseline, reversed) {
		t.Fatalf("reversing the provider's LatestWorks slice and the inventory's SeriesGapCandidates slice changed the result:\n baseline=%+v\n reversed=%+v", baseline, reversed)
	}

	wantOrder := []string{"sa-1", "sa-2", "lw-a", "lw-b"}
	gotOrder := make([]string, len(baseline.Rows))
	for i, row := range baseline.Rows {
		gotOrder[i] = row.WorkID
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("order = %v, want canonical order %v (series id then numeric volume for series-gap rows, then work id for latest rows)", gotOrder, wantOrder)
	}
}

func TestRunner_Latest_ProviderErrorIsPartialFailureNotFatal(t *testing.T) {
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	provider.latestErr = errNonContextGapFailure
	inventory.authors = map[string]bool{}
	provider.details["explicit-1"] = sampleWorkDetail("explicit-1", "Explicit", "a1", "Author", "rec-1")
	work := WorkFacts{WorkID: "explicit-1", Title: "Explicit", Authors: []string{"Author"}}
	selector.byWork["explicit-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"explicit-1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a failing latest branch must not erase a successful explicit candidate", err)
	}
	found := false
	for _, row := range result.Rows {
		if row.WorkID == "explicit-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("Rows = %+v, want explicit-1 present despite the latest branch failing", result.Rows)
	}
}
