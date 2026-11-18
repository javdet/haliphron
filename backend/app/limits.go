package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// A run's installation-wide limits, set from the UI's Settings page and nowhere
// else — not the environment, not Helm. Today there is one: how many
// repositories a run may name, the working one included.
//
// Read from the store on every admission rather than cached. The row is one
// primary-key read, and a cache would make a limit saved on one replica a limit
// the others apply some time later.

// Where the limits in force come from.
const (
	RunLimitsDefault  = "default"
	RunLimitsDatabase = "database"
)

// RunLimits is what is in force.
type RunLimits struct {
	MaxReposPerRun int
	// Source says whether MaxReposPerRun was saved or is the default.
	Source    string
	UpdatedBy string
	UpdatedAt *time.Time
}

// RunLimits reads the limits in force.
func (s *Service) RunLimits(ctx context.Context) (RunLimits, error) {
	out := RunLimits{MaxReposPerRun: run.DefaultMaxRepos, Source: RunLimitsDefault}
	stored, err := s.store.RunLimits(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return RunLimits{}, err
	}
	out.MaxReposPerRun, out.Source = stored.MaxReposPerRun, RunLimitsDatabase
	out.UpdatedBy, out.UpdatedAt = stored.UpdatedBy, &stored.UpdatedAt
	return out, nil
}

// PutRunLimits saves the limits. A lower repository limit does not touch runs
// already admitted: their spec was frozen when they were.
func (s *Service) PutRunLimits(ctx context.Context, maxRepos int, by string) (RunLimits, error) {
	if maxRepos < 1 || maxRepos > run.MaxReposCeiling {
		return RunLimits{}, &run.InvalidRequestError{Field: "max_repos_per_run",
			Detail: fmt.Sprintf("must be between 1 and %d", run.MaxReposCeiling)}
	}
	if _, err := s.store.PutRunLimits(ctx, store.RunLimits{MaxReposPerRun: maxRepos}, by); err != nil {
		return RunLimits{}, err
	}
	return s.RunLimits(ctx)
}

// admissionDefaults is the installation's defaults with the limits saved in the
// UI applied, read for this one admission.
func (s *Service) admissionDefaults(ctx context.Context) (run.Defaults, error) {
	def := s.defaults
	limits, err := s.RunLimits(ctx)
	if err != nil {
		return run.Defaults{}, err
	}
	def.MaxRepos = limits.MaxReposPerRun
	return def, nil
}
