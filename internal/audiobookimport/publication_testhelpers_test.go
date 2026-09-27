package audiobookimport

import (
	"context"
	"fmt"
)

// --- fixtures ---------------------------------------------------------

// pubWork is a fully specified work identity for publication tests. It
// reuses the WorkID convention established by baseWork() (testhelpers_test.go)
// so publication tests do not silently drift from evaluation/projection
// fixtures.
func pubWork() WorkFacts {
	return WorkFacts{
		WorkID:  "work-fellowship-1",
		Title:   "The Fellowship of the Ring",
		Authors: []string{"J.R.R. Tolkien"},
	}
}

// secondPubWork is a second, distinct work identity (different WorkID/
// Title) used by tests that need two simultaneously-eligible, genuinely
// distinct entries in the same durable state (e.g. deterministic
// multi-entry removal order).
func secondPubWork() WorkFacts {
	return WorkFacts{
		WorkID:  "work-two-towers-1",
		Title:   "The Two Towers",
		Authors: []string{"J.R.R. Tolkien"},
	}
}

// pubRecordingWithID carries an explicit AudioSilo recording id: A2 requires
// this to win over the work id whenever present.
func pubRecordingWithID() RecordingFacts {
	return RecordingFacts{
		RecordingID: "rec-inglis-unabridged",
		Narrators:   []string{"Rob Inglis"},
		Language:    "en-GB",
	}
}

// pubRecordingWithoutID has every optional field an ASIN-only release might
// carry, but no RecordingID: A2 must fall back to the work id and must never
// treat the ASIN as identity.
func pubRecordingWithoutID() RecordingFacts {
	return RecordingFacts{
		ASINs:     []string{"B000AY7HGY"},
		Narrators: []string{"Rob Inglis"},
		Language:  "en-GB",
	}
}

// singleFileDecision is one eligible, single-M4B decision whose selected
// file index matches sourceFiles()'s single entry.
func singleFileDecision() Decision {
	return Decision{
		Candidate: ReleaseCandidate{
			Title:     "The Fellowship of the Ring - J.R.R. Tolkien [Unabridged]",
			Recording: pubRecordingWithID(),
			Files:     singleSourceFiles(),
		},
		Eligible: true,
		Selected: []SelectedFile{{FileIndex: 3, Part: 0}},
	}
}

func singleSourceFiles() []AudioFile {
	return []AudioFile{{Index: 3, Path: "inbox/Fellowship/Fellowship.m4b", Size: 500 << 20}}
}

// singleFileProjected returns the exact PlanProjection output for
// singleFileDecision(), computed through the real (already-implemented)
// PlanProjection so this fixture cannot silently drift from projection.go's
// actual naming rules.
func singleFileProjected() []ProjectedFile {
	projected, err := PlanProjection(pubWork(), []Decision{singleFileDecision()})
	if err != nil {
		panic(fmt.Sprintf("singleFileProjected: PlanProjection failed: %v", err))
	}
	return projected
}

// validHash is a well-formed 40-character hex infohash.
const validHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// torrentBytesSelection is a SelectionResult sourced from exact torrent
// bytes (no magnet).
func torrentBytesSelection() SelectionResult {
	d := singleFileDecision()
	return SelectionResult{
		Selected:    &d,
		Hash:        validHash,
		TorrentFile: []byte{0xDE, 0xAD, 0xBE, 0xEF},
		Files:       singleSourceFiles(),
	}
}

// magnetSelection is a SelectionResult sourced from a full magnet URI,
// including tracker and webseed query parameters.
func magnetSelection() SelectionResult {
	d := singleFileDecision()
	return SelectionResult{
		Selected: &d,
		Hash:     validHash,
		Magnet:   "magnet:?xt=urn:btih:" + validHash + "&dn=Fellowship&tr=udp%3A%2F%2Ftracker.example%3A80&ws=https%3A%2F%2Fweb.example%2Ffile",
		Files:    singleSourceFiles(),
	}
}

// hashOnlySelection is a SelectionResult carrying neither torrent bytes nor
// a magnet: the canonical hash alone identifies the source.
func hashOnlySelection() SelectionResult {
	d := singleFileDecision()
	return SelectionResult{Selected: &d, Hash: validHash, Files: singleSourceFiles()}
}

func basePublicationRequest() PublicationRequest {
	return PublicationRequest{
		Work:      pubWork(),
		Recording: pubRecordingWithID(),
		Selection: torrentBytesSelection(),
		Projected: singleFileProjected(),
	}
}

