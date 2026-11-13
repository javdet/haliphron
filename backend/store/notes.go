package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/jackc/pgx/v5/pgconn"
)

// Run notes: what a run has learned, written for the runs it starts.
//
// Two rules live here rather than in app, because only a transaction can keep
// them. A run's quota is counted under a per-run advisory lock, so that the
// parallel tool calls of one agent cannot each read a count taken before any
// of them wrote. And the table's partitions are created and dropped only
// under one advisory lock, after rechecking the catalogue, so that every
// replica may run the maintenance and the self-healing insert may run it too,
// and each partition is still created once and dropped once.
//
// Which limits apply, and what a refusal says, is app's (app/notes.go).
//
// Values never reach a log line or an error message from this file: a note is
// text written by a model that may have read a prompt-injected repository,
// and it may hold anything the model saw.

// noteLockClass namespaces the per-run advisory lock a note write takes. The
// two-key form keeps it apart from the single-key locks (migrations,
// lockWriteKey, notePartitionKey), as leaseLockClass is.
const noteLockClass = 0x4e54 // "NT"

// notePartitionKey is the advisory lock every partition create and drop takes.
const notePartitionKey int64 = 7318604251938406157

// notePartitionLockTimeout bounds how long partition DDL waits for its lock on
// run_notes. A DROP waiting behind one slow read would otherwise hold every
// read and write that arrives after it; giving up and trying again at the
// next tick costs nothing.
const notePartitionLockTimeout = "2s"

// sqlStateLockNotAvailable is lock_timeout firing.
const sqlStateLockNotAvailable = "55P03"

var (
	// ErrNoteExpired is a write by a run older than the note retention: its
	// notes are already invisible, and its partition may be gone.
	ErrNoteExpired = errors.New("store: this run's notes have expired")
	// ErrNotePartitionMissing is an insert with no partition for its row.
	// The caller ensures the partitions and tries once more.
	ErrNotePartitionMissing = errors.New("store: no partition for the note")
)

// NoteQuotaError is a write that would take its run over a quota. It carries
// the run's usage without the note being written.
type NoteQuotaError struct {
	Notes, MaxNotes int
	Bytes, MaxBytes int64
	// Adding is the size the refused write would have added.
	Adding int64
}

func (e *NoteQuotaError) Error() string {
	return fmt.Sprintf("store: note quota: %d of %d notes, %d of %d bytes, adding %d",
		e.Notes, e.MaxNotes, e.Bytes, e.MaxBytes, e.Adding)
}

// NoteLimits are what PutNote enforces inside its transaction.
type NoteLimits struct {
	MaxNotes  int
	MaxBytes  int64
	Retention time.Duration
}

// Note is one visible note, as a reader in some run's scope sees it.
type Note struct {
	Key string
	// Value is empty when the read asked for keys only.
	Value string
	Size  int
	// RunID is the run that wrote it; LevelsUp is how far above the reader
	// that run is, 0 being the reader itself.
	RunID     runv1.ULID
	LevelsUp  int
	UpdatedAt time.Time
}

// NoteQuery narrows ReadNotes.
type NoteQuery struct {
	// Key reads one key. Prefix reads the keys starting with it. Neither is
	// every key in scope.
	Key    string
	Prefix string
	// Values says whether the values are read, or only the keys.
	Values bool
	// Limit bounds the answer; ReadNotes reports whether more existed.
	Limit int
}

