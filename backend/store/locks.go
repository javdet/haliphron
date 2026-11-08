package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Resource locks: a registry of "this run is working on X", with
// housekeeping. What a lock means and what conflicts with what is the agents'
// judgement; this file never compares two locks.
//
// It owns the two rules that make the registry usable at all. Every lock
// write takes one transaction-level advisory lock, so a lock's seq order is
// its commit order; and SetLock returns, from inside that transaction, every
// live lock of another run with a lower seq. Together they make "for any two
// locks, the later one's answer names the earlier one" true, which is what
// lets two agents that want the same thing agree which of them is second.
//
// The other rules live in the schema: the trigger that deletes a run's locks
// when its status becomes terminal, and the expiry every read filters on.

// lockWriteKey is the advisory lock every lock write takes. Chosen once and
// written down, like the migration lock in db: a key derived from anything is
// a key two builds can fail to share.
const lockWriteKey int64 = 4729105830174629183

// MaxLocksPerRun bounds the live locks one run may hold. An agent talked into
// squatting on every name it can think of is stopped here.
const MaxLocksPerRun = 8

// ErrTooManyLocks is a set_lock over MaxLocksPerRun.
var ErrTooManyLocks = errors.New("store: the run holds as many locks as it may")

// Lock is a live lock with its holder, as every read returns it. Nothing of
// the holder's prompt, result or credentials is here: these rows are read by
// other agents, and a lock list must not be a way to read another run.
type Lock struct {
	ID          runv1.ULID
	Seq         int64
	RunID       runv1.ULID
	LeaseEpoch  int64
	Name        string
	Description string
	AcquiredAt  time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time

	HolderStatus    string
	HolderRole      string
	HolderCreatedAt time.Time
	HolderStartedAt *time.Time
	// HolderLastActivityAt is runs.updated_at, which every accepted heartbeat
	// moves (the lease extension is an UPDATE, and runs_guard touches the
	// column on every UPDATE). A holder whose activity stopped minutes ago
	// while its status says Running is a holder whose cluster went quiet.
	HolderLastActivityAt time.Time
}

const lockColumns = `
	SELECT l.id, l.seq, l.run_id, l.lease_epoch, l.name, l.description,
	       l.acquired_at, l.updated_at, l.expires_at,
	       r.status, r.role_name, r.created_at, r.started_at, r.updated_at
	FROM resource_locks l JOIN runs r ON r.id = l.run_id`

// SetLock registers or refreshes the run's lock on name and returns it with
// the live locks of other runs that were set before it.
//
// ttl is how long the lock lives; it is clamped to the run's own timeout,
// which is also what zero means. A name the run already holds keeps its id,
// seq and acquired_at — its place in the order — and has its description and
// expiry replaced. A name the run held and let expire is a new lock with a new
// place: reviving the old seq would put it ahead of locks that were set while
// it was invisible, and their holders never saw it.
func (s *Store) SetLock(ctx context.Context, runID runv1.ULID, name, description string, ttl time.Duration) (Lock, []Lock, error) {
	var (
		mine    Lock
		earlier []Lock
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockWriteKey); err != nil {
			return fmt.Errorf("store: take the lock-write lock: %w", err)
		}

		// FOR SHARE, so that a status change racing this insert waits for it
		// and then deletes the lock in its trigger, rather than committing
		// first and leaving a lock behind on a run that has ended.
		var (
			status  string
			epoch   int64
			timeout int32
		)
		err := tx.QueryRowContext(ctx,
			`SELECT status, lease_epoch, timeout_seconds FROM runs WHERE id = $1 FOR SHARE`,
			runID).Scan(&status, &epoch, &timeout)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read run %s: %w", runID, err)
		case isTerminalStatus(status):
			return ErrRunTerminal
		}

		secs := int64(timeout)
		if ttl > 0 && int64(ttl/time.Second) < secs {
			secs = int64(ttl / time.Second)
		}

		if _, err := tx.ExecContext(ctx, `
			DELETE FROM resource_locks
			WHERE run_id = $1 AND name = $2 AND expires_at <= now()`, runID, name); err != nil {
			return fmt.Errorf("store: clear an expired lock: %w", err)
		}

		var held int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM resource_locks
			WHERE run_id = $1 AND name <> $2 AND expires_at > now()`, runID, name).Scan(&held); err != nil {
			return fmt.Errorf("store: count the locks of %s: %w", runID, err)
		}
		if held >= MaxLocksPerRun {
			return ErrTooManyLocks
		}

		var id runv1.ULID
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO resource_locks (id, run_id, lease_epoch, name, description, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
			ON CONFLICT (run_id, name) DO UPDATE SET
				description = EXCLUDED.description,
				expires_at  = EXCLUDED.expires_at,
				lease_epoch = EXCLUDED.lease_epoch,
				updated_at  = now()
			RETURNING id`,
			newID(), runID, epoch, name, description, float64(secs)).Scan(&id); err != nil {
			return fmt.Errorf("store: set lock %q of %s: %w", name, runID, err)
		}

		if mine, err = scanLock(tx.QueryRowContext(ctx, lockColumns+` WHERE l.id = $1`, id)); err != nil {
			return err
		}
		earlier, err = queryLocks(ctx, tx, lockColumns+`
			WHERE l.seq < $1 AND l.run_id <> $2 AND l.expires_at > now()
			ORDER BY l.seq`, mine.Seq, runID)
		return err
	})
	if err != nil {
		return Lock{}, nil, err
	}
	return mine, earlier, nil
}

