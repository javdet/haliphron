package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Deferred runs: a run request waiting for locks to go, for a time, or both.
//
// What a deferred run may be is app's. This file stores the row and owns the
// one statement that makes firing safe across replicas: the fence in
// linkDeferredFire, which runs inside InsertRun's transaction and nowhere
// else. A row becomes 'submitted' there or not at all.

// Deferred run states.
const (
	DeferredWaiting   = "waiting"
	DeferredSubmitted = "submitted"
	DeferredExpired   = "expired"
	DeferredCancelled = "cancelled"
)

// DeferredRun is a row, with which of its waited locks are still live.
type DeferredRun struct {
	ID          runv1.ULID
	ParentRunID runv1.ULID
	// Template is the run request, in the public API's snake_case shape.
	Template     json.RawMessage
	WaitForLocks []runv1.ULID
	// LiveLocks is the subset of WaitForLocks that is still held, computed on
	// read. The row is due when it is empty and NotBefore has passed.
	LiveLocks  []runv1.ULID
	NotBefore  *time.Time
	GiveUpAt   time.Time
	State      string
	RunID      runv1.ULID
	Reason     string
	CreatedBy  string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// ErrDeferredSettled is a fire or a cancel that found the row no longer
// waiting: another replica fired it, it expired, or it was cancelled.
var ErrDeferredSettled = errors.New("store: the deferred run is no longer waiting")

const deferredColumns = `
	SELECT d.id, d.parent_run_id, d.template, d.wait_for_locks::text[],
	       ARRAY(SELECT l.id FROM resource_locks l
	             WHERE l.id = ANY(d.wait_for_locks) AND l.expires_at > now()
	             ORDER BY l.seq)::text[],
	       d.not_before, d.give_up_at, d.state, d.run_id, d.reason,
	       d.created_by, d.created_at, d.finished_at
	FROM deferred_runs d`

// InsertDeferred writes a waiting row.
//
// It is counted against the parent's fan-out the way a child is, in the same
// transaction as the insert and under the parent's row lock, for the reason
// InsertRun counts children that way: an agent issuing several calls at once
// must not have every one of them read the same count. Children already
// started and rows still waiting both count; InsertRun checks again at fire
// time against the children alone.
func (s *Store) InsertDeferred(ctx context.Context, d DeferredRun, maxChildren int32) (DeferredRun, error) {
	var out DeferredRun
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if maxChildren > 0 {
			if _, err := tx.ExecContext(ctx,
				`SELECT id FROM runs WHERE id = $1 FOR UPDATE`, d.ParentRunID); err != nil {
				return fmt.Errorf("store: lock parent %s: %w", d.ParentRunID, err)
			}
			var used int32
			if err := tx.QueryRowContext(ctx, `
				SELECT (SELECT count(*) FROM runs WHERE parent_run_id = $1)
				     + (SELECT count(*) FROM deferred_runs WHERE parent_run_id = $1 AND state = 'waiting')`,
				d.ParentRunID).Scan(&used); err != nil {
				return fmt.Errorf("store: count children of %s: %w", d.ParentRunID, err)
			}
			if used >= maxChildren {
				return ErrTooManyChildren
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO deferred_runs (id, parent_run_id, template, wait_for_locks,
			                           not_before, give_up_at, created_by)
			VALUES ($1, $2, $3::jsonb, $4::text[]::ulid[], $5, $6, $7)`,
			d.ID, d.ParentRunID, []byte(d.Template), ulidArray(d.WaitForLocks),
			nullTime(d.NotBefore), d.GiveUpAt, d.CreatedBy); err != nil {
			return fmt.Errorf("store: insert deferred run %s: %w", d.ID, err)
		}
		var err error
		out, err = scanDeferred(tx.QueryRowContext(ctx, deferredColumns+` WHERE d.id = $1`, d.ID))
		return err
	})
	return out, err
}

// DeferredByID reads one row.
func (s *Store) DeferredByID(ctx context.Context, id runv1.ULID) (DeferredRun, error) {
	d, err := scanDeferred(s.db.QueryRowContext(ctx, deferredColumns+` WHERE d.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return DeferredRun{}, ErrNotFound
	}
	return d, err
}

// DeferredFilter is the list as the API asks for it.
type DeferredFilter struct {
	State  string
	Parent runv1.ULID
	// RunID finds the row a run was fired from.
	RunID runv1.ULID
	Limit int
}

// ListDeferred returns rows, newest first.
func (s *Store) ListDeferred(ctx context.Context, f DeferredFilter) ([]DeferredRun, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.State != "" {
		add("d.state = $%d", f.State)
	}
	if f.Parent != "" {
		add("d.parent_run_id = $%d", f.Parent)
	}
	if f.RunID != "" {
		add("d.run_id = $%d", f.RunID)
	}
	query := deferredColumns
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY d.created_at DESC, d.id DESC LIMIT $%d", len(args))
	return s.queryDeferred(ctx, query, args...)
}

// DueDeferred returns waiting rows that may fire now: not_before passed or
// unset, give_up_at not yet passed, and none of the waited locks live. Oldest
// first. The clock is the database's, like the expiry of the locks it reads.
//
// It takes no lock: the fence in InsertRun decides which replica fires a row.
func (s *Store) DueDeferred(ctx context.Context, limit int) ([]DeferredRun, error) {
	return s.queryDeferred(ctx, deferredColumns+`
		WHERE d.state = 'waiting'
		  AND (d.not_before IS NULL OR d.not_before <= now())
		  AND d.give_up_at > now()
		  AND NOT EXISTS (SELECT 1 FROM resource_locks l
		                  WHERE l.id = ANY(d.wait_for_locks) AND l.expires_at > now())
		ORDER BY d.created_at, d.id
		LIMIT $1`, limit)
}

// CancelDeferred ends a waiting row. It answers ErrNotFound for no row and
// ErrDeferredSettled for one that is no longer waiting.
func (s *Store) CancelDeferred(ctx context.Context, id runv1.ULID, reason string) (DeferredRun, error) {
	return s.endDeferred(ctx, id, DeferredCancelled, reason)
}

// ExpireDeferredRun ends a waiting row that cannot become a run — admission
// refused it at fire time — with the reason, where the operator looks.
func (s *Store) ExpireDeferredRun(ctx context.Context, id runv1.ULID, reason string) (DeferredRun, error) {
	return s.endDeferred(ctx, id, DeferredExpired, reason)
}

func (s *Store) endDeferred(ctx context.Context, id runv1.ULID, state, reason string) (DeferredRun, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE deferred_runs SET state = $2, reason = $3, finished_at = now()
		WHERE id = $1 AND state = 'waiting'`, id, state, truncate(reason, 1024))
	if err != nil {
		return DeferredRun{}, fmt.Errorf("store: end deferred run %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.DeferredByID(ctx, id); err != nil {
			return DeferredRun{}, err
		}
		return DeferredRun{}, ErrDeferredSettled
	}
	return s.DeferredByID(ctx, id)
}

// ExpireDeferred ends every waiting row whose give_up_at has passed, and
// returns them.
func (s *Store) ExpireDeferred(ctx context.Context) ([]DeferredRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE deferred_runs SET state = 'expired', finished_at = now(),
		       reason = 'gave up waiting: give_up_at passed before the run could start'
		WHERE state = 'waiting' AND give_up_at <= now()
		RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("store: expire deferred runs: %w", err)
	}
	ids, err := scanIDs(rows)
	rows.Close()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := make([]DeferredRun, 0, len(ids))
	for _, id := range ids {
		d, err := s.DeferredByID(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, nil
}

// linkDeferredFire is the fence, inside the run's own insert and after it:
// run_id has a foreign key to the run. Zero rows means another replica fired
// the row first, or it was cancelled or expired meanwhile, and the run is
// rolled back. Two concurrent fires serialise on the row lock this UPDATE
// takes; the second re-reads state after the first commits and changes
// nothing.
func linkDeferredFire(ctx context.Context, tx *sql.Tx, id, runID runv1.ULID) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE deferred_runs SET state = 'submitted', run_id = $2, finished_at = now()
		WHERE id = $1 AND state = 'waiting'`, id, runID)
	if err != nil {
		return fmt.Errorf("store: fire deferred run %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrDeferredSettled
	}
	return nil
}

func (s *Store) queryDeferred(ctx context.Context, query string, args ...any) ([]DeferredRun, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list deferred runs: %w", err)
	}
	defer rows.Close()

	out := []DeferredRun{}
	for rows.Next() {
		d, err := scanDeferred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDeferred(row rowScanner) (DeferredRun, error) {
	var (
		d         DeferredRun
		waits     []string
		live      []string
		notBefore sql.NullTime
		runID     sql.NullString
		finished  sql.NullTime
	)
	err := row.Scan(&d.ID, &d.ParentRunID, &d.Template, pgArray(&waits), pgArray(&live),
		&notBefore, &d.GiveUpAt, &d.State, &runID, &d.Reason,
		&d.CreatedBy, &d.CreatedAt, &finished)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeferredRun{}, err
		}
		return DeferredRun{}, fmt.Errorf("store: scan deferred run: %w", err)
	}
	d.WaitForLocks = toULIDs(waits)
	d.LiveLocks = toULIDs(live)
	d.NotBefore = timePtr(notBefore)
	d.RunID = runv1.ULID(runID.String)
	d.FinishedAt = timePtr(finished)
	return d, nil
}

func ulidArray(ids []runv1.ULID) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return textArray(out)
}

func toULIDs(in []string) []runv1.ULID {
	out := make([]runv1.ULID, 0, len(in))
	for _, s := range in {
		out = append(out, runv1.ULID(s))
	}
	return out
}
