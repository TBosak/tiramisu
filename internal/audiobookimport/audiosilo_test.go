package audiobookimport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- test client construction -------------------------------------------

func newAudioSiloServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *requestRecorder) {
	t.Helper()
	rec := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, rec
}

func newAudioSiloClient(t *testing.T, baseURL string) (*AudioSiloClient, *bodyCloseTracker) {
	t.Helper()
	httpClient, tracker := trackedClient()
	client, err := NewAudioSiloClient(baseURL, httpClient)
	if err != nil {
		t.Fatalf("NewAudioSiloClient(%q): unexpected error: %v", baseURL, err)
	}
	return client, tracker
}

// newAudioSiloClientWithHTTPClient builds a client around whatever
// *http.Client the caller supplies, unmodified - used by redirect-safety
// tests that must prove the AudioSilo client itself refuses a cross-origin
// redirect, rather than relying on a test fake's CheckRedirect to block it
// before the client's own logic ever runs.
func newAudioSiloClientWithHTTPClient(t *testing.T, baseURL string, httpClient *http.Client) *AudioSiloClient {
	t.Helper()
	client, err := NewAudioSiloClient(baseURL, httpClient)
	if err != nil {
		t.Fatalf("NewAudioSiloClient(%q): unexpected error: %v", baseURL, err)
	}
	return client
}

// followingHTTPClient is an ordinary *http.Client with no CheckRedirect
// override, i.e. exactly what ships in the standard library and what a
// careless caller might hand the constructor: it auto-follows redirects,
// including cross-origin ones. Redirect-safety tests use it deliberately so
// the assertion exercises the client's own logic instead of the test
// double's transport-level block.
func followingHTTPClient() *http.Client {
	return &http.Client{}
}

// --- A1: search --------------------------------------------------------

func TestAudioSiloClient_SearchWorks_RequestShape(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		limit     int
		wantQuery string
		wantLimit string
	}{
		{"plain query default-ish limit", "project hail mary", 20, "project hail mary", "20"},
		{"reserved characters", "rock & roll: a memoir?", 20, "rock & roll: a memoir?", "20"},
		{"unicode query", "日本語のタイトル", 20, "日本語のタイトル", "20"},
		{"limit zero falls back to default", "dune", 0, "dune", "20"},
		{"negative limit falls back to default", "dune", -5, "dune", "20"},
		{"limit above cap clamps to 50", "dune", 1000, "dune", "50"},
		{"limit exactly at cap", "dune", 50, "dune", "50"},
		{"limit exactly one", "dune", 1, "dune", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"results":[]}`))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			if _, err := client.SearchWorks(context.Background(), tc.query, tc.limit); err != nil {
				t.Fatalf("SearchWorks: unexpected error: %v", err)
			}
			if rec.count() != 1 {
				t.Fatalf("request count = %d, want 1", rec.count())
			}
			req := rec.last()
			if req.Method != http.MethodGet {
				t.Errorf("method = %q, want GET", req.Method)
			}
			if req.Path != "/api/v1/works/search" {
				t.Errorf("path = %q, want /api/v1/works/search", req.Path)
			}
			if got := req.Query.Get("q"); got != tc.wantQuery {
				t.Errorf("q = %q, want %q", got, tc.wantQuery)
			}
			if got := req.Query.Get("limit"); got != tc.wantLimit {
				t.Errorf("limit = %q, want %q", got, tc.wantLimit)
			}
		})
	}
}

func TestAudioSiloClient_SearchWorks_BasePathPrefixPreserved(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client, _ := newAudioSiloClient(t, server.URL+"/proxy/audiosilo")

	if _, err := client.SearchWorks(context.Background(), "dune", 10); err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	req := rec.last()
	if req.Path != "/proxy/audiosilo/api/v1/works/search" {
		t.Errorf("path = %q, want /proxy/audiosilo/api/v1/works/search", req.Path)
	}
}

func TestAudioSiloClient_SearchWorks_PreservesServerRankOrder(t *testing.T) {
	body := `{"results":[
		{"id":"c-work","title":"C","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]},
		{"id":"a-work","title":"A","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]},
		{"id":"b-work","title":"B","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]}
	]}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("len(hits) = %d, want 3", len(hits))
	}
	gotOrder := []string{hits[0].ID, hits[1].ID, hits[2].ID}
	wantOrder := []string{"c-work", "a-work", "b-work"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v (rank order must not be re-sorted)", gotOrder, wantOrder)
		}
	}
}

func TestAudioSiloClient_SearchWorks_MapsFullCardAndNarrators(t *testing.T) {
	body := `{"results":[{
		"id":"project-hail-mary",
		"title":"Project Hail Mary",
		"authors":[{"id":"andy-weir","name":"Andy Weir"}],
		"series":{"id":"the-stormlight-archive","name":"The Stormlight Archive","position":"2.5"},
		"release_date":"2021-05-04",
		"cover_url":"https://example.invalid/cover.jpg",
		"added_at":"2021-05-04",
		"narrators":[{"id":"ray-porter","name":"Ray Porter"},{"id":"second-narrator","name":"Second Narrator"}]
	}]}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "project hail mary", 10)
	if err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(hits))
	}
	got := hits[0]
	if got.ID != "project-hail-mary" || got.Title != "Project Hail Mary" {
		t.Errorf("id/title = %q/%q, want project-hail-mary/Project Hail Mary", got.ID, got.Title)
	}
	if len(got.Authors) != 1 || got.Authors[0].ID != "andy-weir" || got.Authors[0].Name != "Andy Weir" {
		t.Errorf("authors = %+v, want [{andy-weir Andy Weir}]", got.Authors)
	}
	if got.Series == nil || got.Series.ID != "the-stormlight-archive" || got.Series.Position != "2.5" {
		t.Errorf("series = %+v, want id the-stormlight-archive position 2.5", got.Series)
	}
	if got.ReleaseDate != "2021-05-04" {
		t.Errorf("release_date = %q, want 2021-05-04", got.ReleaseDate)
	}
	if got.CoverURL == nil || *got.CoverURL != "https://example.invalid/cover.jpg" {
		t.Errorf("cover_url = %v, want https://example.invalid/cover.jpg", got.CoverURL)
	}
	if got.AddedAt == nil || *got.AddedAt != "2021-05-04" {
		t.Errorf("added_at = %v, want 2021-05-04", got.AddedAt)
	}
	if len(got.Narrators) != 2 || got.Narrators[0].Name != "Ray Porter" || got.Narrators[1].Name != "Second Narrator" {
		t.Errorf("narrators = %+v, want [Ray Porter, Second Narrator] in order", got.Narrators)
	}
}

func TestAudioSiloClient_SearchWorks_NullableFieldsAbsentNotInvented(t *testing.T) {
	body := `{"results":[{
		"id":"standalone-work",
		"title":"A Standalone Work",
		"authors":[{"id":"a","name":"A"}],
		"series":null,
		"cover_url":null,
		"added_at":null,
		"narrators":[]
	}]}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "standalone", 10)
	if err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(hits))
	}
	got := hits[0]
	if got.Series != nil {
		t.Errorf("series = %+v, want nil (null means no membership, not an invented empty struct)", got.Series)
	}
	if got.CoverURL != nil {
		t.Errorf("cover_url = %v, want nil", got.CoverURL)
	}
	if got.AddedAt != nil {
		t.Errorf("added_at = %v, want nil", got.AddedAt)
	}
	if got.ReleaseDate != "" {
		t.Errorf("release_date = %q, want empty string (field omitted by server)", got.ReleaseDate)
	}
	if len(got.Narrators) != 0 {
		t.Errorf("narrators = %+v, want empty, not nil-vs-empty confusion turned into invented entries", got.Narrators)
	}
}