// PutNote writes or overwrites one of the run's own notes.
//
// It returns false when the note already held exactly this value: the row is
// not touched, so an idempotent retry creates no row version at all.
func (s *Store) PutNote(ctx context.Context, runID runv1.ULID, key, value string, limits NoteLimits) (bool, error) {
	var changed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::integer, hashtext($2::text))`,
			noteLockClass, runID); err != nil {
			return fmt.Errorf("store: take the note lock of %s: %w", runID, err)
		}

		// FOR KEY SHARE holds off a concurrent delete of the run, whose
		// trigger would otherwise remove the notes it could see and miss the
		// one this transaction is about to write. It does not conflict with
		// the ordinary updates a heartbeat makes. The expiry is checked
		// against the clock now, not the transaction's start, so that a
		// partition dropped while this waited for the advisory lock is
		// refused here rather than met as a missing partition.
		var (
			created time.Time
			fresh   bool
		)
		err := tx.QueryRowContext(ctx, `
			SELECT created_at, created_at > clock_timestamp() - make_interval(secs => $2)
			FROM runs WHERE id = $1 FOR KEY SHARE`,
			runID, limits.Retention.Seconds()).Scan(&created, &fresh)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read run %s: %w", runID, err)
		case !fresh:
			return ErrNoteExpired
		}

		// The key being written is left out of the count: an overwrite is
		// charged its new size, not the old one plus the new one.
		var (
			notes int
			bytes int64
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*), coalesce(sum(octet_length(key) + octet_length(value)), 0)
			FROM run_notes
			WHERE run_id = $1 AND owner_created_at = $2 AND key <> $3`,
			runID, created, key).Scan(&notes, &bytes); err != nil {
			return fmt.Errorf("store: count the notes of %s: %w", runID, err)
		}
		adding := int64(len(key) + len(value))
		if notes+1 > limits.MaxNotes || bytes+adding > limits.MaxBytes {
			return &NoteQuotaError{Notes: notes, MaxNotes: limits.MaxNotes,
				Bytes: bytes, MaxBytes: limits.MaxBytes, Adding: adding}
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO run_notes (run_id, owner_created_at, key, value)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (run_id, owner_created_at, key) DO UPDATE
				SET value = EXCLUDED.value, updated_at = now()
				WHERE run_notes.value IS DISTINCT FROM EXCLUDED.value`,
			runID, created, key, value)
		if err != nil {
			if isNoPartition(err) {
				return ErrNotePartitionMissing
			}
			// The key, never the value: see the top of this file. A
			// PostgreSQL error's text is its message and SQLSTATE; the detail
			// that can quote a row is not part of it.
			return fmt.Errorf("store: write note %q of %s: %w", key, runID, err)
		}
		n, _ := res.RowsAffected()
		changed = n > 0
		return nil
	})
	return changed, err
}

// ReadNotes returns the notes visible to the reader: its own and its
// ancestors', the nearest level winning on a key present at several, ordered
// by key. A note whose owner is older than retention is invisible.
//
// The scope is the reader's lineage plus itself, read in the same statement,
// so an ancestor's write that committed before the statement is seen: reads
// are live. It also reports whether the limit cut the answer short.
func (s *Store) ReadNotes(ctx context.Context, reader runv1.ULID, q NoteQuery, retention time.Duration) ([]Note, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH scope AS (
			SELECT u.run_id::ulid AS run_id, u.ord
			FROM runs r, unnest(r.lineage::text[] || r.id::text) WITH ORDINALITY AS u(run_id, ord)
			WHERE r.id = $1
		)
		SELECT DISTINCT ON (n.key)
		       n.key, CASE WHEN $5 THEN n.value ELSE '' END, octet_length(n.value),
		       n.run_id, n.updated_at, (SELECT max(ord) FROM scope) - s.ord
		FROM run_notes n
		JOIN scope s ON s.run_id = n.run_id
		WHERE n.owner_created_at > now() - make_interval(secs => $2)
		  AND ($3 = '' OR n.key = $3)
		  AND ($4 = '' OR starts_with(n.key, $4))
		ORDER BY n.key, s.ord DESC
		LIMIT $6`,
		reader, retention.Seconds(), q.Key, q.Prefix, q.Values, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: read the notes of %s: %w", reader, err)
	}
	defer rows.Close()

	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.Key, &n.Value, &n.Size, &n.RunID, &n.UpdatedAt, &n.LevelsUp); err != nil {
			return nil, false, fmt.Errorf("store: scan a note: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: read the notes of %s: %w", reader, err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// DeleteNote removes one of the run's own notes, and reports whether there
// was one. An ancestor's note under the same key is not touched, by the WHERE
// clause, and the answer does not say whether one exists.
func (s *Store) DeleteNote(ctx context.Context, runID runv1.ULID, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM run_notes
		WHERE run_id = $1 AND key = $2
		  AND owner_created_at = (SELECT created_at FROM runs WHERE id = $1)`, runID, key)
	if err != nil {
		return false, fmt.Errorf("store: delete note %q of %s: %w", key, runID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NotePartition is one attached partition of run_notes.
type NotePartition struct {
	Name string
	// Day is the UTC day it holds: runs created from Day to Day+24h.
	Day   time.Time
	Bytes int64
}

// notePartitionName is the one spelling of a partition's name. Its bounds are
// derived from it, so nothing but this function may produce one.
func notePartitionName(day time.Time) string {
	return "run_notes_" + day.UTC().Format("20060102")
}

var notePartitionPattern = regexp.MustCompile(`^run_notes_([0-9]{8})$`)

// NotePartitions lists the attached partitions of run_notes, oldest first,
// with the space each takes (heap, TOAST and index). A table that is not a
// partition of run_notes is never listed, whatever its name.
func (s *Store) NotePartitions(ctx context.Context) ([]NotePartition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.relname, pg_total_relation_size(c.oid)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'run_notes'::regclass
		ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("store: list note partitions: %w", err)
	}
	defer rows.Close()

	var out []NotePartition
	for rows.Next() {
		var p NotePartition
		if err := rows.Scan(&p.Name, &p.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan a note partition: %w", err)
		}
		m := notePartitionPattern.FindStringSubmatch(p.Name)
		if m == nil {
			// Attached by hand. Not ours to reason about, and never dropped.
			continue
		}
		day, err := time.Parse("20060102", m[1])
		if err != nil {
			continue
		}
		p.Day = day
		out = append(out, p)
	}
	return out, rows.Err()
}

// EnsureNotePartitions creates every missing day partition from the day of
// now - behind to the day of now + ahead, by the database's clock, and returns
// the names it created. When none is missing it issues no DDL at all.
func (s *Store) EnsureNotePartitions(ctx context.Context, behind, ahead time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d FROM generate_series(
			date_trunc('day', now() - make_interval(secs => $1), 'UTC'),
			date_trunc('day', now() + make_interval(secs => $2), 'UTC'),
			interval '1 day') AS d`, behind.Seconds(), ahead.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: plan note partitions: %w", err)
	}
	var days []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: plan note partitions: %w", err)
		}
		days = append(days, d.UTC())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: plan note partitions: %w", err)
	}

	existing, err := s.NotePartitions(ctx)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Name] = true
	}

	var created []string
	for _, day := range days {
		if have[notePartitionName(day)] {
			continue
		}
		made, err := s.createNotePartition(ctx, day)
		if err != nil {
			return created, err
		}
		if made {
			created = append(created, notePartitionName(day))
		}
	}
	return created, nil
}

