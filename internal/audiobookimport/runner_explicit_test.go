package audiobookimport

import (
	"context"
	"errors"
	"testing"
)

// Requirement A1: explicit work ids/queries are resolved first, in stable
// input order; duplicate explicit inputs collapse to one work while
// retaining an explicit discovery reason; empty/invalid explicit input fails
// before any downstream call. Requirement I1 (empty required ids fail
// before calls) and E1 (invalid input fails before provider/source/publish
// side effects) overlap this contract and are exercised here too.

func newRunnerDeps() (RunnerDeps, *fakeDiscoveryProvider, *fakeOwnedInventory, *fakeCommittedIdentitySource, *fakeSourceSelector, *fakePublisher, *fakePacer) {
	provider := newFakeDiscoveryProvider()
	inventory := newFakeOwnedInventory()
	identities := newFakeCommittedIdentitySource()
	selector := newFakeSourceSelector()
	publisher := newFakePublisher()
	pacer := &fakePacer{}
	deps := RunnerDeps{
		Provider:   provider,
		Inventory:  inventory,
		Identities: identities,
		Selector:   selector,
		Publisher:  publisher,
		Pacer:      pacer,
		Limits: DiscoveryLimits{
			MaxCandidates:     10,
			MaxImports:        10,
			MaxLatestExamined: 10,
			MaxSeriesExamined: 10,
			SearchLimit:       5,
			LatestLimit:       5,
			Selection: SelectionLimits{
				MaxQueries: 2, MaxResultsPerQuery: 5, MaxCandidatesInspected: 5,
				MaxSourceBytes: 1 << 20, MaxReleaseSizeBytes: 1 << 30, MinConfidence: 0,
			},
		},
	}
	return deps, provider, inventory, identities, selector, publisher, pacer
}

func TestRunner_ExplicitWorkIDs_StableOrder(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book One", "a1", "Author One", "rec-1")
	provider.details["w2"] = sampleWorkDetail("w2", "Book Two", "a2", "Author Two", "rec-2")
	for id, title := range map[string]string{"w1": "Book One", "w2": "Book Two"} {
		work := WorkFacts{WorkID: id, Title: title, Authors: []string{"x"}}
		sel := sampleEligibleSelection(work)
		selector.byWork[id] = func(sel SelectionResult) func(context.Context, Target, SelectionLimits) (SelectionResult, error) {
			return func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
				return sel, nil
			}
		}(sel)
		configurePublishSuccess(publisher, work)
	}

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w2", "w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2", len(result.Rows))
	}
	if result.Rows[0].WorkID != "w2" || result.Rows[1].WorkID != "w1" {
		t.Errorf("Rows = %+v, want order [w2 w1] matching the given input order", result.Rows)
	}
	for _, row := range result.Rows {
		if len(row.Reasons) != 1 || row.Reasons[0] != DiscoveryReasonExplicit {
			t.Errorf("row %q Reasons = %v, want exactly [explicit]", row.WorkID, row.Reasons)
		}
	}
}

func TestRunner_DuplicateExplicitWorkIDs_CollapseToOneRow(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	work := WorkFacts{WorkID: "dup-1", Title: "Duplicated Book", Authors: []string{"x"}}
	provider.details["dup-1"] = sampleWorkDetail("dup-1", "Duplicated Book", "a1", "Author", "rec-1")
	sel := sampleEligibleSelection(work)
	selector.byWork["dup-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sel, nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"dup-1", "dup-1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 (duplicate explicit ids collapse)", len(result.Rows))
	}
	if got := result.Rows[0].Reasons; len(got) != 1 || got[0] != DiscoveryReasonExplicit {
		t.Errorf("Reasons = %v, want exactly one [explicit] reason, not duplicated per occurrence", got)
	}
	if calls := selector.callList(); len(calls) != 1 {
		t.Errorf("selector called %d times, want exactly 1 for the deduplicated candidate", len(calls))
	}
}

