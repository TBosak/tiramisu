package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/audiobookjob"
	serviceconfig "tiramisu/internal/config"
)

type Config struct {
	ExplicitWorkIDs []string
	ExplicitQueries []string
	Apply           bool
	MaxCandidates   int
	MaxImports      int
	StatePath       string
	ConfigPath      string
	LibraryID       string
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

func ParseConfig(args []string) (Config, error) {
	var cfg Config
	fs := flag.NewFlagSet("audiobookimport", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var workIDs, queries stringList
	var dryRun bool
	fs.Var(&workIDs, "work-id", "AudioSilo work id (repeatable)")
	fs.Var(&queries, "query", "AudioSilo work query (repeatable)")
	fs.BoolVar(&dryRun, "dry-run", false, "plan without external mutation")
	fs.BoolVar(&cfg.Apply, "apply", false, "publish changes")
	fs.IntVar(&cfg.MaxCandidates, "max-candidates", 20, "maximum candidates")
	fs.IntVar(&cfg.MaxImports, "max-imports", 5, "maximum successful imports")
	fs.StringVar(&cfg.StatePath, "state", "", "durable state path")
	fs.StringVar(&cfg.ConfigPath, "config", "", "service config path")
	fs.StringVar(&cfg.LibraryID, "library-id", "", "Audiobookshelf library id")
	if err := fs.Parse(args); err != nil {
		return Config{}, errors.New("invalid audiobookimport flags")
	}
	if dryRun && cfg.Apply {
		return Config{}, errors.New("dry-run and apply are mutually exclusive")
	}
	cfg.ExplicitWorkIDs, cfg.ExplicitQueries = workIDs, queries
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type Dependencies struct {
	Build func(context.Context, Config) (audiobookimport.RunnerDeps, error)
}

// LiveDependencies is the standalone command's declaration-only bridge to
// the native audiobook dependency factory. The behavioral implementation is
// supplied with the scheduler wiring slice.
func LiveDependencies(cli Config) (Dependencies, error) {
	data, err := os.ReadFile(cli.ConfigPath)
	if err != nil {
		return Dependencies{}, errors.New("unable to read service configuration")
	}
	var svc serviceconfig.Config
	if err := json.Unmarshal(data, &svc); err != nil {
		return Dependencies{}, errors.New("unable to parse service configuration")
	}
	build := func(ctx context.Context, current Config) (audiobookimport.RunnerDeps, error) {
		limits := svc.Audiobooks.Limits
		limits.MaxCandidates = current.MaxCandidates
		limits.MaxImports = current.MaxImports
		cfg := audiobookjob.Config{
			AudioSiloURL: svc.Audiobooks.AudioSiloURL, AudioSiloToken: svc.Audiobooks.AudioSiloToken,
			AudiobookshelfURL: svc.Audiobooks.AudiobookshelfURL, AudiobookshelfToken: svc.Audiobooks.AudiobookshelfToken,
			AudiobookshelfLibraryID: current.LibraryID, LibraryURL: fmt.Sprintf("http://127.0.0.1:%d", svc.MetricsPort),
			StatePath: current.StatePath, RemovalPolicy: svc.Audiobooks.RemovalPolicy,
			ProwlarrCfg: svc.Prowlarr, Categories: svc.Audiobooks.Categories, IndexerIDs: svc.Audiobooks.IndexerIDs,
			Limits: limits, PaceSeconds: svc.Audiobooks.PaceSeconds,
		}
		return audiobookjob.BuildLiveDeps(ctx, cfg)
	}
	return Dependencies{Build: build}, nil
}

func RunCLI(ctx context.Context, cfg Config, deps Dependencies, stdout, stderr io.Writer) int {
	if err := validateConfig(cfg); err != nil || ctx.Err() != nil || deps.Build == nil || stdout == nil || stderr == nil {
		writeSafe(stderr, "invalid audiobook import configuration")
		return 2
	}
	runnerDeps, err := deps.Build(ctx, cfg)
	if err != nil {
		writeSafe(stderr, "unable to construct audiobook import dependencies")
		return 1
	}
	result, runErr := audiobookimport.Run(ctx, runnerDeps, audiobookimport.RunRequest{
		ExplicitWorkIDs: append([]string(nil), cfg.ExplicitWorkIDs...),
		ExplicitQueries: append([]string(nil), cfg.ExplicitQueries...),
		DryRun:          !cfg.Apply,
	})
	failed := false
	for _, row := range result.Rows {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", row.WorkID, row.Status, row.Reason)
		if row.Status == audiobookimport.StatusFailed {
			failed = true
		}
	}
	if runErr != nil || failed {
		writeSafe(stderr, "audiobook import run failed")
		return 1
	}
	return 0
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.StatePath) == "" || strings.TrimSpace(cfg.LibraryID) == "" {
		return errors.New("state path and library id are required")
	}
	if len(cfg.ExplicitWorkIDs) == 0 && len(cfg.ExplicitQueries) == 0 {
		return errors.New("at least one work id or query is required")
	}
	for _, value := range append(append([]string(nil), cfg.ExplicitWorkIDs...), cfg.ExplicitQueries...) {
		if strings.TrimSpace(value) == "" {
			return errors.New("blank explicit input")
		}
	}
	if cfg.MaxCandidates <= 0 || cfg.MaxCandidates > 100000 || cfg.MaxImports <= 0 || cfg.MaxImports > 100000 {
		return errors.New("invalid limits")
	}
	return nil
}

func writeSafe(w io.Writer, message string) {
	if w != nil {
		fmt.Fprintln(w, message)
	}
}

func main() {
	cfg, err := ParseConfig(os.Args[1:])
	if err != nil {
		writeSafe(os.Stderr, err.Error())
		os.Exit(2)
	}
	deps, err := LiveDependencies(cfg)
	if err != nil {
		writeSafe(os.Stderr, "unable to construct audiobook import dependencies")
		os.Exit(1)
	}
	code := RunCLI(context.Background(), cfg, deps, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}
