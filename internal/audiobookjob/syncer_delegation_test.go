package audiobookjob_test

// Requirement coverage (A1/I2, unblocked by the lead's declaration-only
// Syncer.Build/Syncer.RunFunc injection seam - see job.go): the real
// Syncer.Run must construct dependencies exactly once, delegate exactly one
// audiobookimport.Run with apply semantics (DryRun == false), carry the
// caller's context (identity and cancellation) to both calls, pass its
// configured Config - including StatePath - to Build unchanged, and reuse
// that same configured StatePath across repeated invocations of the same
// Syncer instance (a completed scheduled-style run followed by a
// manual-style run must not drift onto a different state file).
//
// Run() is currently a declaration-only stub that always returns one fixed
// error regardless of Cfg/Build/RunFunc, so every test below is a genuine
// RED anchor today: Build/RunFunc call counts stay at zero and Run() never
// returns the nil error these tests expect from a well-behaved delegation.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
)

// A1: Run must call Build exactly once with its own Cfg (StatePath included,
// unchanged), then call RunFunc exactly once with the deps Build returned
// and RunRequest.DryRun == false (apply semantics, never dry-run, for the
// job the scheduler and CLI apply path both use).
func TestSyncer_Run_CallsBuildAndRunFuncExactlyOnce_WithApplySemantics(t *testing.T) {
	cfg := audiobookjob.Config{StatePath: "/configured/audiobooks_state.json", AudiobookshelfLibraryID: "lib-1"}
	wantDeps := audiobookimport.RunnerDeps{Limits: audiobookimport.DiscoveryLimits{MaxCandidates: 42}}

	var buildCalls, runCalls int
	var gotCfg audiobookjob.Config
	var gotDeps audiobookimport.RunnerDeps
	var gotReq audiobookimport.RunRequest

	syncer := &audiobookjob.Syncer{
		Cfg: cfg,
		Build: func(_ context.Context, c audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			buildCalls++
			gotCfg = c
			return wantDeps, nil
		},
		RunFunc: func(_ context.Context, deps audiobookimport.RunnerDeps, req audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			runCalls++
			gotDeps = deps
			gotReq = req
			return audiobookimport.RunResult{}, nil
		},
	}

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}

	if buildCalls != 1 {
		t.Fatalf("Build called %d times, want exactly 1", buildCalls)
	}
	if runCalls != 1 {
		t.Fatalf("RunFunc called %d times, want exactly 1", runCalls)
	}
	if !reflect.DeepEqual(gotCfg, cfg) {
		t.Fatalf("Build received Config = %+v, want %+v (Syncer.Cfg passed through unchanged)", gotCfg, cfg)
	}
	if gotCfg.StatePath != cfg.StatePath {
		t.Fatalf("Build received StatePath = %q, want %q", gotCfg.StatePath, cfg.StatePath)
	}
	if !reflect.DeepEqual(gotDeps, wantDeps) {
		t.Fatalf("RunFunc received RunnerDeps = %+v, want the value Build returned %+v", gotDeps, wantDeps)
	}
	if gotReq.DryRun {
		t.Fatal("RunFunc received RunRequest.DryRun = true, want false (apply semantics)")
	}
}

// A1: when Build fails, RunFunc must never be called (no partial/garbage
// delegation), and Run() must surface a non-nil error.
func TestSyncer_Run_BuildFailure_NeverCallsRunFunc(t *testing.T) {
	runCalls := 0
	syncer := &audiobookjob.Syncer{
		Build: func(context.Context, audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			return audiobookimport.RunnerDeps{}, errors.New("construction failed")
		},
		RunFunc: func(context.Context, audiobookimport.RunnerDeps, audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			runCalls++
			return audiobookimport.RunResult{}, nil
		},
	}

	if err := syncer.Run(context.Background()); err == nil {
		t.Fatal("Run() = nil error, want non-nil when Build fails")
	}
	if runCalls != 0 {
		t.Fatalf("RunFunc called %d times after a Build failure, want 0", runCalls)
	}
}

type ctxMarkerKey struct{}