// --- A2: latest + work detail -------------------------------------------

func TestAudioSiloClient_LatestWorks_RequestShape(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		wantLimit string
	}{
		{"explicit limit", 30, "30"},
		{"zero falls back to default 12", 0, "12"},
		{"negative falls back to default 12", -1, "12"},
		{"above cap clamps to 50", 999, "50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"works":[]}`))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			if _, err := client.LatestWorks(context.Background(), tc.limit); err != nil {
				t.Fatalf("LatestWorks: unexpected error: %v", err)
			}
			req := rec.last()
			if req.Path != "/api/v1/works/latest" {
				t.Errorf("path = %q, want /api/v1/works/latest", req.Path)
			}
			if got := req.Query.Get("limit"); got != tc.wantLimit {
				t.Errorf("limit = %q, want %q", got, tc.wantLimit)
			}
		})
	}
}

func TestAudioSiloClient_LatestWorks_MapsOrderedWorkCards(t *testing.T) {
	body := `{"works":[
		{"id":"second-work","title":"Second","authors":[{"id":"b","name":"B Author"}],"series":null,"cover_url":null,"added_at":"2024-02-01"},
		{"id":"first-work","title":"First","authors":[{"id":"a","name":"A Author"}],"series":{"id":"s","name":"Series","position":"3"},"cover_url":"https://example.invalid/c.jpg","added_at":"2024-01-01"}
	]}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	works, err := client.LatestWorks(context.Background(), 30)
	if err != nil {
		t.Fatalf("LatestWorks: unexpected error: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("len(works) = %d, want 2 (a client that discards the response and returns none must fail this)", len(works))
	}
	if works[0].ID != "second-work" || works[1].ID != "first-work" {
		t.Fatalf("order = [%q %q], want [second-work first-work] (server rank order preserved)", works[0].ID, works[1].ID)
	}
	first := works[1]
	if first.Title != "First" {
		t.Errorf("Title = %q, want First", first.Title)
	}
	if len(first.Authors) != 1 || first.Authors[0].ID != "a" || first.Authors[0].Name != "A Author" {
		t.Errorf("Authors = %+v, want [{a A Author}]", first.Authors)
	}
	if first.Series == nil || first.Series.Position != "3" {
		t.Errorf("Series = %+v, want position 3", first.Series)
	}
	if first.CoverURL == nil || *first.CoverURL != "https://example.invalid/c.jpg" {
		t.Errorf("CoverURL = %v, want https://example.invalid/c.jpg", first.CoverURL)
	}
	if first.AddedAt == nil || *first.AddedAt != "2024-01-01" {
		t.Errorf("AddedAt = %v, want 2024-01-01", first.AddedAt)
	}
}

func TestAudioSiloClient_WorkDetail_RequestIsEscaped(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"space and apostrophe", "a work's title"},
		{"unicode", "café-édition-2"},
		{"reserved characters", "a/b?c&d=e"},
		{"percent sign literal", "100%-guarantee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","title":"T","authors":[{"id":"a","name":"A Author"}],"language":"en","series":[],"recordings":null}`))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			detail, err := client.WorkDetail(context.Background(), tc.id)
			if err != nil {
				t.Fatalf("WorkDetail(%q): unexpected error: %v", tc.id, err)
			}
			req := rec.last()
			wantEscaped := "/api/v1/works/" + url.PathEscape(tc.id)
			if req.EscapedPath != wantEscaped {
				t.Errorf("escaped path = %q, want %q (id must be escaped, not concatenated raw)", req.EscapedPath, wantEscaped)
			}
			if req.Method != http.MethodGet {
				t.Errorf("method = %q, want GET", req.Method)
			}
			if detail.ID != "x" {
				t.Errorf("ID = %q, want x (required identity field must be mapped)", detail.ID)
			}
			if detail.Title != "T" {
				t.Errorf("Title = %q, want T", detail.Title)
			}
			if len(detail.Authors) != 1 || detail.Authors[0].ID != "a" || detail.Authors[0].Name != "A Author" {
				t.Errorf("Authors = %+v, want [{a A Author}]", detail.Authors)
			}
			if detail.Language != "en" {
				t.Errorf("Language = %q, want en", detail.Language)
			}
		})
	}
}

