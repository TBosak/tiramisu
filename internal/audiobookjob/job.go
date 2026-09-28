package audiobookjob

import (
	"context"
	"errors"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/prowlarr"
)

// Config is the declaration boundary shared by the native scheduler and the
// standalone audiobook command. Behavioral validation and construction are
// supplied by this package's implementation slice.
type Config struct {
	AudioSiloURL            string
	AudioSiloToken          string
	AudiobookshelfURL       string
	AudiobookshelfToken     string
	AudiobookshelfLibraryID string
	LibraryURL              string
	StatePath               string
	RemovalPolicy           audiobookimport.RemovalPolicy
	ProwlarrCfg             prowlarr.ConfigProwlarr
	Categories              []int
	IndexerIDs              []int
	Limits                  audiobookimport.DiscoveryLimits
	PaceSeconds             int
}

// Syncer declares the scheduler adapter for the audiobook controller.
type Syncer struct {
	Cfg Config
	// Build defaults to BuildLiveDeps when nil.
	Build func(context.Context, Config) (audiobookimport.RunnerDeps, error)
	// RunFunc defaults to audiobookimport.Run when nil.
	RunFunc func(context.Context, audiobookimport.RunnerDeps, audiobookimport.RunRequest) (audiobookimport.RunResult, error)
}

func (s *Syncer) Name() string { return "audiobooks" }

func (s *Syncer) Run(ctx context.Context) error {
	build := s.Build
	if build == nil {
		build = BuildLiveDeps
	}
	run := s.RunFunc
	if run == nil {
		run = audiobookimport.Run
	}
	deps, err := build(ctx, s.Cfg)
	if err != nil {
		return errors.New("audiobook dependency construction failed")
	}
	_, err = run(ctx, deps, audiobookimport.RunRequest{DryRun: false})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("audiobook synchronization failed")
	}
	return nil
}
