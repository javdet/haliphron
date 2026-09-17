package store

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Every domain in 0001 that mirrors an enum on the Cluster API, and where that
// enum lives in the document.
var mirroredEnums = map[string][]string{
	"agent_type":        {"components", "schemas", "AgentType", "enum"},
	"git_provider":      {"components", "schemas", "GitProvider", "enum"},
	"run_phase":         {"components", "schemas", "Phase", "enum"},
	"failure_class":     {"components", "schemas", "FailureClass", "enum"},
	"completion_status": {"components", "schemas", "CompletionReport", "properties", "status", "enum"},
	"pr_action":         {"components", "schemas", "RepoResult", "properties", "prAction", "enum"},
	"rejection_code": {"components", "schemas", "AckRequest", "properties", "rejection",
		"properties", "code", "enum"},
}

// A value accepted by one side and refused by the other is not a cosmetic
// difference. The backend would hand out a lease whose own record it cannot
// write, which surfaces as a run that dies at admission with a constraint
// violation nobody can connect to the cluster that caused it.
func TestDomainsMatchOpenAPIEnums(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	doc := openAPI(t)

	for domain, path := range mirroredEnums {
		t.Run(domain, func(t *testing.T) {
			want := enumAt(t, doc, path...)
			got := domainValues(t, conn, domain)

			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(want, got) {
				t.Errorf("domain %s accepts %v, the Cluster API says %v", domain, got, want)
			}
		})
	}
}

// The store owns these sets: the Cluster API leaves the corresponding fields
// free-form because they are informational for a controller. This test does not
// compare them to anything — it records them, so that adding a value is a
// visible change to a contract rather than an edit to a CHECK constraint.
func TestStoreOwnedEnums(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	for domain, want := range map[string][]string{
		"run_status": {"Queued", "Leased", "Dispatched", "Starting", "Running",
			"Unknown", "Succeeded", "Failed", "TimedOut", "Cancelled"},
		"cluster_status": {"Registering", "Active", "Unreachable", "Revoked"},
		"created_via":    {"api", "mcp", "ui", "slack", "schedule", "workflow", "agent"},
		"token_kind":     {"user", "service", "run-mcp"},
		"secret_kind":    {"managed", "referenced"},
		"actor_kind":     {"user", "token", "cluster", "system", "agent"},
	} {
		got := domainValues(t, conn, domain)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("domain %s accepts %v, want %v", domain, got, want)
		}
	}
}

// run_status is a superset of run_phase by construction: every phase the
// controller can report has to be a status the backend can hold. A phase with
// no corresponding status is a report that arrives and cannot be applied.
func TestEveryPhaseHasAStatus(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	phases := domainValues(t, conn, "run_phase")
	statuses := domainValues(t, conn, "run_status")

	for _, p := range phases {
		// Pending is the one phase deliberately renamed on the way in: the
		// controller's "CR exists, no Job yet" is the backend's Dispatched.
		if p == "Pending" {
			continue
		}
		if !slices.Contains(statuses, p) {
			t.Errorf("phase %s has no matching run_status", p)
		}
	}
}

// The generated rank column is the ordering half of the rule in section 5 of
// the Cluster API contract. Phase.Rank in Go is the other copy of it, used by
// the controller. This executes the schema's copy and compares.
func TestObservedRankMatchesGoPhaseRank(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	ctx := context.Background()

	fx := seed(t, conn)

	for _, phase := range domainValues(t, conn, "run_phase") {
		if _, err := conn.ExecContext(ctx,
			`UPDATE runs SET observed_phase = $1 WHERE id = $2`, phase, fx.runID); err != nil {
			t.Fatalf("set phase %s: %v", phase, err)
		}
		var rank int
		if err := conn.QueryRowContext(ctx,
			`SELECT observed_rank FROM runs WHERE id = $1`, fx.runID).Scan(&rank); err != nil {
			t.Fatalf("read rank for %s: %v", phase, err)
		}
		if want := runv1.Phase(phase).Rank(); rank != want {
			t.Errorf("phase %s: schema ranks it %d, api/run/v1 ranks it %d", phase, rank, want)
		}
	}

	// No phase yet ranks zero, matching Phase.Rank's treatment of a phase this
	// build has never heard of: it loses every comparison instead of moving a
	// run backwards.
	if _, err := conn.ExecContext(ctx,
		`UPDATE runs SET observed_phase = NULL WHERE id = $1`, fx.runID); err != nil {
		t.Fatalf("clear phase: %v", err)
	}
	var rank int
	if err := conn.QueryRowContext(ctx,
		`SELECT observed_rank FROM runs WHERE id = $1`, fx.runID).Scan(&rank); err != nil {
		t.Fatalf("read rank for NULL: %v", err)
	}
	if rank != 0 {
		t.Errorf("an unobserved run ranks %d, want 0", rank)
	}
}

