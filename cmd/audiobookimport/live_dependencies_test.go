package main

// Requirement coverage (see .tdd-state/audiobook-scheduler/brief.md):
//
//   A8/E1: LiveDependencies is the standalone command's bridge to the same
//     live dependency graph the native scheduler uses. Whatever the service
//     config path names, it must fail before any dependency construction
//     when that config cannot be read or parsed - the same "validate before
//     any client/state call" contract audiobookjob.BuildLiveDeps enforces,
//     just reached through a config file path instead of an in-memory
//     Config value.
//
// LiveDependencies is currently a declaration-only stub (added by the lead
// as this slice's seam) that always returns a fixed error regardless of its
// argument. The negative-path tests below therefore currently pass
// vacuously (any input yields an error) and are kept as locked regression
// tests for that boundary; TestLiveDependencies_ValidServiceConfig_
// ConstructsWorkingDependencies is this file's actual RED anchor for A8: it
// supplies a config.json whose "audiobooks"/"prowlarr" sections are a
// mechanical, tag-for-tag JSON encoding of the config.AudiobookConfig and
// prowlarr.ConfigProwlarr structs the lead declared for this slice (no
// json tags on audiobookimport.DiscoveryLimits/RemovalPolicy means Go's
// default encoding uses the exported Go field names verbatim - confirmed by
// reading internal/audiobookimport/source.go and publication.go), and
// expects a nil error with a working Dependencies.Build. If the real
// adapter needs a field this file could not infer from the declared types
// (for example, deriving the Library HTTP API's URL from a service port
// rather than from the audiobooks section), that is called out as a
// residual gap in this slice's manifest rather than guessed at here.
//
// This file intentionally imports only what cmd/audiobookimport/main.go
// itself already imports (tiramisu/internal/audiobookimport plus stdlib):
// internal/config and internal/audiobookjob both transitively import
// internal/prowlarr, which pulls in a package with a pre-existing (and
// unrelated) Windows build-tag bug - see this slice's manifest. Once
// LiveDependencies is implemented for real it will presumably import
// internal/audiobookjob itself, and cmd/audiobookimport will inherit that
// same baseline limitation; that is an environment fact of this feature
// needing Prowlarr, not something a test-only change can route around.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// E1: a config path that does not exist must fail before any dependency
// construction; Dependencies.Build must remain unusable (nil).
func TestLiveDependencies_MissingConfigPath_FailsBeforeAnyIO(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		StatePath:  filepath.Join(dir, "state.json"),
		LibraryID:  "lib-1",
		ConfigPath: filepath.Join(dir, "does-not-exist.json"),
	}

	deps, err := LiveDependencies(cfg)
	if err == nil {
		t.Fatal("LiveDependencies(missing config path) = nil error, want non-nil")
	}
	if deps.Build != nil {
		t.Fatal("LiveDependencies(missing config path) returned a usable Build func, want nil")
	}
}

// E1: malformed JSON at the config path must fail the same way.
func TestLiveDependencies_MalformedConfigJSON_Fails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeFile(t, configPath, `{ this is not valid JSON `)

	cfg := Config{StatePath: filepath.Join(dir, "state.json"), LibraryID: "lib-1", ConfigPath: configPath}
	deps, err := LiveDependencies(cfg)
	if err == nil {
		t.Fatal("LiveDependencies(malformed config JSON) = nil error, want non-nil")
	}
	if deps.Build != nil {
		t.Fatal("LiveDependencies(malformed config JSON) returned a usable Build func, want nil")
	}
}

// E1/malformed-input: a config path that is a directory (not a file) must
// fail the same way as a missing path, not panic or hang.
func TestLiveDependencies_ConfigPathIsDirectory_Fails(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StatePath: filepath.Join(dir, "state.json"), LibraryID: "lib-1", ConfigPath: dir}

	deps, err := LiveDependencies(cfg)
	if err == nil {
		t.Fatal("LiveDependencies(directory as config path) = nil error, want non-nil")
	}
	if deps.Build != nil {
		t.Fatal("LiveDependencies(directory as config path) returned a usable Build func, want nil")
	}
}

