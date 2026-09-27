package audiobookimport

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

const testConsecutiveThreshold = 3

var testGrace = 24 * time.Hour

func removalDepsFor(store StateStore, rem LibraryRemover, scan AudiobookshelfScanner) RemovalDeps {
	return RemovalDeps{
		Remover:                 rem,
		Scanner:                 scan,
		Store:                   store,
		Policy:                  RemovalPolicy{ConsecutiveMissingThreshold: testConsecutiveThreshold, Grace: testGrace},
		Limits:                  PublicationLimits{StateLimits: StateLimits{MaxEntries: 1000, MaxStringLength: 4096}, MaxAttempts: 5},
		AudiobookshelfLibraryID: testLibraryID,
	}
}

func ownedScannedEntry(id ExternalIdentity, consecutiveMissing int, missingSince time.Time) StateEntry {
	return StateEntry{
		Namespace:          id.Namespace,
		ExternalID:         id.ID,
		ProjectedPaths:     []string{"J.R.R. Tolkien/The Fellowship of the Ring/The Fellowship of the Ring.m4b"},
		OwnedPrefix:        "J.R.R. Tolkien/The Fellowship of the Ring",
		ControllerOwned:    true,
		Stage:              StageScanned,
		ConsecutiveMissing: consecutiveMissing,
		MissingSince:       missingSince,
		UpdatedAt:          time.Now(),
	}
}

func removalIdentity(t *testing.T) ExternalIdentity {
	t.Helper()
	return publishedIdentity(t)
}

func freshRemovalDeps(entry StateEntry) (RemovalDeps, *scriptedRemover, *scriptedScanner, *fakeStore, ExternalIdentity) {
	id := ExternalIdentity{Namespace: entry.Namespace, ID: entry.ExternalID}
	log := &callLog{}
	store := newFakeStore(buildState(entry))
	store.log = log
	rem := &scriptedRemover{log: log, errs: []error{nil}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	deps := removalDepsFor(store, rem, scan)
	return deps, rem, scan, store, id
}

// TestRemoval_A7_BelowThresholdNeverRemoves covers A7: an owned entry whose
// consecutive-missing count (after this run) stays below the configured
// threshold must not be removed.
func TestRemoval_A7_BelowThresholdNeverRemoves(t *testing.T) {
	id := removalIdentity(t)
	entry := ownedScannedEntry(id, testConsecutiveThreshold-2, time.Now().Add(-2*testGrace))
	deps, rem, scan, _, _ := freshRemovalDeps(entry)

	now := time.Now()
	result, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 below threshold", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 when nothing was removed", len(scan.calls))
	}
	if len(result.Removed) != 0 {
		t.Errorf("Removed = %v, want empty", result.Removed)
	}
}

// TestRemoval_A7_ThresholdMetButGraceNotElapsedNeverRemoves covers A7's
// "both satisfied" requirement in isolation: the consecutive-missing count
// reaches the threshold, but the grace duration has not yet elapsed.
func TestRemoval_A7_ThresholdMetButGraceNotElapsedNeverRemoves(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-1*time.Minute))
	deps, rem, scan, _, _ := freshRemovalDeps(entry)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 while grace has not elapsed", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestRemoval_A7_BothThresholdsSatisfiedRemovesOwnedPrefixOnce covers A7:
// once both the consecutive-missing and grace thresholds are satisfied, the
// controller removes exactly the owned prefix once, commits before
// scanning, then scans.
func TestRemoval_A7_BothThresholdsSatisfiedRemovesOwnedPrefixOnce(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-testGrace))
	deps, rem, scan, store, _ := freshRemovalDeps(entry)

	result, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 1 {
		t.Fatalf("Remover.Remove called %d times, want exactly 1", len(rem.calls))
	}
	if rem.calls[0].Type != LibraryTypeAudiobook {
		t.Errorf("Remove request Type = %q, want %q", rem.calls[0].Type, LibraryTypeAudiobook)
	}
	if rem.calls[0].Prefix != entry.OwnedPrefix {
		t.Errorf("Remove request Prefix = %q, want %q", rem.calls[0].Prefix, entry.OwnedPrefix)
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1", len(scan.calls))
	}
	if len(result.Removed) != 1 || result.Removed[0] != id {
		t.Errorf("Removed = %v, want [%+v]", result.Removed, id)
	}
	removeSeq := store.log.indexOf("remover.remove")
	scanSeq := store.log.indexOf("scanner.scan")
	if removeSeq < 0 || scanSeq < 0 || scanSeq < removeSeq {
		t.Errorf("expected remover.remove before scanner.scan, got %v", store.log.names())
	}
	// A save recording exactly StageRemoved must land strictly before the
	// scan call - not merely "some save eventually contained Removed",
	// which an implementation that scans immediately after remove and saves
	// Removed only afterward would still satisfy.
	if !stageDurablyReachedBefore(store, id, StageRemoved, scanSeq) {
		t.Errorf("no save recorded exactly Stage=%q strictly before scanner.scan (seq %d); timeline=%+v", StageRemoved, scanSeq, stageTimeline(store, id))
	}
}

