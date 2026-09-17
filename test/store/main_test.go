// Package store holds the contract tests for the haliphron schema.
//
// They run against a real PostgreSQL, for the same reason the CRD tests run
// against a real API server: everything worth testing here — generated
// columns, partial indexes, domain constraints, FOR UPDATE SKIP LOCKED and the
// guard trigger — is behaviour of the database and of nothing else. A test
// against a fake would assert that the fake agrees with itself.
//
// What is being tested is not "does the DDL parse". It is the set of claims
// docs/contracts/run-store.md makes: that the store's enums are the wire
// contract's enums, that the phase rank in the schema is the phase rank in Go,
// that two concurrent leases are disjoint, that the fence closes at the moment
// ownership is revoked rather than at the next lease, and that a terminal run
// cannot quietly become something else.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/automagicops/haliphron/db"
	_ "github.com/jackc/pgx/v5/stdlib"
	"sigs.k8s.io/yaml"
)

const templateDB = "haliphron_contract_template"

var (
	adminDSN string
	dbSeq    atomic.Int64
)

func TestMain(m *testing.M) {
	adminDSN = os.Getenv("HALIPHRON_TEST_DSN")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr,
			"HALIPHRON_TEST_DSN is not set; these tests need a real PostgreSQL (make db-test)")
		os.Exit(1)
	}
	if err := buildTemplate(); err != nil {
		fmt.Fprintf(os.Stderr, "prepare template database: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// buildTemplate migrates one database once. Every test then gets its own copy
// of it with CREATE DATABASE ... TEMPLATE, which is a file copy rather than a
// replay of the migrations — tests stay isolated without paying for the schema
// each time.
func buildTemplate() error {
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := waitForPostgres(ctx, admin); err != nil {
		return err
	}
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+templateDB+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop template: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	tmpl, err := sql.Open("pgx", dsnFor(templateDB))
	if err != nil {
		return err
	}
	defer tmpl.Close()

	if err := db.Migrate(ctx, tmpl); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

func waitForPostgres(ctx context.Context, conn *sql.DB) error {
	var last error
	for {
		if err := conn.PingContext(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres did not become ready: %w", last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// newDB returns a migrated database private to this test.
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("haliphron_t%d_%d", os.Getpid(), dbSeq.Add(1))

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec("CREATE DATABASE " + name + " TEMPLATE " + templateDB); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	conn, err := sql.Open("pgx", dsnFor(name))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		cleanup, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	return conn
}

// newEmptyDB returns a database with no schema at all, for the migration test.
func newEmptyDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("haliphron_e%d_%d", os.Getpid(), dbSeq.Add(1))

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	conn, err := sql.Open("pgx", dsnFor(name))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		cleanup, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	return conn
}

func dsnFor(name string) string {
	if i := strings.LastIndex(adminDSN, "/"); i >= 0 {
		return adminDSN[:i+1] + name
	}
	return adminDSN
}

// openAPI loads the Cluster API document. It is the source the store's enums
// are checked against: the Go types generate the CRD, the drift test in
// test/contract pins the Go types to this document, and the tests here pin
// this document to the schema. All four artifacts are then held together by a
// chain of tests rather than by anyone remembering.
func openAPI(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "api", "cluster", "v1", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// dig walks a decoded document by key, failing the test on a missing step.
func dig(t *testing.T, node any, path ...string) any {
	t.Helper()
	for i, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("openapi: %s is not an object", strings.Join(path[:i], "."))
		}
		node, ok = m[key]
		if !ok {
			t.Fatalf("openapi: no %s", strings.Join(path[:i+1], "."))
		}
	}
	return node
}

func enumAt(t *testing.T, doc map[string]any, path ...string) []string {
	t.Helper()
	raw, ok := dig(t, doc, path...).([]any)
	if !ok {
		t.Fatalf("openapi: %s is not a list", strings.Join(path, "."))
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("openapi: %s holds a non-string value %v", strings.Join(path, "."), v)
		}
		out = append(out, s)
	}
	return out
}
