package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"tiramisu/internal/audiobookimport"
)

// Requirement A8: the standalone CLI supports explicit work/query inputs,
// --dry-run and --apply as mutually exclusive modes, bounded numeric
// options, state/config paths, and cancellation. It validates all
// configuration before constructing live dependencies, returns a nonzero
// error for invalid/failed runs, prints deterministic safe result rows, and
// delegates one run to the same runner used by the scheduler. Re-running
// apply resumes through durable state rather than duplicating publication.
//
// Requirement E1: invalid mode/limits/ids/dependencies/library id/state or
// config paths fail before provider/source/publish/pacing/output side
// effects.
//
// Requirement E5: CLI output/config/state never expose Audiobookshelf,
// AudioSilo, or Prowlarr credentials or raw source material, including when
// an injected dependency error embeds a sentinel.
//
// The production seam this file exercises (Config, ParseConfig,
// Dependencies, RunCLI) does not exist yet; see the manifest returned with
// this slice for the exact minimal declarations requested from the lead.

// stubProvider/stubInventory/stubIdentities/stubSelector/stubPublisher/
// stubPacer are minimal, deterministic implementations of the
// audiobookimport package's exported runner interfaces, used only to prove
// CLI-level behavior (flag parsing, validation-before-construction, output,
// exit codes, cancellation delegation). The runner's own decision logic is
// exercised by internal/audiobookimport's own tests, not repeated here.

type stubProvider struct {
	detail audiobookimport.AudioSiloWorkDetail
}

func (s stubProvider) SearchWorks(ctx context.Context, query string, limit int) ([]audiobookimport.AudioSiloWorkCard, error) {
	return nil, nil
}
func (s stubProvider) LatestWorks(ctx context.Context, limit int) ([]audiobookimport.AudioSiloWorkCard, error) {
	return nil, nil
}
func (s stubProvider) WorkDetail(ctx context.Context, id string) (audiobookimport.AudioSiloWorkDetail, error) {
	if id != s.detail.ID {
		return audiobookimport.AudioSiloWorkDetail{}, errors.New("no such work")
	}
	return s.detail, nil
}

type stubInventory struct{}

func (stubInventory) OwnedAuthorIDs(ctx context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (stubInventory) OwnedSeriesIDs(ctx context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (stubInventory) SeriesGapCandidates(ctx context.Context, seriesID string) ([]audiobookimport.SeriesGapCandidate, error) {
	return nil, nil
}

type stubIdentities struct{}

func (stubIdentities) ExistingAudiobookshelfIdentities(ctx context.Context) (map[audiobookimport.ExternalIdentity]bool, error) {
	return map[audiobookimport.ExternalIdentity]bool{}, nil
}
func (stubIdentities) CommittedIdentities(ctx context.Context) (map[audiobookimport.ExternalIdentity]bool, error) {
	return map[audiobookimport.ExternalIdentity]bool{}, nil
}

type stubSelector struct {
	result audiobookimport.SelectionResult
}

func (s stubSelector) SelectSource(ctx context.Context, target audiobookimport.Target, limits audiobookimport.SelectionLimits) (audiobookimport.SelectionResult, error) {
	return s.result, nil
}

type stubPublisher struct{ err error }

func (s stubPublisher) Publish(ctx context.Context, req audiobookimport.PublicationRequest) (audiobookimport.PublicationResult, error) {
	if s.err != nil {
		return audiobookimport.PublicationResult{}, s.err
	}
	return audiobookimport.PublicationResult{Stage: audiobookimport.StageScanned}, nil
}

type stubPacer struct{}

func (stubPacer) Wait(ctx context.Context) error { return ctx.Err() }

// recordingPublisher is a stateful CandidatePublisher fake shared across two
// separate RunCLI invocations. It simulates the accepted, durable-state
// Publish() policy well enough to prove CLI-level resume behavior: a first
// apply reports a fresh import, and a second apply for the same work id
// reports AlreadyPresent (existing/resumed) without a second real external
// write, matching what the real resumable publisher in publication.go
// already guarantees.
//
// requests/requestCount count every call the CLI made to Publish, which is
// expected to be one per apply invocation (the boundary is always reached,
// even on resume). writes/writeCount are a *separate* counter that only
// advances the first time a given identity is actually written — exactly
// the distinction review-2 asked for, so that "two calls, one real write" is
// an observed fact from two independent counters rather than an assertion
// that infers "no duplicate write" merely from AlreadyPresent being set on
// the second call.
type recordingPublisher struct {
	mu        sync.Mutex
	published map[string]bool
	requests  []audiobookimport.PublicationRequest
	writes    int
}

func (p *recordingPublisher) Publish(ctx context.Context, req audiobookimport.PublicationRequest) (audiobookimport.PublicationResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if req.DryRun {
		return audiobookimport.PublicationResult{Stage: audiobookimport.StagePlanned}, nil
	}
	if p.published == nil {
		p.published = map[string]bool{}
	}
	already := p.published[req.Work.WorkID]
	if !already {
		p.writes++
	}
	p.published[req.Work.WorkID] = true
	return audiobookimport.PublicationResult{Stage: audiobookimport.StageScanned, AlreadyPresent: already}, nil
}

func (p *recordingPublisher) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *recordingPublisher) writeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writes
}