func TestRunner_ExplicitQuery_ResolvesFirstSearchHit(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	provider.searchResults["fellowship"] = []AudioSiloWorkCard{{ID: "w-fellowship", Title: "The Fellowship of the Ring", Authors: []AudioSiloPerson{{ID: "a1", Name: "Tolkien"}}}}
	provider.details["w-fellowship"] = sampleWorkDetail("w-fellowship", "The Fellowship of the Ring", "a1", "Tolkien", "rec-1")
	work := WorkFacts{WorkID: "w-fellowship", Title: "The Fellowship of the Ring", Authors: []string{"Tolkien"}}
	sel := sampleEligibleSelection(work)
	selector.byWork["w-fellowship"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sel, nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitQueries: []string{"fellowship"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 || result.Rows[0].WorkID != "w-fellowship" {
		t.Fatalf("Rows = %+v, want one row for w-fellowship", result.Rows)
	}
	if search, _, _ := provider.callCounts(); search != 1 {
		t.Errorf("SearchWorks called %d times, want 1", search)
	}
}

func TestRunner_ExplicitQuery_NoResultsIsSafeSkipNotFatal(t *testing.T) {
	deps, provider, _, _, _, _, _ := newRunnerDeps()
	provider.searchResults["nothing-found"] = nil

	result, err := Run(context.Background(), deps, RunRequest{ExplicitQueries: []string{"nothing-found"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a query with no provider hits is a safe skip, not a fatal error)", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 row recording the unresolved query", len(result.Rows))
	}
	if result.Rows[0].Status != StatusSkipped {
		t.Errorf("Status = %v, want %v", result.Rows[0].Status, StatusSkipped)
	}
}

func TestRunner_EmptyExplicitInput_FailsBeforeAnyDownstreamCall(t *testing.T) {
	tests := []struct {
		name string
		req  RunRequest
	}{
		{"empty work id in list", RunRequest{ExplicitWorkIDs: []string{"w1", ""}}},
		{"blank work id in list", RunRequest{ExplicitWorkIDs: []string{"   "}}},
		{"empty query in list", RunRequest{ExplicitQueries: []string{""}}},
		{"blank query in list", RunRequest{ExplicitQueries: []string{"   "}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, provider, inventory, identities, selector, publisher, pacer := newRunnerDeps()
			_, err := Run(context.Background(), deps, tt.req)
			if err == nil {
				t.Fatalf("Run() error = nil, want an error for invalid explicit input %+v", tt.req)
			}
			search, latest, detail := provider.callCounts()
			if search != 0 || latest != 0 || detail != 0 {
				t.Errorf("provider calls (search=%d latest=%d detail=%d), want all zero before validation failure", search, latest, detail)
			}
			if inventory.authorCalls != 0 || inventory.seriesCalls != 0 || len(inventory.gapCalls) != 0 {
				t.Errorf("inventory was called, want zero calls before validation failure")
			}
			if identities.existingCalls != 0 || identities.committedCalls != 0 {
				t.Errorf("identity source was called, want zero calls before validation failure")
			}
			if len(selector.callList()) != 0 {
				t.Errorf("selector was called, want zero calls before validation failure")
			}
			if publisher.requestCount() != 0 {
				t.Errorf("publisher was called, want zero calls before validation failure")
			}
			if pacer.callCount() != 0 {
				t.Errorf("pacer was called, want zero calls before validation failure")
			}
		})
	}
}

func TestRunner_NoExplicitInputAndNoDiscoverySources_ReturnsEmptyResult(t *testing.T) {
	deps, _, inventory, _, _, _, _ := newRunnerDeps()
	// No owned series and no latest relation possible: an empty inventory
	// must not force a provider error, it is simply zero candidates.
	inventory.series = map[string]bool{}
	inventory.authors = map[string]bool{}

	result, err := Run(context.Background(), deps, RunRequest{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a legitimately empty run", err)
	}
	if len(result.Rows) != 0 {
		t.Errorf("len(Rows) = %d, want 0", len(result.Rows))
	}
}

func TestRunner_NilRequiredDependency_FailsBeforeCalls(t *testing.T) {
	deps, _, _, _, _, _, _ := newRunnerDeps()
	deps.Selector = nil
	_, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}})
	if err == nil {
		t.Fatalf("Run() error = nil, want an error for a nil required dependency")
	}
}

func TestRunner_ExplicitWorkID_ProviderDetailError_RecordedNotFatal(t *testing.T) {
	deps, provider, _, _, _, _, _ := newRunnerDeps()
	provider.detailErr["missing-work"] = errors.New("no such work")

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"missing-work"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (one explicit resolution failure is a row, not a fatal error, per A1/E2)", err)
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusFailed {
		t.Fatalf("Rows = %+v, want one failed row for the unresolved explicit id", result.Rows)
	}
}
