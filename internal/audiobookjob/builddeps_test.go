package audiobookjob_test

// Requirement coverage (see .tdd-state/audiobook-scheduler/brief.md):
//
//   A5/E1/I3: audiobookjob.Validate must reject a missing/invalid Config
//     before any client is constructed: missing AudioSilo/Audiobookshelf/
//     Library URLs, missing Audiobookshelf library id, missing state path,
//     disabled Prowlarr, empty Prowlarr API key while enabled, non-positive
//     discovery/selection limits, and an invalid removal policy.
//   A6: BuildLiveDeps validates before constructing any client (proven via
//     canary httptest servers that must never receive a request, in both
//     the invalid and the valid case, since construction itself must not
//     make network calls), and on a valid Config returns a complete
//     RunnerDeps graph (every dependency interface populated, Limits
//     propagated verbatim).
//   I2: BuildLiveDeps takes ctx as its first argument and must reject an
//     already-cancelled context before constructing anything.
//   I4/E2/E5: a construction failure (e.g. a credential-shaped value that
//     cannot form a valid HTTP(S) origin) must never surface the offending
//     token/URL value in the returned error text.
//
// Validate/BuildLiveDeps are currently declaration-only stubs (added by the
// lead as this slice's seam): Validate does not exist as a symbol at all yet
// -- see this manifest's seam request -- so the Validate-specific cases
// below are written against audiobookjob.BuildLiveDeps directly, which is
// declared and always returns a fixed "not implemented" error today. Tests
// asserting outcomes BuildLiveDeps cannot yet distinguish (e.g. that an
// invalid Config produces a *different* rejection reason than a valid one)
// are intentionally out of scope until Validate exists.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
	"tiramisu/internal/prowlarr"
)