func TestAudioSiloClient_WorkDetail_MapsSeriesMembershipsAndRecordings(t *testing.T) {
	body := `{
		"id":"the-way-of-kings",
		"title":"The Way of Kings",
		"authors":[{"id":"brandon-sanderson","name":"Brandon Sanderson"}],
		"language":"en",
		"series":[
			{"id":"the-stormlight-archive","name":"The Stormlight Archive","position":"1"},
			{"id":"cosmere-reading-order","name":"Cosmere Reading Order","position":"1-3.5"}
		],
		"recordings":[
			{
				"id":"kramer-2010",
				"narrators":[{"id":"michael-kramer","name":"Michael Kramer"},{"id":"kate-reading","name":"Kate Reading"}],
				"abridged": true,
				"runtime_min": 2735,
				"publisher": "Recorded Books",
				"asin": [{"region":"us","asin":"B0041OS8RC"},{"region":"gb","asin":"B00FKS3P8Y"}],
				"isbn": ["9781400118863","9780765365279"],
				"chapter_count": 88
			},
			{
				"id":"minimal-recording",
				"narrators":[],
				"asin": [],
				"isbn": [],
				"chapter_count": 0
			}
		]
	}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	detail, err := client.WorkDetail(context.Background(), "the-way-of-kings")
	if err != nil {
		t.Fatalf("WorkDetail: unexpected error: %v", err)
	}
	if len(detail.Series) != 2 {
		t.Fatalf("len(series) = %d, want 2 (work card only carries the first; detail must carry every membership)", len(detail.Series))
	}
	if detail.Series[0].Position != "1" || detail.Series[1].Position != "1-3.5" {
		t.Errorf("series positions = %q, %q, want \"1\", \"1-3.5\" preserved as strings", detail.Series[0].Position, detail.Series[1].Position)
	}
	if len(detail.Recordings) != 2 {
		t.Fatalf("len(recordings) = %d, want 2", len(detail.Recordings))
	}

	full := detail.Recordings[0]
	if full.ID != "kramer-2010" {
		t.Errorf("recording id = %q, want kramer-2010", full.ID)
	}
	if len(full.Narrators) != 2 || full.Narrators[0].Name != "Michael Kramer" || full.Narrators[1].Name != "Kate Reading" {
		t.Errorf("narrators = %+v, want [Michael Kramer, Kate Reading]", full.Narrators)
	}
	if full.Abridged != AbridgementAbridged {
		t.Errorf("abridged = %v, want AbridgementAbridged when the server states true", full.Abridged)
	}
	if full.RuntimeMinutes == nil || *full.RuntimeMinutes != 2735 {
		t.Errorf("runtime = %v, want 2735", full.RuntimeMinutes)
	}
	if full.Publisher != "Recorded Books" {
		t.Errorf("publisher = %q, want Recorded Books", full.Publisher)
	}
	if full.ChapterCount != 88 {
		t.Errorf("chapter_count = %d, want 88", full.ChapterCount)
	}
	if len(full.ASINs) != 2 || full.ASINs[0].Region != "us" || full.ASINs[0].ASIN != "B0041OS8RC" || full.ASINs[1].Region != "gb" || full.ASINs[1].ASIN != "B00FKS3P8Y" {
		t.Errorf("asins = %+v, want [{us B0041OS8RC} {gb B00FKS3P8Y}] (regional values distinct, in order)", full.ASINs)
	}
	if len(full.ISBNs) != 2 || full.ISBNs[0] != "9781400118863" || full.ISBNs[1] != "9780765365279" {
		t.Errorf("isbns = %+v, want [9781400118863 9780765365279]", full.ISBNs)
	}

	minimal := detail.Recordings[1]
	if minimal.Abridged != AbridgementUnknown {
		t.Errorf("minimal recording abridged = %v, want AbridgementUnknown: an absent key must not be guessed as Unabridged", minimal.Abridged)
	}
	if minimal.RuntimeMinutes != nil {
		t.Errorf("minimal recording runtime = %v, want nil when omitted", minimal.RuntimeMinutes)
	}
	if minimal.Publisher != "" {
		t.Errorf("minimal recording publisher = %q, want empty when omitted", minimal.Publisher)
	}
}

func TestAudioSiloClient_WorkDetail_NullRecordingsIsValidNoRecording(t *testing.T) {
	body := `{"id":"brand-new-work","title":"Brand New Work","authors":[{"id":"a","name":"A"}],"language":"en","series":[],"recordings":null}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	detail, err := client.WorkDetail(context.Background(), "brand-new-work")
	if err != nil {
		t.Fatalf("WorkDetail: unexpected error for null recordings: %v", err)
	}
	if detail.Recordings != nil {
		t.Errorf("recordings = %+v, want nil (null means no known recording, not an error)", detail.Recordings)
	}
}

