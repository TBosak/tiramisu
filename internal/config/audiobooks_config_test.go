package config_test

// Requirement coverage (see .tdd-state/audiobook-scheduler/brief.md):
//
//   A5/I3: audiobook configuration that is entirely absent from a config.json
//     (a pre-existing install upgrading in place) must leave the feature
//     disabled and must not alter unrelated scheduler/music fields.
//   E5: a JSON round trip through Config preserves the audiobooks section
//     alongside every other scheduler entry (movies/tv/watchlist/music),
//     matching the existing music_sync-preservation contract this package
//     already relies on in settings.html.
//
// These tests exercise only the struct/JSON-tag declarations the lead added
// to internal/config/config.go (AudiobookConfig, Config.Audiobooks,
// SchedulerConfig.AudiobooksSync) plus Go's standard encoding/json zero-value
// and omission semantics - no Validate function exists yet for this package
// (see this slice's manifest: the bounded, deterministic validation half of
// A5/I3, e.g. rejecting an out-of-range day/hour/minute, has no declared
// entry point to test against and is reported as a residual gap rather than
// guessed at here). These tests deliberately avoid calling LoadConfig(),
// which has real side effects (env var overrides, telemetry ID generation
// and an unconditional file write) unrelated to this slice.
//
// Environment note: this package could not be *run* on the Windows/no-CGO
// host these tests were authored on - it transitively imports
// tiramisu/internal/prowlarr -> tiramisu/internal/library, and
// internal/library/containment_other.go has a pre-existing `//go:build
// !linux` file that references syscall.Stat_t, which does not exist on
// Windows. That bug predates this slice and is out of this test author's
// write boundary (a non-test production file outside internal/musicimport).
// These tests were instead verified with `GOOS=linux GOARCH=amd64
// CGO_ENABLED=0 go vet ./internal/config/...`, which type-checks cleanly;
// see this slice's manifest for the exact command and result.

import (
	"encoding/json"
	"reflect"
	"testing"

	"tiramisu/internal/config"
)

// A5/I3: the zero value of AudiobookConfig (what an install gets when the
// "audiobooks" key is entirely absent from config.json) must be inert.
func TestAudiobookConfig_ZeroValue_IsDisabled(t *testing.T) {
	var cfg config.Config
	if cfg.Audiobooks.Enabled {
		t.Fatal("zero-value Config.Audiobooks.Enabled = true, want false")
	}
	if !reflect.DeepEqual(cfg.Audiobooks, config.AudiobookConfig{}) {
		t.Fatalf("zero-value Config.Audiobooks = %+v, want the zero value", cfg.Audiobooks)
	}
}

// A5/I3: the zero value of SchedulerConfig.AudiobooksSync (what a
// hand-built SchedulerConfig gets if a caller does not set it) must not
// enable automatic scheduling.
func TestSchedulerConfig_ZeroValue_AudiobooksSyncDisabled(t *testing.T) {
	var sc config.SchedulerConfig
	if sc.AudiobooksSync.Enabled {
		t.Fatal("zero-value SchedulerConfig.AudiobooksSync.Enabled = true, want false")
	}
}