func (p *recordingPublisher) lastRequest() audiobookimport.PublicationRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

func (p *recordingPublisher) requestWorkIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, len(p.requests))
	for i, req := range p.requests {
		ids[i] = req.Work.WorkID
	}
	return ids
}

func workingDeps() audiobookimport.RunnerDeps {
	detail := audiobookimport.AudioSiloWorkDetail{
		ID: "w1", Title: "Book", Language: "en",
		Authors: []audiobookimport.AudioSiloPerson{{ID: "a1", Name: "Author"}},
		Recordings: []audiobookimport.AudioSiloRecording{{
			ID: "rec-1", Narrators: []audiobookimport.AudioSiloPerson{{ID: "n1", Name: "Narrator"}},
			ASINs: []audiobookimport.AudioSiloASIN{}, ISBNs: []string{}, ChapterCount: 5,
		}},
	}
	work := audiobookimport.WorkFacts{WorkID: "w1", Title: "Book", Authors: []string{"Author"}}
	files := []audiobookimport.AudioFile{{Index: 0, Path: "Book.m4b", Size: 100 << 20}}
	candidate := audiobookimport.ReleaseCandidate{Title: "Book Author [Unabridged]", Seeders: 10, Files: files}
	decision := audiobookimport.Decision{Candidate: candidate, Eligible: true, Selected: []audiobookimport.SelectedFile{{FileIndex: 0, Part: 1}}}
	selection := audiobookimport.SelectionResult{Selected: &decision, Hash: "0123456789abcdef0123456789abcdef01234567", Files: files}
	_ = work
	return audiobookimport.RunnerDeps{
		Provider:   stubProvider{detail: detail},
		Inventory:  stubInventory{},
		Identities: stubIdentities{},
		Selector:   stubSelector{result: selection},
		Publisher:  stubPublisher{},
		Pacer:      stubPacer{},
		Limits: audiobookimport.DiscoveryLimits{
			MaxCandidates: 5, MaxImports: 5, MaxLatestExamined: 5, MaxSeriesExamined: 5,
			SearchLimit: 5, LatestLimit: 5,
			Selection: audiobookimport.SelectionLimits{
				MaxQueries: 2, MaxResultsPerQuery: 5, MaxCandidatesInspected: 5,
				MaxSourceBytes: 1 << 20, MaxReleaseSizeBytes: 1 << 30, MinConfidence: 0,
			},
		},
	}
}

func TestParseConfig_DryRunAndApplyAreMutuallyExclusive(t *testing.T) {
	_, err := ParseConfig([]string{"--work-id=w1", "--dry-run", "--apply"})
	if err == nil {
		t.Fatalf("ParseConfig() error = nil, want an error when --dry-run and --apply are both given")
	}
}