// --- A3: lookup ----------------------------------------------------------

func TestAudioSiloClient_Lookup_SendsExactlyOneNormalizedQueryParam(t *testing.T) {
	cases := []struct {
		name      string
		asin      string
		isbn      string
		wantAsin  string
		wantIsbn  string
		wantAsinP bool
		wantIsbnP bool
	}{
		{"asin only", "B08G9PRS1K", "", "B08G9PRS1K", "", true, false},
		{"isbn only, hyphens stripped", "", "978-1-4272-0926-9", "", "9781427209269", false, true},
		{"both given: asin takes precedence, isbn omitted entirely", "B08G9PRS1K", "9781427209269", "B08G9PRS1K", "", true, false},
		{"asin surrounding whitespace trimmed", "  B08G9PRS1K  ", "", "B08G9PRS1K", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"work":{"id":"w","title":"T","authors":[],"series":null,"cover_url":null,"added_at":null},"recording_id":"r"}`))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			if _, err := client.Lookup(context.Background(), tc.asin, tc.isbn); err != nil {
				t.Fatalf("Lookup: unexpected error: %v", err)
			}
			req := rec.last()
			if req.Path != "/api/v1/lookup" {
				t.Fatalf("path = %q, want /api/v1/lookup", req.Path)
			}
			_, hasAsin := req.Query["asin"]
			_, hasIsbn := req.Query["isbn"]
			if hasAsin != tc.wantAsinP || hasIsbn != tc.wantIsbnP {
				t.Fatalf("present asin=%v isbn=%v, want asin=%v isbn=%v (exactly one query param)", hasAsin, hasIsbn, tc.wantAsinP, tc.wantIsbnP)
			}
			if tc.wantAsinP && req.Query.Get("asin") != tc.wantAsin {
				t.Errorf("asin = %q, want %q", req.Query.Get("asin"), tc.wantAsin)
			}
			if tc.wantIsbnP && req.Query.Get("isbn") != tc.wantIsbn {
				t.Errorf("isbn = %q, want %q", req.Query.Get("isbn"), tc.wantIsbn)
			}
			if len(req.Query) != 1 {
				t.Errorf("query params = %v, want exactly one of asin/isbn", req.Query)
			}
		})
	}
}

func TestAudioSiloClient_Lookup_EmptyBothFailsBeforeNetwork(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work":{"id":"w","title":"T","authors":[],"series":null,"cover_url":null,"added_at":null},"recording_id":"r"}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	_, err := client.Lookup(context.Background(), "", "")
	if err == nil {
		t.Fatal("Lookup(\"\", \"\"): got nil error, want a validation error before any network call")
	}
	if rec.count() != 0 {
		t.Errorf("request count = %d, want 0 (must fail before network I/O)", rec.count())
	}
}

func TestAudioSiloClient_Lookup_MapsWorkAndRecordingIDDistinctly(t *testing.T) {
	body := `{"work":{"id":"project-hail-mary","title":"Project Hail Mary","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":"2021-05-04"},"recording_id":"ray-porter-2021"}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	result, err := client.Lookup(context.Background(), "B08G9PRS1K", "")
	if err != nil {
		t.Fatalf("Lookup: unexpected error: %v", err)
	}
	if result.Work.ID != "project-hail-mary" {
		t.Errorf("work id = %q, want project-hail-mary", result.Work.ID)
	}
	if result.RecordingID != "ray-porter-2021" {
		t.Errorf("recording id = %q, want ray-porter-2021", result.RecordingID)
	}
	if result.Work.ID == result.RecordingID {
		t.Errorf("work id and recording id must never be conflated, got both = %q", result.Work.ID)
	}
}

// --- E2: redirect handling ------------------------------------------------

func TestAudioSiloClient_WorkDetail_FollowsSameOriginRelativeRedirect(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/works/retired-slug":
			w.Header().Set("Location", "/api/v1/works/canonical-slug")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMovedPermanently)
			_, _ = w.Write([]byte(`{"redirect":"canonical-slug"}`))
		case "/api/v1/works/canonical-slug":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"canonical-slug","title":"Canonical","authors":[],"language":"en","series":[],"recordings":null}`))
		default:
			http.NotFound(w, r)
		}
	})
	client, _ := newAudioSiloClient(t, server.URL)

	detail, err := client.WorkDetail(context.Background(), "retired-slug")
	if err != nil {
		t.Fatalf("WorkDetail: unexpected error following redirect: %v", err)
	}
	if detail.ID != "canonical-slug" {
		t.Errorf("id = %q, want canonical-slug (must follow the redirect and return the canonical work)", detail.ID)
	}
	if rec.count() != 2 {
		t.Errorf("request count = %d, want 2 (one 301, one follow-up)", rec.count())
	}
}

func TestAudioSiloClient_WorkDetail_FollowsRedirectWithEscapedCanonicalID(t *testing.T) {
	canonical := "café-édition-2"
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/api/v1/works/retired-slug" {
			w.Header().Set("Location", "/api/v1/works/"+url.PathEscape(canonical))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMovedPermanently)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"redirect":%q}`, canonical)))
			return
		}
		if r.URL.EscapedPath() == "/api/v1/works/"+url.PathEscape(canonical) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":%q,"title":"T","authors":[],"language":"en","series":[],"recordings":null}`, canonical)))
			return
		}
		http.NotFound(w, r)
	})
	client, _ := newAudioSiloClient(t, server.URL)

	detail, err := client.WorkDetail(context.Background(), "retired-slug")
	if err != nil {
		t.Fatalf("WorkDetail: unexpected error: %v", err)
	}
	if detail.ID != canonical {
		t.Errorf("id = %q, want %q", detail.ID, canonical)
	}
	if rec.count() != 2 {
		t.Errorf("request count = %d, want 2", rec.count())
	}
}

