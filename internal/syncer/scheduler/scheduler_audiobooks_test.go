package scheduler_test

// Requirement coverage (see .tdd-state/audiobook-scheduler/brief.md):
//
//   A2: manual/scheduled overlap is impossible; two simultaneous TriggerRun
//       callers for the same job admit exactly one body, the other observes
//       scheduler.ErrAlreadyRunning, deterministically and without relying on
//       scheduler luck or sleeps. Existing job semantics (movies/tv/music/
//       watchlist) remain compatible, exercised here as independent job names
//       sharing one Scheduler instance.
//   A3: Stop cancels the exact running job's context, the runner observes
//       context.Canceled, status becomes "stopped", and a later manual run
//       starts cleanly. Stop of an idle job returns scheduler.ErrNotRunning.
//   I1: the non-overlap transition is atomic with context/cancel
//       registration - status cannot say idle while a body is running, and
//       Stop cannot miss a body that has been admitted.
//   I2: cancellation is observed by the runner (errors.Is-compatible with
//       context.Canceled); no goroutine is leaked past job completion.
//   E3: concurrent run returns ErrAlreadyRunning; idle stop returns
//       ErrNotRunning; an unknown job name is a distinct, unknown-job error.
//   E4: a state-save failure cannot permit an overlapping body or lose the
//       in-memory running/cancel state, and a later successful run remains
//       possible after completion.
//
// These tests exercise the generic Scheduler with jobs literally named
// "audiobooks" (and, for the compatibility case, alongside "movies"), since
// TriggerRun/StopJob/Status are name-agnostic: the audiobook job's runtime
// non-overlap/cancel/status guarantee is the same generic mechanism these
// other jobs already rely on. They do not require any audiobook-specific
// config, live dependency factory, or main.go/settings/dashboard wiring - see
// this slice's returned seam request for the parts that do.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"tiramisu/internal/syncer/scheduler"
)

// fakeSyncer is a deterministic, channel-controlled scheduler.Syncer. When
// proceed is nil, Run returns immediately with err. When proceed is set, Run
// blocks until proceed is closed (or receivable) or ctx is done; in the
// latter case it reports the observed ctx.Err() on observed, if non-nil,
// before returning it.
type fakeSyncer struct {
	name     string
	proceed  chan struct{}
	observed chan error
	// done, when non-nil, receives once per completed Run invocation
	// (buffered by the caller), letting a test drain exactly as many
	// completions as it admitted bodies without sleeping or polling.
	done chan struct{}
	err  error
}

func (f *fakeSyncer) Name() string { return f.name }

func (f *fakeSyncer) Run(ctx context.Context) error {
	if f.proceed == nil {
		if f.done != nil {
			defer func() { f.done <- struct{}{} }()
		}
		return f.err
	}
	select {
	case <-f.proceed:
		return f.err
	case <-ctx.Done():
		err := ctx.Err()
		if f.observed != nil {
			f.observed <- err
		}
		return err
	}
}

func newScheduler(t *testing.T, jobs map[string]scheduler.Syncer, statePath string) *scheduler.Scheduler {
	t.Helper()
	return scheduler.New(scheduler.SchedulerConfig{}, jobs, statePath)
}

// A2/I1/E3: two simultaneous TriggerRun callers for the same job must admit
// exactly one body; the immediate return values alone prove or disprove this
// (no waiting on job completion is needed), so the assertion is fully
// synchronous per trial. The scenario is repeated across many trials with a
// fresh scheduler and a shared channel gate per trial to maximize the chance
// of exposing a non-atomic check-then-set admission without ever sleeping,
// polling, or depending on scheduler luck for any single trial to matter.
func TestScheduler_TriggerRun_ConcurrentCallers_AdmitExactlyOneBody(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	const trials = 300
	for trial := 0; trial < trials; trial++ {
		fs := &fakeSyncer{name: "audiobooks", done: make(chan struct{}, 2)}
		sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": fs}, statePath)

		gate := make(chan struct{})
		results := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		for i := 0; i < 2; i++ {
			i := i
			go func() {
				defer wg.Done()
				<-gate
				results[i] = sched.TriggerRun("audiobooks")
			}()
		}
		close(gate)
		wg.Wait()

		nilCount, alreadyRunningCount := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				nilCount++
			case errors.Is(err, scheduler.ErrAlreadyRunning):
				alreadyRunningCount++
			default:
				t.Fatalf("trial %d: unexpected TriggerRun error: %v", trial, err)
			}
		}
		// Drain exactly as many completions as bodies we admitted, so this
		// trial's background goroutine(s) finish before the next trial reuses
		// the shared state file (no sleeping or polling: a blocking receive on
		// a channel the fake itself signals).
		for i := 0; i < nilCount; i++ {
			<-fs.done
		}
		if nilCount != 1 || alreadyRunningCount != 1 {
			t.Fatalf("trial %d: non-atomic admission: got %d admitted body(ies) and %d ErrAlreadyRunning, want exactly 1 and 1", trial, nilCount, alreadyRunningCount)
		}
	}
}