// canary is an httptest server that records how many requests it received,
// so a test can prove construction never touches the network.
type canary struct {
	server *httptest.Server
	hits   int32
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	c := &canary{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.hits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(c.server.Close)
	return c
}

// serviceConfigJSON renders a service config.json whose "audiobooks" and
// "prowlarr" sections are a mechanical field-for-field JSON encoding of the
// config.AudiobookConfig / prowlarr.ConfigProwlarr structs this slice's seam
// declared (see internal/config/config.go's diff; no json tags on
// audiobookimport.DiscoveryLimits/RemovalPolicy means Go's default encoding
// uses the exported Go field names verbatim). maxCandidates/maxImports are
// deliberately parameterized so a test can plant "decoy" values here and
// prove the CLI's own -max-candidates/-max-imports flags win instead.
func serviceConfigJSON(t *testing.T, audioSiloURL, audiobookshelfURL, libraryID, statePath, prowlarrURL string, maxCandidates, maxImports int) string {
	t.Helper()
	doc := map[string]any{
		"metrics_port": 9080,
		"prowlarr": map[string]any{
			"enabled": true,
			"api_key": "prowlarr-key-should-not-leak",
			"url":     prowlarrURL,
		},
		"audiobooks": map[string]any{
			"enabled":                   true,
			"audiosilo_url":             audioSiloURL,
			"audiosilo_token":           "audiosilo-token-should-not-leak",
			"audiobookshelf_url":        audiobookshelfURL,
			"audiobookshelf_token":      "audiobookshelf-token-should-not-leak",
			"audiobookshelf_library_id": libraryID,
			"state_path":                statePath,
			"pace_seconds":              10,
			"removal_policy": map[string]any{
				"ConsecutiveMissingThreshold": 3,
				"Grace":                       int64(24 * 3600 * 1e9),
			},
			"limits": map[string]any{
				"MaxCandidates":     maxCandidates,
				"MaxImports":        maxImports,
				"MaxLatestExamined": 20,
				"MaxSeriesExamined": 20,
				"SearchLimit":       5,
				"LatestLimit":       5,
				"Selection": map[string]any{
					"MaxQueries":             2,
					"MaxResultsPerQuery":     5,
					"MaxCandidatesInspected": 5,
					"MaxSourceBytes":         1 << 20,
					"MinSeeders":             0,
					"MaxReleaseSizeBytes":    1 << 30,
					"MinConfidence":          0,
					"Categories":             []int{100},
					"IndexerIDs":             []int{5},
				},
			},
			"categories":  []int{100},
			"indexer_ids": []int{5},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture service config: %v", err)
	}
	return string(b)
}

// A8: given a config path whose audiobooks/prowlarr sections are complete
// and valid, LiveDependencies must return a Dependencies whose Build itself
// succeeds and produces a real graph - not merely a non-nil no-op func (the
// exact counterexample review-1.md calls out). Construction must not touch
// the network (proven with canaries, exactly as
// internal/audiobookjob/builddeps_test.go proves for BuildLiveDeps
// directly), and the resulting RunnerDeps.Limits must reflect the service
// config's limits section. This is this file's primary RED anchor for A8:
// it currently fails because LiveDependencies is an unconditional stub.
func TestLiveDependencies_ValidServiceConfig_BuildProducesRealWorkingGraph(t *testing.T) {
	audioSilo, audiobookshelf, prowlarr := newCanary(t), newCanary(t), newCanary(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "audiobooks_state.json")
	writeFile(t, configPath, serviceConfigJSON(t, audioSilo.server.URL, audiobookshelf.server.URL, "lib-1", statePath, prowlarr.server.URL, 10, 5))

	cliCfg := Config{StatePath: statePath, LibraryID: "lib-1", MaxCandidates: 10, MaxImports: 5, ConfigPath: configPath}
	deps, err := LiveDependencies(cliCfg)
	if err != nil {
		t.Fatalf("LiveDependencies(valid config) = %v, want nil error", err)
	}
	if deps.Build == nil {
		t.Fatal("LiveDependencies(valid config) returned a nil Build func")
	}

	runnerDeps, err := deps.Build(context.Background(), cliCfg)
	if audioSilo.hits != 0 || audiobookshelf.hits != 0 || prowlarr.hits != 0 {
		t.Fatalf("Dependencies.Build made a network call during construction (audiosilo=%d audiobookshelf=%d prowlarr=%d)", audioSilo.hits, audiobookshelf.hits, prowlarr.hits)
	}
	if err != nil {
		t.Fatalf("Dependencies.Build(valid config) = %v, want nil error", err)
	}
	if runnerDeps.Provider == nil || runnerDeps.Inventory == nil || runnerDeps.Identities == nil ||
		runnerDeps.Selector == nil || runnerDeps.Publisher == nil || runnerDeps.Pacer == nil {
		t.Fatalf("Dependencies.Build(valid config) returned an incomplete RunnerDeps graph: %+v", runnerDeps)
	}
	if runnerDeps.Limits.MaxCandidates != 10 || runnerDeps.Limits.MaxImports != 5 {
		t.Fatalf("Dependencies.Build(valid config) Limits = %+v, want MaxCandidates=10 MaxImports=5 from the service config", runnerDeps.Limits)
	}
}

// A8: the CLI's own -max-candidates/-max-imports flags (cmd/audiobookimport's
// existing, already-implemented ParseConfig contract) must override whatever
// the service config.json happens to say, the same way -library-id already
// documents itself as "Audiobookshelf library id" and must therefore take
// precedence over the service config's audiobookshelf_library_id. Without
// this, a manual CLI apply and the native scheduled apply could silently
// diverge on limits/identity even when pointed at the same config file.
func TestLiveDependencies_CLIOverrides_WinOverServiceConfigLimits(t *testing.T) {
	audioSilo, audiobookshelf, prowlarr := newCanary(t), newCanary(t), newCanary(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "audiobooks_state.json")
	// The service config's own limits are decoys (999/999): the CLI flags
	// below must win.
	writeFile(t, configPath, serviceConfigJSON(t, audioSilo.server.URL, audiobookshelf.server.URL, "lib-1", statePath, prowlarr.server.URL, 999, 999))

	cliCfg := Config{StatePath: statePath, LibraryID: "lib-1", MaxCandidates: 7, MaxImports: 3, ConfigPath: configPath}
	deps, err := LiveDependencies(cliCfg)
	if err != nil {
		t.Fatalf("LiveDependencies: %v", err)
	}
	runnerDeps, err := deps.Build(context.Background(), cliCfg)
	if err != nil {
		t.Fatalf("Dependencies.Build: %v", err)
	}
	if runnerDeps.Limits.MaxCandidates != 7 || runnerDeps.Limits.MaxImports != 3 {
		t.Fatalf("Dependencies.Build Limits = %+v, want the CLI's MaxCandidates=7 MaxImports=3 to override the service config's 999/999", runnerDeps.Limits)
	}
}

// A8: the CLI's -state and -library-id flags must be the ones actually
// used for durable state and Audiobookshelf library scoping, not the
// service config's own audiobooks.state_path/audiobookshelf_library_id -
// otherwise a manual CLI apply pointed at a different state file than the
// scheduler's could silently read/write the wrong state. Proven the same
// way internal/audiobookjob/liveadapters_test.go proves BuildLiveDeps reads
// its configured StatePath: a malformed file at the CLI's own -state path
// must break a state-backed call even though the service config's
// state_path (a decoy, pointed elsewhere and left absent) would not.
func TestLiveDependencies_CLIStatePathOverride_IsActuallyUsed(t *testing.T) {
	audioSilo, audiobookshelf, prowlarr := newCanary(t), newCanary(t), newCanary(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	decoyStatePath := filepath.Join(dir, "decoy_state.json") // left absent
	cliStatePath := filepath.Join(dir, "cli_state.json")
	if err := os.WriteFile(cliStatePath, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatalf("seed malformed CLI state file: %v", err)
	}
	writeFile(t, configPath, serviceConfigJSON(t, audioSilo.server.URL, audiobookshelf.server.URL, "lib-1", decoyStatePath, prowlarr.server.URL, 10, 5))

	cliCfg := Config{StatePath: cliStatePath, LibraryID: "lib-1", MaxCandidates: 10, MaxImports: 5, ConfigPath: configPath}
	deps, err := LiveDependencies(cliCfg)
	if err != nil {
		t.Fatalf("LiveDependencies: %v", err)
	}
	runnerDeps, err := deps.Build(context.Background(), cliCfg)
	if err != nil {
		t.Fatalf("Dependencies.Build(valid config) = %v, want nil error (construction itself should not fail)", err)
	}
	if _, err := runnerDeps.Identities.CommittedIdentities(context.Background()); err == nil {
		t.Fatal("Identities.CommittedIdentities succeeded despite a malformed state file at the CLI's -state path; the CLI override is not actually being used")
	}
}