// UnsetLocks removes locks of the run: those with the given ids or names, or
// every one of them when both lists are empty. Another run's lock is never
// removed, by construction of the WHERE clause, and an id that names one is
// simply not in the answer.
func (s *Store) UnsetLocks(ctx context.Context, runID runv1.ULID, ids []runv1.ULID, names []string) ([]runv1.ULID, error) {
	query := `DELETE FROM resource_locks WHERE run_id = $1`
	args := []any{runID}
	if len(ids) > 0 || len(names) > 0 {
		idText := make([]string, 0, len(ids))
		for _, id := range ids {
			idText = append(idText, string(id))
		}
		query += ` AND (id = ANY($2::text[]) OR name = ANY($3::text[]))`
		args = append(args, textArray(idText), textArray(names))
	}
	rows, err := s.db.QueryContext(ctx, query+` RETURNING id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: unset locks of %s: %w", runID, err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// LockFilter narrows ListLocks.
type LockFilter struct {
	// RunID lists one holder's locks.
	RunID runv1.ULID
}

// ListLocks returns every live lock, oldest first.
func (s *Store) ListLocks(ctx context.Context, f LockFilter) ([]Lock, error) {
	if f.RunID != "" {
		return queryLocks(ctx, s.db, lockColumns+`
			WHERE l.expires_at > now() AND l.run_id = $1 ORDER BY l.seq`, f.RunID)
	}
	return queryLocks(ctx, s.db, lockColumns+` WHERE l.expires_at > now() ORDER BY l.seq`)
}

// LockByID reads one live lock.
func (s *Store) LockByID(ctx context.Context, id runv1.ULID) (Lock, error) {
	l, err := scanLock(s.db.QueryRowContext(ctx, lockColumns+`
		WHERE l.id = $1 AND l.expires_at > now()`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, ErrNotFound
	}
	return l, err
}

// LiveLockIDs returns those of ids that name a live lock.
func (s *Store) LiveLockIDs(ctx context.Context, ids []runv1.ULID) ([]runv1.ULID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	text := make([]string, 0, len(ids))
	for _, id := range ids {
		text = append(text, string(id))
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM resource_locks
		WHERE id = ANY($1::text[]) AND expires_at > now()
		ORDER BY seq`, textArray(text))
	if err != nil {
		return nil, fmt.Errorf("store: look up locks: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// ForceUnsetLock removes any run's lock. The operator's path, from REST with
// admin scope; an agent never reaches it, or one prompt injection would clear
// every lock in the installation. It returns the lock as it was, for the
// audit record.
func (s *Store) ForceUnsetLock(ctx context.Context, id runv1.ULID) (Lock, error) {
	var out Lock
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		l, err := scanLock(tx.QueryRowContext(ctx, lockColumns+` WHERE l.id = $1 FOR UPDATE OF l`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_locks WHERE id = $1`, id); err != nil {
			return fmt.Errorf("store: remove lock %s: %w", id, err)
		}
		out = l
		return nil
	})
	return out, err
}

// DeleteExpiredLocks removes the locks whose expiry has passed. They are
// already invisible to every read; this is only the rows going.
func (s *Store) DeleteExpiredLocks(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired locks: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// rowsQueryer is a pool or a transaction, for a read that returns rows.
type rowsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryLocks(ctx context.Context, q rowsQueryer, query string, args ...any) ([]Lock, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list locks: %w", err)
	}
	defer rows.Close()

	out := []Lock{}
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLock(row rowScanner) (Lock, error) {
	var (
		l       Lock
		role    sql.NullString
		started sql.NullTime
	)
	err := row.Scan(&l.ID, &l.Seq, &l.RunID, &l.LeaseEpoch, &l.Name, &l.Description,
		&l.AcquiredAt, &l.UpdatedAt, &l.ExpiresAt,
		&l.HolderStatus, &role, &l.HolderCreatedAt, &started, &l.HolderLastActivityAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lock{}, err
		}
		return Lock{}, fmt.Errorf("store: scan lock: %w", err)
	}
	l.HolderRole = role.String
	l.HolderStartedAt = timePtr(started)
	return l, nil
}
