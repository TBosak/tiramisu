package audiobookimport

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Requirement A5: each remaining candidate is processed sequentially through
// detail resolution, source selection, projection, and publication.
// Per-run candidate/import caps are enforced before calls. Below-confidence/
// no-source/incoherent releases are recorded as safe skips; one candidate
// failure is recorded and later candidates continue unless the caller
// context is cancelled. Apply publishes through the existing resumable
// publisher; dry run returns the exact planned publication with no external
// mutation.
//
// Requirement A6: pacing occurs only between admitted provider/source
// attempts through an injected context-aware pacer. There is no sleep,
// goroutine, polling, or retry loop. Cancellation during provider, pacing,
// source selection, or publication stops later calls and remains
// errors.Is compatible.
//
// Requirements I3 (cancellation wins, no process-global state), I4
// (discovery calls only the injected boundaries), I5 (dry run/apply share
// everything except publication mode), E2 (partial failure is safe), E3
// (redacted, correlated errors; non-context errors do not consume the
// import cap), E4 (caps charged exactly once after dedup).

func setUpTwoExplicitCandidates(deps RunnerDeps, provider *fakeDiscoveryProvider, selector *fakeSourceSelector, publisher *fakePublisher, ids ...string) {
	for _, id := range ids {
		provider.details[id] = sampleWorkDetail(id, id, "a1", "Author", "rec-"+id)
		work := WorkFacts{WorkID: id, Title: id, Authors: []string{"Author"}}
		func(id string, work WorkFacts) {
			selector.byWork[id] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
				return sampleEligibleSelection(work), nil
			}
		}(id, work)
		configurePublishSuccess(publisher, work)
	}
}

func TestRunner_CandidateCap_EnforcedBeforeCalls(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	deps.Limits.MaxCandidates = 1
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "c1", "c2")

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"c1", "c2"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 1 {
		t.Fatalf("selector called %d times, want exactly 1 (candidate cap = 1)", len(selector.callList()))
	}
	var c2 ResultRow
	for _, row := range result.Rows {
		if row.WorkID == "c2" {
			c2 = row
		}
	}
	if c2.Status != StatusSkipped || c2.Reason != RunReasonCandidateCapReached {
		t.Errorf("c2 row = %+v, want Skipped/%v", c2, RunReasonCandidateCapReached)
	}
}

func TestRunner_ImportCap_EnforcedBeforeCalls(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	deps.Limits.MaxImports = 1
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "i1", "i2")

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"i1", "i2"}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if publisher.requestCount() != 1 {
		t.Fatalf("publisher called %d times, want exactly 1 (import cap = 1)", publisher.requestCount())
	}
	var i2 ResultRow
	for _, row := range result.Rows {
		if row.WorkID == "i2" {
			i2 = row
		}
	}
	if i2.Status != StatusSkipped || i2.Reason != RunReasonImportCapReached {
		t.Errorf("i2 row = %+v, want Skipped/%v", i2, RunReasonImportCapReached)
	}
}

func TestRunner_NoSourceSelected_IsSafeSkip(t *testing.T) {
	deps, provider, _, _, selector, _, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	selector.byWork["w1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return SelectionResult{}, nil
	}

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusSkipped || result.Rows[0].Reason != RunReasonNoSource {
		t.Fatalf("Rows = %+v, want one Skipped/%v row", result.Rows, RunReasonNoSource)
	}
}

func TestRunner_SourceError_RecordedAsFailed_LaterCandidatesContinue(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	provider.details["fail-1"] = sampleWorkDetail("fail-1", "Fail", "a1", "Author", "rec-fail")
	provider.details["ok-1"] = sampleWorkDetail("ok-1", "Ok", "a1", "Author", "rec-ok")
	selector.byWork["fail-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return SelectionResult{}, errors.New("indexer said: token=super-secret-abc123")
	}
	okWork := WorkFacts{WorkID: "ok-1", Title: "Ok", Authors: []string{"Author"}}
	selector.byWork["ok-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(okWork), nil
	}
	configurePublishSuccess(publisher, okWork)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"fail-1", "ok-1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (one candidate's source error is recorded, not fatal)", err)
	}
	byWork := map[string]ResultRow{}
	for _, row := range result.Rows {
		byWork[row.WorkID] = row
	}
	failRow, ok := byWork["fail-1"]
	if !ok || failRow.Status != StatusFailed || failRow.Reason != RunReasonSourceError {
		t.Fatalf("fail-1 row = %+v, want Failed/%v", failRow, RunReasonSourceError)
	}
	if got := string(failRow.Reason); containsSubstring(got, "super-secret-abc123") {
		t.Errorf("Reason %q leaks raw error text, want a canonical redacted code", got)
	}
	okRow, ok := byWork["ok-1"]
	if !ok || okRow.Status != StatusPlanned {
		t.Fatalf("ok-1 row = %+v, want Planned: a later candidate must still be processed", okRow)
	}
}