// TestRemoval_A7_FirstAbsencePersistsCountOneAndMissingSinceNow covers A7:
// the first run that observes an owned entry absent must durably persist
// ConsecutiveMissing=1 and MissingSince=now - missing tracking must not be
// left unchanged forever, or a below-threshold entry could stay
// unremovable indefinitely even after truly going missing for a long time.
func TestRemoval_A7_FirstAbsencePersistsCountOneAndMissingSinceNow(t *testing.T) {
	id := removalIdentity(t)
	entry := ownedScannedEntry(id, 0, time.Time{})
	deps, rem, _, store, _ := freshRemovalDeps(entry)

	now := time.Now().Truncate(time.Second)
	if _, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 (still below threshold on first absence)", len(rem.calls))
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded for the first observed absence")
	}
	got, ok := last.Entries[id.Key()]
	if !ok {
		t.Fatalf("no durable entry recorded for %+v", id)
	}
	if got.ConsecutiveMissing != 1 {
		t.Errorf("ConsecutiveMissing = %d after the first absence, want 1", got.ConsecutiveMissing)
	}
	if !got.MissingSince.Equal(now) {
		t.Errorf("MissingSince = %v, want %v (now, on first observed absence)", got.MissingSince, now)
	}
}

// TestRemoval_A7_LaterAbsenceIncrementsWithoutChangingFirstTimestamp covers
// A7: once MissingSince is set, a later run that still finds the entry
// absent must increment the count but must not move MissingSince forward -
// otherwise the grace window would reset every run and the entry could
// never actually satisfy the grace threshold.
func TestRemoval_A7_LaterAbsenceIncrementsWithoutChangingFirstTimestamp(t *testing.T) {
	id := removalIdentity(t)
	firstMissing := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	entry := ownedScannedEntry(id, 1, firstMissing)
	deps, rem, _, store, _ := freshRemovalDeps(entry)

	later := firstMissing.Add(2 * time.Hour) // still short of testGrace (24h) and below testConsecutiveThreshold
	if _, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, later); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 (still below threshold/grace)", len(rem.calls))
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded for the later observed absence")
	}
	got, ok := last.Entries[id.Key()]
	if !ok {
		t.Fatalf("no durable entry recorded for %+v", id)
	}
	if got.ConsecutiveMissing != 2 {
		t.Errorf("ConsecutiveMissing = %d after a second consecutive absence, want 2", got.ConsecutiveMissing)
	}
	if !got.MissingSince.Equal(firstMissing) {
		t.Errorf("MissingSince = %v, want unchanged %v (the first-observed timestamp)", got.MissingSince, firstMissing)
	}
}

// TestRemoval_A7_ReappearedEntryIsNeverRemoved covers A7: an entry that
// reappears in the seen set must never be removed, and its missing
// tracking must durably reset, regardless of prior counts.
func TestRemoval_A7_ReappearedEntryIsNeverRemoved(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold+5, now.Add(-10*testGrace))
	deps, rem, scan, store, _ := freshRemovalDeps(entry)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{id: true}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 for a reappeared entry", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
	last, ok := store.lastSave()
	if !ok {
		t.Fatalf("no durable save recorded a reset for the reappeared entry")
	}
	got, ok := last.Entries[id.Key()]
	if !ok {
		t.Fatalf("no durable entry recorded for %+v after reappearing", id)
	}
	if got.ConsecutiveMissing != 0 {
		t.Errorf("ConsecutiveMissing = %d after reappearing, want 0", got.ConsecutiveMissing)
	}
	if !got.MissingSince.IsZero() {
		t.Errorf("MissingSince = %v after reappearing, want zero", got.MissingSince)
	}
}