// canary is an httptest server that records whether it was ever hit. Its
// zero value is ready to use.
type canary struct {
	server *httptest.Server
	hits   int32
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	c := &canary{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *canary) hitCount() int32 { return atomic.LoadInt32(&c.hits) }

type liveCanaries struct {
	audioSilo, audiobookshelf, library, prowlarr *canary
}

func newLiveCanaries(t *testing.T) *liveCanaries {
	t.Helper()
	return &liveCanaries{
		audioSilo:      newCanary(t),
		audiobookshelf: newCanary(t),
		library:        newCanary(t),
		prowlarr:       newCanary(t),
	}
}

func (lc *liveCanaries) assertNeverHit(t *testing.T) {
	t.Helper()
	if got := lc.audioSilo.hitCount(); got != 0 {
		t.Errorf("AudioSilo canary received %d request(s); construction must validate first", got)
	}
	if got := lc.audiobookshelf.hitCount(); got != 0 {
		t.Errorf("Audiobookshelf canary received %d request(s); construction must validate first", got)
	}
	if got := lc.library.hitCount(); got != 0 {
		t.Errorf("Library canary received %d request(s); construction must validate first", got)
	}
	if got := lc.prowlarr.hitCount(); got != 0 {
		t.Errorf("Prowlarr canary received %d request(s); construction must validate first", got)
	}
}

// hugeLimit/hugeLimit64/hugeDuration are chosen to be far beyond any
// plausible real bound (billions of candidates, exabytes, a millennium of
// grace) without pinning an exact threshold this test author cannot know -
// see I3's "positive finite maxima".
const (
	hugeLimit    = 1 << 40
	hugeLimit64  = int64(1) << 62
	hugeDuration = 100 * 365 * 24 * time.Hour
)

func validLimits() audiobookimport.DiscoveryLimits {
	return audiobookimport.DiscoveryLimits{
		MaxCandidates:     10,
		MaxImports:        5,
		MaxLatestExamined: 20,
		MaxSeriesExamined: 20,
		SearchLimit:       5,
		LatestLimit:       5,
		Selection: audiobookimport.SelectionLimits{
			MaxQueries:             2,
			MaxResultsPerQuery:     5,
			MaxCandidatesInspected: 5,
			MaxSourceBytes:         1 << 20,
			MaxReleaseSizeBytes:    1 << 30,
			MinConfidence:          0,
			MinSeeders:             0,
		},
	}
}

// validConfig returns a Config that satisfies every requirement this slice's
// brief lists for A5/I3 (present URLs/library/state, positive bounded
// limits, an enabled Prowlarr with credentials, a sane removal policy), with
// every network-reaching URL pointed at a canary so BuildLiveDeps' "validate
// before constructing clients" contract is provable even on a config that
// should otherwise succeed.
func validConfig(t *testing.T, lc *liveCanaries, stateDir string) audiobookjob.Config {
	t.Helper()
	return audiobookjob.Config{
		AudioSiloURL:            lc.audioSilo.server.URL,
		AudioSiloToken:          "audiosilo-token-should-not-leak",
		AudiobookshelfURL:       lc.audiobookshelf.server.URL,
		AudiobookshelfToken:     "audiobookshelf-token-should-not-leak",
		AudiobookshelfLibraryID: "lib-1",
		LibraryURL:              lc.library.server.URL,
		StatePath:               filepath.Join(stateDir, "audiobooks_state.json"),
		RemovalPolicy:           audiobookimport.RemovalPolicy{ConsecutiveMissingThreshold: 3, Grace: 24 * time.Hour},
		ProwlarrCfg:             prowlarr.ConfigProwlarr{Enabled: true, APIKey: "prowlarr-key-should-not-leak", URL: lc.prowlarr.server.URL},
		Categories:              []int{100},
		IndexerIDs:              []int{5},
		Limits:                  validLimits(),
		PaceSeconds:             10,
	}
}

// A6: a fully valid Config must produce a complete, usable RunnerDeps graph
// without ever touching the network during construction. This is the
// slice's primary RED anchor for A6: BuildLiveDeps is currently a
// declaration-only stub that always errors, so this fails today even though
// every input is valid.
func TestBuildLiveDeps_ValidConfig_ConstructsCompleteRunnerDepsGraphWithoutNetworkCalls(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())

	deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	lc.assertNeverHit(t)
	if err != nil {
		t.Fatalf("BuildLiveDeps(valid config) = %v, want nil error", err)
	}
	if deps.Provider == nil {
		t.Error("RunnerDeps.Provider is nil")
	}
	if deps.Inventory == nil {
		t.Error("RunnerDeps.Inventory is nil")
	}
	if deps.Identities == nil {
		t.Error("RunnerDeps.Identities is nil")
	}
	if deps.Selector == nil {
		t.Error("RunnerDeps.Selector is nil")
	}
	if deps.Publisher == nil {
		t.Error("RunnerDeps.Publisher is nil")
	}
	if deps.Pacer == nil {
		t.Error("RunnerDeps.Pacer is nil")
	}
	if !reflect.DeepEqual(deps.Limits, cfg.Limits) {
		t.Errorf("RunnerDeps.Limits = %+v, want cfg.Limits verbatim %+v", deps.Limits, cfg.Limits)
	}
}

// I2: an already-cancelled context must be rejected before any client is
// constructed.
func TestBuildLiveDeps_CancelledContext_FailsWithoutClientConstruction(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := audiobookjob.BuildLiveDeps(ctx, cfg)
	lc.assertNeverHit(t)
	if err == nil {
		t.Fatal("BuildLiveDeps(cancelled ctx) = nil error, want non-nil")
	}
}