// A2 compatibility: two distinct jobs sharing one Scheduler instance must
// not block each other's admission - the non-overlap guard is per job name.
func TestScheduler_TriggerRun_DistinctJobs_DoNotBlockEachOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.json")

		audiobooks := &fakeSyncer{name: "audiobooks", proceed: make(chan struct{})}
		movies := &fakeSyncer{name: "movies"}
		sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": audiobooks, "movies": movies}, statePath)

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("TriggerRun(audiobooks): %v", err)
		}
		synctest.Wait()

		if err := sched.TriggerRun("movies"); err != nil {
			t.Fatalf("TriggerRun(movies) should not be blocked by the running audiobooks job: %v", err)
		}
		synctest.Wait()

		if !sched.Status()["audiobooks"].Running {
			t.Fatalf("expected audiobooks still running")
		}
		if sched.Status()["movies"].Running {
			t.Fatalf("expected movies to have already completed")
		}

		close(audiobooks.proceed)
		synctest.Wait()
	})
}

// A3/I1/I2/E3: Stop cancels exactly the running job's context; the runner
// observes context.Canceled; status transitions to "stopped"; and a later
// manual run starts cleanly on the same job.
func TestScheduler_StopJob_CancelsObservedByRunnerThenAllowsCleanRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.json")

		observed := make(chan error, 1)
		fs := &fakeSyncer{name: "audiobooks", proceed: make(chan struct{}), observed: observed}
		sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": fs}, statePath)

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("TriggerRun: %v", err)
		}
		synctest.Wait()

		if !sched.Status()["audiobooks"].Running {
			t.Fatalf("expected running=true once admitted")
		}

		if err := sched.StopJob("audiobooks"); err != nil {
			t.Fatalf("StopJob: %v", err)
		}

		// Blocking receive, not a poll: StopJob's cancel() guarantees the
		// fake's ctx.Done() branch fires and sends here before Run returns.
		if err := <-observed; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the runner to observe context.Canceled, got %v", err)
		}

		synctest.Wait()

		st := sched.Status()["audiobooks"]
		if st.Running {
			t.Fatalf("expected running=false after stop, got true")
		}
		if st.LastStatus != "stopped" {
			t.Fatalf("expected canonical status %q, got %q", "stopped", st.LastStatus)
		}

		// A later manual run must be able to start normally.
		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("second TriggerRun after stop: %v", err)
		}
		synctest.Wait()
		close(fs.proceed)
		synctest.Wait()

		st2 := sched.Status()["audiobooks"]
		if st2.Running {
			t.Fatalf("expected the clean restart to have completed")
		}
		if st2.LastStatus != "ok" {
			t.Fatalf("expected canonical status %q for the clean restart, got %q", "ok", st2.LastStatus)
		}
	})
}

// E3: stopping an idle job returns ErrNotRunning.
func TestScheduler_StopJob_Idle_ReturnsErrNotRunning(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	fs := &fakeSyncer{name: "audiobooks"}
	sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": fs}, statePath)

	if err := sched.StopJob("audiobooks"); !errors.Is(err, scheduler.ErrNotRunning) {
		t.Fatalf("StopJob(idle) = %v, want ErrNotRunning", err)
	}
}

