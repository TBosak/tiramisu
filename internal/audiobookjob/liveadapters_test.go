package audiobookjob_test

// Requirement coverage (see .tdd-state/audiobook-scheduler/brief.md and
// .tdd-state/audiobook-scheduler/review-1.md's A6 finding): a successful
// BuildLiveDeps must return a graph whose interfaces are backed by real,
// wire-correct adapters - not six inert dummy structs. These tests exercise
// each returned interface against a deterministic httptest server and
// assert the concrete HTTP interaction (method/path/query/auth), not merely
// that a value round-trips. They deliberately stop short of asserting the
// exact identity-resolution mapping between an Audiobookshelf catalog item
// (ASIN/ISBN) and an ExternalIdentity, since that cross-reference is real
// business logic this test author cannot safely infer from the declared
// types without guessing wrong; what is asserted - that the call reaches
// Audiobookshelf at all, with the right auth and library id - is exactly
// what distinguishes a real adapter from an inert dummy, which is the
// counterexample under test.
//
// All of these currently fail at the "err != nil" guard below, because
// BuildLiveDeps is a declaration-only stub: they are RED for A6 today and
// become live, exact-wire-shape assertions the moment BuildLiveDeps is
// implemented for real.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"tiramisu/internal/audiobookjob"
)

// recordedRequest captures the parts of an inbound request this file needs
// to assert on, without retaining the body (never needed here) or letting a
// handler call testing.T from a server goroutine.
type recordedRequest struct {
	method string
	path   string
	query  url.Values
	auth   string
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{
		method: req.Method,
		path:   req.URL.Path,
		query:  req.URL.Query(),
		auth:   req.Header.Get("Authorization"),
	})
}

func (r *requestRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func (r *requestRecorder) findByPathContains(substr string) (recordedRequest, bool) {
	for _, req := range r.all() {
		if strings.Contains(req.path, substr) {
			return req, true
		}
	}
	return recordedRequest{}, false
}

// A6: the returned Provider must be a real AudioSilo-backed
// DiscoveryProvider: SearchWorks/LatestWorks/WorkDetail must hit AudioSilo's
// documented endpoints (see internal/audiobookimport/provider.go, the
// existing production AudioSiloClient this graph presumably wraps) with the
// expected query parameters, decode the response into the interface's own
// return types, and never send an Authorization header (AudioSilo has none -
// see TestAudioSiloClient_NeverSendsAuthorizationHeader).
func TestBuildLiveDeps_Provider_IsRealAudioSiloAdapter(t *testing.T) {
	rec := &requestRecorder{}
	audioSilo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/works/search":
			w.Write([]byte(`{"results":[{"id":"w1","title":"Book One","authors":[{"id":"a1","name":"Author One"}],"series":null,"release_date":"2020-01-01","cover_url":null,"added_at":null,"narrators":[{"id":"n1","name":"Narrator One"}]}]}`))
		case r.URL.Path == "/api/v1/works/latest":
			w.Write([]byte(`{"works":[{"id":"w2","title":"Book Two","authors":[{"id":"a2","name":"Author Two"}],"series":null,"release_date":"2021-01-01","cover_url":null,"added_at":null}]}`))
		case r.URL.Path == "/api/v1/works/w1":
			w.Write([]byte(`{"id":"w1","title":"Book One","authors":[{"id":"a1","name":"Author One"}],"language":"en","series":[],"recordings":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer audioSilo.Close()

	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	cfg.AudioSiloURL = audioSilo.URL

	deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLiveDeps(valid config) = %v, want nil error", err)
	}
	if deps.Provider == nil {
		t.Fatal("RunnerDeps.Provider is nil")
	}

	searchHits, err := deps.Provider.SearchWorks(context.Background(), "book one", 5)
	if err != nil {
		t.Fatalf("Provider.SearchWorks: %v", err)
	}
	if len(searchHits) != 1 || searchHits[0].ID != "w1" || searchHits[0].Title != "Book One" {
		t.Fatalf("Provider.SearchWorks results = %+v, want one hit with id=w1 title=Book One", searchHits)
	}
	if req, ok := rec.findByPathContains("/api/v1/works/search"); !ok {
		t.Fatal("no request reached AudioSilo's search endpoint")
	} else {
		if req.query.Get("q") != "book one" {
			t.Fatalf("search request q = %q, want %q", req.query.Get("q"), "book one")
		}
		if req.query.Get("limit") != "5" {
			t.Fatalf("search request limit = %q, want %q", req.query.Get("limit"), "5")
		}
		if req.auth != "" {
			t.Fatalf("search request carried an Authorization header %q, want none (AudioSilo is unauthenticated)", req.auth)
		}
	}

	latest, err := deps.Provider.LatestWorks(context.Background(), 3)
	if err != nil {
		t.Fatalf("Provider.LatestWorks: %v", err)
	}
	if len(latest) != 1 || latest[0].ID != "w2" {
		t.Fatalf("Provider.LatestWorks results = %+v, want one work with id=w2", latest)
	}
	if req, ok := rec.findByPathContains("/api/v1/works/latest"); !ok {
		t.Fatal("no request reached AudioSilo's latest endpoint")
	} else if req.query.Get("limit") != "3" {
		t.Fatalf("latest request limit = %q, want %q", req.query.Get("limit"), "3")
	}

	detail, err := deps.Provider.WorkDetail(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Provider.WorkDetail: %v", err)
	}
	if detail.ID != "w1" || detail.Title != "Book One" || detail.Language != "en" {
		t.Fatalf("Provider.WorkDetail = %+v, want id=w1 title=%q language=en", detail, "Book One")
	}
	if _, ok := rec.findByPathContains("/api/v1/works/w1"); !ok {
		t.Fatal("no request reached AudioSilo's work-detail endpoint")
	}
}

// A6: the returned Identities source must actually reach Audiobookshelf's
// inventory endpoint with Bearer authentication and the configured library
// id - proving a real adapter is wired, not an inert dummy that never makes
// a request. The exact ASIN/ISBN-to-ExternalIdentity resolution is not
// asserted here (see file header).
func TestBuildLiveDeps_Identities_ReachesAudiobookshelfWithAuthAndLibraryID(t *testing.T) {
	rec := &requestRecorder{}
	const libraryID = "lib-narrow-1"
	const token = "audiobookshelf-token-should-not-leak"
	audiobookshelf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[],"total":0}`))
	}))
	defer audiobookshelf.Close()

	lc := newLiveCanaries(t)
	cfg := validConfig(t, lc, t.TempDir())
	cfg.AudiobookshelfURL = audiobookshelf.URL
	cfg.AudiobookshelfToken = token
	cfg.AudiobookshelfLibraryID = libraryID

	deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLiveDeps(valid config) = %v, want nil error", err)
	}
	if deps.Identities == nil {
		t.Fatal("RunnerDeps.Identities is nil")
	}

	if _, err := deps.Identities.ExistingAudiobookshelfIdentities(context.Background()); err != nil {
		t.Fatalf("Identities.ExistingAudiobookshelfIdentities: %v", err)
	}

	req, ok := rec.findByPathContains("/api/libraries/" + libraryID + "/items")
	if !ok {
		t.Fatalf("no request reached Audiobookshelf's library-items endpoint for library %q; recorded: %+v", libraryID, rec.all())
	}
	if req.auth != "Bearer "+token {
		t.Fatalf("Audiobookshelf request Authorization = %q, want %q", req.auth, "Bearer "+token)
	}
}

