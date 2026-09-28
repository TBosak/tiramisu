package audiobookjob_test

// Requirement coverage (A6 removal, per the lead's accepted seam and the
// explicit follow-up instruction): audiobookjob.BuildLiveRemovalDeps is now
// a declared boundary (a stub that always errors). These tests cover only
// its construction/live-wiring contract - NOT any automatic Syncer-driven
// reconciliation, and NOT treating a bounded discovery run's partial rows as
// an authoritative seen set. audiobookimport.Reconcile is called here
// directly, with an explicit, deterministic, hand-built authoritative seen
// map, exactly as its own conservative API requires.
//
// Every test below fails at the "err != nil" guard today: BuildLiveRemovalDeps
// is a declaration-only stub. They become live, exact-wire-shape assertions
// the moment it is implemented for real.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
)

// A6: a fully valid Config must produce a complete RemovalDeps graph without
// touching the network, whose Policy and AudiobookshelfLibraryID are the
// configured values verbatim (RemovalPolicy and the library id are both
// plain comparable values, so this is an exact equality check, not a
// structural approximation).
func TestBuildLiveRemovalDeps_ValidConfig_ConstructsGraphWithConfiguredPolicyAndLibraryIDWithoutNetworkCalls(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())

	deps, err := audiobookjob.BuildLiveRemovalDeps(context.Background(), cfg)
	lc.assertNeverHit(t)
	if err != nil {
		t.Fatalf("BuildLiveRemovalDeps(valid config) = %v, want nil error", err)
	}
	if deps.Remover == nil {
		t.Error("RemovalDeps.Remover is nil")
	}
	if deps.Scanner == nil {
		t.Error("RemovalDeps.Scanner is nil")
	}
	if deps.Store == nil {
		t.Error("RemovalDeps.Store is nil")
	}
	if deps.Policy != cfg.RemovalPolicy {
		t.Errorf("RemovalDeps.Policy = %+v, want cfg.RemovalPolicy verbatim %+v", deps.Policy, cfg.RemovalPolicy)
	}
	if deps.AudiobookshelfLibraryID != cfg.AudiobookshelfLibraryID {
		t.Errorf("RemovalDeps.AudiobookshelfLibraryID = %q, want %q", deps.AudiobookshelfLibraryID, cfg.AudiobookshelfLibraryID)
	}
}

// I2: an already-cancelled context must be rejected before any client is
// constructed.
func TestBuildLiveRemovalDeps_CancelledContext_FailsWithoutClientConstruction(t *testing.T) {
	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := audiobookjob.BuildLiveRemovalDeps(ctx, cfg)
	lc.assertNeverHit(t)
	if err == nil {
		t.Fatal("BuildLiveRemovalDeps(cancelled ctx) = nil error, want non-nil")
	}
}

// A5/E1: every one of these single-field mutations from an otherwise valid
// Config must fail before any client is constructed. Scoped to the fields
// removal construction actually needs (Audiobookshelf reach + auth, the
// Library reach, the durable state path, and a sane removal policy) rather
// than repeating the full discovery-side table.
func TestBuildLiveRemovalDeps_InvalidConfig_FailsBeforeAnyClientConstruction(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *audiobookjob.Config)
	}{
		{"missing Audiobookshelf URL", func(c *audiobookjob.Config) { c.AudiobookshelfURL = "" }},
		{"missing Audiobookshelf token", func(c *audiobookjob.Config) { c.AudiobookshelfToken = "" }},
		{"missing Audiobookshelf library id", func(c *audiobookjob.Config) { c.AudiobookshelfLibraryID = "" }},
		{"missing Library URL", func(c *audiobookjob.Config) { c.LibraryURL = "" }},
		{"missing state path", func(c *audiobookjob.Config) { c.StatePath = "" }},
		{"non-positive removal ConsecutiveMissingThreshold", func(c *audiobookjob.Config) { c.RemovalPolicy.ConsecutiveMissingThreshold = 0 }},
		{"negative removal Grace", func(c *audiobookjob.Config) { c.RemovalPolicy.Grace = -time.Hour }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLiveCanaries(t)
			cfg := validConfig(t, lc, t.TempDir())
			tc.mutate(&cfg)

			_, err := audiobookjob.BuildLiveRemovalDeps(context.Background(), cfg)
			lc.assertNeverHit(t)
			if err == nil {
				t.Fatalf("BuildLiveRemovalDeps(%s) = nil error, want a validation failure", tc.name)
			}
		})
	}
}

