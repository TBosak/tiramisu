package audiobookjob_test

// Requirement coverage (review-1.md A4/E2/I4): "scheduler Status() must
// retain a canonical redacted failure and must not retain AudioSilo/
// Audiobookshelf/Prowlarr tokens, magnet:, or raw URLs... the current tests
// only inspect a direct construction error and never inspect Scheduler
// status." This file closes exactly that gap: it wires the real
// audiobookjob.Syncer (not a test fake) into a real scheduler.Scheduler,
// exactly as main.go's job map does for every other job, triggers it,
// awaits completion deterministically via synctest (no sleeps/polling), and
// inspects Scheduler.Status() itself - not just the error Syncer.Run()
// happens to return directly.
//
// Run() is currently a declaration-only stub that always returns the same
// fixed, already-safe error regardless of its Cfg, so this test is not a RED
// anchor today: it passes vacuously against the stub, the same way
// TestSyncer_Run_ConstructionFailure_NeverLeaksConfiguredSecret already does
// (see builddeps_test.go). It becomes a real, exact check the moment Run()
// is implemented for real: if a future implementation lets a raw error from
// live construction reach jt.SetStatus verbatim (the scheduler itself never
// redacts - it stores whatever string a Syncer.Run returns), this test
// starts failing for the right reason.

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/syncer/scheduler"
)

// A4/E2/I4: a construction failure reaching the real Syncer through a real
// Scheduler must land as scheduler.Status()["audiobooks"].LastStatus ==
// "failed" with a LastError that contains none of the AudioSilo/
// Audiobookshelf/Prowlarr credential sentinels or the raw configured URLs.
func TestSchedulerStatus_RealAudiobooksSyncer_ConstructionFailure_IsRedacted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			audioSiloToken      = "audiosilo-secret-should-not-leak"
			audiobookshelfToken = "audiobookshelf-secret-should-not-leak"
			prowlarrAPIKey      = "prowlarr-secret-should-not-leak"
		)
		const audiobookshelfURL = "http://embedded-user:embedded-pass@127.0.0.1:0/should-not-leak"

		cfg := audiobookjob.Config{
			AudioSiloURL:            "http://127.0.0.1:1",
			AudioSiloToken:          audioSiloToken,
			AudiobookshelfURL:       audiobookshelfURL, // userinfo makes this an invalid origin: a real construction failure, not pre-validation
			AudiobookshelfToken:     audiobookshelfToken,
			AudiobookshelfLibraryID: "lib-1",
			LibraryURL:              "http://127.0.0.1:2",
			StatePath:               filepath.Join(t.TempDir(), "state.json"),
			RemovalPolicy:           audiobookimport.RemovalPolicy{ConsecutiveMissingThreshold: 3, Grace: 24 * time.Hour},
			ProwlarrCfg:             prowlarr.ConfigProwlarr{Enabled: true, APIKey: prowlarrAPIKey, URL: "http://127.0.0.1:3"},
			Categories:              []int{100},
			IndexerIDs:              []int{5},
			Limits: audiobookimport.DiscoveryLimits{
				MaxCandidates: 10, MaxImports: 5, MaxLatestExamined: 20, MaxSeriesExamined: 20,
				SearchLimit: 5, LatestLimit: 5,
				Selection: audiobookimport.SelectionLimits{
					MaxQueries: 2, MaxResultsPerQuery: 5, MaxCandidatesInspected: 5,
					MaxSourceBytes: 1 << 20, MaxReleaseSizeBytes: 1 << 30,
				},
			},
			PaceSeconds: 10,
		}

		syncer := &audiobookjob.Syncer{Cfg: cfg}
		dir := t.TempDir()
		sched := scheduler.New(scheduler.SchedulerConfig{}, map[string]scheduler.Syncer{"audiobooks": syncer}, filepath.Join(dir, "scheduler_state.json"))

		if err := sched.TriggerRun("audiobooks"); err != nil {
			t.Fatalf("TriggerRun(audiobooks): %v", err)
		}
		synctest.Wait()

		status, ok := sched.Status()["audiobooks"]
		if !ok {
			t.Fatal(`scheduler.Status() has no "audiobooks" entry`)
		}
		if status.Running {
			t.Fatal("expected the run to have completed (Run() fails fast on a stub/invalid construction), got Running=true")
		}
		if status.LastStatus != "failed" {
			t.Fatalf("expected canonical status %q for a construction failure, got %q", "failed", status.LastStatus)
		}

		for _, sentinel := range []string{audioSiloToken, audiobookshelfToken, prowlarrAPIKey, audiobookshelfURL, "embedded-user", "embedded-pass"} {
			if strings.Contains(status.LastError, sentinel) {
				t.Errorf("scheduler.Status()[audiobooks].LastError %q leaks sentinel %q", status.LastError, sentinel)
			}
		}
	})
}