// --- fakes --------------------------------------------------------------

// callEvent is one recorded dependency invocation, in global call order, so
// tests can assert cross-dependency ordering (e.g. "store save before scan").
type callEvent struct {
	name string
	seq  int
}

type callLog struct {
	events []callEvent
}

func (l *callLog) record(name string) int {
	seq := len(l.events)
	l.events = append(l.events, callEvent{name: name, seq: seq})
	return seq
}

func (l *callLog) names() []string {
	names := make([]string, len(l.events))
	for i, e := range l.events {
		names[i] = e.name
	}
	return names
}

func (l *callLog) indexOf(name string) int {
	for _, e := range l.events {
		if e.name == name {
			return e.seq
		}
	}
	return -1
}

func (l *callLog) count(name string) int {
	n := 0
	for _, e := range l.events {
		if e.name == name {
			n++
		}
	}
	return n
}

// seqOfNth returns the global call-order sequence of the n-th (0-based)
// occurrence of name, or -1 if there are fewer than n+1 occurrences. It lets
// a test correlate a specific store.save call (by its position among all
// Save calls, which lines up 1:1 with fakeStore.saveCalls) with that save's
// true position in the overall cross-dependency call order.
func (l *callLog) seqOfNth(name string, n int) int {
	seen := 0
	for _, e := range l.events {
		if e.name != name {
			continue
		}
		if seen == n {
			return e.seq
		}
		seen++
	}
	return -1
}

// publishStep is one scripted answer for a scriptedPublisher call.
type publishStep struct {
	result LibraryPublishResult
	err    error
}

// scriptedPublisher answers exactly its scripted steps, in call order. A
// call beyond the script is a loud, distinct error rather than a silent
// zero value, so an implementation that retries or double-adds fails
// clearly instead of being masked.
type scriptedPublisher struct {
	log     *callLog
	steps   []publishStep
	calls   []LibraryPublishRequest
	markers []string
}

func (p *scriptedPublisher) Add(ctx context.Context, req LibraryPublishRequest) (LibraryPublishResult, error) {
	if p.log != nil {
		p.log.record("publisher.add")
	}
	idx := len(p.calls)
	p.calls = append(p.calls, req)
	p.markers = append(p.markers, markerFrom(ctx))
	if idx >= len(p.steps) {
		return LibraryPublishResult{}, fmt.Errorf("scriptedPublisher: unscripted call %d", idx+1)
	}
	step := p.steps[idx]
	return step.result, step.err
}

// scriptedRemover is the LibraryRemover analogue of scriptedPublisher.
type scriptedRemover struct {
	log     *callLog
	errs    []error
	calls   []LibraryRemoveRequest
	markers []string
}

func (r *scriptedRemover) Remove(ctx context.Context, req LibraryRemoveRequest) error {
	if r.log != nil {
		r.log.record("remover.remove")
	}
	idx := len(r.calls)
	r.calls = append(r.calls, req)
	r.markers = append(r.markers, markerFrom(ctx))
	if idx >= len(r.errs) {
		return fmt.Errorf("scriptedRemover: unscripted call %d", idx+1)
	}
	return r.errs[idx]
}

// scanStep is one scripted answer for a scriptedScanner call.
type scanStep struct {
	result AudiobookshelfScanResult
	err    error
}

// scriptedScanner is the AudiobookshelfScanner analogue of scriptedPublisher.
type scriptedScanner struct {
	log        *callLog
	steps      []scanStep
	calls      []string // library IDs
	markers    []string
	onEachCall func()
}

func (s *scriptedScanner) Scan(ctx context.Context, libraryID string) (AudiobookshelfScanResult, error) {
	if s.log != nil {
		s.log.record("scanner.scan")
	}
	idx := len(s.calls)
	s.calls = append(s.calls, libraryID)
	s.markers = append(s.markers, markerFrom(ctx))
	if s.onEachCall != nil {
		s.onEachCall()
	}
	if idx >= len(s.steps) {
		return AudiobookshelfScanResult{}, fmt.Errorf("scriptedScanner: unscripted call %d", idx+1)
	}
	step := s.steps[idx]
	return step.result, step.err
}

