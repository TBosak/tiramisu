package audiobookjob

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/library"
	"tiramisu/internal/prowlarr"
)

func TestLibraryClientAddAppendsCanonicalHashSuffix(t *testing.T) {
	var got library.AddRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"already_present":false}`))
	}))
	defer server.Close()

	client, err := newLibraryClient(server.URL)
	if err != nil {
		t.Fatalf("newLibraryClient: %v", err)
	}
	_, err = client.Add(context.Background(), audiobookimport.LibraryPublishRequest{
		Type:  audiobookimport.LibraryTypeAudiobook,
		Title: "The Martian",
		Hash:  "079a2887bb3ac78680d1c3766773f6cbf4169d05",
		Files: []audiobookimport.LibraryFileRequest{{
			SourcePath: "The Martian/The Martian.mp3",
			Path:       "Andy Weir/The Martian/1 - The Martian.mp3",
			ExternalIdentity: audiobookimport.ExternalIdentity{
				Namespace: audiobookimport.IdentityNamespaceAudioSiloRecording,
				ID:        "r-c-bray-2020",
			},
		}},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	want := "Andy Weir/The Martian/1 - The Martian_f4169d05.mp3"
	if len(got.Files) != 1 || got.Files[0].Path != want {
		t.Fatalf("Library path = %#v, want %q", got.Files, want)
	}
}

func TestValidateConfigRejectsUnusableSelectionConfidence(t *testing.T) {
	cfg := Config{
		AudioSiloURL: "https://meta.example", AudioSiloToken: "public",
		AudiobookshelfURL: "https://abs.example", AudiobookshelfToken: "secret",
		AudiobookshelfLibraryID: "library", LibraryURL: "http://127.0.0.1:9080",
		StatePath:     filepath.Join(t.TempDir(), "state.json"),
		RemovalPolicy: audiobookimport.RemovalPolicy{ConsecutiveMissingThreshold: 3, Grace: time.Hour},
		ProwlarrCfg:   prowlarr.ConfigProwlarr{Enabled: true, URL: "http://127.0.0.1:9696", APIKey: "secret"},
		PaceSeconds:   1,
		Limits: audiobookimport.DiscoveryLimits{
			MaxCandidates: 1, MaxImports: 1, MaxLatestExamined: 1, MaxSeriesExamined: 1,
			SearchLimit: 1, LatestLimit: 1,
			Selection: audiobookimport.SelectionLimits{
				MaxQueries: 1, MaxResultsPerQuery: 1, MaxCandidatesInspected: 1,
				MaxSourceBytes: 1 << 20, MaxReleaseSizeBytes: 1 << 30,
				MinConfidence: 7,
			},
		},
	}
	if err := validateConfig(context.Background(), cfg); err == nil {
		t.Fatal("validateConfig accepted MinConfidence above the selector's maximum")
	}
}