// TestRemoval_A7_ForeignOrManualEntryIsNeverRemoved covers A7: an entry not
// explicitly marked controller-owned must never be removed, even when
// every threshold is met.
func TestRemoval_A7_ForeignOrManualEntryIsNeverRemoved(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold+10, now.Add(-10*testGrace))
	entry.ControllerOwned = false
	deps, rem, scan, _, _ := freshRemovalDeps(entry)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 for a foreign/manual entry", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestRemoval_A7_UnsafeOrEmptyOwnedPrefixNeverRemoved covers A7/E1/I4: an
// owned, threshold-satisfied entry whose recorded prefix is unsafe (path
// traversal / absolute) or empty must fail closed before any remove call,
// the same as an unsafe projected path must during publish.
func TestRemoval_A7_UnsafeOrEmptyOwnedPrefixNeverRemoved(t *testing.T) {
	for _, prefix := range []string{"../../foreign", "/etc", "", "C:\\Windows"} {
		t.Run(prefix, func(t *testing.T) {
			id := removalIdentity(t)
			now := time.Now()
			entry := ownedScannedEntry(id, testConsecutiveThreshold+2, now.Add(-10*testGrace))
			entry.OwnedPrefix = prefix
			deps, rem, scan, _, _ := freshRemovalDeps(entry)

			_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
			if err == nil {
				t.Fatalf("Reconcile() error = nil, want error for an unsafe/empty owned prefix %q", prefix)
			}
			if len(rem.calls) != 0 {
				t.Errorf("Remover.Remove called %d times, want 0 for prefix %q", len(rem.calls), prefix)
			}
			if len(scan.calls) != 0 {
				t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
			}
		})
	}
}

// TestRemoval_I4_MultipleEligibleEntriesProcessedInStableKeyOrder covers
// A7/I3/I4: when two entries are simultaneously eligible for removal in one
// Reconcile call, the resulting Remove order must be deterministic (stable
// by identity key), not an accident of Go's randomized map iteration order.
// Both maps below hold the exact same two entries, constructed with their
// keys inserted in opposite source order, and are Reconciled independently
// several times: every resulting Remove order must match the single
// identity-key-sorted order.
func TestRemoval_I4_MultipleEligibleEntriesProcessedInStableKeyOrder(t *testing.T) {
	now := time.Now()
	idA, err := ResolveExternalIdentity(pubWork(), pubRecordingWithID())
	if err != nil {
		t.Fatalf("ResolveExternalIdentity() error = %v, want nil", err)
	}
	idB, err := ResolveExternalIdentity(secondPubWork(), RecordingFacts{})
	if err != nil {
		t.Fatalf("ResolveExternalIdentity() error = %v, want nil", err)
	}
	if idA == idB {
		t.Fatalf("test fixture bug: idA and idB must be distinct, got %+v twice", idA)
	}
	entryA := ownedScannedEntry(idA, testConsecutiveThreshold+1, now.Add(-2*testGrace))
	entryA.OwnedPrefix = "J.R.R. Tolkien/The Fellowship of the Ring"
	entryB := ownedScannedEntry(idB, testConsecutiveThreshold+1, now.Add(-2*testGrace))
	entryB.OwnedPrefix = "J.R.R. Tolkien/The Two Towers"

	wantOrder := []string{idA.Key(), idB.Key()}
	sort.Strings(wantOrder)

	var orders [][]string
	for run := 0; run < 8; run++ {
		// Alternate literal insertion order across runs; Go map iteration is
		// randomized independent of literal order, so repeating the
		// Reconcile call is what actually exercises nondeterminism, not the
		// literal order by itself.
		var entries map[string]StateEntry
		if run%2 == 0 {
			entries = map[string]StateEntry{idA.Key(): entryA, idB.Key(): entryB}
		} else {
			entries = map[string]StateEntry{idB.Key(): entryB, idA.Key(): entryA}
		}
		log := &callLog{}
		store := newFakeStore(State{Version: CurrentStateVersion, Entries: entries})
		store.log = log
		rem := &scriptedRemover{log: log, errs: []error{nil, nil}}
		scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
		deps := removalDepsFor(store, rem, scan)

		if _, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now); err != nil {
			t.Fatalf("run %d: Reconcile() error = %v, want nil", run, err)
		}
		if len(rem.calls) != 2 {
			t.Fatalf("run %d: Remover.Remove called %d times, want exactly 2", run, len(rem.calls))
		}
		gotOrder := []string{rem.calls[0].Prefix, rem.calls[1].Prefix}
		orders = append(orders, gotOrder)
	}
	wantPrefixOrder := []string{entryA.OwnedPrefix, entryB.OwnedPrefix}
	if idB.Key() < idA.Key() {
		wantPrefixOrder = []string{entryB.OwnedPrefix, entryA.OwnedPrefix}
	}
	for run, got := range orders {
		if got[0] != wantPrefixOrder[0] || got[1] != wantPrefixOrder[1] {
			t.Errorf("run %d: Remove order = %v, want stable key order %v", run, got, wantPrefixOrder)
		}
	}
	for run := 1; run < len(orders); run++ {
		if orders[run][0] != orders[0][0] || orders[run][1] != orders[0][1] {
			t.Errorf("run %d order %v differs from run 0 order %v, want identical (deterministic) order across runs", run, orders[run], orders[0])
		}
	}
}

