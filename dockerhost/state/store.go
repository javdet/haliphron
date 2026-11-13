package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the build stays CGO_ENABLED=0

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ErrNotFound is a run with no row.
var ErrNotFound = errors.New("state: no such run")

// ErrExists is an insert for a run that already has a row.
var ErrExists = errors.New("state: the run already has a row")

// ErrConflict is a compare-and-swap that lost: somebody wrote the row first.
var ErrConflict = errors.New("state: the row changed since it was read")

// Store is the SQLite file.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// migrations are applied in order and recorded in schema_migrations. Append
// only: a migration that has run on a host is never edited.
var migrations = []string{
	`CREATE TABLE runs (
		run_id                TEXT PRIMARY KEY,
		epoch                 INTEGER NOT NULL,
		lease_attempt         INTEGER NOT NULL DEFAULT 1,
		spec                  TEXT NOT NULL,
		spec_raw_sha256       TEXT NOT NULL DEFAULT '',
		status                TEXT NOT NULL DEFAULT '{}',
		leased_at             TEXT NOT NULL,
		acked_at              TEXT,
		cancel_requested_at   TEXT,
		cancel_grace_seconds  INTEGER NOT NULL DEFAULT 0,
		cancel_reason         TEXT NOT NULL DEFAULT '',
		abandoned_at          TEXT,
		callback_token        TEXT NOT NULL,
		materials             TEXT NOT NULL,
		version               INTEGER NOT NULL DEFAULT 1,
		created_at            TEXT NOT NULL,
		terminal_at           TEXT
	)`,
	// The images agent containers ran, for the image collector: it removes
	// only images that appear here, so an image some other workload on the host
	// uses is never touched.
	`CREATE TABLE images (
		ref           TEXT PRIMARY KEY,
		last_used_at  TEXT NOT NULL
	)`,
}

// Open opens or creates the store at path, in a directory it makes private.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", filepath.Dir(path), err)
	}
	// WAL lets `runs list` read while the controller writes; busy_timeout
	// makes the two wait on each other instead of failing.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	// One connection: SQLite serialises writers anyway, and one connection
	// makes every transaction see the previous one's commit without relying on
	// shared-cache semantics.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The file holds every run's git token in the clear (decision 7). Its mode
	// is the protection, so it is enforced on every open, not only on create.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			if err := os.Chmod(p, 0o600); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("state: chmod %s: %w", p, err)
			}
		}
	}
	return s, nil
}

