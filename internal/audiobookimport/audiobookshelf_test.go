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

const absTestToken = "abs-secret-token-3f9c7a"

func newAudiobookshelfServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *requestRecorder) {
	t.Helper()
	rec := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, rec
}

func newAudiobookshelfClient(t *testing.T, baseURL, token string) (*AudiobookshelfClient, *bodyCloseTracker) {
	t.Helper()
	httpClient, tracker := trackedClient()
	client, err := NewAudiobookshelfClient(baseURL, token, httpClient)
	if err != nil {
		t.Fatalf("NewAudiobookshelfClient(%q): unexpected error: %v", baseURL, err)
	}
	return client, tracker
}

// newAudiobookshelfClientWithHTTPClient builds a client around whatever
// *http.Client the caller supplies, unmodified - used by redirect-safety
// tests that must prove the Audiobookshelf client itself refuses to follow a
// redirect (and never exposes the Bearer token to another origin), rather
// than relying on a test fake's CheckRedirect to block it before the
// client's own logic ever runs.
func newAudiobookshelfClientWithHTTPClient(t *testing.T, baseURL, token string, httpClient *http.Client) *AudiobookshelfClient {
	t.Helper()
	client, err := NewAudiobookshelfClient(baseURL, token, httpClient)
	if err != nil {
		t.Fatalf("NewAudiobookshelfClient(%q): unexpected error: %v", baseURL, err)
	}
	return client
}

func absItemJSON(id, title, author, series, asin, isbn string) string {
	return fmt.Sprintf(`{"id":%q,"media":{"metadata":{"title":%q,"authorName":%q,"seriesName":%q,"asin":%q,"isbn":%q}}}`,
		id, title, author, series, asin, isbn)
}

// --- A4: inventory pagination ----------------------------------------------

func TestAudiobookshelfClient_Inventory_RequestShape(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	if _, err := client.Inventory(context.Background(), "lib-1", 10, 5); err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("request count = %d, want 1", rec.count())
	}
	req := rec.last()
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.Path != "/api/libraries/lib-1/items" {
		t.Errorf("path = %q, want /api/libraries/lib-1/items", req.Path)
	}
	if got := req.Query.Get("limit"); got != "10" {
		t.Errorf("limit = %q, want 10", got)
	}
	if got := req.Query.Get("page"); got != "0" {
		t.Errorf("page = %q, want 0 for the first page", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+absTestToken {
		t.Errorf("Authorization = %q, want Bearer %s", got, absTestToken)
	}
}

func TestAudiobookshelfClient_Inventory_BasePathPrefixPreserved(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL+"/proxy/abs", absTestToken)

	if _, err := client.Inventory(context.Background(), "lib-1", 10, 5); err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	req := rec.last()
	if req.Path != "/proxy/abs/api/libraries/lib-1/items" {
		t.Errorf("path = %q, want /proxy/abs/api/libraries/lib-1/items", req.Path)
	}
}