// TestRemoval_E4_RemoveFailureLeavesEntryCommittedAndDoesNotScan covers E4:
// a removal failure must leave the owned entry in its last committed stage
// and must not scan.
func TestRemoval_E4_RemoveFailureLeavesEntryCommittedAndDoesNotScan(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-testGrace))
	deps, rem, scan, store, _ := freshRemovalDeps(entry)
	rem.errs = []error{errors.New("remove failed")}

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err == nil {
		t.Fatalf("Reconcile() error = nil, want an error from the failing remove")
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 after a failed remove", len(scan.calls))
	}
	if e, ok := store.state.Entries[id.Key()]; !ok || e.Stage == StageRemoved || e.Stage == StageRemovalScanned {
		t.Errorf("committed entry = %+v (ok=%v), want it to remain at its last durable stage (%q), not advanced to removed", e, ok, StageScanned)
	}
}

// TestRemoval_E4_PostRemoveStoreFailureAllowsRetriedRemoveOnResume covers
// E4's hardest case, and is discussed in the accompanying report: Remover.
// Remove succeeds, but the store write that would durably record that
// success fails. Exactly-once external removal cannot be guaranteed by the
// declared interfaces here - LibraryRemover.Remove returns only an error,
// with no AlreadyAbsent/idempotency signal analogous to
// LibraryPublishResult.AlreadyPresent - so once the durable write fails,
// Reconcile cannot tell from local state alone whether the external remove
// already took effect.
//
// The safe-resume contract this test pins is the smallest one achievable
// with the current interfaces: (1) Reconcile returns an error and must not
// scan, since removal durability was not proven; (2) the durably committed
// stage must not advance to Removed, since that was never confirmed; (3) a
// later resumed Reconcile call is REQUIRED to retry Remove - state alone
// cannot distinguish "never attempted" from "attempted but unrecorded" -
// so this test deliberately does NOT assert "no duplicate remove" (that
// guarantee is exclusive to TestRemoval_E4_ScanFailureResumesFromRemovalWithoutSecondRemove,
// which covers the case where the durable write DID succeed). Real safety
// therefore depends on the production LibraryRemover being idempotent for
// an already-absent prefix, the same idempotency Library.Add already
// exposes via AlreadyPresent; this test's second call models that
// expectation by succeeding.
func TestRemoval_E4_PostRemoveStoreFailureAllowsRetriedRemoveOnResume(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-testGrace))
	log := &callLog{}
	store := newFakeStore(buildState(entry))
	store.log = log
	store.failSave = func(s State) bool {
		e, ok := s.Entries[id.Key()]
		return ok && e.Stage == StageRemoved
	}
	rem := &scriptedRemover{log: log, errs: []error{nil}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	deps := removalDepsFor(store, rem, scan)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err == nil {
		t.Fatalf("Reconcile() error = nil, want an error when the post-remove durable write fails")
	}
	if len(rem.calls) != 1 {
		t.Errorf("Remover.Remove called %d times, want exactly 1 (the remove itself succeeded)", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 (removal durability was not proven)", len(scan.calls))
	}
	if e, ok := store.state.Entries[id.Key()]; ok && e.Stage == StageRemoved {
		t.Errorf("committed entry = %+v, want Stage != %q (the failed save must not be treated as durable)", e, StageRemoved)
	}

	// Resume: the store write now succeeds. State alone cannot tell "remove
	// never happened" from "remove happened but wasn't recorded", so a
	// retried Remove call is required, not merely permitted - see the
	// safe-resume contract discussed above.
	store.failSave = nil
	rem.errs = append(rem.errs, nil)
	result, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("resumed Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 2 {
		t.Errorf("Remover.Remove called %d times across both attempts, want 2 (the resume must retry remove, since the store could not confirm the first attempt)", len(rem.calls))
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1 once removal is durably recorded", len(scan.calls))
	}
	if len(result.Removed) != 1 || result.Removed[0] != id {
		t.Errorf("Removed = %v, want [%+v]", result.Removed, id)
	}
}

// TestRemoval_E4_ScanFailureResumesFromRemovalWithoutSecondRemove covers
// E4: a scan failure after a durably committed removal must not attempt a
// second remove, and a later resume must call scan only.
func TestRemoval_E4_ScanFailureResumesFromRemovalWithoutSecondRemove(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-testGrace))
	log := &callLog{}
	store := newFakeStore(buildState(entry))
	store.log = log
	rem := &scriptedRemover{log: log, errs: []error{nil}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{err: errors.New("scan failed")}}}
	deps := removalDepsFor(store, rem, scan)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err == nil {
		t.Fatalf("Reconcile() error = nil, want an error from the failing scan")
	}
	if len(rem.calls) != 1 {
		t.Errorf("Remover.Remove called %d times, want exactly 1", len(rem.calls))
	}

	// Resume: same store, no further mutation of the missing/seen inputs.
	// scriptedScanner indexes its steps by the running call count
	// (len(s.calls)), which the first Reconcile call already advanced to 1,
	// so the resumed answer must be appended, not substituted in place, or
	// the second call would index past the replacement script and fail as
	// "unscripted" for a harness reason unrelated to the behavior under test.
	rem.errs = append(rem.errs, errors.New("scriptedRemover: should not be called again"))
	scan.steps = append(scan.steps, scanStep{result: AudiobookshelfScanResult{StatusCode: 200}})
	scanCallsBeforeResume := len(scan.calls)
	result, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("resumed Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 1 {
		t.Errorf("Remover.Remove called %d times across both attempts, want 1 (no duplicate remove)", len(rem.calls))
	}
	// scan.calls accumulates across both Reconcile invocations (the first,
	// failed attempt already recorded one), so the resumed-call obligation
	// is exactly one additional Scan on top of whatever was already there,
	// not a reset to 1 in total.
	if got := len(scan.calls) - scanCallsBeforeResume; got != 1 {
		t.Errorf("Scanner.Scan called %d times on the resumed invocation, want exactly 1", got)
	}
	if len(result.Removed) != 1 || result.Removed[0] != id {
		t.Errorf("Removed = %v, want [%+v] on the resumed call", result.Removed, id)
	}
}

