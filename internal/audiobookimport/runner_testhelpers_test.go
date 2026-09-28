package audiobookimport

import (
	"context"
	"errors"
	"sync"
)

// This file provides deterministic in-memory fakes for the audiobook
// discovery runner (audiobook-discovery-runner slice, requirements A1-A8,
// I1-I5, E1-E5). None of these fakes touch the network, the filesystem, a
// clock, or a goroutine: every fake is driven purely by the calling
// goroutine, so no Sleep/Gosched/polling is required to make behavior
// deterministic.
//
// The production seam these fakes implement (DiscoveryProvider,
// OwnedInventory, CommittedIdentitySource, Pacer, CandidateSourceSelector,
// CandidatePublisher, DiscoveryLimits, RunnerDeps, RunRequest, ResultRow,
// RunResult, Run, plus the DiscoveryReason/CandidateStatus/Reason constants
// referenced below) was accepted from the lead exactly as requested.

// withMarker/markerFrom (defined in sourceselection_testhelpers_test.go) let
// a test attach an opaque value to the context passed into Run and then
// assert every injected dependency actually observed that same context,
// rather than a freshly built one, at its own call boundary. This is the
// deterministic replacement for a goroutine dump or wall-clock probe.

// fakeDiscoveryProvider answers SearchWorks/LatestWorks/WorkDetail from
// fixed maps so tests control exactly which AudioSilo rows the runner sees,
// without an httptest server.
type fakeDiscoveryProvider struct {
	mu sync.Mutex

	searchResults map[string][]AudioSiloWorkCard
	searchErr     map[string]error
	latest        []AudioSiloWorkCard
	latestErr     error
	details       map[string]AudioSiloWorkDetail
	detailErr     map[string]error

	searchCalls []struct {
		query string
		limit int
	}
	latestCalls []int
	detailCalls []string

	searchMarker string
	latestMarker string
	detailMarker string

	// onCall, when set, runs before every call and can cancel a context or
	// otherwise inject a side effect deterministically instead of sleeping.
	onCall func()
	// onDetailCall, when set, runs only for WorkDetail, before the context is
	// checked, so a test can target cancellation at detail resolution
	// specifically without also firing during Search/Latest.
	onDetailCall func(id string)
}

func newFakeDiscoveryProvider() *fakeDiscoveryProvider {
	return &fakeDiscoveryProvider{
		searchResults: map[string][]AudioSiloWorkCard{},
		searchErr:     map[string]error{},
		details:       map[string]AudioSiloWorkDetail{},
		detailErr:     map[string]error{},
	}
}

func (f *fakeDiscoveryProvider) SearchWorks(ctx context.Context, query string, limit int) ([]AudioSiloWorkCard, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.mu.Lock()
	f.searchMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls = append(f.searchCalls, struct {
		query string
		limit int
	}{query, limit})
	if err, ok := f.searchErr[query]; ok {
		return nil, err
	}
	return append([]AudioSiloWorkCard(nil), f.searchResults[query]...), nil
}

func (f *fakeDiscoveryProvider) LatestWorks(ctx context.Context, limit int) ([]AudioSiloWorkCard, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.mu.Lock()
	f.latestMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCalls = append(f.latestCalls, limit)
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return append([]AudioSiloWorkCard(nil), f.latest...), nil
}

func (f *fakeDiscoveryProvider) WorkDetail(ctx context.Context, id string) (AudioSiloWorkDetail, error) {
	if f.onCall != nil {
		f.onCall()
	}
	if f.onDetailCall != nil {
		f.onDetailCall(id)
	}
	f.mu.Lock()
	f.detailMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AudioSiloWorkDetail{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailCalls = append(f.detailCalls, id)
	if err, ok := f.detailErr[id]; ok {
		return AudioSiloWorkDetail{}, err
	}
	detail, ok := f.details[id]
	if !ok {
		return AudioSiloWorkDetail{}, errors.New("no such work in fake provider")
	}
	return detail, nil
}

func (f *fakeDiscoveryProvider) callCounts() (search, latest, detail int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.searchCalls), len(f.latestCalls), len(f.detailCalls)
}

func (f *fakeDiscoveryProvider) markers() (search, latest, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchMarker, f.latestMarker, f.detailMarker
}