func TestRunner_NonContextPublishError_DoesNotConsumeImportCap(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	deps.Limits.MaxImports = 1
	provider.details["fail-1"] = sampleWorkDetail("fail-1", "Fail", "a1", "Author", "rec-fail")
	provider.details["ok-1"] = sampleWorkDetail("ok-1", "Ok", "a1", "Author", "rec-ok")
	failWork := WorkFacts{WorkID: "fail-1", Title: "Fail", Authors: []string{"Author"}}
	okWork := WorkFacts{WorkID: "ok-1", Title: "Ok", Authors: []string{"Author"}}
	selector.byWork["fail-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(failWork), nil
	}
	selector.byWork["ok-1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(okWork), nil
	}
	publisher.byWork["fail-1"] = func(ctx context.Context, req PublicationRequest) (PublicationResult, error) {
		return PublicationResult{}, errors.New("library add failed")
	}
	configurePublishSuccess(publisher, okWork)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"fail-1", "ok-1"}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	byWork := map[string]ResultRow{}
	for _, row := range result.Rows {
		byWork[row.WorkID] = row
	}
	if row := byWork["fail-1"]; row.Status != StatusFailed || row.Reason != RunReasonPublishError {
		t.Fatalf("fail-1 row = %+v, want Failed/%v", row, RunReasonPublishError)
	}
	if row := byWork["ok-1"]; row.Status != StatusImported {
		t.Fatalf("ok-1 row = %+v, want Imported: a non-context publish failure must not consume the import cap", row)
	}
}

// TestRunner_DryRun_SendsExactPlannedPublicationRequest replaces an earlier,
// over-constrained version of this test that forbade the runner from calling
// CandidatePublisher at all on a dry run. The brief (A5/I5) requires the
// *existing* publication boundary to plan a dry run: CandidatePublisher must
// still be called, with PublicationRequest.DryRun=true and the exact Work,
// Recording, Selection, and PlanProjection paths apply would use, so that dry
// run and apply share one code path and only the flag differs.
func TestRunner_DryRun_SendsExactPlannedPublicationRequest(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	detail := sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	provider.details["w1"] = detail
	work := WorkFacts{WorkID: "w1", Title: "Book", Authors: []string{"Author"}}
	selection := sampleEligibleSelection(work)
	selector.byWork["w1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return selection, nil
	}
	configurePublishSuccess(publisher, work)

	if _, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	reqs := publisher.requestsFor("w1")
	if len(reqs) != 1 {
		t.Fatalf("publisher requests for w1 = %d, want exactly 1: a dry run must still reach the publication boundary", len(reqs))
	}
	req := reqs[0]
	if !req.DryRun {
		t.Errorf("DryRun = false, want true for a dry run")
	}
	wantWork := expectedWorkFacts(detail)
	if !equalWorkFacts(req.Work, wantWork) {
		t.Errorf("Work = %+v, want %+v", req.Work, wantWork)
	}
	wantRecording := recordingFactsFromWorkRecording(detail.Recordings[0], detail.Language)
	if !equalRecordingFacts(req.Recording, wantRecording) {
		t.Errorf("Recording = %+v, want %+v", req.Recording, wantRecording)
	}
	if !reflect.DeepEqual(req.Selection, selection) {
		t.Errorf("Selection = %+v, want the unmodified SelectionResult %+v returned by the selector", req.Selection, selection)
	}
	wantProjected, err := PlanProjection(wantWork, []Decision{*selection.Selected})
	if err != nil {
		t.Fatalf("PlanProjection() error = %v, want nil", err)
	}
	if !equalProjected(req.Projected, wantProjected) {
		t.Errorf("Projected = %+v, want %+v", req.Projected, wantProjected)
	}
}

