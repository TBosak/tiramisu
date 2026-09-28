package audiobookimport

import (
	"context"
	"errors"
	"testing"
)

// Requirement A6/I3/E3: the caller's context must reach every injected
// dependency (provider, inventory, identities, selector, publisher, pacer),
// and cancellation injected at any dependency boundary — not just pacing —
// must stop later candidates and remain errors.Is(context.Canceled)
// compatible. These tests use a marker value threaded through context.Value
// and closures that call the test's own cancel function deterministically,
// so there is no sleep, goroutine, or polling anywhere in this file.

func buildContextPropagationScenario() (RunnerDeps, *fakeDiscoveryProvider, *fakeOwnedInventory, *fakeCommittedIdentitySource, *fakeSourceSelector, *fakePublisher, *fakePacer) {
	deps, provider, inventory, identities, selector, publisher, pacer := newRunnerDeps()
	inventory.series = map[string]bool{"s1": true}
	inventory.authors = map[string]bool{"au1": true}
	inventory.seriesGaps["s1"] = []SeriesGapCandidate{{SeriesID: "s1", WorkID: "sg1", Volume: "1", Owned: false}}
	provider.latest = []AudioSiloWorkCard{latestCard("lw1", "Latest One", "au1", "")}
	for _, id := range []string{"e1", "sg1", "lw1"} {
		provider.details[id] = sampleWorkDetail(id, id, "a1", "Author", "rec-"+id)
		work := WorkFacts{WorkID: id, Title: id, Authors: []string{"Author"}}
		func(id string, work WorkFacts) {
			selector.byWork[id] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
				return sampleEligibleSelection(work), nil
			}
		}(id, work)
		configurePublishSuccess(publisher, work)
	}
	return deps, provider, inventory, identities, selector, publisher, pacer
}

func TestRunner_ContextMarker_PropagatesToEveryDependencyBoundary(t *testing.T) {
	deps, provider, inventory, identities, selector, publisher, pacer := buildContextPropagationScenario()

	ctx := withMarker(context.Background(), "run-marker-abc")
	result, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"e1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("len(Rows) = %d, want 3 (explicit + series-gap + latest candidates)", len(result.Rows))
	}

	if _, latestMarker, detailMarker := provider.markers(); latestMarker != "run-marker-abc" || detailMarker != "run-marker-abc" {
		t.Errorf("provider markers = (latest:%q detail:%q), want both %q", latestMarker, detailMarker, "run-marker-abc")
	}
	if authorMarker, seriesMarker, gapMarker := inventory.markers(); authorMarker != "run-marker-abc" || seriesMarker != "run-marker-abc" || gapMarker != "run-marker-abc" {
		t.Errorf("inventory markers = (author:%q series:%q gap:%q), want all %q", authorMarker, seriesMarker, gapMarker, "run-marker-abc")
	}
	if existingMarker, committedMarker := identities.markers(); existingMarker != "run-marker-abc" || committedMarker != "run-marker-abc" {
		t.Errorf("identity markers = (existing:%q committed:%q), want both %q", existingMarker, committedMarker, "run-marker-abc")
	}
	if got := selector.lastMarker(); got != "run-marker-abc" {
		t.Errorf("selector marker = %q, want %q", got, "run-marker-abc")
	}
	if got := publisher.lastMarker(); got != "run-marker-abc" {
		t.Errorf("publisher marker = %q, want %q", got, "run-marker-abc")
	}
	if got := pacer.lastMarker(); got != "run-marker-abc" {
		t.Errorf("pacer marker = %q, want %q", got, "run-marker-abc")
	}
}

func TestRunner_CancellationDuringDetailResolution_StopsLaterCandidates(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "c1", "c2")
	ctx, cancel := context.WithCancel(context.Background())
	provider.onDetailCall = func(id string) {
		if id == "c1" {
			cancel()
		}
	}

	_, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"c1", "c2"}, DryRun: true})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called, want 0: detail resolution never completed for c1, so no candidate should reach selection")
	}
	if _, _, detail := provider.callCounts(); detail != 0 {
		t.Errorf("WorkDetail called %d times for c2, want 0: cancellation during c1's detail resolution must stop c2 from starting", detail)
	}
}

func TestRunner_CancellationDuringSourceSelection_StopsLaterCandidates(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	provider.details["c1"] = sampleWorkDetail("c1", "c1", "a1", "Author", "rec-c1")
	provider.details["c2"] = sampleWorkDetail("c2", "c2", "a1", "Author", "rec-c2")
	ctx, cancel := context.WithCancel(context.Background())
	selector.byWork["c1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		cancel()
		return SelectionResult{}, ctx.Err()
	}
	work2 := WorkFacts{WorkID: "c2", Title: "c2", Authors: []string{"Author"}}
	selector.byWork["c2"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work2), nil
	}
	configurePublishSuccess(publisher, work2)

	_, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"c1", "c2"}, DryRun: true})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if calls := selector.callList(); len(calls) != 1 {
		t.Fatalf("selector called %d times, want exactly 1: cancellation during c1's selection must stop c2 from starting", len(calls))
	}
	if publisher.requestCount() != 0 {
		t.Errorf("publisher called %d times, want 0", publisher.requestCount())
	}
}

func TestRunner_CancellationDuringPublication_StopsLaterCandidatesAndPacing(t *testing.T) {
	deps, provider, _, _, selector, publisher, pacer := newRunnerDeps()
	provider.details["c1"] = sampleWorkDetail("c1", "c1", "a1", "Author", "rec-c1")
	provider.details["c2"] = sampleWorkDetail("c2", "c2", "a1", "Author", "rec-c2")
	work1 := WorkFacts{WorkID: "c1", Title: "c1", Authors: []string{"Author"}}
	work2 := WorkFacts{WorkID: "c2", Title: "c2", Authors: []string{"Author"}}
	selector.byWork["c1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work1), nil
	}
	selector.byWork["c2"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work2), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	publisher.byWork["c1"] = func(ctx context.Context, req PublicationRequest) (PublicationResult, error) {
		cancel()
		return PublicationResult{}, ctx.Err()
	}
	configurePublishSuccess(publisher, work2)

	_, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"c1", "c2"}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if calls := selector.callList(); len(calls) != 1 {
		t.Fatalf("selector called %d times, want exactly 1: cancellation during c1's publish must stop c2 from starting", len(calls))
	}
	reqs := publisher.requestsFor("c2")
	if len(reqs) != 0 {
		t.Errorf("publisher requests for c2 = %d, want 0: publication must not proceed for a later candidate after cancellation", len(reqs))
	}
	if got := pacer.callCount(); got != 0 {
		t.Errorf("pacer called %d times, want 0: cancellation during c1's publish must pre-empt pacing before c2", got)
	}
}