// fakeOwnedInventory answers owned-author/series/series-gap questions from
// fixed maps.
type fakeOwnedInventory struct {
	authors    map[string]bool
	series     map[string]bool
	seriesGaps map[string][]SeriesGapCandidate
	authorsErr error
	seriesErr  error
	gapErrFor  map[string]error

	mu           sync.Mutex
	gapCalls     []string
	authorCalls  int
	seriesCalls  int
	authorMarker string
	seriesMarker string
	gapMarker    string
}

func newFakeOwnedInventory() *fakeOwnedInventory {
	return &fakeOwnedInventory{
		authors:    map[string]bool{},
		series:     map[string]bool{},
		seriesGaps: map[string][]SeriesGapCandidate{},
		gapErrFor:  map[string]error{},
	}
}

func (f *fakeOwnedInventory) OwnedAuthorIDs(ctx context.Context) (map[string]bool, error) {
	f.mu.Lock()
	f.authorMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorCalls++
	if f.authorsErr != nil {
		return nil, f.authorsErr
	}
	out := make(map[string]bool, len(f.authors))
	for k, v := range f.authors {
		out[k] = v
	}
	return out, nil
}

func (f *fakeOwnedInventory) OwnedSeriesIDs(ctx context.Context) (map[string]bool, error) {
	f.mu.Lock()
	f.seriesMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seriesCalls++
	if f.seriesErr != nil {
		return nil, f.seriesErr
	}
	out := make(map[string]bool, len(f.series))
	for k, v := range f.series {
		out[k] = v
	}
	return out, nil
}

func (f *fakeOwnedInventory) SeriesGapCandidates(ctx context.Context, seriesID string) ([]SeriesGapCandidate, error) {
	f.mu.Lock()
	f.gapMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gapCalls = append(f.gapCalls, seriesID)
	if err, ok := f.gapErrFor[seriesID]; ok {
		return nil, err
	}
	return append([]SeriesGapCandidate(nil), f.seriesGaps[seriesID]...), nil
}

func (f *fakeOwnedInventory) markers() (author, series, gap string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorMarker, f.seriesMarker, f.gapMarker
}

// fakeCommittedIdentitySource answers already-known-identity questions from
// fixed sets.
type fakeCommittedIdentitySource struct {
	existing     map[ExternalIdentity]bool
	committed    map[ExternalIdentity]bool
	existingErr  error
	committedErr error

	mu              sync.Mutex
	existingCalls   int
	committedCalls  int
	existingMarker  string
	committedMarker string
}

func newFakeCommittedIdentitySource() *fakeCommittedIdentitySource {
	return &fakeCommittedIdentitySource{existing: map[ExternalIdentity]bool{}, committed: map[ExternalIdentity]bool{}}
}

func (f *fakeCommittedIdentitySource) ExistingAudiobookshelfIdentities(ctx context.Context) (map[ExternalIdentity]bool, error) {
	f.mu.Lock()
	f.existingMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.existingCalls++
	if f.existingErr != nil {
		return nil, f.existingErr
	}
	out := make(map[ExternalIdentity]bool, len(f.existing))
	for k, v := range f.existing {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCommittedIdentitySource) CommittedIdentities(ctx context.Context) (map[ExternalIdentity]bool, error) {
	f.mu.Lock()
	f.committedMarker = markerFrom(ctx)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committedCalls++
	if f.committedErr != nil {
		return nil, f.committedErr
	}
	out := make(map[ExternalIdentity]bool, len(f.committed))
	for k, v := range f.committed {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCommittedIdentitySource) markers() (existing, committed string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existingMarker, f.committedMarker
}

// fakePacer counts Wait calls and lets a test cancel the context on a
// specific call instead of sleeping to synchronize with cancellation.
type fakePacer struct {
	mu        sync.Mutex
	calls     int
	cancelOn  int
	cancel    context.CancelFunc
	forcedErr error
	marker    string
}

func (p *fakePacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	p.marker = markerFrom(ctx)
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if p.forcedErr != nil {
		return p.forcedErr
	}
	if p.cancelOn != 0 && n == p.cancelOn && p.cancel != nil {
		p.cancel()
		return ctx.Err()
	}
	return nil
}

func (p *fakePacer) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakePacer) lastMarker() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.marker
}