func TestAudioSiloClient_WorkDetail_RedirectFailureCases(t *testing.T) {
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, evilCount *int)
	}{
		{
			name: "missing Location header",
			handler: func(w http.ResponseWriter, r *http.Request, evilCount *int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMovedPermanently)
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "empty Location header",
			handler: func(w http.ResponseWriter, r *http.Request, evilCount *int) {
				w.Header().Set("Location", "")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMovedPermanently)
				_, _ = w.Write([]byte(`{"redirect":""}`))
			},
		},
		{
			name: "invalid Location value",
			handler: func(w http.ResponseWriter, r *http.Request, evilCount *int) {
				w.Header().Set("Location", "://not a valid url")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMovedPermanently)
				_, _ = w.Write([]byte(`{}`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var evilCount int
			server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				tc.handler(w, r, &evilCount)
			})
			client, _ := newAudioSiloClient(t, server.URL)

			_, err := client.WorkDetail(context.Background(), "retired-slug")
			if err == nil {
				t.Fatalf("WorkDetail: got nil error, want failure for %s", tc.name)
			}
		})
	}
}

func TestAudioSiloClient_WorkDetail_RedirectLoopFailsAndBoundsRequests(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		var next string
		if r.URL.Path == "/api/v1/works/a" {
			next = "/api/v1/works/b"
		} else {
			next = "/api/v1/works/a"
		}
		w.Header().Set("Location", next)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{"redirect":"x"}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	_, err := client.WorkDetail(context.Background(), "a")
	if err == nil {
		t.Fatal("WorkDetail: got nil error for an infinite redirect loop, want failure")
	}
	if rec.count() > 10 {
		t.Errorf("request count = %d, want a small bounded number of follows (no infinite loop)", rec.count())
	}
}