// TestParseConfig_DefaultsToDryRunModeWithoutEitherFlag isolates the mode
// default from the state/library-id requirement: both --state and
// --library-id are supplied here so this test cannot pass or fail for the
// wrong reason (an earlier version of this test omitted them, which
// contradicted TestParseConfig_RequiresStateAndLibraryPaths treating those
// same flags as mandatory).
func TestParseConfig_DefaultsToDryRunModeWithoutEitherFlag(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	if cfg.Apply {
		t.Errorf("Apply = true, want false (dry run) when neither flag is given")
	}
}

func TestParseConfig_BoundedNumericOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"negative max-candidates", []string{"--work-id=w1", "--max-candidates=-1"}},
		{"zero max-candidates", []string{"--work-id=w1", "--max-candidates=0"}},
		{"absurdly large max-candidates", []string{"--work-id=w1", "--max-candidates=999999999999"}},
		{"negative max-imports", []string{"--work-id=w1", "--max-imports=-1"}},
		{"non-numeric max-candidates", []string{"--work-id=w1", "--max-candidates=abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseConfig(tt.args); err == nil {
				t.Fatalf("ParseConfig(%v) error = nil, want an error", tt.args)
			}
		})
	}
}

func TestParseConfig_RequiresStateAndLibraryPaths(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing state path", []string{"--work-id=w1", "--library-id=lib1", "--state="}},
		{"missing library id", []string{"--work-id=w1", "--state=state.json", "--library-id="}},
		{"no work id or query", []string{"--state=state.json", "--library-id=lib1"}},
		{"blank explicit work id", []string{"--work-id= ", "--state=state.json", "--library-id=lib1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseConfig(tt.args); err == nil {
				t.Fatalf("ParseConfig(%v) error = nil, want an error", tt.args)
			}
		})
	}
}

func TestRunCLI_InvalidConfig_NeverConstructsDependencies(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	cfg.MaxCandidates = -1 // corrupt after parse to isolate RunCLI's own validation
	built := false
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		built = true
		return audiobookimport.RunnerDeps{}, nil
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), cfg, deps, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("RunCLI() code = 0, want nonzero for invalid config")
	}
	if built {
		t.Fatalf("Dependencies.Build was called, want config validated before any live dependency is constructed")
	}
}

func TestRunCLI_DryRun_PrintsDeterministicRowsAndReachesPublisherWithDryRunTrue(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1", "--dry-run"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	publisher := &recordingPublisher{}
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		runnerDeps := workingDeps()
		runnerDeps.Publisher = publisher
		return runnerDeps, nil
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), cfg, deps, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunCLI() code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "w1") {
		t.Errorf("stdout = %q, want it to mention work id w1", stdout.String())
	}
	if !strings.Contains(stdout.String(), "planned") {
		t.Errorf("stdout = %q, want a deterministic planned status for a dry run", stdout.String())
	}
	if publisher.requestCount() != 1 {
		t.Fatalf("publisher called %d times, want exactly 1: a dry run must still reach the publication boundary", publisher.requestCount())
	}
	if !publisher.lastRequest().DryRun {
		t.Errorf("DryRun = false, want true: the CLI dry-run mode must set PublicationRequest.DryRun")
	}
}

func TestRunCLI_Apply_DelegatesToSameRunnerAndReachesPublisherWithDryRunFalse(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1", "--apply"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	publisher := &recordingPublisher{}
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		runnerDeps := workingDeps()
		runnerDeps.Publisher = publisher
		return runnerDeps, nil
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), cfg, deps, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunCLI() code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported") {
		t.Errorf("stdout = %q, want an imported status for a successful apply", stdout.String())
	}
	if publisher.requestCount() != 1 {
		t.Fatalf("publisher called %d times, want exactly 1", publisher.requestCount())
	}
	if publisher.lastRequest().DryRun {
		t.Errorf("DryRun = true, want false: apply must set PublicationRequest.DryRun=false")
	}
}