func TestAudiobookshelfClient_Inventory_EscapesReservedAndUnicodeLibraryID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"reserved characters", "lib/with?reserved&chars"},
		{"unicode", "bibliothèque-1"},
		{"space", "my library"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"results":[],"total":0}`))
			})
			client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

			if _, err := client.Inventory(context.Background(), tc.id, 10, 5); err != nil {
				t.Fatalf("Inventory(%q): unexpected error: %v", tc.id, err)
			}
			req := rec.last()
			wantEscaped := "/api/libraries/" + url.PathEscape(tc.id) + "/items"
			if req.EscapedPath != wantEscaped {
				t.Errorf("escaped path = %q, want %q (library id must be escaped, not concatenated raw)", req.EscapedPath, wantEscaped)
			}
		})
	}
}

func TestAudiobookshelfClient_Inventory_EmptyLibrary(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want empty", items)
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want 1 (must not keep paging past a declared total of 0)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_NullResultsIsEmpty(t *testing.T) {
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":null,"total":0}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error for null results: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want empty for null results", items)
	}
}

func TestAudiobookshelfClient_Inventory_SinglePageShortOfLimit(t *testing.T) {
	body := `{"results":[` +
		absItemJSON("i1", "Book One", "Author A", "Series A", "B000001111", "9781111111111") + `,` +
		absItemJSON("i2", "Book Two", "Author B", "", "", "") +
		`],"total":2}`
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].ID != "i1" || items[0].Title != "Book One" || items[0].AuthorName != "Author A" ||
		items[0].SeriesName != "Series A" || items[0].ASIN != "B000001111" || items[0].ISBN != "9781111111111" {
		t.Errorf("items[0] = %+v, want mapped book metadata for dedup", items[0])
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want 1 (a short page ends pagination)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_StopsAtTrustworthyDeclaredTotalWithoutExtraRequest(t *testing.T) {
	full := `{"results":[` + absItemJSON("i1", "A", "", "", "", "") + `,` + absItemJSON("i2", "B", "", "", "", "") + `],"total":2}`
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(full))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 2, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (a full page that exactly reaches a declared total must stop immediately, not fetch a confirmatory empty page)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_FullPageWithIndeterminateTotalFetchesNextPage(t *testing.T) {
	// "total" is entirely absent, so a full page is genuinely ambiguous:
	// the client cannot tell whether more items remain without asking.
	full := `{"results":[` + absItemJSON("i1", "A", "", "", "", "") + `,` + absItemJSON("i2", "B", "", "", "", "") + `]}`
	empty := `{"results":[]}`
	var calls int
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls == 0 {
			calls++
			_, _ = w.Write([]byte(full))
			return
		}
		_, _ = w.Write([]byte(empty))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 2, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if rec.count() != 2 {
		t.Errorf("request count = %d, want 2 (a full page with no declared total is ambiguous; must fetch the next page to confirm end)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_MultiPage(t *testing.T) {
	pages := []string{
		`{"results":[` + absItemJSON("i1", "A", "", "", "", "") + `,` + absItemJSON("i2", "B", "", "", "", "") + `],"total":5}`,
		`{"results":[` + absItemJSON("i3", "C", "", "", "", "") + `,` + absItemJSON("i4", "D", "", "", "", "") + `],"total":5}`,
		`{"results":[` + absItemJSON("i5", "E", "", "", "", "") + `],"total":5}`,
	}
	var idx int
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if idx >= len(pages) {
			_, _ = w.Write([]byte(`{"results":[],"total":5}`))
			return
		}
		_, _ = w.Write([]byte(pages[idx]))
		idx++
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 2, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("len(items) = %d, want 5", len(items))
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	want := []string{"i1", "i2", "i3", "i4", "i5"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v in page order", ids, want)
		}
	}
	if rec.count() != 3 {
		t.Errorf("request count = %d, want 3 (stop at the short final page)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_AcceptsLegacyLibraryItemsKey(t *testing.T) {
	body := `{"libraryItems":[` + absItemJSON("legacy-1", "Legacy Book", "Author L", "", "", "") + `],"total":1}`
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err != nil {
		t.Fatalf("Inventory: unexpected error for legacy libraryItems key: %v", err)
	}
	if len(items) != 1 || items[0].ID != "legacy-1" {
		t.Errorf("items = %+v, want one item with id legacy-1", items)
	}
}

func TestAudiobookshelfClient_Inventory_LyingTotalHitsBoundAndFails(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		id := "i-" + page
		_, _ = w.Write([]byte(`{"results":[` + absItemJSON(id, "T", "", "", "", "") + `,` + absItemJSON(id+"b", "T2", "", "", "", "") + `],"total":1000000}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 2, 3)
	if err == nil {
		t.Fatal("Inventory: got nil error for a lying total that never reaches a short page, want a bound failure")
	}
	if items != nil {
		t.Errorf("items = %+v, want nil (no partial success when the bound is hit)", items)
	}
	if rec.count() != 3 {
		t.Errorf("request count = %d, want exactly 3 (the configured maxPages bound)", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_RepeatedPageHitsBoundAndFails(t *testing.T) {
	body := `{"results":[` + absItemJSON("stuck-1", "T", "", "", "", "") + `,` + absItemJSON("stuck-2", "T2", "", "", "", "") + `],"total":1000000}`
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	_, err := client.Inventory(context.Background(), "lib-1", 2, 3)
	if err == nil {
		t.Fatal("Inventory: got nil error for pagination stuck on a repeated page, want a bound failure")
	}
	if rec.count() != 3 {
		t.Errorf("request count = %d, want exactly 3 (bounded, never loops indefinitely)", rec.count())
	}
}

// --- A5: scan ---------------------------------------------------------------

func TestAudiobookshelfClient_Scan_RequestShapeAndAccepts2xx(t *testing.T) {
	statuses := []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			})
			client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

			result, err := client.Scan(context.Background(), "lib-1")
			if err != nil {
				t.Fatalf("Scan: unexpected error for status %d: %v", status, err)
			}
			if result.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", result.StatusCode, status)
			}
			if rec.count() != 1 {
				t.Fatalf("request count = %d, want 1 (no polling)", rec.count())
			}
			req := rec.last()
			if req.Method != http.MethodPost {
				t.Errorf("method = %q, want POST", req.Method)
			}
			if req.Path != "/api/libraries/lib-1/scan" {
				t.Errorf("path = %q, want /api/libraries/lib-1/scan", req.Path)
			}
			if got := req.Query.Get("force"); got != "1" {
				t.Errorf("force = %q, want 1", got)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer "+absTestToken {
				t.Errorf("Authorization = %q, want Bearer %s", got, absTestToken)
			}
		})
	}
}

