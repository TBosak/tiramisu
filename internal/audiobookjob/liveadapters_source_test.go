package audiobookjob_test

// Requirement coverage (A6, review-2.md): liveadapters_test.go already
// proves the Provider (AudioSilo) and one Identities call reach their real
// backends. This file closes the remaining A6 gap review-2.md named by
// exact citation: Inventory.OwnedAuthorIDs/OwnedSeriesIDs/
// SeriesGapCandidates against Audiobookshelf; Selector.SelectSource against
// Prowlarr search (with the configured categories/indexer IDs and API key -
// the real wire contract of the already-implemented, already-tested
// internal/prowlarr.Client.SearchWithOptions, GET {url}/api/v1/search) and
// the Library inspect endpoint (POST {url}/api/library/inspect, per
// main.go's route registration: http.HandleFunc("/api/library/inspect",
// libHandler.Inspect)); and Publisher.Publish against Library add (POST
// {url}/api/library/add) and authenticated Audiobookshelf scan (POST
// {url}/api/libraries/{id}/scan, matching AudiobookshelfClient.Scan's
// already-tested wire format) using the configured library id.
//
// Each subtest builds a fresh RunnerDeps graph (a fresh BuildLiveDeps call)
// before calling the one method under test, specifically to avoid a false
// negative if a real implementation memoizes an Audiobookshelf/Prowlarr
// fetch across calls on one graph - this file does not assume anything
// about that caching choice.
//
// Every test below currently fails at the "err != nil" guard: BuildLiveDeps
// is a declaration-only stub. They become live, exact-wire-shape
// assertions the moment it is implemented for real, and directly defeat
// review-2.md's counterexample ("real provider/identity adapters plus inert
// Selector/Publisher/Inventory... pass every current A6 test").

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
)