// A5/I3: a config.json from before this slice existed (no "audiobooks" key,
// no "audiobooks_sync" key) must unmarshal with the audiobook feature
// disabled and must leave every pre-existing scheduler entry - especially
// music_sync, which settings.html already treats as needing explicit
// preservation - completely unaffected.
func TestConfig_LegacyJSONWithoutAudiobooksKey_StaysDisabledAndPreservesMusicSync(t *testing.T) {
	const legacyJSON = `{
		"scheduler": {
			"enabled": true,
			"movies_sync": {"enabled": true, "days_of_week": [1,4], "hour": 3, "minute": 0},
			"tv_sync": {"enabled": true, "days_of_week": [3,5], "hour": 4, "minute": 0},
			"music_sync": {"enabled": true, "days_of_week": [0], "hour": 15, "minute": 0},
			"watchlist_sync": {"enabled": true, "interval_hours": 1}
		}
	}`

	var cfg config.Config
	if err := json.Unmarshal([]byte(legacyJSON), &cfg); err != nil {
		t.Fatalf("unmarshal legacy config.json: %v", err)
	}

	if cfg.Audiobooks.Enabled {
		t.Fatal("legacy config.json without an audiobooks key produced Audiobooks.Enabled = true")
	}
	if cfg.Scheduler.AudiobooksSync.Enabled {
		t.Fatal("legacy config.json without an audiobooks_sync key produced AudiobooksSync.Enabled = true")
	}

	want := config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 15, Minute: 0}
	if got := cfg.Scheduler.MusicSync; got.Enabled != want.Enabled || got.Hour != want.Hour || got.Minute != want.Minute || len(got.DaysOfWeek) != 1 || got.DaysOfWeek[0] != 0 {
		t.Fatalf("legacy config.json's music_sync was altered by adding audiobooks support: got %+v, want %+v", got, want)
	}
	if !cfg.Scheduler.WatchlistSync.Enabled || cfg.Scheduler.WatchlistSync.IntervalHours != 1 {
		t.Fatalf("legacy config.json's watchlist_sync was altered: got %+v", cfg.Scheduler.WatchlistSync)
	}
}

// E5: saving settings (a JSON round trip through Config) must preserve an
// audiobooks section alongside every other scheduler entry, not just the
// ones a particular UI form happens to render.
func TestConfig_JSONRoundTrip_PreservesAudiobooksAlongsideExistingSchedulerEntries(t *testing.T) {
	original := config.Config{
		Scheduler: config.SchedulerConfig{
			Enabled:        true,
			MoviesSync:     config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1, 4}, Hour: 3, Minute: 0},
			TVSync:         config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{3, 5}, Hour: 4, Minute: 0},
			MusicSync:      config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 15, Minute: 0},
			AudiobooksSync: config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{2}, Hour: 6, Minute: 30},
			WatchlistSync:  config.WatchlistSyncConfig{Enabled: true, IntervalHours: 1},
		},
		Audiobooks: config.AudiobookConfig{
			Enabled:                 true,
			AudioSiloURL:            "https://audiosilo.example.internal",
			AudiobookshelfURL:       "https://audiobookshelf.example.internal",
			AudiobookshelfLibraryID: "lib-1",
			StatePath:               "/var/lib/tiramisu/audiobooks_state.json",
			PaceSeconds:             10,
			Categories:              []int{100},
			IndexerIDs:              []int{5},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var round config.Config
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(round.Audiobooks, original.Audiobooks) {
		t.Fatalf("round-tripped Audiobooks = %+v, want %+v", round.Audiobooks, original.Audiobooks)
	}
	if round.Scheduler.AudiobooksSync.Hour != 6 || round.Scheduler.AudiobooksSync.Minute != 30 {
		t.Fatalf("round-tripped AudiobooksSync = %+v, want Hour=6 Minute=30", round.Scheduler.AudiobooksSync)
	}
	if round.Scheduler.MusicSync.Hour != 15 || round.Scheduler.MusicSync.Minute != 0 {
		t.Fatalf("round trip altered music_sync: got %+v", round.Scheduler.MusicSync)
	}
	if round.Scheduler.WatchlistSync.IntervalHours != 1 {
		t.Fatalf("round trip altered watchlist_sync: got %+v", round.Scheduler.WatchlistSync)
	}

	// Never render/serialize a secret placeholder as a literal in the JSON
	// key names or shape - the fields exist, but this asserts the encoded
	// document uses the declared key names, not a stray secret-shaped key.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &asMap); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	if _, ok := asMap["audiobooks"]; !ok {
		t.Fatal(`marshaled Config is missing the top-level "audiobooks" key`)
	}
}