func TestAudiobookshelfClient_Scan_DoesNotPollOrMutate(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s %s: scan must not issue follow-up requests", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	if _, err := client.Scan(context.Background(), "lib-1"); err != nil {
		t.Fatalf("Scan: unexpected error: %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (scan reports status without polling)", rec.count())
	}
}

// --- E1: validation before network I/O --------------------------------------

func TestAudiobookshelfClient_NewRejectsInvalidBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{"empty", ""},
		{"no scheme", "abs.example.invalid"},
		{"unsupported scheme", "ftp://abs.example.invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAudiobookshelfClient(tc.baseURL, absTestToken, &http.Client{}); err == nil {
				t.Errorf("NewAudiobookshelfClient(%q): got nil error, want a validation error", tc.baseURL)
			}
		})
	}
}

func TestAudiobookshelfClient_Inventory_ValidationFailsBeforeNetwork(t *testing.T) {
	cases := []struct {
		name      string
		libraryID string
		pageSize  int
		maxPages  int
	}{
		{"empty library id", "", 10, 5},
		{"zero page size", "lib-1", 0, 5},
		{"negative page size", "lib-1", -1, 5},
		{"zero max pages", "lib-1", 10, 0},
		{"negative max pages", "lib-1", 10, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"results":[],"total":0}`))
			})
			client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

			_, err := client.Inventory(context.Background(), tc.libraryID, tc.pageSize, tc.maxPages)
			if err == nil {
				t.Fatalf("Inventory(%q, %d, %d): got nil error, want validation error", tc.libraryID, tc.pageSize, tc.maxPages)
			}
			if rec.count() != 0 {
				t.Errorf("request count = %d, want 0 (must fail before network I/O)", rec.count())
			}
		})
	}
}

func TestAudiobookshelfClient_Scan_EmptyLibraryIDFailsBeforeNetwork(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	_, err := client.Scan(context.Background(), "")
	if err == nil {
		t.Fatal("Scan(\"\"): got nil error, want validation error")
	}
	if rec.count() != 0 {
		t.Errorf("request count = %d, want 0 (must fail before network I/O)", rec.count())
	}
}

// --- E2: redirects fail (token must never move origins) ---------------------

func TestAudiobookshelfClient_Inventory_RedirectFails(t *testing.T) {
	// Uses a genuine default *http.Client (followingHTTPClient, no
	// CheckRedirect override) rather than the trackedClient test fake: a
	// fake that blocks redirects at the transport layer would make this
	// pass even if the production client did nothing to prevent a redirect
	// itself. Both a same-origin and a cross-origin redirect must fail,
	// since ABS authorization must never silently move to a different path
	// or host.
	cases := []struct {
		name        string
		locationFor func(serverURL, evilURL string) string
	}{
		{"same-origin redirect", func(serverURL, evilURL string) string {
			return serverURL + "/api/libraries/lib-1/items?limit=10&page=1"
		}},
		{"cross-origin redirect", func(serverURL, evilURL string) string { return evilURL + "/api/libraries/lib-1/items" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evil, evilRec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"results":[],"total":0}`))
			})
			var server *httptest.Server
			server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", tc.locationFor(server.URL, evil.URL))
				w.WriteHeader(http.StatusFound)
			})
			client := newAudiobookshelfClientWithHTTPClient(t, server.URL, absTestToken, followingHTTPClient())

			_, err := client.Inventory(context.Background(), "lib-1", 10, 5)
			if err == nil {
				t.Fatal("Inventory: got nil error for a redirect response, want failure")
			}
			if evilRec.count() != 0 {
				t.Errorf("evil server request count = %d, want 0 (authorization must never move origins)", evilRec.count())
			}
			if rec.count() != 1 {
				t.Errorf("configured server request count = %d, want exactly 1 (the redirect must be rejected on the first response, not followed - even same-origin, even repeatedly - until some later failure)", rec.count())
			}
		})
	}
}