// E3: an unknown job name is a distinct error from both ErrAlreadyRunning and
// ErrNotRunning for both TriggerRun and StopJob.
func TestScheduler_UnknownJob_IsDistinctFromKnownErrors(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": &fakeSyncer{name: "audiobooks"}}, statePath)

	if err := sched.TriggerRun("does-not-exist"); err == nil ||
		errors.Is(err, scheduler.ErrAlreadyRunning) || errors.Is(err, scheduler.ErrNotRunning) {
		t.Fatalf("TriggerRun(unknown) = %v, want a distinct unknown-job error", err)
	}
	if err := sched.StopJob("does-not-exist"); !errors.Is(err, scheduler.ErrNotRunning) {
		// StopJob has no per-name registry check today (only the cancel map),
		// so an unknown job currently reads the same as "not running". This
		// assertion pins today's actual contract; if the lead intentionally
		// introduces a distinct unknown-job error for StopJob as part of this
		// slice, this test's expectation must be revisited with them, not
		// silently loosened.
		t.Fatalf("StopJob(unknown) = %v, want ErrNotRunning", err)
	}
}

// E4/I1: a durable-state save failure must not permit an overlapping body and
// must not lose the in-memory running/cancel state; a later run must still
// succeed once the current body completes, despite every save having failed.
func TestScheduler_TriggerRun_StateSaveFailure_PreservesRunningStateAndAllowsLaterRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		// blocker is a regular file; using it as a path *component* makes
		// every MkdirAll/Save inside the scheduler's state store fail
		// deterministically on both Windows and POSIX.
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		statePath := filepath.Join(blocker, "sub", "state.json")

		fs := &fakeSyncer{name: "audiobooks", proceed: make(chan struct{})}
		sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": fs}, statePath)

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("TriggerRun: %v", err)
		}
		synctest.Wait()

		if !sched.Status()["audiobooks"].Running {
			t.Fatalf("expected running=true to survive a state save failure")
		}
		if err := sched.TriggerRun("audiobooks"); !errors.Is(err, scheduler.ErrAlreadyRunning) {
			t.Fatalf("expected ErrAlreadyRunning despite save failure, got %v", err)
		}

		close(fs.proceed)
		synctest.Wait()

		if sched.Status()["audiobooks"].Running {
			t.Fatalf("expected running=false after completion despite save failure")
		}

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("expected a later run to succeed after completion despite save failures: %v", err)
		}
		synctest.Wait()
	})
}

// E4/I1 (review-1.md): a state-save failure must not cost Stop its ability to
// reach the admitted context. This complements
// TestScheduler_TriggerRun_StateSaveFailure_PreservesRunningStateAndAllowsLaterRun
// (which proves in-memory running state survives a save failure for a body
// that completes on its own) by proving the *cancel* path specifically:
// with saves failing throughout, a channel-blocked body must still be
// reachable by StopJob, and the runner must still observe context.Canceled -
// atomic cancel registration cannot be a casualty of the save failure.
func TestScheduler_StopJob_ReachesAdmittedContext_DespiteStateSaveFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		statePath := filepath.Join(blocker, "sub", "state.json")

		observed := make(chan error, 1)
		fs := &fakeSyncer{name: "audiobooks", proceed: make(chan struct{}), observed: observed}
		sched := newScheduler(t, map[string]scheduler.Syncer{"audiobooks": fs}, statePath)

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("TriggerRun: %v", err)
		}
		synctest.Wait()

		if !sched.Status()["audiobooks"].Running {
			t.Fatalf("expected running=true once admitted, despite save failure")
		}

		if err := sched.StopJob("audiobooks"); err != nil {
			t.Fatalf("StopJob must still reach the admitted context despite every state save failing, got: %v", err)
		}

		if err := <-observed; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the runner to observe context.Canceled despite save failure, got %v", err)
		}

		synctest.Wait()

		st := sched.Status()["audiobooks"]
		if st.Running {
			t.Fatalf("expected running=false after a cancelled body completes, even with save failing")
		}
		if st.LastStatus != "stopped" {
			t.Fatalf("expected canonical status %q despite save failure, got %q", "stopped", st.LastStatus)
		}

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("expected a later run to start cleanly after a cancelled body, despite save failure: %v", err)
		}
		synctest.Wait()
		close(fs.proceed)
		synctest.Wait()
	})
}
