package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/automagicops/haliphron/db"
)

// A migration nobody has ever rolled back is a migration whose Down section is
// a guess. This applies the whole set forwards, unwinds it to nothing, and
// applies it again — which is also the only way the DROP order in each Down
// gets checked against the dependencies the Up created.
func TestMigrationsApplyAndRollBack(t *testing.T) {
	t.Parallel()
	conn := newEmptyDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	head, err := db.Version(ctx, conn)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if head == 0 {
		t.Fatal("migrating an empty database left it at version 0")
	}

	for v := head; v > 0; v-- {
		if err := db.Down(ctx, conn); err != nil {
			t.Fatalf("roll back to %d: %v", v-1, err)
		}
	}

	left := objectsLeft(t, conn)
	if len(left) != 0 {
		t.Errorf("rolling back left %v behind", left)
	}

	if err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if v, _ := db.Version(ctx, conn); v != head {
		t.Errorf("second run reached version %d, first reached %d", v, head)
	}
}

// Migrate is called by every replica at startup, so calling it twice against
// the same database has to be uneventful.
func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	conn := newEmptyDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// Every contract query has to parse against the schema it is shipped with.
// PREPARE does that without executing anything, which is exactly the check
// wanted here: the tests above cover what the statements do, this one covers
// the ones they do not reach.
func TestContractQueriesParseAgainstTheSchema(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	for _, name := range db.QueryNames() {
		t.Run(name, func(t *testing.T) {
			if _, err := conn.Exec("PREPARE check_" + name + " AS " + stripTrailingSemicolon(db.Query(name))); err != nil {
				t.Errorf("query %s does not prepare: %v", name, err)
			}
		})
	}
}

func stripTrailingSemicolon(q string) string {
	for i := len(q) - 1; i >= 0; i-- {
		switch q[i] {
		case ' ', '\n', '\t', '\r':
			continue
		case ';':
			return q[:i]
		default:
			return q
		}
	}
	return q
}

// objectsLeft lists what a full rollback failed to remove: tables, domains and
// functions, minus goose's own bookkeeping table, which survives on purpose —
// it is the record of where the schema is.
func objectsLeft(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	rows, err := conn.Query(`
		SELECT 'table ' || tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
		UNION ALL
		SELECT 'domain ' || domain_name FROM information_schema.domains
		WHERE domain_schema = 'public'
		UNION ALL
		SELECT 'function ' || p.proname FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'`)
	if err != nil {
		t.Fatalf("list remaining objects: %v", err)
	}
	defer rows.Close()

	var left []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan remaining object: %v", err)
		}
		left = append(left, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list remaining objects: %v", err)
	}
	return left
}