func TestAudioSiloClient_WorkDetail_CrossOriginRedirectFails(t *testing.T) {
	// Uses followingHTTPClient (a plain *http.Client with no CheckRedirect
	// override) rather than the trackedClient test fake: a fake that blocks
	// redirects at the transport layer would make this pass even if
	// production called the injected client's Do directly and let a normal
	// client follow the cross-origin 301 itself.
	evil, evilRec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"evil","title":"Evil","authors":[],"language":"en","series":[],"recordings":null}`))
	})
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", evil.URL+"/api/v1/works/evil")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{"redirect":"evil"}`))
	})
	client := newAudioSiloClientWithHTTPClient(t, server.URL, followingHTTPClient())

	_, err := client.WorkDetail(context.Background(), "retired-slug")
	if err == nil {
		t.Fatal("WorkDetail: got nil error for a cross-origin redirect, want failure")
	}
	if evilRec.count() != 0 {
		t.Errorf("evil server request count = %d, want 0 (cross-origin redirect must never be followed)", evilRec.count())
	}
	if rec.count() != 1 {
		t.Errorf("configured server request count = %d, want exactly 1 (the redirect must be rejected on the first response, not followed until some later failure)", rec.count())
	}
}

// --- E3: error classification --------------------------------------------

func TestAudioSiloClient_ErrorClassification(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		retryAfter     string
		wantNotFound   bool
		wantRetryable  bool
		wantRetryAfter time.Duration
		wantHasRetry   bool
	}{
		{"404 is not-found", http.StatusNotFound, "", true, false, 0, false},
		{"408 is retryable", http.StatusRequestTimeout, "", false, true, 0, false},
		{"429 is retryable with Retry-After", http.StatusTooManyRequests, "17", false, true, 17 * time.Second, true},
		{"500 is retryable", http.StatusInternalServerError, "", false, true, 0, false},
		{"503 is retryable", http.StatusServiceUnavailable, "", false, true, 0, false},
		{"400 is terminal, neither classification", http.StatusBadRequest, "", false, false, 0, false},
		{"403 is terminal, neither classification", http.StatusForbidden, "", false, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			_, err := client.SearchWorks(context.Background(), "x", 10)
			if err == nil {
				t.Fatalf("SearchWorks: got nil error for status %d, want an error", tc.status)
			}
			if got := IsNotFound(err); got != tc.wantNotFound {
				t.Errorf("IsNotFound = %v, want %v", got, tc.wantNotFound)
			}
			if got := IsRetryable(err); got != tc.wantRetryable {
				t.Errorf("IsRetryable = %v, want %v", got, tc.wantRetryable)
			}
			var pe *ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("errors.As(err, *ProviderError) = false for status %d, want true: a generic error must not pass this classification", tc.status)
			}
			if pe.Status != tc.status {
				t.Errorf("ProviderError.Status = %d, want %d", pe.Status, tc.status)
			}
			if tc.wantHasRetry {
				if !pe.HasRetryAfter || pe.RetryAfter != tc.wantRetryAfter {
					t.Errorf("RetryAfter = %v (has=%v), want %v", pe.RetryAfter, pe.HasRetryAfter, tc.wantRetryAfter)
				}
			} else if pe.HasRetryAfter {
				t.Errorf("HasRetryAfter = true, want false: no Retry-After header was sent")
			}
		})
	}
}

func TestAudioSiloClient_ErrorDoesNotLeakResponseBody(t *testing.T) {
	secret := "token-shaped-text-Bearer-sk-abcdef123456"
	marker := "unique-marker-xyzzy-plugh-19283746"
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, secret+" "+marker)))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	_, err := client.SearchWorks(context.Background(), "x", 10)
	if err == nil {
		t.Fatal("SearchWorks: got nil error, want an error for status 500")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks response body text: %v", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error message leaks unrelated response body text: %v", err)
	}
}

func TestAudioSiloClient_NeverSendsAuthorizationHeader(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	if _, err := client.SearchWorks(context.Background(), "x", 10); err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	if auth := rec.last().Header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization header = %q, want empty: the Audiobookshelf token must never reach AudioSilo", auth)
	}
}

// --- E1: validation before network I/O ------------------------------------

func TestAudioSiloClient_NewRejectsInvalidBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{"empty", ""},
		{"no scheme", "meta.audiosilo.app"},
		{"unsupported scheme", "ftp://meta.audiosilo.app"},
		{"javascript scheme", "javascript:alert(1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAudioSiloClient(tc.baseURL, &http.Client{}); err == nil {
				t.Errorf("NewAudioSiloClient(%q): got nil error, want a validation error", tc.baseURL)
			}
		})
	}
}