// TestRemoval_A7_ScanOnlyResumeAfterPriorCommittedRemoval covers A7's final
// adversarial case directly: a prior run already committed the removal
// (Stage=Removed); a fresh Reconcile call must scan only.
func TestRemoval_A7_ScanOnlyResumeAfterPriorCommittedRemoval(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold, now.Add(-testGrace))
	entry.Stage = StageRemoved
	deps, rem, scan, _, _ := freshRemovalDeps(entry)

	result, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 (already committed)", len(rem.calls))
	}
	if len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1", len(scan.calls))
	}
	if len(result.Removed) != 1 || result.Removed[0] != id {
		t.Errorf("Removed = %v, want [%+v]", result.Removed, id)
	}
}

// TestRemoval_I2_UnpublishedEntryIsNeverRemoved covers I2: an entry that
// never reached a published/scanned stage is not a valid removal source and
// must fail closed before any external call, even if marked owned.
func TestRemoval_I2_UnpublishedEntryIsNeverRemoved(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold+3, now.Add(-10*testGrace))
	entry.Stage = StagePlanned
	deps, rem, scan, _, _ := freshRemovalDeps(entry)

	_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (an unpublished entry is simply skipped, not fatal)", err)
	}
	if len(rem.calls) != 0 {
		t.Errorf("Remover.Remove called %d times, want 0 for an entry that was never published", len(rem.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
}

// TestRemoval_E1_InvalidPolicyFailsBeforeCalls covers E1/I3: invalid
// removal policy/limits values must fail before any external call.
func TestRemoval_E1_InvalidPolicyFailsBeforeCalls(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold, now.Add(-10*testGrace))

	for _, tc := range []struct {
		name   string
		mutate func(*RemovalDeps)
	}{
		{name: "nil_remover", mutate: func(d *RemovalDeps) { d.Remover = nil }},
		{name: "nil_scanner", mutate: func(d *RemovalDeps) { d.Scanner = nil }},
		{name: "nil_store", mutate: func(d *RemovalDeps) { d.Store = nil }},
		{name: "negative_consecutive_threshold", mutate: func(d *RemovalDeps) { d.Policy.ConsecutiveMissingThreshold = -1 }},
		{name: "negative_grace", mutate: func(d *RemovalDeps) { d.Policy.Grace = -1 * time.Hour }},
		{name: "empty_library_id", mutate: func(d *RemovalDeps) { d.AudiobookshelfLibraryID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, rem, scan, _, _ := freshRemovalDeps(entry)
			tc.mutate(&deps)

			_, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, now)
			if err == nil {
				t.Fatalf("Reconcile() error = nil, want error for %s", tc.name)
			}
			if len(rem.calls) != 0 || len(scan.calls) != 0 {
				t.Errorf("side effects occurred for invalid input %s: remove=%d scan=%d, want both 0", tc.name, len(rem.calls), len(scan.calls))
			}
		})
	}
}

