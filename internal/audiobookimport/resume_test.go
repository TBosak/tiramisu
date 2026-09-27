package audiobookimport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// publishedIdentity resolves the identity basePublicationRequest() will
// produce, so tests can preload store state keyed correctly without
// depending on ResolveExternalIdentity's internals beyond its own contract.
func publishedIdentity(t *testing.T) ExternalIdentity {
	t.Helper()
	id, err := ResolveExternalIdentity(pubWork(), pubRecordingWithID())
	if err != nil {
		t.Fatalf("ResolveExternalIdentity() error = %v, want nil", err)
	}
	return id
}

func scannedStateFor(id ExternalIdentity, projected []ProjectedFile) State {
	return buildState(StateEntry{
		Namespace:       id.Namespace,
		ExternalID:      id.ID,
		ProjectedPaths:  projectedPaths(projected),
		ControllerOwned: true,
		Stage:           StageScanned,
		ScanStatusCode:  200,
		UpdatedAt:       time.Now(),
	})
}

// publishedStateAt builds a durable state for id at the given stage, with
// the given attempt count, so retry-bound and resume tests can preload a
// precise prior stage without duplicating buildState/ProjectedPaths
// boilerplate.
func publishedStateAt(id ExternalIdentity, projected []ProjectedFile, stage Stage, attempts int) State {
	return buildState(StateEntry{
		Namespace:       id.Namespace,
		ExternalID:      id.ID,
		ProjectedPaths:  projectedPaths(projected),
		ControllerOwned: true,
		Stage:           stage,
		AttemptCount:    attempts,
		UpdatedAt:       time.Now(),
	})
}

// TestResume_A3_ExactRerunPerformsNeitherAddNorScan covers A3: re-running a
// request that already completed publish+scan must not add or scan again.
func TestResume_A3_ExactRerunPerformsNeitherAddNorScan(t *testing.T) {
	req := basePublicationRequest()
	id := publishedIdentity(t)

	log := &callLog{}
	store := newFakeStore(scannedStateFor(id, req.Projected))
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0 on an exact rerun after a completed scan", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 on an exact rerun after a completed scan", len(scan.calls))
	}
	if result.Stage != StageScanned {
		t.Errorf("Stage = %q, want %q", result.Stage, StageScanned)
	}
}

// TestResume_A3_AlreadyPresentAdvancesWithoutDuplicateAdd covers A3: a
// Library AlreadyPresent answer advances state without a second add
// attempt, and still proceeds to scan.
func TestResume_A3_AlreadyPresentAdvancesWithoutDuplicateAdd(t *testing.T) {
	deps, pub, scan, _ := freshDeps()
	pub.steps = []publishStep{{result: LibraryPublishResult{AlreadyPresent: true}}}
	req := basePublicationRequest()

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 1 {
		t.Errorf("Publisher.Add called %d times, want exactly 1", len(pub.calls))
	}
	if !result.AlreadyPresent {
		t.Errorf("AlreadyPresent = false, want true")
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want 1 (AlreadyPresent still advances to scan)", len(scan.calls))
	}
}

// TestResume_A3_SuccessfulAddRecordedDurablyBeforeScanStarts covers A3/E3: a
// save that recorded exactly StagePublished must have happened strictly
// before scanner.scan's own position in the call order - not merely "some
// save eventually contained Published" (which an implementation that scans
// immediately after add, then saves Published only afterward, would still
// satisfy).
func TestResume_A3_SuccessfulAddRecordedDurablyBeforeScanStarts(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()

	if _, err := Publish(context.Background(), deps, req); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	addSeq := store.log.indexOf("publisher.add")
	scanSeq := store.log.indexOf("scanner.scan")
	if addSeq < 0 || scanSeq < 0 {
		t.Fatalf("expected both publisher.add and scanner.scan to be recorded, got %v", store.log.names())
	}
	if scanSeq < addSeq {
		t.Errorf("scanner.scan (seq %d) ran before publisher.add (seq %d)", scanSeq, addSeq)
	}
	id := publishedIdentity(t)
	if !stageDurablyReachedBefore(store, id, StagePublished, scanSeq) {
		t.Errorf("no save recorded exactly Stage=%q strictly before scanner.scan (seq %d); timeline=%+v", StagePublished, scanSeq, stageTimeline(store, id))
	}
	if len(pub.calls) != 1 {
		t.Errorf("Publisher.Add called %d times, want exactly 1", len(pub.calls))
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1", len(scan.calls))
	}
}