// createNotePartition creates and attaches one day, unless another replica
// did so first. It reports whether this call created it.
//
// Two steps in one transaction. CREATE TABLE ... PARTITION OF would take
// ACCESS EXCLUSIVE on run_notes; a standalone table ATTACHed afterwards takes
// SHARE UPDATE EXCLUSIVE, which lets reads and writes go on, and its
// validation scan is free on an empty table. LIKE copies the columns, their
// defaults and the CHECK constraints the attach requires to match, so the
// partition cannot drift from the parent.
func (s *Store) createNotePartition(ctx context.Context, day time.Time) (bool, error) {
	name := notePartitionName(day)
	from := day.UTC().Format(time.RFC3339)
	to := day.UTC().AddDate(0, 0, 1).Format(time.RFC3339)

	var made bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, notePartitionKey); err != nil {
			return fmt.Errorf("store: take the note partition lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '`+notePartitionLockTimeout+`'`); err != nil {
			return fmt.Errorf("store: set the lock timeout: %w", err)
		}
		attached, err := isNotePartition(ctx, tx, name)
		if err != nil || attached {
			return err
		}
		// name is notePartitionName's output and from/to are RFC 3339
		// renderings of a time: nothing here is spliced from outside.
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE `+name+` (
				LIKE run_notes INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
				PRIMARY KEY (run_id, owner_created_at, key)
			) WITH (fillfactor = 90)`); err != nil {
			return fmt.Errorf("store: create note partition %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE run_notes ATTACH PARTITION `+name+`
			FOR VALUES FROM ('`+from+`') TO ('`+to+`')`); err != nil {
			return fmt.Errorf("store: attach note partition %s: %w", name, err)
		}
		made = true
		return nil
	})
	return made, err
}