// The ulid domain is the store's copy of the ULID pattern on the wire. The
// negative cases are the ones that matter: Crockford base32 omits I, L, O and U
// precisely so that a transcribed identifier cannot become a different valid
// one, and a store that accepts them gives that guarantee away.
func TestULIDDomainRejectsWhatTheWireRejects(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	doc := openAPI(t)

	pattern, ok := dig(t, doc, "components", "schemas", "ULID", "pattern").(string)
	if !ok {
		t.Fatal("openapi: ULID.pattern is not a string")
	}
	if got := domainPattern(t, conn, "ulid"); got != pattern {
		t.Fatalf("ulid domain checks %q, the Cluster API says %q", got, pattern)
	}

	wire := regexp.MustCompile(pattern)
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"the contract's own example", "01J8X4K2ZQ7YB3M9F0R5W6T8CD"},
		{"lowercase", "01j8x4k2zq7yb3m9f0r5w6t8cd"},
		{"too short", "01J8X4K2ZQ7YB3M9F0R5W6T8C"},
		{"too long", "01J8X4K2ZQ7YB3M9F0R5W6T8CDE"},
		{"letter I", "01J8X4K2ZQ7YB3M9F0R5W6T8CI"},
		{"letter L", "01J8X4K2ZQ7YB3M9F0R5W6T8CL"},
		{"letter O", "01J8X4K2ZQ7YB3M9F0R5W6T8CO"},
		{"letter U", "01J8X4K2ZQ7YB3M9F0R5W6T8CU"},
		{"a uuid", "0192f0e5-4b7c-7c3e-9f2a-1d5b6e8a9c01"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.Exec(`SELECT $1::ulid`, tc.value)
			accepted := err == nil
			if want := wire.MatchString(tc.value); accepted != want {
				t.Errorf("ulid domain accepted=%v, wire pattern accepted=%v for %q",
					accepted, want, tc.value)
			}
		})
	}
}

// numeric(18,6) is not a taste: the wire pattern allows twelve digits before
// the point and six after, which is precision 18 and scale 6 exactly. This
// pins the derivation, so that widening the pattern without widening the column
// fails here rather than at the first bill that does not fit.
func TestMoneyPrecisionIsDerivedFromTheWirePattern(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	doc := openAPI(t)

	pattern, ok := dig(t, doc, "components", "schemas", "MoneyUSD", "pattern").(string)
	if !ok {
		t.Fatal("openapi: MoneyUSD.pattern is not a string")
	}
	intDigits, fracDigits := decimalShape(t, pattern)

	var precision, scale int
	err := conn.QueryRow(`
		SELECT numeric_precision, numeric_scale
		FROM information_schema.domains
		WHERE domain_name = 'money_usd'`).Scan(&precision, &scale)
	if err != nil {
		t.Fatalf("read money_usd domain: %v", err)
	}
	if scale != fracDigits {
		t.Errorf("money_usd scale is %d, the wire allows %d decimal places", scale, fracDigits)
	}
	if precision != intDigits+fracDigits {
		t.Errorf("money_usd precision is %d, the wire allows %d+%d digits",
			precision, intDigits, fracDigits)
	}

	// The largest value the wire permits survives the round trip unrounded.
	largest := strings.Repeat("9", intDigits) + "." + strings.Repeat("9", fracDigits)
	var back string
	if err := conn.QueryRow(`SELECT ($1::money_usd)::text`, largest).Scan(&back); err != nil {
		t.Fatalf("store the largest permitted cost: %v", err)
	}
	if back != largest {
		t.Errorf("largest permitted cost came back as %s, want %s", back, largest)
	}

	// Negative costs are refused here although the wire pattern allows a sign.
	// A self-declared negative spend is a broken or hostile pod, and it should
	// fail at the write rather than offset another run inside a SUM.
	if _, err := conn.Exec(`SELECT $1::money_usd`, "-1.000000"); err == nil {
		t.Error("money_usd accepted a negative cost")
	}
}

// ---------------------------------------------------------------------------

var (
	literal      = regexp.MustCompile(`'((?:[^']|'')*)'`)
	decimalParts = regexp.MustCompile(`\[0-9\]\{1,(\d+)\}`)
)

// queryer is the slice of *sql.DB these helpers need.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// domainValues reads the value list back out of a domain's CHECK constraint.
// Reading it from the catalogue rather than from the DDL file is what makes
// this a test of the database that will actually run, and it catches drift in
// both directions: a value the wire has and the store does not, and a value
// the store quietly kept after the wire dropped it.
func domainValues(t *testing.T, conn queryer, domain string) []string {
	t.Helper()
	def := constraintDef(t, conn, domain)
	matches := literal.FindAllStringSubmatch(def, -1)
	if len(matches) == 0 {
		t.Fatalf("domain %s has no value literals in %q", domain, def)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.ReplaceAll(m[1], "''", "'"))
	}
	return out
}

func domainPattern(t *testing.T, conn queryer, domain string) string {
	t.Helper()
	def := constraintDef(t, conn, domain)
	m := literal.FindStringSubmatch(def)
	if m == nil {
		t.Fatalf("domain %s has no pattern literal in %q", domain, def)
	}
	return strings.ReplaceAll(m[1], "''", "'")
}

func constraintDef(t *testing.T, conn queryer, domain string) string {
	t.Helper()
	var def string
	err := conn.QueryRow(`
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_type t ON t.oid = c.contypid
		WHERE t.typname = $1`, domain).Scan(&def)
	if err != nil {
		t.Fatalf("read CHECK constraint of domain %s: %v", domain, err)
	}
	return def
}

// decimalShape extracts the digit counts from a decimal wire pattern such as
// ^-?[0-9]{1,12}(\.[0-9]{1,6})?$.
func decimalShape(t *testing.T, pattern string) (intDigits, fracDigits int) {
	t.Helper()
	m := decimalParts.FindAllStringSubmatch(pattern, -1)
	if len(m) != 2 {
		t.Fatalf("cannot read the digit counts out of %q", pattern)
	}
	return atoi(t, m[0][1]), atoi(t, m[1][1])
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("not a number: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