// TestResume_A3_ScannedStageAndStatusDurablySavedOnInitialRun covers A3/A5:
// on the very first successful run (add then scan, both succeeding), the
// final durable state must record StageScanned with the scan's status code
// - not just "eventually reach Scanned" but land there in the store the
// initial run itself produces.
func TestResume_A3_ScannedStageAndStatusDurablySavedOnInitialRun(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	scan.steps = []scanStep{{result: AudiobookshelfScanResult{StatusCode: 201}}}
	req := basePublicationRequest()

	if _, err := Publish(context.Background(), deps, req); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded on the initial run")
	}
	id := publishedIdentity(t)
	entry, ok := last.Entries[id.Key()]
	if !ok || entry.Stage != StageScanned || entry.ScanStatusCode != 201 {
		t.Errorf("final durable entry = %+v (ok=%v), want Stage=%q ScanStatusCode=201", entry, ok, StageScanned)
	}
	_ = pub
}

// TestResume_E2_StoreLoadFailurePreventsAdd covers E2: a store failure
// before add (here, a failing initial Load) must prevent the add.
func TestResume_E2_StoreLoadFailurePreventsAdd(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	store.loadErr = errors.New("disk read failed")
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error when the store cannot be loaded")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestResume_E2_PlannedStateSaveFailurePreventsAdd covers E2's other named
// case: a store failure while durably recording the commit-to-attempt
// (StagePlanned) must prevent Add, distinct from a Load failure.
func TestResume_E2_PlannedStateSaveFailurePreventsAdd(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	id := publishedIdentity(t)
	store.failSave = func(s State) bool {
		entry, ok := s.Entries[id.Key()]
		return ok && entry.Stage == StagePlanned
	}
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error when the pre-add planned-state save fails")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestResume_E2_AddFailureLeavesStateRetryable covers E2: an add failure
// must leave state eligible for a later add retry (no scan attempted, and a
// second Publish call may still attempt Add).
func TestResume_E2_AddFailureLeavesStateRetryable(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{
		{err: errors.New("add failed")},
		{result: LibraryPublishResult{}},
	}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error from the failing add")
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 after a failed add", len(scan.calls))
	}

	// Retry: a second Publish call (state carried over in the same store)
	// must be allowed to attempt Add again.
	if _, err := Publish(context.Background(), deps, req); err != nil {
		t.Fatalf("retry Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 2 {
		t.Errorf("Publisher.Add called %d times across both attempts, want 2 (retry allowed)", len(pub.calls))
	}
}

// TestResume_I3_FailedAttemptAdvancesCounterDurably covers I3: a below-limit
// failed add attempt must advance the durably saved, bounded attempt
// counter - the retry bound is only meaningful if failures are actually
// counted.
func TestResume_I3_FailedAttemptAdvancesCounterDurably(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{{err: errors.New("add failed")}}}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	if _, err := Publish(context.Background(), deps, req); err == nil {
		t.Fatalf("Publish() error = nil, want an error from the failing add")
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded after the failed attempt")
	}
	id := publishedIdentity(t)
	entry, ok := last.Entries[id.Key()]
	if !ok {
		t.Fatalf("no durable entry recorded for %+v after the failed attempt", id)
	}
	if entry.AttemptCount < 1 {
		t.Errorf("AttemptCount = %d after one failed attempt, want >= 1", entry.AttemptCount)
	}
	if entry.Stage == StagePublished || entry.Stage == StageScanned {
		t.Errorf("Stage = %q after a failed add, want a retryable pre-publish stage", entry.Stage)
	}
}

// TestResume_I3_MaxAttemptsReachedFailsClosedBeforeAdd covers I3: once the
// durably recorded attempt count reaches the configured bound, Publish must
// fail closed before calling Add again rather than retrying forever.
func TestResume_I3_MaxAttemptsReachedFailsClosedBeforeAdd(t *testing.T) {
	id := publishedIdentity(t)
	req := basePublicationRequest()
	log := &callLog{}
	store := newFakeStore(publishedStateAt(id, req.Projected, StagePlanned, 5))
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan) // deps.Limits.MaxAttempts == 5
	if deps.Limits.MaxAttempts != 5 {
		t.Fatalf("test fixture bug: expected publishDeps' MaxAttempts to be 5, got %d", deps.Limits.MaxAttempts)
	}

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error once AttemptCount reaches MaxAttempts")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0 once the attempt bound is reached", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestResume_E2_StoreFailureAfterSuccessfulAddNeverScans covers E2: once
// Add has succeeded, a store failure while durably recording that success
// must return an error and must never scan, because durability was not
// proven.
func TestResume_E2_StoreFailureAfterSuccessfulAddNeverScans(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	id := publishedIdentity(t)
	store.failSave = func(s State) bool {
		entry, ok := s.Entries[id.Key()]
		return ok && (entry.Stage == StagePublished || entry.Stage == StageScanned)
	}
	pub := &scriptedPublisher{log: log, steps: []publishStep{{result: LibraryPublishResult{}}}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error when the post-add durable write fails")
	}
	if len(pub.calls) != 1 {
		t.Errorf("Publisher.Add called %d times, want exactly 1 (add itself succeeded)", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 (publication durability was not proven)", len(scan.calls))
	}
}

// TestResume_A5_ScanFailureLeavesResumablePublishedStage covers A5/E3: a
// scan failure after a durable publish is terminal for that invocation but
// resumable, and does not rewind the committed publish.
func TestResume_A5_ScanFailureLeavesResumablePublishedStage(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{{result: LibraryPublishResult{}}}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{err: errors.New("scanner unreachable")}}}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error from the failing scan")
	}
	if len(pub.calls) != 1 {
		t.Errorf("Publisher.Add called %d times, want exactly 1", len(pub.calls))
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1 (the scan was attempted and failed)", len(scan.calls))
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded, want the publish commit persisted before the scan attempt")
	}
	id := publishedIdentity(t)
	entry, ok := last.Entries[id.Key()]
	if !ok || entry.Stage != StagePublished {
		t.Errorf("last durable entry = %+v (ok=%v), want Stage=%q", entry, ok, StagePublished)
	}
	scanSeq := store.log.indexOf("scanner.scan")
	if !stageDurablyReachedBefore(store, id, StagePublished, scanSeq) {
		t.Errorf("no save recorded exactly Stage=%q strictly before scanner.scan (seq %d); timeline=%+v", StagePublished, scanSeq, stageTimeline(store, id))
	}
}