func TestRunner_Apply_SendsByteIdenticalRequestExceptDryRunFlag(t *testing.T) {
	buildAndRun := func(t *testing.T, dryRun bool) PublicationRequest {
		t.Helper()
		deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
		detail := sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
		provider.details["w1"] = detail
		work := WorkFacts{WorkID: "w1", Title: "Book", Authors: []string{"Author"}}
		selection := sampleEligibleSelection(work)
		selector.byWork["w1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
			return selection, nil
		}
		configurePublishSuccess(publisher, work)
		if _, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: dryRun}); err != nil {
			t.Fatalf("Run(DryRun=%v) error = %v, want nil", dryRun, err)
		}
		reqs := publisher.requestsFor("w1")
		if len(reqs) != 1 {
			t.Fatalf("publisher requests for w1 = %d, want exactly 1", len(reqs))
		}
		return reqs[0]
	}

	dryReq := buildAndRun(t, true)
	applyReq := buildAndRun(t, false)
	if !dryReq.DryRun || applyReq.DryRun {
		t.Fatalf("DryRun flags = dry:%v apply:%v, want true then false", dryReq.DryRun, applyReq.DryRun)
	}
	if !equalPublicationRequestIgnoringDryRun(dryReq, applyReq) {
		t.Errorf("dry run and apply publication requests differ beyond DryRun:\n dry=%+v\n apply=%+v", dryReq, applyReq)
	}
}

func expectedWorkFacts(detail AudioSiloWorkDetail) WorkFacts {
	authors := make([]string, len(detail.Authors))
	for i, p := range detail.Authors {
		authors[i] = p.Name
	}
	return WorkFacts{WorkID: detail.ID, Title: detail.Title, Authors: authors}
}

func equalWorkFacts(a, b WorkFacts) bool {
	return a.WorkID == b.WorkID && a.Title == b.Title && a.Series == b.Series && a.Volume == b.Volume && equalStrings(a.Authors, b.Authors)
}

func recordingFactsFromWorkRecording(rec AudioSiloRecording, language string) RecordingFacts {
	narrators := make([]string, len(rec.Narrators))
	for i, p := range rec.Narrators {
		narrators[i] = p.Name
	}
	asins := make([]string, len(rec.ASINs))
	for i, a := range rec.ASINs {
		asins[i] = a.ASIN
	}
	runtime := 0
	if rec.RuntimeMinutes != nil {
		runtime = *rec.RuntimeMinutes
	}
	return RecordingFacts{
		RecordingID:    rec.ID,
		ASINs:          asins,
		ISBNs:          append([]string(nil), rec.ISBNs...),
		Narrators:      narrators,
		Language:       language,
		RuntimeMinutes: runtime,
		Publisher:      rec.Publisher,
		ChapterCount:   rec.ChapterCount,
		Abridged:       rec.Abridged,
	}
}

func equalRecordingFacts(a, b RecordingFacts) bool {
	return a.RecordingID == b.RecordingID && a.Language == b.Language && a.RuntimeMinutes == b.RuntimeMinutes &&
		a.Publisher == b.Publisher && a.ChapterCount == b.ChapterCount && a.Abridged == b.Abridged &&
		equalStrings(a.Narrators, b.Narrators) && equalStrings(a.ASINs, b.ASINs) && equalStrings(a.ISBNs, b.ISBNs)
}

func equalPublicationRequestIgnoringDryRun(a, b PublicationRequest) bool {
	a.DryRun, b.DryRun = false, false
	return reflect.DeepEqual(a, b)
}

func TestRunner_Apply_PublishesAndReportsImported(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "w1")

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if publisher.requestCount() != 1 {
		t.Fatalf("publisher called %d times, want exactly 1 for apply", publisher.requestCount())
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusImported {
		t.Fatalf("Rows = %+v, want one Imported row", result.Rows)
	}
}

func TestRunner_Pacing_OccursOnlyBetweenAdmittedAttempts(t *testing.T) {
	deps, provider, _, _, selector, publisher, pacer := newRunnerDeps()
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "p1", "p2", "p3")

	if _, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"p1", "p2", "p3"}, DryRun: true}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := pacer.callCount(); got != 2 {
		t.Fatalf("pacer called %d times, want exactly 2 (between 3 admitted attempts, none before the first)", got)
	}
}

func TestRunner_Pacing_SkipsCappedCandidates(t *testing.T) {
	deps, provider, _, _, selector, publisher, pacer := newRunnerDeps()
	deps.Limits.MaxCandidates = 1
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "p1", "p2")

	if _, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"p1", "p2"}, DryRun: true}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := pacer.callCount(); got != 0 {
		t.Errorf("pacer called %d times, want 0: a capped candidate never becomes an admitted attempt", got)
	}
}