// A6: OwnedAuthorIDs, OwnedSeriesIDs, and SeriesGapCandidates must each
// reach Audiobookshelf's inventory endpoint with the configured library id
// and Bearer token - not merely return an empty map/slice without ever
// calling out.
func TestBuildLiveDeps_Inventory_EachMethodReachesAudiobookshelf(t *testing.T) {
	const libraryID = "lib-inventory-1"

	newDeps := func(t *testing.T) (audiobookimport.RunnerDeps, *requestRecorder, string) {
		t.Helper()
		rec := &requestRecorder{}
		audiobookshelf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[],"total":0}`))
		}))
		t.Cleanup(audiobookshelf.Close)

		lc := newLiveCanaries(t)
		cfg := validConfig(t, lc, t.TempDir())
		cfg.AudiobookshelfURL = audiobookshelf.URL
		cfg.AudiobookshelfLibraryID = libraryID

		deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildLiveDeps: %v", err)
		}
		return deps, rec, libraryID
	}

	t.Run("OwnedAuthorIDs", func(t *testing.T) {
		deps, rec, libID := newDeps(t)
		if deps.Inventory == nil {
			t.Fatal("RunnerDeps.Inventory is nil")
		}
		if _, err := deps.Inventory.OwnedAuthorIDs(context.Background()); err != nil {
			t.Fatalf("Inventory.OwnedAuthorIDs: %v", err)
		}
		if _, ok := rec.findByPathContains("/api/libraries/" + libID + "/items"); !ok {
			t.Fatalf("OwnedAuthorIDs made no request to Audiobookshelf's library-items endpoint for library %q; recorded: %+v", libID, rec.all())
		}
	})

	t.Run("OwnedSeriesIDs", func(t *testing.T) {
		deps, rec, libID := newDeps(t)
		if _, err := deps.Inventory.OwnedSeriesIDs(context.Background()); err != nil {
			t.Fatalf("Inventory.OwnedSeriesIDs: %v", err)
		}
		if _, ok := rec.findByPathContains("/api/libraries/" + libID + "/items"); !ok {
			t.Fatalf("OwnedSeriesIDs made no request to Audiobookshelf's library-items endpoint for library %q; recorded: %+v", libID, rec.all())
		}
	})

	t.Run("SeriesGapCandidates", func(t *testing.T) {
		deps, rec, libID := newDeps(t)
		if _, err := deps.Inventory.SeriesGapCandidates(context.Background(), "series-1"); err != nil {
			t.Fatalf("Inventory.SeriesGapCandidates: %v", err)
		}
		if _, ok := rec.findByPathContains("/api/libraries/" + libID + "/items"); !ok {
			t.Fatalf("SeriesGapCandidates made no request to Audiobookshelf's library-items endpoint for library %q; recorded: %+v", libID, rec.all())
		}
	})
}

// A6: SelectSource must reach Prowlarr's real search endpoint
// (GET {url}/api/v1/search) with the configured API key, categories, and
// indexer IDs (internal/prowlarr.Client.SearchWithOptions's own, already
// production, wire contract), and must reach the Library inspect endpoint
// (POST {url}/api/library/inspect, main.go's registered route) rather than
// selecting a source without ever checking what the Library already has.
func TestBuildLiveDeps_Selector_SelectSource_ReachesProwlarrAndLibraryInspect(t *testing.T) {
	prowlarrRec := &requestRecorder{}
	prowlarrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prowlarrRec.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer prowlarrServer.Close()

	libraryRec := &requestRecorder{}
	libraryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		libraryRec.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer libraryServer.Close()

	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	cfg.ProwlarrCfg.URL = prowlarrServer.URL
	cfg.LibraryURL = libraryServer.URL
	cfg.Categories = []int{100, 101}
	cfg.IndexerIDs = []int{7, 9}

	deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLiveDeps: %v", err)
	}
	if deps.Selector == nil {
		t.Fatal("RunnerDeps.Selector is nil")
	}

	target := audiobookimport.Target{
		Work:      audiobookimport.WorkFacts{WorkID: "w1", Title: "Book One", Authors: []string{"Author One"}},
		Recording: audiobookimport.RecordingFacts{RecordingID: "r1"},
	}
	// SelectSource itself may legitimately decide there is no eligible
	// source (empty Prowlarr results, above); the point under test is
	// whether it reached out at all, not what it decided.
	if _, err := deps.Selector.SelectSource(context.Background(), target, cfg.Limits.Selection); err != nil {
		t.Fatalf("Selector.SelectSource: %v", err)
	}

	req, ok := prowlarrRec.findByPathContains("/api/v1/search")
	if !ok {
		t.Fatalf("SelectSource made no request to Prowlarr's search endpoint; recorded: %+v", prowlarrRec.all())
	}
	if req.query.Get("apikey") != cfg.ProwlarrCfg.APIKey {
		t.Fatalf("Prowlarr search apikey = %q, want %q", req.query.Get("apikey"), cfg.ProwlarrCfg.APIKey)
	}
	if got := req.query.Get("categories"); got != "100,101" {
		t.Fatalf("Prowlarr search categories = %q, want %q", got, "100,101")
	}
	if got := req.query.Get("indexerIds"); got != "7,9" {
		t.Fatalf("Prowlarr search indexerIds = %q, want %q", got, "7,9")
	}

	if _, ok := libraryRec.findByPathContains("/api/library/inspect"); !ok {
		t.Fatalf("SelectSource made no request to the Library inspect endpoint; recorded: %+v", libraryRec.all())
	}
}

// A6: Publish must reach the Library add endpoint (POST
// {url}/api/library/add) and, using the configured library id, Audiobookshelf's
// authenticated scan endpoint (POST {url}/api/libraries/{id}/scan, matching
// AudiobookshelfClient.Scan's already-tested wire format) - not merely
// report success without ever filing anything or triggering a rescan.
func TestBuildLiveDeps_Publisher_Publish_ReachesLibraryAddAndAudiobookshelfScan(t *testing.T) {
	const libraryID = "lib-publish-1"

	libraryRec := &requestRecorder{}
	libraryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		libraryRec.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"already_present": false}`))
	}))
	defer libraryServer.Close()

	audiobookshelfRec := &requestRecorder{}
	audiobookshelfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audiobookshelfRec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer audiobookshelfServer.Close()

	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	cfg.LibraryURL = libraryServer.URL
	cfg.AudiobookshelfURL = audiobookshelfServer.URL
	cfg.AudiobookshelfLibraryID = libraryID

	deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLiveDeps: %v", err)
	}
	if deps.Publisher == nil {
		t.Fatal("RunnerDeps.Publisher is nil")
	}

	work := audiobookimport.WorkFacts{WorkID: "w1", Title: "Book One", Authors: []string{"Author One"}}
	recording := audiobookimport.RecordingFacts{RecordingID: "r1"}
	selection := audiobookimport.SelectionResult{
		Selected: &audiobookimport.Decision{
			Eligible: true,
			Selected: []audiobookimport.SelectedFile{{FileIndex: 0, Part: 1}},
		},
		Hash:  "0123456789abcdef0123456789abcdef01234567",
		Files: []audiobookimport.AudioFile{{Index: 0, Path: "book.mp3", Size: 1024}},
	}
	projected := []audiobookimport.ProjectedFile{{DecisionIndex: 0, FileIndex: 0, Path: "Author One/Book One/book.mp3"}}
	if _, err := deps.Publisher.Publish(context.Background(), audiobookimport.PublicationRequest{
		Work: work, Recording: recording, Selection: selection, Projected: projected, DryRun: false,
	}); err != nil {
		t.Fatalf("Publisher.Publish: %v", err)
	}

	if _, ok := libraryRec.findByPathContains("/api/library/add"); !ok {
		t.Fatalf("Publish made no request to the Library add endpoint; recorded: %+v", libraryRec.all())
	}

	req, ok := audiobookshelfRec.findByPathContains("/api/libraries/" + libraryID + "/scan")
	if !ok {
		t.Fatalf("Publish made no request to Audiobookshelf's scan endpoint for library %q; recorded: %+v", libraryID, audiobookshelfRec.all())
	}
	if req.auth != "Bearer "+cfg.AudiobookshelfToken {
		t.Fatalf("Audiobookshelf scan request Authorization = %q, want %q", req.auth, "Bearer "+cfg.AudiobookshelfToken)
	}
}