func TestAudiobookshelfClient_Scan_RedirectFails(t *testing.T) {
	cases := []struct {
		name        string
		locationFor func(serverURL, evilURL string) string
	}{
		{"same-origin redirect", func(serverURL, evilURL string) string { return serverURL + "/api/libraries/lib-1/scan?force=1" }},
		{"cross-origin redirect", func(serverURL, evilURL string) string { return evilURL + "/api/libraries/lib-1/scan" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evil, evilRec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			var server *httptest.Server
			server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", tc.locationFor(server.URL, evil.URL))
				w.WriteHeader(http.StatusTemporaryRedirect)
			})
			client := newAudiobookshelfClientWithHTTPClient(t, server.URL, absTestToken, followingHTTPClient())

			_, err := client.Scan(context.Background(), "lib-1")
			if err == nil {
				t.Fatal("Scan: got nil error for a redirect response, want failure")
			}
			if evilRec.count() != 0 {
				t.Errorf("evil server request count = %d, want 0 (authorization must never move origins)", evilRec.count())
			}
			if rec.count() != 1 {
				t.Errorf("configured server request count = %d, want exactly 1 (the redirect must be rejected on the first response, not followed - even same-origin, even repeatedly - until some later failure)", rec.count())
			}
		})
	}
}

// --- E3: error classification + I3: token never leaks -----------------------

func TestAudiobookshelfClient_ErrorClassification(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		retryAfter     string
		wantNotFound   bool
		wantRetryable  bool
		wantHasRetry   bool
		wantRetryAfter time.Duration
	}{
		{"401 is terminal", http.StatusUnauthorized, "", false, false, false, 0},
		{"404 is not-found", http.StatusNotFound, "", true, false, false, 0},
		{"408 is retryable", http.StatusRequestTimeout, "", false, true, false, 0},
		{"429 is retryable with Retry-After", http.StatusTooManyRequests, "22", false, true, true, 22 * time.Second},
		{"500 is retryable", http.StatusInternalServerError, "", false, true, false, 0},
		{"503 is retryable", http.StatusServiceUnavailable, "", false, true, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"denied"}`))
			})
			client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

			_, err := client.Inventory(context.Background(), "lib-1", 10, 5)
			if err == nil {
				t.Fatalf("Inventory: got nil error for status %d, want an error", tc.status)
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

func TestAudiobookshelfClient_ErrorNeverLeaksTokenOrBody(t *testing.T) {
	marker := "unique-marker-xyzzy-plugh-19283746"
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving server that echoes the caller's own token, plus
		// unrelated body text, in its error body - the client must not
		// surface any of it regardless.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"invalid token %s %s"}`, absTestToken, marker)))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	_, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err == nil {
		t.Fatal("Inventory: got nil error for 401, want an error")
	}
	if strings.Contains(err.Error(), absTestToken) {
		t.Errorf("error leaks the configured token: %v", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error leaks unrelated response body text: %v", err)
	}
}