// fakeSourceSelector answers SelectSource per work id (matched on
// target.Work.WorkID) so each candidate in a scenario can have distinct
// selection behavior.
type fakeSourceSelector struct {
	mu     sync.Mutex
	byWork map[string]func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error)
	calls  []string
	marker string
	onCall func(workID string)
}

func newFakeSourceSelector() *fakeSourceSelector {
	return &fakeSourceSelector{byWork: map[string]func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error){}}
}

func (f *fakeSourceSelector) SelectSource(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, target.Work.WorkID)
	f.marker = markerFrom(ctx)
	fn := f.byWork[target.Work.WorkID]
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(target.Work.WorkID)
	}
	if err := ctx.Err(); err != nil {
		return SelectionResult{}, err
	}
	if fn == nil {
		return SelectionResult{}, errors.New("fake selector has no configured response for work id")
	}
	return fn(ctx, target, limits)
}

func (f *fakeSourceSelector) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSourceSelector) lastMarker() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marker
}

// fakePublisher answers Publish keyed by the candidate's work id (taken
// directly from PublicationRequest.Work.WorkID) rather than by a
// self-computed ExternalIdentity: the test author must not have to predict
// exactly which RecordingFacts the runner resolves identity from, only which
// work is being published. Each call is recorded in full so tests can assert
// exact field-for-field parity between a dry run and an apply request.
type fakePublisher struct {
	mu       sync.Mutex
	byWork   map[string]func(ctx context.Context, req PublicationRequest) (PublicationResult, error)
	requests []PublicationRequest
	marker   string
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{byWork: map[string]func(ctx context.Context, req PublicationRequest) (PublicationResult, error){}}
}

func (f *fakePublisher) Publish(ctx context.Context, req PublicationRequest) (PublicationResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.marker = markerFrom(ctx)
	fn := f.byWork[req.Work.WorkID]
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	if fn == nil {
		return PublicationResult{}, errors.New("fake publisher has no configured response for work id")
	}
	return fn(ctx, req)
}

func (f *fakePublisher) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakePublisher) requestsFor(workID string) []PublicationRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PublicationRequest
	for _, req := range f.requests {
		if req.Work.WorkID == workID {
			out = append(out, req)
		}
	}
	return out
}

func (f *fakePublisher) lastMarker() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marker
}

// configurePublishSuccess registers a handler for work that reports the plan
// as planned on a dry run and as freshly scanned/imported on apply, mirroring
// what the accepted Publish() policy in publication.go itself returns for a
// first-time, successful publication (A5/I5: the only observable difference
// between dry run and apply is req.DryRun and the resulting Stage/Status).
func configurePublishSuccess(publisher *fakePublisher, work WorkFacts) {
	publisher.byWork[work.WorkID] = func(ctx context.Context, req PublicationRequest) (PublicationResult, error) {
		if req.DryRun {
			return PublicationResult{Stage: StagePlanned}, nil
		}
		return PublicationResult{Stage: StageScanned}, nil
	}
}

// sampleWorkDetail builds a minimal, valid AudioSiloWorkDetail for work id
// with one recording so tests do not repeat the same literal structure.
func sampleWorkDetail(workID, title, authorID, authorName string, recordingID string) AudioSiloWorkDetail {
	return AudioSiloWorkDetail{
		ID:       workID,
		Title:    title,
		Language: "en",
		Authors:  []AudioSiloPerson{{ID: authorID, Name: authorName}},
		Recordings: []AudioSiloRecording{
			{ID: recordingID, Narrators: []AudioSiloPerson{{ID: "narr-1", Name: "A Narrator"}}, ASINs: []AudioSiloASIN{}, ISBNs: []string{}, ChapterCount: 10},
		},
	}
}

func sampleEligibleSelection(t WorkFacts) SelectionResult {
	files := []AudioFile{{Index: 0, Path: t.Title + ".m4b", Size: 100 << 20}}
	candidate := ReleaseCandidate{Title: t.Title + " " + firstUsable(t.Authors) + " [Unabridged]", Seeders: 20, Files: files}
	decision := Decision{Candidate: candidate, Eligible: true, Selected: []SelectedFile{{FileIndex: 0, Part: 1}}}
	return SelectionResult{Selected: &decision, Hash: "0123456789abcdef0123456789abcdef01234567", Files: files}
}