// DropExpiredNotePartitions drops every partition whose whole day is older
// than retention by the database's clock: every run it holds notes for was
// created before now - retention, so every note in it is already invisible.
// Each drop is its own transaction with a lock timeout; one that cannot get
// its lock in time is reported in busy and left for the next call.
func (s *Store) DropExpiredNotePartitions(ctx context.Context, retention time.Duration) (dropped, busy []string, err error) {
	var cutoff time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT now() - make_interval(secs => $1)`,
		retention.Seconds()).Scan(&cutoff); err != nil {
		return nil, nil, fmt.Errorf("store: note retention cutoff: %w", err)
	}
	parts, err := s.NotePartitions(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range parts {
		if p.Day.AddDate(0, 0, 1).After(cutoff) {
			continue
		}
		gone, err := s.dropNotePartition(ctx, p.Name)
		switch {
		case pgCode(err) == sqlStateLockNotAvailable:
			busy = append(busy, p.Name)
		case err != nil:
			return dropped, busy, err
		case gone:
			dropped = append(dropped, p.Name)
		}
	}
	return dropped, busy, nil
}

// dropNotePartition drops one partition, after checking under the lock that
// it is still a partition of run_notes. DROP takes ACCESS EXCLUSIVE on
// run_notes for as long as it takes to unlink the files, which is why it
// waits at most notePartitionLockTimeout for that lock.
func (s *Store) dropNotePartition(ctx context.Context, name string) (bool, error) {
	if !notePartitionPattern.MatchString(name) {
		return false, fmt.Errorf("store: %q is not a note partition name", name)
	}
	var gone bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, notePartitionKey); err != nil {
			return fmt.Errorf("store: take the note partition lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '`+notePartitionLockTimeout+`'`); err != nil {
			return fmt.Errorf("store: set the lock timeout: %w", err)
		}
		attached, err := isNotePartition(ctx, tx, name)
		if err != nil || !attached {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+name); err != nil {
			return fmt.Errorf("store: drop note partition %s: %w", name, err)
		}
		gone = true
		return nil
	})
	return gone, err
}

func isNotePartition(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var attached bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = 'run_notes'::regclass AND c.relname = $1)`, name).Scan(&attached); err != nil {
		return false, fmt.Errorf("store: look up note partition %s: %w", name, err)
	}
	return attached, nil
}

// NoteUsage is the table's footprint, for the metrics.
type NoteUsage struct {
	Partitions int
	Bytes      int64
	Notes      int64
}

// NoteUsage sums the partitions' sizes and counts the stored notes, expired
// ones included: this is what is on disk, not what is visible.
func (s *Store) NoteUsage(ctx context.Context) (NoteUsage, error) {
	parts, err := s.NotePartitions(ctx)
	if err != nil {
		return NoteUsage{}, err
	}
	out := NoteUsage{Partitions: len(parts)}
	for _, p := range parts {
		out.Bytes += p.Bytes
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM run_notes`).Scan(&out.Notes); err != nil {
		return NoteUsage{}, fmt.Errorf("store: count notes: %w", err)
	}
	return out, nil
}

// isNoPartition is "no partition of relation found for row", which an insert
// meets when the day its owner run was created has no partition. PostgreSQL
// reports it as a check violation, 23514, and it is told apart from a CHECK
// constraint failing by naming no constraint.
func isNoPartition(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23514" && pg.ConstraintName == ""
}

// pgCode is the SQLSTATE of a PostgreSQL error, or "" for anything else.
func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}