func TestAudioSiloClient_SearchWorks_EmptyQueryFailsBeforeNetwork(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	_, err := client.SearchWorks(context.Background(), "", 10)
	if err == nil {
		t.Fatal("SearchWorks(\"\", ...): got nil error, want validation error")
	}
	if rec.count() != 0 {
		t.Errorf("request count = %d, want 0 (must fail before network I/O)", rec.count())
	}
}

func TestAudioSiloClient_WorkDetail_EmptyIDFailsBeforeNetwork(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","title":"T","authors":[],"language":"en","series":[],"recordings":null}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	_, err := client.WorkDetail(context.Background(), "")
	if err == nil {
		t.Fatal("WorkDetail(\"\"): got nil error, want validation error")
	}
	if rec.count() != 0 {
		t.Errorf("request count = %d, want 0 (must fail before network I/O)", rec.count())
	}
}

// --- E4: malformed / partial data ------------------------------------------

func TestAudioSiloClient_SearchWorks_MalformedJSONFails(t *testing.T) {
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":`)) // truncated
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "x", 10)
	if err == nil {
		t.Fatal("SearchWorks: got nil error for malformed JSON, want an error")
	}
	if hits != nil {
		t.Errorf("hits = %+v, want nil on decode failure (no partial success)", hits)
	}
}

func TestAudioSiloClient_SearchWorks_MissingRequiredFieldsFails(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing id", `{"results":[{"title":"No ID","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]}]}`},
		{"missing title", `{"results":[{"id":"no-title","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]}]}`},
		{"missing authors key entirely", `{"results":[{"id":"no-authors","title":"No Authors","series":null,"cover_url":null,"added_at":null,"narrators":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			hits, err := client.SearchWorks(context.Background(), "x", 10)
			if err == nil {
				t.Fatalf("SearchWorks: got nil error for %s, want an error", tc.name)
			}
			if hits != nil {
				t.Errorf("hits = %+v, want nil (no partial success)", hits)
			}
		})
	}
}

func TestAudioSiloClient_SearchWorks_ToleratesUnknownFields(t *testing.T) {
	body := `{
		"meta": {"took_ms": 3, "unexpected_future_field": {"nested": true}},
		"results": [{
			"id": "project-hail-mary",
			"title": "Project Hail Mary",
			"authors": [{"id":"andy-weir","name":"Andy Weir","unexpected_person_field":"x"}],
			"series": null,
			"cover_url": null,
			"added_at": null,
			"narrators": [],
			"unexpected_work_field": "should be ignored",
			"score": 12.5
		}]
	}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("SearchWorks: unexpected error decoding a response with unknown top-level and nested fields: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "project-hail-mary" || hits[0].Title != "Project Hail Mary" {
		t.Fatalf("hits = %+v, want one hit id=project-hail-mary title=\"Project Hail Mary\" (unknown fields must not break decoding)", hits)
	}
}

func TestAudioSiloClient_WorkDetail_InvalidRecordingShapeFails(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"missing recording id",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"narrators":[],"asin":[],"isbn":[],"chapter_count":0}]}`,
		},
		{
			"missing narrators array entirely",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"id":"r1","asin":[],"isbn":[],"chapter_count":0}]}`,
		},
		{
			"missing asin array entirely",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"id":"r1","narrators":[],"isbn":[],"chapter_count":0}]}`,
		},
		{
			"missing isbn array entirely",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"id":"r1","narrators":[],"asin":[],"chapter_count":0}]}`,
		},
		{
			"missing chapter_count entirely",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"id":"r1","narrators":[],"asin":[],"isbn":[]}]}`,
		},
		{
			"chapter_count wrong type",
			`{"id":"w","title":"T","authors":[],"language":"en","series":[],"recordings":[{"id":"r1","narrators":[],"asin":[],"isbn":[],"chapter_count":"eighty-eight"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})
			client, _ := newAudioSiloClient(t, server.URL)

			_, err := client.WorkDetail(context.Background(), "w")
			if err == nil {
				t.Fatalf("WorkDetail: got nil error for %s, want an error", tc.name)
			}
		})
	}
}

