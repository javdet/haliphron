package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A run's installation-wide limits, kept as data so an operator changes them
// from the UI. The semantics are at the top of db/migrations/0023_run_limits.sql.
// The defaults it falls back to before the first save are the run package's.

// RunLimits is the row.
type RunLimits struct {
	// MaxReposPerRun counts the working repository too.
	MaxReposPerRun int
	UpdatedBy      string
	UpdatedAt      time.Time
}

// RunLimits reads the stored limits, or ErrNotFound when none were ever saved.
//
// Read on every admission rather than cached: it is one row by primary key, and
// a value saved on one replica then applies to the next request on all of them.
func (s *Store) RunLimits(ctx context.Context) (RunLimits, error) {
	var r RunLimits
	err := s.db.QueryRowContext(ctx,
		`SELECT max_repos_per_run, updated_by, updated_at FROM run_limits`).
		Scan(&r.MaxReposPerRun, &r.UpdatedBy, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunLimits{}, ErrNotFound
	}
	if err != nil {
		return RunLimits{}, fmt.Errorf("store: read run limits: %w", err)
	}
	return r, nil
}

// PutRunLimits saves the limits and records the change in the same
// transaction, as PutTokenRetention does. The table's CHECK is the last word
// on the range; the app layer refuses out-of-range values before they get here.
func (s *Store) PutRunLimits(ctx context.Context, limits RunLimits, by string) (RunLimits, error) {
	out := RunLimits{MaxReposPerRun: limits.MaxReposPerRun}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO run_limits (max_repos_per_run, updated_by, updated_at)
			VALUES ($1, $2, clock_timestamp())
			ON CONFLICT (tenant_id) DO UPDATE SET
			       max_repos_per_run = EXCLUDED.max_repos_per_run, updated_by = EXCLUDED.updated_by,
			       updated_at = EXCLUDED.updated_at
			RETURNING updated_by, updated_at`, limits.MaxReposPerRun, by).
			Scan(&out.UpdatedBy, &out.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store: save run limits: %w", err)
		}
		return appendAudit(ctx, tx, AuditEntry{
			Actor: by, ActorKind: "user", Action: AuditRunLimitsSaved,
			SubjectKind: "run_limits", SubjectID: "max_repos_per_run",
			Payload: map[string]any{"maxReposPerRun": limits.MaxReposPerRun},
		})
	})
	if err != nil {
		return RunLimits{}, err
	}
	return out, nil
}