// TestResume_A5_ScanRetryResumesWithoutDuplicateAdd covers A5/E3: retrying
// after a scan failure calls scan only, never a second add, and persists
// the scan success status.
func TestResume_A5_ScanRetryResumesWithoutDuplicateAdd(t *testing.T) {
	id := publishedIdentity(t)
	req := basePublicationRequest()

	published := publishedStateAt(id, req.Projected, StagePublished, 1)
	log := &callLog{}
	store := newFakeStore(published)
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 202}}}}
	deps := publishDeps(store, pub, scan)

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0 (scan-only resume)", len(pub.calls))
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1", len(scan.calls))
	}
	if result.ScanStatusCode != 202 {
		t.Errorf("ScanStatusCode = %d, want 202 (scan success status persisted)", result.ScanStatusCode)
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded after the resumed scan")
	}
	entry, ok := last.Entries[id.Key()]
	if !ok || entry.Stage != StageScanned || entry.ScanStatusCode != 202 {
		t.Errorf("last durable entry = %+v (ok=%v), want Stage=%q ScanStatusCode=202", entry, ok, StageScanned)
	}
}

// TestResume_E3_ScanErrorIsRedacted covers E3: scan errors must not leak
// the underlying response body/text verbatim.
func TestResume_E3_ScanErrorIsRedacted(t *testing.T) {
	const secretMarker = "RESPONSE-BODY-SECRET-MARKER"
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{{result: LibraryPublishResult{}}}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{err: errors.New("scan failed: " + secretMarker)}}}
	deps := publishDeps(store, pub, scan)
	req := basePublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error from the failing scan")
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Errorf("error %q leaks the scan response marker", err.Error())
	}
}