func TestRunner_CancellationDuringPacing_StopsLaterCandidates(t *testing.T) {
	deps, provider, _, _, selector, publisher, pacer := newRunnerDeps()
	ctx, cancel := context.WithCancel(context.Background())
	pacer.cancel = cancel
	pacer.cancelOn = 1
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "c1", "c2", "c3")

	_, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"c1", "c2", "c3"}, DryRun: true})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if calls := selector.callList(); len(calls) > 2 {
		t.Fatalf("selector called %d times, want at most 2 (cancellation stops later candidates)", len(calls))
	}
}

func TestRunner_CancellationBeforeStart_ReturnsImmediately(t *testing.T) {
	deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
	setUpTwoExplicitCandidates(deps, provider, selector, publisher, "w1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called, want 0 for an already-cancelled context")
	}
}

func TestRunner_DryRunAndApply_ShareCandidateConstruction(t *testing.T) {
	build := func() (RunnerDeps, *fakeDiscoveryProvider, *fakeSourceSelector, *fakePublisher) {
		deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
		setUpTwoExplicitCandidates(deps, provider, selector, publisher, "a1", "a2")
		return deps, provider, selector, publisher
	}

	dryDeps, _, drySelector, _ := build()
	dryResult, err := Run(context.Background(), dryDeps, RunRequest{ExplicitWorkIDs: []string{"a1", "a2"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run(dry) error = %v, want nil", err)
	}
	applyDeps, _, applySelector, applyPublisher := build()
	applyResult, err := Run(context.Background(), applyDeps, RunRequest{ExplicitWorkIDs: []string{"a1", "a2"}})
	if err != nil {
		t.Fatalf("Run(apply) error = %v, want nil", err)
	}

	if len(dryResult.Rows) != len(applyResult.Rows) {
		t.Fatalf("row counts differ: dry=%d apply=%d", len(dryResult.Rows), len(applyResult.Rows))
	}
	for i := range dryResult.Rows {
		dry, apply := dryResult.Rows[i], applyResult.Rows[i]
		if dry.WorkID != apply.WorkID {
			t.Errorf("row %d WorkID differs: dry=%q apply=%q", i, dry.WorkID, apply.WorkID)
		}
		if dry.Status != StatusPlanned || apply.Status != StatusImported {
			t.Errorf("row %d Status = dry:%v apply:%v, want Planned vs Imported as the only difference", i, dry.Status, apply.Status)
		}
	}
	dryCalls, applyCalls := drySelector.callList(), applySelector.callList()
	if len(dryCalls) != len(applyCalls) {
		t.Fatalf("selector call counts differ: dry=%d apply=%d", len(dryCalls), len(applyCalls))
	}
	for i := range dryCalls {
		if dryCalls[i] != applyCalls[i] {
			t.Errorf("selector call %d differs: dry=%q apply=%q", i, dryCalls[i], applyCalls[i])
		}
	}
	if applyPublisher.requestCount() != 2 {
		t.Errorf("apply publisher calls = %d, want 2", applyPublisher.requestCount())
	}
}

func TestRunner_DryRunAndApply_PublisherRequestsMatchExceptDryRunFlag(t *testing.T) {
	build := func() (RunnerDeps, *fakePublisher) {
		deps, provider, _, _, selector, publisher, _ := newRunnerDeps()
		setUpTwoExplicitCandidates(deps, provider, selector, publisher, "a1", "a2")
		return deps, publisher
	}

	dryDeps, dryPublisher := build()
	if _, err := Run(context.Background(), dryDeps, RunRequest{ExplicitWorkIDs: []string{"a1", "a2"}, DryRun: true}); err != nil {
		t.Fatalf("Run(dry) error = %v, want nil", err)
	}
	applyDeps, applyPublisher := build()
	if _, err := Run(context.Background(), applyDeps, RunRequest{ExplicitWorkIDs: []string{"a1", "a2"}}); err != nil {
		t.Fatalf("Run(apply) error = %v, want nil", err)
	}

	if dryPublisher.requestCount() != 2 {
		t.Fatalf("dry run publisher calls = %d, want 2: dry run must reach the same publisher as apply", dryPublisher.requestCount())
	}
	for _, workID := range []string{"a1", "a2"} {
		dryReqs, applyReqs := dryPublisher.requestsFor(workID), applyPublisher.requestsFor(workID)
		if len(dryReqs) != 1 || len(applyReqs) != 1 {
			t.Fatalf("work %q: dry requests = %d, apply requests = %d, want 1 each", workID, len(dryReqs), len(applyReqs))
		}
		if !equalPublicationRequestIgnoringDryRun(dryReqs[0], applyReqs[0]) {
			t.Errorf("work %q: dry and apply publication requests differ beyond DryRun:\n dry=%+v\n apply=%+v", workID, dryReqs[0], applyReqs[0])
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