// A5/E1/I3: every one of these single-field mutations from an otherwise
// valid Config must fail before any client is constructed. Each case name
// documents the specific requirement it locks down.
func TestBuildLiveDeps_InvalidConfig_FailsBeforeAnyClientConstruction(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *audiobookjob.Config)
	}{
		{"missing AudioSilo URL", func(c *audiobookjob.Config) { c.AudioSiloURL = "" }},
		{"missing Audiobookshelf URL", func(c *audiobookjob.Config) { c.AudiobookshelfURL = "" }},
		{"missing Audiobookshelf library id", func(c *audiobookjob.Config) { c.AudiobookshelfLibraryID = "" }},
		{"missing Library URL", func(c *audiobookjob.Config) { c.LibraryURL = "" }},
		{"missing state path", func(c *audiobookjob.Config) { c.StatePath = "" }},
		{"missing Audiobookshelf token", func(c *audiobookjob.Config) { c.AudiobookshelfToken = "" }},
		{"missing AudioSilo token", func(c *audiobookjob.Config) { c.AudioSiloToken = "" }},
		{"Prowlarr disabled", func(c *audiobookjob.Config) { c.ProwlarrCfg.Enabled = false }},
		{"Prowlarr enabled with empty API key", func(c *audiobookjob.Config) {
			c.ProwlarrCfg.Enabled = true
			c.ProwlarrCfg.APIKey = ""
		}},
		{"Prowlarr enabled with empty URL", func(c *audiobookjob.Config) {
			c.ProwlarrCfg.Enabled = true
			c.ProwlarrCfg.URL = ""
		}},
		{"zero MaxCandidates", func(c *audiobookjob.Config) { c.Limits.MaxCandidates = 0 }},
		{"negative MaxImports", func(c *audiobookjob.Config) { c.Limits.MaxImports = -1 }},
		{"zero MaxLatestExamined", func(c *audiobookjob.Config) { c.Limits.MaxLatestExamined = 0 }},
		{"zero MaxSeriesExamined", func(c *audiobookjob.Config) { c.Limits.MaxSeriesExamined = 0 }},
		{"zero SearchLimit", func(c *audiobookjob.Config) { c.Limits.SearchLimit = 0 }},
		{"zero LatestLimit", func(c *audiobookjob.Config) { c.Limits.LatestLimit = 0 }},
		{"zero Selection.MaxQueries", func(c *audiobookjob.Config) { c.Limits.Selection.MaxQueries = 0 }},
		{"zero Selection.MaxResultsPerQuery", func(c *audiobookjob.Config) { c.Limits.Selection.MaxResultsPerQuery = 0 }},
		{"zero Selection.MaxCandidatesInspected", func(c *audiobookjob.Config) { c.Limits.Selection.MaxCandidatesInspected = 0 }},
		{"zero Selection.MaxSourceBytes", func(c *audiobookjob.Config) { c.Limits.Selection.MaxSourceBytes = 0 }},
		{"zero Selection.MaxReleaseSizeBytes", func(c *audiobookjob.Config) { c.Limits.Selection.MaxReleaseSizeBytes = 0 }},
		{"negative Selection.MinSeeders", func(c *audiobookjob.Config) { c.Limits.Selection.MinSeeders = -1 }},
		{"negative Selection.MinConfidence", func(c *audiobookjob.Config) { c.Limits.Selection.MinConfidence = -1 }},
		{"non-positive PaceSeconds", func(c *audiobookjob.Config) { c.PaceSeconds = 0 }},
		{"non-positive removal ConsecutiveMissingThreshold", func(c *audiobookjob.Config) { c.RemovalPolicy.ConsecutiveMissingThreshold = 0 }},
		{"negative removal Grace", func(c *audiobookjob.Config) { c.RemovalPolicy.Grace = -time.Hour }},

		// Upper bounds: "positive finite maxima" (I3) means some cap must
		// exist, not merely that zero/negative is rejected. hugeLimit is
		// chosen far beyond any plausible real bound (not a guess at the
		// exact threshold), so this is robust to whatever specific finite
		// maximum the implementation picks.
		{"absurdly large MaxCandidates", func(c *audiobookjob.Config) { c.Limits.MaxCandidates = hugeLimit }},
		{"absurdly large MaxImports", func(c *audiobookjob.Config) { c.Limits.MaxImports = hugeLimit }},
		{"absurdly large MaxLatestExamined", func(c *audiobookjob.Config) { c.Limits.MaxLatestExamined = hugeLimit }},
		{"absurdly large MaxSeriesExamined", func(c *audiobookjob.Config) { c.Limits.MaxSeriesExamined = hugeLimit }},
		{"absurdly large SearchLimit", func(c *audiobookjob.Config) { c.Limits.SearchLimit = hugeLimit }},
		{"absurdly large LatestLimit", func(c *audiobookjob.Config) { c.Limits.LatestLimit = hugeLimit }},
		{"absurdly large Selection.MaxQueries", func(c *audiobookjob.Config) { c.Limits.Selection.MaxQueries = hugeLimit }},
		{"absurdly large Selection.MaxResultsPerQuery", func(c *audiobookjob.Config) { c.Limits.Selection.MaxResultsPerQuery = hugeLimit }},
		{"absurdly large Selection.MaxCandidatesInspected", func(c *audiobookjob.Config) { c.Limits.Selection.MaxCandidatesInspected = hugeLimit }},
		{"absurdly large Selection.MaxSourceBytes", func(c *audiobookjob.Config) { c.Limits.Selection.MaxSourceBytes = hugeLimit64 }},
		{"absurdly large Selection.MaxReleaseSizeBytes", func(c *audiobookjob.Config) { c.Limits.Selection.MaxReleaseSizeBytes = hugeLimit64 }},
		{"absurdly large Selection.MinConfidence", func(c *audiobookjob.Config) { c.Limits.Selection.MinConfidence = hugeLimit }},
		{"absurdly large Selection.MinSeeders", func(c *audiobookjob.Config) { c.Limits.Selection.MinSeeders = hugeLimit }},
		{"absurdly large PaceSeconds", func(c *audiobookjob.Config) { c.PaceSeconds = hugeLimit }},
		{"absurdly large removal ConsecutiveMissingThreshold", func(c *audiobookjob.Config) { c.RemovalPolicy.ConsecutiveMissingThreshold = hugeLimit }},
		{"absurdly large removal Grace", func(c *audiobookjob.Config) { c.RemovalPolicy.Grace = hugeDuration }},

		// Prowlarr narrowing (Categories/IndexerIDs): malformed, duplicate,
		// and non-positive entries must all be rejected before construction.
		{"negative category id", func(c *audiobookjob.Config) { c.Categories = []int{-1} }},
		{"zero category id", func(c *audiobookjob.Config) { c.Categories = []int{0} }},
		{"duplicate category ids", func(c *audiobookjob.Config) { c.Categories = []int{100, 100} }},
		{"negative indexer id", func(c *audiobookjob.Config) { c.IndexerIDs = []int{-1} }},
		{"zero indexer id", func(c *audiobookjob.Config) { c.IndexerIDs = []int{0} }},
		{"duplicate indexer ids", func(c *audiobookjob.Config) { c.IndexerIDs = []int{5, 5} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLiveCanaries(t)
			cfg := validConfig(t, lc, t.TempDir())
			tc.mutate(&cfg)

			_, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
			lc.assertNeverHit(t)
			if err == nil {
				t.Fatalf("BuildLiveDeps(%s) = nil error, want a validation failure", tc.name)
			}
		})
	}
}