// TestResume_I1_CancellationDuringResumeNeverRewindsCommittedPublish covers
// I1/A5: cancelling a resume attempt must surface an errors.Is-compatible
// cancellation error and must never rewind an already-committed publish.
func TestResume_I1_CancellationDuringResumeNeverRewindsCommittedPublish(t *testing.T) {
	id := publishedIdentity(t)
	req := basePublicationRequest()
	published := publishedStateAt(id, req.Projected, StagePublished, 1)
	log := &callLog{}
	store := newFakeStore(published)
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Publish(ctx, deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 on cancellation", len(scan.calls))
	}
	entry := store.state.Entries[id.Key()]
	if entry.Stage != StagePublished {
		t.Errorf("stored stage = %q after cancellation, want unchanged %q (cancellation never rewinds a committed publish)", entry.Stage, StagePublished)
	}
}

// TestResume_I2_ImpossibleStageFailsClosedBeforeCalls covers I2: an
// impossible/conflicting saved stage for this identity must fail closed
// before any external call.
func TestResume_I2_ImpossibleStageFailsClosedBeforeCalls(t *testing.T) {
	id := publishedIdentity(t)
	req := basePublicationRequest()
	bogus := publishedStateAt(id, req.Projected, Stage("bogus-impossible-stage"), 0)
	log := &callLog{}
	store := newFakeStore(bogus)
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want error for an impossible saved stage")
	}
	if len(pub.calls) != 0 || len(scan.calls) != 0 {
		t.Errorf("side effects occurred for an impossible stage: add=%d scan=%d, want both 0", len(pub.calls), len(scan.calls))
	}
}

// TestResume_E5_ConflictingRerunFailsClosed covers E5: once published, a
// rerun that would change projected paths or canonical source identity for
// the same external identity must fail closed rather than overwrite.
func TestResume_E5_ConflictingRerunFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PublicationRequest)
	}{
		{
			name: "different_projected_paths",
			mutate: func(r *PublicationRequest) {
				r.Projected[0].Path = "J.R.R. Tolkien/The Fellowship of the Ring/Renamed.m4b"
			},
		},
		{
			name: "different_canonical_hash",
			mutate: func(r *PublicationRequest) {
				r.Selection.Hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// First run: commit a publish from a clean store.
			log1 := &callLog{}
			store := newFakeStore(emptyState())
			store.log = log1
			pub := &scriptedPublisher{log: log1, steps: []publishStep{{result: LibraryPublishResult{}}}}
			scan := &scriptedScanner{log: log1, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
			deps := publishDeps(store, pub, scan)
			first := basePublicationRequest()
			if _, err := Publish(context.Background(), deps, first); err != nil {
				t.Fatalf("initial Publish() error = %v, want nil", err)
			}
			committed := cloneState(store.state)

			// Second run: same identity, conflicting content.
			second := basePublicationRequest()
			tc.mutate(&second)
			pub.steps = append(pub.steps, publishStep{result: LibraryPublishResult{}})
			scan.steps = append(scan.steps, scanStep{result: AudiobookshelfScanResult{StatusCode: 200}})

			_, err := Publish(context.Background(), deps, second)
			if err == nil {
				t.Fatalf("Publish() error = nil, want error for a conflicting rerun (%s)", tc.name)
			}
			if len(pub.calls) != 1 {
				t.Errorf("Publisher.Add called %d times total, want 1 (no second add attempted for a conflicting rerun)", len(pub.calls))
			}
			if !stateEntriesEqual(store.state, committed) {
				t.Errorf("stored state changed on a conflicting rerun: got %+v, want unchanged %+v", store.state, committed)
			}
		})
	}
}

func projectedPaths(projected []ProjectedFile) []string {
	paths := make([]string, len(projected))
	for i, p := range projected {
		paths[i] = p.Path
	}
	return paths
}

func stateEntriesEqual(a, b State) bool {
	if a.Version != b.Version || len(a.Entries) != len(b.Entries) {
		return false
	}
	for k, av := range a.Entries {
		bv, ok := b.Entries[k]
		if !ok || av.Stage != bv.Stage || len(av.ProjectedPaths) != len(bv.ProjectedPaths) {
			return false
		}
		for i := range av.ProjectedPaths {
			if av.ProjectedPaths[i] != bv.ProjectedPaths[i] {
				return false
			}
		}
	}
	return true
}
