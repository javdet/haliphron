package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Retention for failed runs, kept as data so an operator can change it from
// the UI. The semantics are at the top of db/migrations/0012_run_retention.sql.
// The default it falls back to before the first save is the app layer's.

// RunRetention is the row.
type RunRetention struct {
	// Failed is how long a Failed run is kept after it finished; zero keeps
	// it for ever.
	Failed    time.Duration
	UpdatedBy string
	UpdatedAt time.Time
}

// RunRetention reads the stored retention, or ErrNotFound when none was ever
// saved.
func (s *Store) RunRetention(ctx context.Context) (RunRetention, error) {
	var (
		r       RunRetention
		seconds int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT failed_seconds, updated_by, updated_at FROM run_retention`).
		Scan(&seconds, &r.UpdatedBy, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRetention{}, ErrNotFound
	}
	if err != nil {
		return RunRetention{}, fmt.Errorf("store: read run retention: %w", err)
	}
	r.Failed = time.Duration(seconds) * time.Second
	return r, nil
}

// PutRunRetention saves the retention and records the change in the same
// transaction: the next pass deletes by the new number, and what it deleted
// has to be traceable to who chose it.
func (s *Store) PutRunRetention(ctx context.Context, failed time.Duration, by string) (RunRetention, error) {
	seconds := int64(failed / time.Second)
	out := RunRetention{Failed: time.Duration(seconds) * time.Second}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO run_retention (failed_seconds, updated_by, updated_at)
			VALUES ($1, $2, clock_timestamp())
			ON CONFLICT (tenant_id) DO UPDATE SET
			       failed_seconds = EXCLUDED.failed_seconds, updated_by = EXCLUDED.updated_by,
			       updated_at = EXCLUDED.updated_at
			RETURNING updated_by, updated_at`, seconds, by).
			Scan(&out.UpdatedBy, &out.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store: save run retention: %w", err)
		}
		return appendAudit(ctx, tx, AuditEntry{
			Actor: by, ActorKind: "user", Action: AuditRunRetentionSaved,
			SubjectKind: "run_retention", SubjectID: "failed",
			Payload: map[string]any{"failedSeconds": seconds},
		})
	})
	if err != nil {
		return RunRetention{}, err
	}
	return out, nil
}