// I2: the exact context.Context value the caller passed to Run must reach
// both Build and RunFunc - proven via a value marker rather than any timing
// assumption, so this holds regardless of how Run is internally structured.
func TestSyncer_Run_PropagatesCallerContextIdentityToBuildAndRunFunc(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxMarkerKey{}, "marker-value")
	var buildSawMarker, runSawMarker bool

	syncer := &audiobookjob.Syncer{
		Build: func(gotCtx context.Context, _ audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			buildSawMarker = gotCtx.Value(ctxMarkerKey{}) == "marker-value"
			return audiobookimport.RunnerDeps{}, nil
		},
		RunFunc: func(gotCtx context.Context, _ audiobookimport.RunnerDeps, _ audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			runSawMarker = gotCtx.Value(ctxMarkerKey{}) == "marker-value"
			return audiobookimport.RunResult{}, nil
		},
	}

	if err := syncer.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if !buildSawMarker {
		t.Error("Build did not receive the caller's context (value marker missing)")
	}
	if !runSawMarker {
		t.Error("RunFunc did not receive the caller's context (value marker missing)")
	}
}

// I2: an already-cancelled caller context must be observable by Build
// (Run must not silently swallow ctx.Err() and proceed as if nothing
// happened), and Run() must propagate a non-nil error rather than treating
// it as success.
func TestSyncer_Run_CancelledContext_ObservedByBuildAndPropagated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buildSawCancellation bool
	syncer := &audiobookjob.Syncer{
		Build: func(gotCtx context.Context, _ audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			buildSawCancellation = errors.Is(gotCtx.Err(), context.Canceled)
			return audiobookimport.RunnerDeps{}, gotCtx.Err()
		},
		RunFunc: func(context.Context, audiobookimport.RunnerDeps, audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			t.Fatal("RunFunc must not be called when Build fails due to a cancelled context")
			return audiobookimport.RunResult{}, nil
		},
	}

	if err := syncer.Run(ctx); err == nil {
		t.Fatal("Run() = nil error, want non-nil for an already-cancelled context")
	}
	if !buildSawCancellation {
		t.Fatal("Build did not observe the caller's context cancellation")
	}
}

// I2: whatever RunFunc itself decides based on the context it was given
// must not be discarded by Run - proven by having RunFunc observe
// cancellation directly and asserting Run() faithfully surfaces that
// outcome, without presuming whether Build or RunFunc is "responsible" for
// the first ctx.Err() check (an implementation detail this test does not
// overconstrain).
func TestSyncer_Run_RunFuncObservedCancellation_IsPropagated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	syncer := &audiobookjob.Syncer{
		Build: func(context.Context, audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			// This fake Build ignores ctx state entirely, so the test
			// exercises RunFunc's own observation of cancellation rather
			// than relying on Build to short-circuit first.
			return audiobookimport.RunnerDeps{}, nil
		},
		RunFunc: func(gotCtx context.Context, _ audiobookimport.RunnerDeps, _ audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			return audiobookimport.RunResult{}, gotCtx.Err()
		},
	}

	err := syncer.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want an error wrapping context.Canceled (RunFunc's own result must not be discarded)", err)
	}
}

// A1: the same Syncer instance, run twice in sequence (a completed
// scheduled-style run followed by a manual-style run - exactly how the
// native scheduler and a manual "run now" trigger would both invoke the
// same registered job), must present the identical configured StatePath to
// Build both times: durable state must never drift across invocations of
// one Syncer.
func TestSyncer_Run_TwoSequentialRuns_ReuseTheSameConfiguredStatePathUnchanged(t *testing.T) {
	const statePath = "/configured/audiobooks_state.json"
	var seenStatePaths []string

	syncer := &audiobookjob.Syncer{
		Cfg: audiobookjob.Config{StatePath: statePath},
		Build: func(_ context.Context, c audiobookjob.Config) (audiobookimport.RunnerDeps, error) {
			seenStatePaths = append(seenStatePaths, c.StatePath)
			return audiobookimport.RunnerDeps{}, nil
		},
		RunFunc: func(context.Context, audiobookimport.RunnerDeps, audiobookimport.RunRequest) (audiobookimport.RunResult, error) {
			return audiobookimport.RunResult{}, nil
		},
	}

	// First invocation: stands in for a scheduled tick's automatic run.
	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("first Run() (scheduled-style) = %v, want nil error", err)
	}
	// Second invocation on the same instance: stands in for a subsequent
	// manual "run now" trigger.
	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("second Run() (manual-style) = %v, want nil error", err)
	}

	if len(seenStatePaths) != 2 {
		t.Fatalf("Build was called %d times across two runs, want exactly 2", len(seenStatePaths))
	}
	for i, got := range seenStatePaths {
		if got != statePath {
			t.Fatalf("run %d: Build saw StatePath = %q, want the unchanged configured path %q", i, got, statePath)
		}
	}
}