func cloneState(s State) State {
	clone := State{Version: s.Version, Entries: make(map[string]StateEntry, len(s.Entries))}
	for k, v := range s.Entries {
		entry := v
		entry.ProjectedPaths = append([]string(nil), v.ProjectedPaths...)
		clone.Entries[k] = entry
	}
	return clone
}

// fakeStore is an in-memory StateStore. failSave, when non-nil, is
// evaluated against the state about to be written; a true result fails that
// specific Save call (without mutating the store), so tests can target
// exactly the write that would durably commit a specific transition (e.g.
// "the write that records the successful add") without hard-coding a call
// index that varies across conforming implementations.
type fakeStore struct {
	log         *callLog
	state       State
	loadErr     error
	failSave    func(State) bool
	saveErr     error
	saveCalls   []State
	loadCalls   int
	loadMarkers []string
	saveMarkers []string
}

func newFakeStore(initial State) *fakeStore {
	return &fakeStore{state: cloneState(initial)}
}

func (f *fakeStore) Load(ctx context.Context) (State, error) {
	if f.log != nil {
		f.log.record("store.load")
	}
	f.loadCalls++
	f.loadMarkers = append(f.loadMarkers, markerFrom(ctx))
	if f.loadErr != nil {
		return State{}, f.loadErr
	}
	return cloneState(f.state), nil
}

func (f *fakeStore) Save(ctx context.Context, state State) error {
	if f.log != nil {
		f.log.record("store.save")
	}
	f.saveMarkers = append(f.saveMarkers, markerFrom(ctx))
	snapshot := cloneState(state)
	f.saveCalls = append(f.saveCalls, snapshot)
	if f.failSave != nil && f.failSave(snapshot) {
		if f.saveErr != nil {
			return f.saveErr
		}
		return fmt.Errorf("fakeStore: scripted save failure")
	}
	f.state = snapshot
	return nil
}

func (f *fakeStore) lastSave() (State, bool) {
	if len(f.saveCalls) == 0 {
		return State{}, false
	}
	return f.saveCalls[len(f.saveCalls)-1], true
}

// emptyState is a fresh, valid, empty durable state.
func emptyState() State {
	return State{Version: CurrentStateVersion, Entries: map[string]StateEntry{}}
}

const testLibraryID = "lib-audiobooks-1"

// --- stage-correlated ordering helpers -----------------------------------

// stageSaveEvent pairs one store.save call's true global position in the
// shared callLog with the stage it recorded for one tracked identity, so a
// test can prove "this specific save, at this specific point in the overall
// call order, recorded this stage" rather than merely "some save eventually
// contained this stage".
type stageSaveEvent struct {
	seq     int
	stage   Stage
	present bool
}

// stageTimeline returns one stageSaveEvent per Save call the store
// received, in call order, for the given identity.
func stageTimeline(store *fakeStore, id ExternalIdentity) []stageSaveEvent {
	timeline := make([]stageSaveEvent, len(store.saveCalls))
	for i, snapshot := range store.saveCalls {
		seq := store.log.seqOfNth("store.save", i)
		entry, ok := snapshot.Entries[id.Key()]
		timeline[i] = stageSaveEvent{seq: seq, stage: entry.Stage, present: ok}
	}
	return timeline
}

// stageDurablyReachedBefore reports whether some Save call recorded the
// identity at exactly `want` (not merely "eventually reached that stage at
// some point"), and that save's true position in the overall call order is
// strictly before beforeSeq. It is the tool for proving e.g. "Published was
// durably saved before scanner.scan started", which a save that lands
// chronologically after the scan call cannot satisfy even if its content
// happens to equal Published.
func stageDurablyReachedBefore(store *fakeStore, id ExternalIdentity, want Stage, beforeSeq int) bool {
	for _, ev := range stageTimeline(store, id) {
		if ev.present && ev.stage == want && ev.seq < beforeSeq {
			return true
		}
	}
	return false
}

// buildState assembles a State whose map is keyed by each entry's own
// ExternalIdentity.Key(), so fixtures never construct a state whose map key
// disagrees with the entry it stores (the mismatch a conflicting-key test
// must trigger deliberately, not one every other fixture stumbles into by
// using unrelated placeholder keys).
func buildState(entries ...StateEntry) State {
	m := make(map[string]StateEntry, len(entries))
	for _, e := range entries {
		id := ExternalIdentity{Namespace: e.Namespace, ID: e.ExternalID}
		m[id.Key()] = e
	}
	return State{Version: CurrentStateVersion, Entries: m}
}