// TestRemoval_I1_ContextPropagatesToEveryDependency covers I1: the caller's
// context reaches Remover, Scanner, and Store - not just the external
// calls but every Load/Save as well.
func TestRemoval_I1_ContextPropagatesToEveryDependency(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold-1, now.Add(-testGrace))
	deps, rem, scan, store, _ := freshRemovalDeps(entry)

	ctx := withMarker(context.Background(), "reconcile-marker")
	if _, err := Reconcile(ctx, deps, map[ExternalIdentity]bool{}, now); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	for i, m := range rem.markers {
		if m != "reconcile-marker" {
			t.Errorf("Remover.Remove call %d marker = %q, want %q", i, m, "reconcile-marker")
		}
	}
	for i, m := range scan.markers {
		if m != "reconcile-marker" {
			t.Errorf("Scanner.Scan call %d marker = %q, want %q", i, m, "reconcile-marker")
		}
	}
	if len(store.loadMarkers) == 0 {
		t.Errorf("Store.Load was never called, want at least one call to prove context propagation")
	}
	for i, m := range store.loadMarkers {
		if m != "reconcile-marker" {
			t.Errorf("Store.Load call %d marker = %q, want %q", i, m, "reconcile-marker")
		}
	}
	if len(store.saveMarkers) == 0 {
		t.Errorf("Store.Save was never called, want at least one call to prove context propagation")
	}
	for i, m := range store.saveMarkers {
		if m != "reconcile-marker" {
			t.Errorf("Store.Save call %d marker = %q, want %q", i, m, "reconcile-marker")
		}
	}
}

// TestRemoval_I1_CancellationBeforeStartPreventsAllSideEffects covers I1:
// cancellation before Reconcile starts must prevent every side effect and
// surface an errors.Is-compatible error.
func TestRemoval_I1_CancellationBeforeStartPreventsAllSideEffects(t *testing.T) {
	id := removalIdentity(t)
	now := time.Now()
	entry := ownedScannedEntry(id, testConsecutiveThreshold, now.Add(-10*testGrace))
	deps, rem, scan, store, _ := freshRemovalDeps(entry)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Reconcile(ctx, deps, map[ExternalIdentity]bool{}, now)
	if err == nil {
		t.Fatalf("Reconcile() error = nil, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(rem.calls) != 0 || len(scan.calls) != 0 || len(store.saveCalls) != 0 {
		t.Errorf("side effects occurred after cancellation: remove=%d scan=%d save=%d, want all 0", len(rem.calls), len(scan.calls), len(store.saveCalls))
	}
}

// TestRemoval_I3_ExtremeCountersDoNotPanic covers I3: extreme stored
// counters/timestamps/durations must not overflow or panic Reconcile.
func TestRemoval_I3_ExtremeCountersDoNotPanic(t *testing.T) {
	id := removalIdentity(t)
	entry := ownedScannedEntry(id, int(^uint(0)>>1), time.Unix(0, 0))
	deps, _, _, _, _ := freshRemovalDeps(entry)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Reconcile() panicked on extreme counters: %v", r)
		}
	}()
	if _, err := Reconcile(context.Background(), deps, map[ExternalIdentity]bool{}, time.Now()); err != nil {
		t.Logf("Reconcile() returned error %v for extreme counters (acceptable if rejected safely)", err)
	}
}
