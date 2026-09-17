// Package db carries the haliphron schema and the one supported way to apply
// it.
//
// The migrations are embedded rather than shipped as files beside a binary: the
// schema a build applies has to be the schema that build was tested against,
// and a directory mounted into a container is a thing an operator can get
// wrong. The same embedded set is what test/store runs against a real
// PostgreSQL, so "it applied in CI" and "it applied at the customer" are
// statements about the same bytes.
//
// The semantics of the schema — what each invariant is for, who writes which
// column, and which queries are contract — are in docs/contracts/run-store.md.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	migrationsDir = "migrations"

	// Chosen once and written down, not derived from a hash of anything. A
	// lock id that changes with a string literal is a lock two builds of the
	// same service can fail to share.
	advisoryLockID = 8623491275304918233
)

var setupOnce struct {
	sync.Once
	err error
}

// FS exposes the embedded migrations for tooling that reads them directly —
// the contract tests, and a `goose -dir` style inspection during development.
func FS() embed.FS { return migrationsFS }

// Migrate brings the database up to the schema this build carries.
//
// Every replica calls it at startup, so it takes a session-level advisory lock
// first. Without one, N replicas rolling at once race to create the same table
// and N-1 crash-loop on a duplicate object error — which looks exactly like a
// broken migration and gets diagnosed as one. The lock is session-level rather
// than transaction-level because a migration marked NO TRANSACTION (a
// concurrent index build, when this schema eventually needs one) runs outside
// any transaction and would drop a transaction-scoped lock halfway through.
//
// A replica that arrives second blocks here until the first finishes and then
// finds nothing to do. That is the intended behaviour: a rollout waits for its
// schema instead of starting against half of it.
func Migrate(ctx context.Context, db *sql.DB) error {
	if err := setup(); err != nil {
		return err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(advisoryLockID)); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// The lock is released with the session in any case; this just returns
		// it before the connection goes back to the pool.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", int64(advisoryLockID))
	}()

	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Down rolls back the most recent migration. It exists for the contract tests
// and for local development, and the backend never calls it: rolling a schema
// backwards over live data is a decision an operator makes with the data in
// front of them, not something a process does on startup.
func Down(ctx context.Context, db *sql.DB) error {
	if err := setup(); err != nil {
		return err
	}
	if err := goose.DownContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// Version reports the migration version currently applied.
func Version(ctx context.Context, db *sql.DB) (int64, error) {
	if err := setup(); err != nil {
		return 0, err
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// setup configures the package-level state goose keeps. It is idempotent
// because the dialect and the base FS are global in that library and this
// package is the only thing in the system allowed to set them.
func setup() error {
	setupOnce.Do(func() {
		goose.SetBaseFS(migrationsFS)
		goose.SetLogger(goose.NopLogger())
		setupOnce.err = goose.SetDialect("postgres")
	})
	if setupOnce.err != nil {
		return fmt.Errorf("configure migration dialect: %w", setupOnce.err)
	}
	return nil
}