// I3 boundary control: minimal-but-legal positive values (the smallest
// value that satisfies "positive") and a single, non-duplicate narrowing
// entry must succeed. Without this, an implementation that rejects every
// input (including the zero/negative/huge cases above) would still pass
// TestBuildLiveDeps_InvalidConfig_FailsBeforeAnyClientConstruction.
func TestBuildLiveDeps_MinimalPositiveLimitsAndSingleNarrowingEntry_Succeeds(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	cfg.Limits = audiobookimport.DiscoveryLimits{
		MaxCandidates: 1, MaxImports: 1, MaxLatestExamined: 1, MaxSeriesExamined: 1,
		SearchLimit: 1, LatestLimit: 1,
		Selection: audiobookimport.SelectionLimits{
			MaxQueries: 1, MaxResultsPerQuery: 1, MaxCandidatesInspected: 1,
			MaxSourceBytes: 1, MaxReleaseSizeBytes: 1, MinConfidence: 0, MinSeeders: 0,
		},
	}
	cfg.PaceSeconds = 1
	cfg.RemovalPolicy = audiobookimport.RemovalPolicy{ConsecutiveMissingThreshold: 1, Grace: time.Second}
	cfg.Categories = []int{1}
	cfg.IndexerIDs = []int{1}

	_, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	lc.assertNeverHit(t)
	if err != nil {
		t.Fatalf("BuildLiveDeps(minimal positive config) = %v, want nil error", err)
	}
}

