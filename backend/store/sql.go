package store

import (
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/jackc/pgx/v5/pgconn"
)

// The small pieces every statement in this package needs: identifiers, arrays
// and the one error class the callers act on.
//
// The array handling is here rather than in a dependency because the schema
// uses exactly two array types — text[] and agent_type[] — and both are read
// and written as text[]. A driver-level array codec would be a larger surface
// than the four lines it saves.

func newID() runv1.ULID { return run.MustULID(time.Now()) }

// randomToken mints the secret half of a bootstrap or API token: 160 bits,
// base32 without padding, so it survives being copied out of a terminal.
func randomToken() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: read randomness: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// textArray renders a Go slice as a Postgres array literal. Empty is '{}' and
// never NULL: every array column in this schema is NOT NULL DEFAULT '{}', and
// a NULL would fail the constraint rather than mean "unset".
func textArray(in []string) string {
	if len(in) == 0 {
		return "{}"
	}
	quoted := make([]string, 0, len(in))
	for _, v := range in {
		quoted = append(quoted, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)+`"`)
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// runtimePhaseArray renders the checkpoint for a runtime_phase[] column. It
// deduplicates and keeps the contract's execution order rather than the order
// the reports happened to arrive in: the column is read back as "what is done",
// and a list ordered by arrival is a list that reads differently after a
// retry that reported out of sequence.
func runtimePhaseArray(in []runv1.RuntimePhase) string {
	seen := make(map[runv1.RuntimePhase]bool, len(in))
	for _, p := range in {
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for _, p := range runv1.RuntimePhases {
		if seen[p] {
			out = append(out, string(p))
		}
	}
	return textArray(out)
}

// runtimePhases converts a scanned text array back. A value this build does not
// recognise is dropped rather than carried: the list is handed to an entrypoint
// as "phases you may skip", and skipping a phase whose name means nothing here
// is the one way this column could cost money.
func runtimePhases(in []string) []runv1.RuntimePhase {
	known := make(map[string]bool, len(runv1.RuntimePhases))
	for _, p := range runv1.RuntimePhases {
		known[string(p)] = true
	}
	var out []runv1.RuntimePhase
	for _, p := range runv1.RuntimePhases {
		for _, got := range in {
			if got == string(p) && known[got] {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func agentTypeArray(in []runv1.AgentType) string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, string(a))
	}
	return textArray(out)
}

// pgArray scans a Postgres array literal into a string slice. The literals this
// schema produces hold RFC 1123 names, SemVer strings and enum values — no
// commas, no braces, no quotes — so the parse is a split with a quote-aware
// pass rather than a grammar.
type pgArrayScanner struct{ dest *[]string }

func pgArray(dest *[]string) sql.Scanner { return pgArrayScanner{dest: dest} }

func (s pgArrayScanner) Scan(src any) error {
	*s.dest = nil
	var raw string
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("store: cannot scan %T as a text array", src)
	}

	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") || !strings.HasSuffix(raw, "}") {
		return fmt.Errorf("store: %q is not an array literal", raw)
	}
	raw = raw[1 : len(raw)-1]
	if raw == "" {
		return nil
	}

	var (
		out     []string
		current strings.Builder
		quoted  bool
		escaped bool
	)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case escaped:
			current.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	out = append(out, current.String())
	*s.dest = out
	return nil
}

var _ driver.Valuer = (*nullInt32Value)(nil)

type nullInt32Value struct{ v *int32 }

func (n nullInt32Value) Value() (driver.Value, error) {
	if n.v == nil {
		return nil, nil
	}
	return int64(*n.v), nil
}

func nullInt32(v *int32) driver.Valuer { return nullInt32Value{v: v} }

func scanIDs(rows *sql.Rows) ([]runv1.ULID, error) {
	var out []runv1.ULID
	for rows.Next() {
		var id runv1.ULID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// isUniqueViolation tells a constraint this code expects to hit from one it
// does not. The expected ones are idempotency rules — a name already taken, a
// key already registered — and they are answers rather than faults.
func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	return pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

// isGuardViolation reports whether the runs_guard trigger refused the write.
//
// It is worth telling apart because it means something specific: the ordering
// code produced a state the contract says is impossible — an epoch regression,
// a second terminal phase, an edit to a frozen spec. Every one of those is a
// defect in this process rather than a transient failure, and retrying it is a
// busy loop.
func isGuardViolation(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	return pg.Code == "23000"
}