// OpenReadOnly opens an existing store for the runs subcommand. It never
// migrates and never writes.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the clock, for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("state: create schema_migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("state: read schema version: %w", err)
	}
	if current > len(migrations) {
		// A newer controller migrated this file and an older one was started
		// on it. Refusing is the only safe answer: the older code does not
		// know what the newer columns mean.
		return fmt.Errorf("state: the store is at schema version %d and this binary knows %d; "+
			"run the newer controller", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("state: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			i+1, formatTime(s.now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("state: record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

const columns = `run_id, epoch, lease_attempt, spec, spec_raw_sha256, status, leased_at, acked_at,
	cancel_requested_at, cancel_grace_seconds, cancel_reason, abandoned_at, callback_token,
	materials, version, created_at, terminal_at`

// Insert writes a new row. It fails with ErrExists when the run already has
// one, whatever its epoch: the caller decides what an existing row means.
func (s *Store) Insert(ctx context.Context, r Run) (Run, error) {
	now := s.now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.LeasedAt.IsZero() {
		r.LeasedAt = now
	}
	r.Version = 1
	vals, err := encode(r)
	if err != nil {
		return Run{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runs (`+columns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, vals...)
	if err != nil {
		if isConstraint(err) {
			return Run{}, ErrExists
		}
		return Run{}, fmt.Errorf("state: insert %s: %w", r.RunID, err)
	}
	return r, nil
}

// Get reads one run.
func (s *Store) Get(ctx context.Context, runID runv1.ULID) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM runs WHERE run_id = ?`, string(runID))
	r, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// List reads every run, oldest first.
func (s *Store) List(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM runs ORDER BY created_at, run_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Update writes r if the row is still at r.Version, and returns the row as
// written. ErrConflict means somebody else wrote first; the caller re-reads and
// decides again, exactly as a controller does on a resourceVersion conflict.
func (s *Store) Update(ctx context.Context, r Run) (Run, error) {
	if r.Status.Phase.IsTerminal() && r.TerminalAt == nil {
		t := s.now().UTC()
		r.TerminalAt = &t
	}
	if !r.Status.Phase.IsTerminal() {
		r.TerminalAt = nil
	}
	expected := r.Version
	r.Version = expected + 1
	vals, err := encode(r)
	if err != nil {
		return Run{}, err
	}
	// vals[0] is run_id; the SET list takes the rest, then the key and the
	// expected version.
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		epoch = ?, lease_attempt = ?, spec = ?, spec_raw_sha256 = ?, status = ?, leased_at = ?, acked_at = ?,
		cancel_requested_at = ?, cancel_grace_seconds = ?, cancel_reason = ?, abandoned_at = ?,
		callback_token = ?, materials = ?, version = ?, created_at = ?, terminal_at = ?
		WHERE run_id = ? AND version = ?`,
		append(vals[1:], vals[0], expected)...)
	if err != nil {
		return Run{}, fmt.Errorf("state: update %s: %w", r.RunID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, fmt.Errorf("state: update %s: %w", r.RunID, err)
	}
	if n == 0 {
		return Run{}, ErrConflict
	}
	return r, nil
}

// Mutate reads a run, applies fn and writes the result, repeating on a
// conflict. fn reports whether it changed anything; a false skips the write.
// It returns the row as last read or written, and ErrNotFound when there is
// no row.
func (s *Store) Mutate(ctx context.Context, runID runv1.ULID, fn func(*Run) bool) (Run, error) {
	for i := 0; ; i++ {
		r, err := s.Get(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if !fn(&r) {
			return r, nil
		}
		written, err := s.Update(ctx, r)
		if errors.Is(err, ErrConflict) && i < 20 {
			continue
		}
		return written, err
	}
}

// Delete removes a run's row, and with it the run's secrets.
func (s *Store) Delete(ctx context.Context, runID runv1.ULID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ?`, string(runID)); err != nil {
		return fmt.Errorf("state: delete %s: %w", runID, err)
	}
	return nil
}

// TouchImage records that an agent container used an image.
func (s *Store) TouchImage(ctx context.Context, ref string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO images (ref, last_used_at) VALUES (?, ?)
		ON CONFLICT(ref) DO UPDATE SET last_used_at = excluded.last_used_at`, ref, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("state: record image %s: %w", ref, err)
	}
	return nil
}

// ImageUse is one row of the images table.
type ImageUse struct {
	Ref        string
	LastUsedAt time.Time
}

// Images lists the images agents used, most recent first.
func (s *Store) Images(ctx context.Context) ([]ImageUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref, last_used_at FROM images ORDER BY last_used_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("state: list images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ImageUse
	for rows.Next() {
		var ref, at string
		if err := rows.Scan(&ref, &at); err != nil {
			return nil, err
		}
		t, _ := parseTime(at)
		out = append(out, ImageUse{Ref: ref, LastUsedAt: t})
	}
	return out, rows.Err()
}

// ForgetImage drops an image from the table once it has been removed.
func (s *Store) ForgetImage(ctx context.Context, ref string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE ref = ?`, ref)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (Run, error) {
	var (
		r                                      Run
		runID, spec, status, materials, leased string
		acked, cancelAt, abandoned, terminal   sql.NullString
		created                                string
	)
	if err := row.Scan(&runID, &r.Epoch, &r.LeaseAttempt, &spec, &r.SpecRawSHA256, &status, &leased, &acked,
		&cancelAt, &r.CancelGraceSeconds, &r.CancelReason, &abandoned, &r.CallbackToken,
		&materials, &r.Version, &created, &terminal); err != nil {
		return Run{}, err
	}
	r.RunID = runv1.ULID(runID)
	if err := json.Unmarshal([]byte(spec), &r.Spec); err != nil {
		return Run{}, fmt.Errorf("state: decode spec of %s: %w", runID, err)
	}
	if err := json.Unmarshal([]byte(status), &r.Status); err != nil {
		return Run{}, fmt.Errorf("state: decode status of %s: %w", runID, err)
	}
	if err := json.Unmarshal([]byte(materials), &r.Materials); err != nil {
		return Run{}, fmt.Errorf("state: decode materials of %s: %w", runID, err)
	}
	var err error
	if r.LeasedAt, err = parseTime(leased); err != nil {
		return Run{}, err
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return Run{}, err
	}
	r.AckedAt = parseNull(acked)
	r.CancelRequestedAt = parseNull(cancelAt)
	r.AbandonedAt = parseNull(abandoned)
	r.TerminalAt = parseNull(terminal)
	return r, nil
}

func encode(r Run) ([]any, error) {
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("state: encode spec: %w", err)
	}
	status, err := json.Marshal(r.Status)
	if err != nil {
		return nil, fmt.Errorf("state: encode status: %w", err)
	}
	if r.Materials.Artifacts.Mode == "" {
		r.Materials.Artifacts.Mode = runv1.ArtifactModeRelay
	}
	materials, err := json.Marshal(r.Materials)
	if err != nil {
		return nil, fmt.Errorf("state: encode materials: %w", err)
	}
	return []any{
		string(r.RunID), r.Epoch, r.LeaseAttempt, string(spec), r.SpecRawSHA256, string(status),
		formatTime(r.LeasedAt), nullTime(r.AckedAt), nullTime(r.CancelRequestedAt),
		r.CancelGraceSeconds, r.CancelReason, nullTime(r.AbandonedAt), r.CallbackToken,
		string(materials), r.Version, formatTime(r.CreatedAt), nullTime(r.TerminalAt),
	}, nil
}

// Times are stored as RFC 3339 with nanoseconds in UTC, which sorts as text.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("state: bad time %q: %w", s, err)
	}
	return t, nil
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseNull(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

func isConstraint(err error) bool {
	// modernc reports SQLITE_CONSTRAINT_PRIMARYKEY in the message; matching the
	// text keeps this package free of the driver's error types.
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// Bundle is the artifact bundle with the mode defaulted, for callers that need
// the whole lease's view of storage.
func (r *Run) Bundle() clusterv1.ArtifactBundle { return r.Materials.Artifacts }