func TestAudiobookshelfClient_TokenSentOnlyToConfiguredOrigin(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	if _, err := client.Inventory(context.Background(), "lib-1", 10, 5); err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if got := rec.last().Header.Get("Authorization"); got != "Bearer "+absTestToken {
		t.Errorf("Authorization = %q, want Bearer %s on the configured origin", got, absTestToken)
	}
}

// --- E4: malformed / oversized / bounded pagination --------------------------

func TestAudiobookshelfClient_Inventory_MalformedJSONFails(t *testing.T) {
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err == nil {
		t.Fatal("Inventory: got nil error for malformed JSON, want an error")
	}
	if items != nil {
		t.Errorf("items = %+v, want nil (no partial success)", items)
	}
}

func TestAudiobookshelfClient_Inventory_MissingRequiredIDFails(t *testing.T) {
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"media":{"metadata":{"title":"No ID"}}}],"total":1}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err == nil {
		t.Fatal("Inventory: got nil error for an item missing its required id, want an error")
	}
	if items != nil {
		t.Errorf("items = %+v, want nil (no partial success)", items)
	}
}

func TestAudiobookshelfClient_OversizedBodyFails(t *testing.T) {
	server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"id":"x","media":{"metadata":{"title":"`))
		padding := strings.Repeat("a", MaxAudiobookshelfResponseBytes+(1<<20))
		w.Write([]byte(padding))
		w.Write([]byte(`"}}}],"total":1}`))
	})
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	items, err := client.Inventory(context.Background(), "lib-1", 10, 5)
	if err == nil {
		t.Fatal("Inventory: got nil error for an oversized body, want an error")
	}
	if items != nil {
		t.Errorf("items = %+v, want nil on an oversized body", items)
	}
}

// --- I1: cancellation + injected client ---------------------------------------

func TestAudiobookshelfClient_UsesInjectedHTTPClient(t *testing.T) {
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	client, tracker := newAudiobookshelfClient(t, server.URL, absTestToken)

	if _, err := client.Inventory(context.Background(), "lib-1", 10, 5); err != nil {
		t.Fatalf("Inventory: unexpected error: %v", err)
	}
	if tracker.requestCount() != 1 {
		t.Errorf("tracker observed %d requests, want 1", tracker.requestCount())
	}
	if rec.count() != 1 {
		t.Errorf("server observed %d requests, want 1", rec.count())
	}
}

func TestAudiobookshelfClient_Inventory_HonorsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0}`))
	})
	t.Cleanup(func() { close(release) })
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Inventory(ctx, "lib-1", 10, 5)
		done <- err
	}()
	awaitSignal(t, started, "waiting for the server to observe the in-flight request")
	cancel()

	err := awaitSignal(t, done, "waiting for Inventory to return after cancellation")
	if err == nil {
		t.Fatal("Inventory: got nil error after context cancellation, want a context-compatible error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (cancellation must not trigger an internal retry)", rec.count())
	}
}

func TestAudiobookshelfClient_Scan_HonorsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server, rec := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(func() { close(release) })
	client, _ := newAudiobookshelfClient(t, server.URL, absTestToken)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Scan(ctx, "lib-1")
		done <- err
	}()
	awaitSignal(t, started, "waiting for the server to observe the in-flight request")
	cancel()

	err := awaitSignal(t, done, "waiting for Scan to return after cancellation")
	if err == nil {
		t.Fatal("Scan: got nil error after context cancellation, want a context-compatible error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if rec.count() != 1 {
		t.Errorf("request count = %d, want exactly 1 (no internal retry)", rec.count())
	}
}

// --- I4: bodies always close --------------------------------------------------

func TestAudiobookshelfClient_ClosesBodyOnEveryStatusPath(t *testing.T) {
	statuses := []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError, http.StatusTooManyRequests}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, _ := newAudiobookshelfServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"results":[],"total":0}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"boom"}`))
				}
			})
			client, tracker := newAudiobookshelfClient(t, server.URL, absTestToken)

			_, _ = client.Inventory(context.Background(), "lib-1", 10, 5)
			if tracker.closeCount() != tracker.requestCount() {
				t.Errorf("close count = %d, request count = %d, want every response body closed", tracker.closeCount(), tracker.requestCount())
			}
		})
	}
}