// I4/E2/E5: a construction-time failure must never leak the credential or
// URL value that caused it. AudiobookshelfURL carrying userinfo is rejected
// by the same base-URL parsing internal/audiobookimport's own provider
// clients already enforce (see provider.go: parseProviderBaseURL rejects a
// URL with embedded user info), so this is a real construction failure, not
// a pre-validation one, once BuildLiveDeps is implemented.
func TestBuildLiveDeps_ConstructionFailure_ErrorNeverLeaksCredentialOrURL(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())

	const secretToken = "s3cr3t-audiobookshelf-token-marker"
	cfg.AudiobookshelfToken = secretToken
	cfg.AudiobookshelfURL = "http://embedded-user:embedded-pass@127.0.0.1:0/should-not-leak"

	_, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildLiveDeps with a userinfo-bearing Audiobookshelf URL = nil error, want non-nil")
	}
	msg := err.Error()
	for _, sentinel := range []string{secretToken, "embedded-user", "embedded-pass", cfg.AudiobookshelfURL} {
		if strings.Contains(msg, sentinel) {
			t.Errorf("BuildLiveDeps error %q leaks sentinel %q", msg, sentinel)
		}
	}
}

// A1: the live wiring's job name is the stable "audiobooks" scheduler
// identity every other requirement in this brief keys off; this is already
// real (non-stub) behavior and should stay green.
func TestSyncer_Name_IsCanonicalAudiobooksJobName(t *testing.T) {
	s := &audiobookjob.Syncer{}
	if got, want := s.Name(), "audiobooks"; got != want {
		t.Fatalf("Syncer.Name() = %q, want %q", got, want)
	}
}

// A1/E2/I4: the real Syncer's Run() (not a test fake) must delegate to the
// live dependency graph and never surface a configured credential in the
// error it returns, even when construction fails. Called directly
// (synchronously) rather than through a Scheduler, so this test needs no
// completion signal to wait on: the scheduler's own admission/cancel/status
// contract for an arbitrary job (including one named "audiobooks") is
// already proven job-name-agnostically in
// internal/syncer/scheduler/scheduler_audiobooks_test.go.
func TestSyncer_Run_ConstructionFailure_NeverLeaksConfiguredSecret(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	// Intentionally invalid: a userinfo-bearing URL is never a valid HTTP(S)
	// origin (see provider.go's parseProviderBaseURL), forcing Run() to fail
	// through the live construction path.
	cfg.AudiobookshelfURL = "http://embedded-user:embedded-pass@127.0.0.1:0/x"
	const secretToken = "s3cr3t-should-never-reach-run-error"
	cfg.AudiobookshelfToken = secretToken

	syncer := &audiobookjob.Syncer{Cfg: cfg}
	err := syncer.Run(context.Background())
	if err == nil {
		t.Fatal("Syncer.Run with an invalid Audiobookshelf URL = nil error, want non-nil")
	}
	msg := err.Error()
	for _, sentinel := range []string{secretToken, "embedded-user", "embedded-pass"} {
		if strings.Contains(msg, sentinel) {
			t.Errorf("Syncer.Run error %q leaks sentinel %q", msg, sentinel)
		}
	}
}