// A6: the configured crash-safe state store must actually be read from
// StatePath, not from some other hardcoded or ignored location. A malformed
// state file at the configured path must surface as an error from a
// state-backed call, and a valid (empty) one must not.
func TestBuildLiveDeps_Identities_UsesConfiguredStatePath(t *testing.T) {
	lc := newLiveCanaries(t)

	t.Run("malformed state file fails", func(t *testing.T) {
		dir := t.TempDir()
		cfg := validConfig(t, lc, dir)
		if err := os.WriteFile(cfg.StatePath, []byte("{ not valid json"), 0o644); err != nil {
			t.Fatalf("seed malformed state file: %v", err)
		}

		deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildLiveDeps(valid config, malformed state file) = %v, want nil error (construction itself should not fail)", err)
		}
		if _, err := deps.Identities.CommittedIdentities(context.Background()); err == nil {
			t.Fatal("Identities.CommittedIdentities succeeded despite a malformed state file at the configured StatePath; the configured path is not actually being read")
		}
	})

	t.Run("valid empty state file succeeds", func(t *testing.T) {
		dir := t.TempDir()
		cfg := validConfig(t, lc, dir)
		if err := os.WriteFile(cfg.StatePath, []byte(`{"version":1,"entries":{}}`), 0o644); err != nil {
			t.Fatalf("seed valid state file: %v", err)
		}

		deps, err := audiobookjob.BuildLiveDeps(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildLiveDeps(valid config, valid state file) = %v, want nil error", err)
		}
		got, err := deps.Identities.CommittedIdentities(context.Background())
		if err != nil {
			t.Fatalf("Identities.CommittedIdentities(valid empty state) = %v, want nil error", err)
		}
		if len(got) != 0 {
			t.Fatalf("Identities.CommittedIdentities(valid empty state) = %v, want empty", got)
		}
	})

	lc.assertNeverHit(t)
}