func TestAudioSiloClient_OversizedBodyFails(t *testing.T) {
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"id":"x","title":"`))
		padding := strings.Repeat("a", MaxAudioSiloResponseBytes+(1<<20))
		w.Write([]byte(padding))
		w.Write([]byte(`","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]}]}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	hits, err := client.SearchWorks(context.Background(), "x", 10)
	if err == nil {
		t.Fatal("SearchWorks: got nil error for an oversized body, want an error")
	}
	if hits != nil {
		t.Errorf("hits = %+v, want nil on an oversized body", hits)
	}
}

// --- I1: cancellation, deadlines, injected client --------------------------

func TestAudioSiloClient_UsesInjectedHTTPClient(t *testing.T) {
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client, tracker := newAudioSiloClient(t, server.URL)

	if _, err := client.SearchWorks(context.Background(), "x", 10); err != nil {
		t.Fatalf("SearchWorks: unexpected error: %v", err)
	}
	if tracker.requestCount() != 1 {
		t.Errorf("tracker observed %d requests, want 1: the client must route requests through the *http.Client it was given", tracker.requestCount())
	}
	if rec.count() != 1 {
		t.Errorf("server observed %d requests, want 1", rec.count())
	}
}

func TestAudioSiloClient_HonorsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	t.Cleanup(func() { close(release) })
	client, _ := newAudioSiloClient(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SearchWorks(ctx, "x", 10)
		done <- err
	}()
	awaitSignal(t, started, "waiting for the server to observe the in-flight request")
	cancel()

	err := awaitSignal(t, done, "waiting for SearchWorks to return after cancellation")
	if err == nil {
		t.Fatal("SearchWorks: got nil error after context cancellation, want a context-compatible error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (cancellation must not trigger an internal retry)", rec.count())
	}
}

func TestAudioSiloClient_HonorsContextDeadline(t *testing.T) {
	// The deadline is already in the past at call time, so there is no
	// wall-clock race between the deadline firing and the handler
	// responding: SearchWorks must observe ctx.Err() != nil deterministically,
	// on every run, regardless of scheduling.
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	_, err := client.SearchWorks(ctx, "x", 10)
	if err == nil {
		t.Fatal("SearchWorks: got nil error past the deadline, want a context-compatible error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if rec.count() != 0 {
		t.Errorf("request count = %d, want 0: a request must not be sent once the deadline has already passed", rec.count())
	}
}

func TestAudioSiloClient_CancellationDuringBodyReadDoesNotRetry(t *testing.T) {
	step := make(chan struct{})
	flushed := make(chan struct{}, 1)
	server, rec := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sw := newStepWriter(w, step)
		// Write and flush the first chunk, THEN signal - so `flushed` can
		// only fire once bytes are actually on the wire for the client to
		// be reading, and cancellation is guaranteed to land mid-body-read
		// rather than possibly racing request setup.
		sw.write(`{"results":[{"id":"x","title":"T","authors":[],"series":null,"cover_url":null,"added_at":null,"narrators":[]}`)
		select {
		case flushed <- struct{}{}:
		default:
		}
		<-step
		// Handler blocks here until the test closes `step`; the body is
		// never completed, simulating cancellation mid-read.
	})
	t.Cleanup(func() { close(step) })
	client, _ := newAudioSiloClient(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SearchWorks(ctx, "x", 10)
		done <- err
	}()

	// Wait for the handler to actually flush its first chunk before
	// cancelling, so the cancellation lands mid-body-read rather than racing
	// request setup. This is a channel handshake, not a sleep.
	awaitSignal(t, flushed, "waiting for the server to flush the first body chunk")
	cancel()
	err := awaitSignal(t, done, "waiting for SearchWorks to return after cancellation during body read")
	if err == nil {
		t.Fatal("SearchWorks: got nil error after cancellation during body read, want a context-compatible error")
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (no internal retry after cancellation)", rec.count())
	}
}

// --- I4: decoded slices do not alias, bodies always close ------------------

func TestAudioSiloClient_ReturnedSlicesDoNotAliasAcrossCalls(t *testing.T) {
	body := `{"results":[{"id":"work-a","title":"A","authors":[{"id":"p","name":"P"}],"series":null,"cover_url":null,"added_at":null,"narrators":[]}]}`
	server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudioSiloClient(t, server.URL)

	first, err := client.SearchWorks(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("SearchWorks (1st): unexpected error: %v", err)
	}
	if len(first) != 1 || len(first[0].Authors) != 1 {
		t.Fatalf("first call hits = %+v, want exactly one hit with one author", first)
	}
	first[0].Title = "MUTATED"
	first[0].Authors[0].Name = "MUTATED"

	second, err := client.SearchWorks(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("SearchWorks (2nd): unexpected error: %v", err)
	}
	if len(second) != 1 || len(second[0].Authors) != 1 {
		t.Fatalf("second call hits = %+v, want exactly one hit with one author", second)
	}
	if second[0].Title != "A" {
		t.Errorf("second call Title = %q after mutating the first result, want unaffected %q (buffers must not alias)", second[0].Title, "A")
	}
	if second[0].Authors[0].Name != "P" {
		t.Errorf("second call Author.Name = %q after mutating the first result, want unaffected %q", second[0].Authors[0].Name, "P")
	}
}

func TestAudioSiloClient_ClosesBodyOnEveryStatusPath(t *testing.T) {
	statuses := []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError, http.StatusTooManyRequests}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, _ := newAudioSiloServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"results":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"boom"}`))
				}
			})
			client, tracker := newAudioSiloClient(t, server.URL)

			_, _ = client.SearchWorks(context.Background(), "x", 10)
			if tracker.closeCount() != tracker.requestCount() {
				t.Errorf("close count = %d, request count = %d, want every response body closed", tracker.closeCount(), tracker.requestCount())
			}
		})
	}
}