// A6: the configured crash-safe state store must actually be read from
// cfg.StatePath, not some other hardcoded or ignored location - proven the
// same way internal/audiobookjob/liveadapters_test.go proves BuildLiveDeps'
// Identities reads its configured StatePath: a malformed file at the
// configured path must break a state-backed call, and a valid one must not.
func TestBuildLiveRemovalDeps_Store_UsesConfiguredStatePath(t *testing.T) {
	lc := newLiveCanaries(t)

	t.Run("malformed state file fails", func(t *testing.T) {
		dir := t.TempDir()
		cfg := validConfig(t, lc, dir)
		if err := os.WriteFile(cfg.StatePath, []byte("{ not valid json"), 0o644); err != nil {
			t.Fatalf("seed malformed state file: %v", err)
		}

		deps, err := audiobookjob.BuildLiveRemovalDeps(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildLiveRemovalDeps(valid config, malformed state file) = %v, want nil error (construction itself should not fail)", err)
		}
		if _, err := deps.Store.Load(context.Background()); err == nil {
			t.Fatal("Store.Load succeeded despite a malformed state file at the configured StatePath; the configured path is not actually being read")
		}
	})

	t.Run("valid empty state file succeeds", func(t *testing.T) {
		dir := t.TempDir()
		cfg := validConfig(t, lc, dir)
		validState, err := json.Marshal(audiobookimport.State{Version: audiobookimport.CurrentStateVersion, Entries: map[string]audiobookimport.StateEntry{}})
		if err != nil {
			t.Fatalf("marshal valid empty state: %v", err)
		}
		if err := os.WriteFile(cfg.StatePath, validState, 0o644); err != nil {
			t.Fatalf("seed valid state file: %v", err)
		}

		deps, err := audiobookjob.BuildLiveRemovalDeps(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildLiveRemovalDeps(valid config, valid state file) = %v, want nil error", err)
		}
		state, err := deps.Store.Load(context.Background())
		if err != nil {
			t.Fatalf("Store.Load(valid empty state) = %v, want nil error", err)
		}
		if len(state.Entries) != 0 {
			t.Fatalf("Store.Load(valid empty state) = %+v, want zero entries", state)
		}
	})

	lc.assertNeverHit(t)
}

// A6: audiobookimport.Reconcile, given the live RemovalDeps this factory
// constructs, a deterministic authoritative seen-identity map (explicitly
// NOT derived from any discovery run - hand-built here, per the caller
// contract audiobookimport.Reconcile already requires), and a
// controller-owned state entry that already satisfies the configured
// removal threshold and grace period, must reach the real Library remove
// endpoint (POST {url}/api/library/remove) and then the authenticated
// Audiobookshelf scan endpoint (POST {url}/api/libraries/{id}/scan) for the
// configured library id - not merely report a removal without ever filing
// it or rescanning.
func TestBuildLiveRemovalDeps_Reconcile_WithAuthoritativeSeenMap_RemovesThroughLibraryAndScansAudiobookshelf(t *testing.T) {
	const libraryID = "lib-removal-1"
	const audiobookshelfToken = "audiobookshelf-token-should-not-leak"

	libraryRec := &requestRecorder{}
	libraryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		libraryRec.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer libraryServer.Close()

	audiobookshelfRec := &requestRecorder{}
	audiobookshelfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audiobookshelfRec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer audiobookshelfServer.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "audiobooks_state.json")

	// A deterministic, fixed instant - not time.Now() - so grace-elapsed
	// arithmetic below is reproducible.
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	const threshold = 3
	const grace = 24 * time.Hour

	id := audiobookimport.ExternalIdentity{Namespace: audiobookimport.IdentityNamespaceAudioSiloWork, ID: "w1"}
	entry := audiobookimport.StateEntry{
		Namespace:          id.Namespace,
		ExternalID:         id.ID,
		OwnedPrefix:        "Author One/Book One",
		ControllerOwned:    true,
		Stage:              audiobookimport.StageScanned,
		ConsecutiveMissing: threshold - 1, // one more miss (this call) reaches the threshold
		MissingSince:       now.Add(-2 * grace),
		UpdatedAt:          now.Add(-2 * grace),
	}
	seedState, err := json.Marshal(audiobookimport.State{
		Version: audiobookimport.CurrentStateVersion,
		Entries: map[string]audiobookimport.StateEntry{id.Key(): entry},
	})
	if err != nil {
		t.Fatalf("marshal seed state: %v", err)
	}
	if err := os.WriteFile(statePath, seedState, 0o644); err != nil {
		t.Fatalf("seed state file: %v", err)
	}

	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, dir)
	cfg.StatePath = statePath
	cfg.LibraryURL = libraryServer.URL
	cfg.AudiobookshelfURL = audiobookshelfServer.URL
	cfg.AudiobookshelfToken = audiobookshelfToken
	cfg.AudiobookshelfLibraryID = libraryID
	cfg.RemovalPolicy = audiobookimport.RemovalPolicy{ConsecutiveMissingThreshold: threshold, Grace: grace}

	deps, err := audiobookjob.BuildLiveRemovalDeps(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLiveRemovalDeps: %v", err)
	}

	// The authoritative seen set for this reconcile pass: explicitly does
	// NOT contain id, and is hand-built here rather than inferred from any
	// discovery run's partial results.
	seen := map[audiobookimport.ExternalIdentity]bool{}

	result, err := audiobookimport.Reconcile(context.Background(), deps, seen, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	found := false
	for _, removed := range result.Removed {
		if removed == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("Reconcile result.Removed = %+v, want it to include %+v", result.Removed, id)
	}

	if _, ok := libraryRec.findByPathContains("/api/library/remove"); !ok {
		t.Fatalf("Reconcile made no request to the Library remove endpoint; recorded: %+v", libraryRec.all())
	}

	req, ok := audiobookshelfRec.findByPathContains("/api/libraries/" + libraryID + "/scan")
	if !ok {
		t.Fatalf("Reconcile made no request to Audiobookshelf's scan endpoint for library %q; recorded: %+v", libraryID, audiobookshelfRec.all())
	}
	if req.auth != "Bearer "+audiobookshelfToken {
		t.Fatalf("Audiobookshelf scan request Authorization = %q, want %q", req.auth, "Bearer "+audiobookshelfToken)
	}
}
