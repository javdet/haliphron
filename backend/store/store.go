// Package store is the backend's Postgres adapter: the RunQueue and Repository
// ports of section 5 of the architecture, over the schema in db/.
//
// Two rules shape everything here.
//
// The four contract queries are not re-typed. lease, expire_ack, expire_lease
// and lock_run are executed as db.Query returns them, because their exact text
// is the contract — the ordering that matches the index, the epoch that is read
// rather than raised, the SKIP LOCKED that makes two concurrent polls disjoint.
// A store with its own copy of the lease statement is a store testing its copy.
//
// No domain decision is made in this package. It locks a row, hands the state
// to run.Apply, and writes what the decision says. The alternative — a stored
// procedure, or an UPDATE with the monotonicity rules in its WHERE clause —
// would put the domain in the schema and make every refinement of the rule a
// migration.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/automagicops/haliphron/db"
	_ "github.com/jackc/pgx/v5/stdlib" // the driver this package is written against
)

// ErrNotFound is a row that is not there. Handlers map it to a 404 with the
// code the contract names; nothing else about a missing row is interesting.
var ErrNotFound = errors.New("store: not found")

// Store holds the pool. One per process.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// The key encryption key for managed secrets. It is installed by the
	// caller and never read from the database: that separation is the whole
	// property, and a KEK that travelled with the rows it wraps would be a
	// longer function call and nothing else.
	kekID string
	kek   []byte
}

// Config is the connection and pool shape.
type Config struct {
	DSN string

	// MaxOpenConns is deliberately small. The long poll does not hold a
	// connection while it waits — it sleeps on a notifier and asks again — so
	// the pool is sized for real work rather than for the number of clusters.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open connects and verifies the connection. It does not migrate: applying the
// schema is a separate call, so that a replica can be started against a
// database an operator migrates by hand.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("store: DSN is required")
	}
	conn, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if cfg.MaxOpenConns <= 0 {
		cfg.MaxOpenConns = 16
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = 4
	}
	if cfg.ConnMaxLifetime <= 0 {
		cfg.ConnMaxLifetime = time.Hour
	}
	conn.SetMaxOpenConns(cfg.MaxOpenConns)
	conn.SetMaxIdleConns(cfg.MaxIdleConns)
	conn.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return New(conn), nil
}

// New wraps an existing pool. The contract tests use it to hand the store the
// per-test database they created from a template.
func New(conn *sql.DB) *Store {
	return &Store{db: conn, now: time.Now}
}

// Migrate applies the embedded schema. Every replica calls it at startup; the
// advisory lock inside makes that safe.
func (s *Store) Migrate(ctx context.Context) error { return db.Migrate(ctx, s.db) }

// DB exposes the pool for the health check and for tests.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// inTx runs fn in a transaction, rolling back on error and on panic.
//
// The rollback on panic is not defensive decoration: without it a panic in a
// handler leaves a row locked by lock_run until the connection is reaped, and
// every subsequent report about that run blocks behind it.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// nullString is the empty-string-means-absent convention this schema uses for
// every optional text column.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func int32Ptr(v sql.NullInt32) *int32 {
	if !v.Valid {
		return nil
	}
	n := v.Int32
	return &n
}