// TestRunCLI_ApplyTwice_SecondRunResumesWithoutDuplicateWrite proves the A8
// resume requirement with two independent counters rather than one
// self-fulfilling inference: requestCount (how many times the CLI reached
// the publication boundary at all) must be 2, one per apply invocation, but
// writeCount (how many times a *new* identity was actually written) must
// stay at 1 — the second call observes the identity it already wrote and
// performs no additional simulated external write. Both calls must also
// target the identical work identity, and the CLI's own printed status must
// distinguish "imported" (first run) from "existing" (second, resumed run).
func TestRunCLI_ApplyTwice_SecondRunResumesWithoutDuplicateWrite(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1", "--apply"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	publisher := &recordingPublisher{}
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		runnerDeps := workingDeps()
		runnerDeps.Publisher = publisher
		return runnerDeps, nil
	}}

	var stdout1, stderr1 bytes.Buffer
	if code := RunCLI(context.Background(), cfg, deps, &stdout1, &stderr1); code != 0 {
		t.Fatalf("first RunCLI() code = %d, want 0, stderr=%q", code, stderr1.String())
	}
	if !strings.Contains(stdout1.String(), "imported") {
		t.Fatalf("first run stdout = %q, want an imported status", stdout1.String())
	}
	if got := publisher.writeCount(); got != 1 {
		t.Fatalf("writeCount after first run = %d, want 1 (the first apply must perform exactly one simulated external write)", got)
	}

	var stdout2, stderr2 bytes.Buffer
	if code := RunCLI(context.Background(), cfg, deps, &stdout2, &stderr2); code != 0 {
		t.Fatalf("second RunCLI() code = %d, want 0, stderr=%q", code, stderr2.String())
	}
	if !strings.Contains(stdout2.String(), "existing") {
		t.Errorf("second run stdout = %q, want an existing/resumed status", stdout2.String())
	}
	if strings.Contains(stdout2.String(), "imported") {
		t.Errorf("second run stdout = %q, must not report a fresh import for a resumed identity", stdout2.String())
	}

	if got := publisher.requestCount(); got != 2 {
		t.Fatalf("requestCount = %d, want exactly 2: the CLI must reach the publication boundary on every apply run, including a resumed one", got)
	}
	if got := publisher.writeCount(); got != 1 {
		t.Fatalf("writeCount = %d, want exactly 1 across both CLI invocations: the second apply must resume, not perform a second external write", got)
	}
	ids := publisher.requestWorkIDs()
	if len(ids) != 2 || ids[0] != ids[1] || ids[0] == "" {
		t.Fatalf("requestWorkIDs = %v, want both calls to target the identical, nonempty work identity", ids)
	}
}

func TestRunCLI_DependencyConstructionFailure_ReturnsNonzeroWithoutLeakingSentinel(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		return audiobookimport.RunnerDeps{}, errors.New("audiobookshelf token=SECRET-ABC failed auth")
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), cfg, deps, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("RunCLI() code = 0, want nonzero when dependency construction fails")
	}
	if strings.Contains(stdout.String(), "SECRET-ABC") || strings.Contains(stderr.String(), "SECRET-ABC") {
		t.Fatalf("output leaks a credential sentinel: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunCLI_PublishFailure_ReturnsNonzeroWithoutLeakingRawSource(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1", "--apply"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		runnerDeps := workingDeps()
		runnerDeps.Publisher = stubPublisher{err: errors.New("magnet:?xt=urn:btih:deadbeef&apikey=SECRET")}
		return runnerDeps, nil
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), cfg, deps, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("RunCLI() code = 0, want nonzero when the run reports a failed candidate")
	}
	if strings.Contains(stdout.String(), "SECRET") || strings.Contains(stderr.String(), "SECRET") ||
		strings.Contains(stdout.String(), "magnet:") || strings.Contains(stderr.String(), "magnet:") {
		t.Fatalf("output leaks raw source material: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunCLI_CancelledContext_ReturnsNonzeroBeforeCompletion(t *testing.T) {
	cfg, err := ParseConfig([]string{"--work-id=w1", "--state=state.json", "--library-id=lib1"})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps := Dependencies{Build: func(ctx context.Context, cfg Config) (audiobookimport.RunnerDeps, error) {
		return workingDeps(), nil
	}}
	var stdout, stderr bytes.Buffer
	code := RunCLI(ctx, cfg, deps, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("RunCLI() code = 0, want nonzero for a cancelled context")
	}
}
