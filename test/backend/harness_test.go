// Package backend holds the contract tests for the haliphron backend.
//
// They run the real backend — the real handlers, the real use cases, the real
// schema on a real PostgreSQL — against FakeController and an in-memory object
// store. That combination is the point. The rules this contract is made of
// fire only in failure: a stale epoch, an expired deadline, a report that
// arrives after the one that supersedes it, two different endings for the same
// attempt. A backend developed against a permissive peer is a backend whose
// failure paths have never run, and those paths are the ones that decide
// whether a customer's run is lost or recovered.
//
// The checklist these implement is section 13 of docs/contracts/cluster-api.md,
// the half headed "Backend, against a fake controller".
//
// Two drivers, deliberately. FakeController answers "does a controller that
// behaves correctly work against this backend", and it refuses to send a
// message a correct controller would not — it applies its own monotonicity
// before it speaks. The probe in probe_test.go answers the other half: what the
// backend does with the messages a correct controller never sends, which is
// most of the decision table.
package backend

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/clusterapi"
	"github.com/automagicops/haliphron/backend/mcp"
	"github.com/automagicops/haliphron/backend/restapi"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const templateDB = "haliphron_backend_template"

var (
	adminDSN string
	dbSeq    atomic.Int64
)

func TestMain(m *testing.M) {
	adminDSN = os.Getenv("HALIPHRON_TEST_DSN")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr,
			"HALIPHRON_TEST_DSN is not set; these tests need a real PostgreSQL (make backend-test)")
		os.Exit(1)
	}
	if err := buildTemplate(); err != nil {
		fmt.Fprintf(os.Stderr, "prepare template database: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// buildTemplate migrates one database once; every test then copies it with
// CREATE DATABASE ... TEMPLATE, which is a file copy rather than a replay of
// the migrations. Tests stay isolated without paying for the schema each time.
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

// harness is one backend: its own database, its own object store, its own
// listener.
type harness struct {
	t *testing.T

	Store     *store.Store
	Artifacts *artifacts.Memory
	App       *app.Service
	Server    *httptest.Server
	Public    *httptest.Server
	MCP       *httptest.Server
	Logs      *logRecorder

	// lastResponse is the last public API response, for the assertions about
	// headers rather than bodies: a redirect's Location, a no-store on a
	// credential.
	lastResponse *http.Response

	// down simulates the control plane being away: migrations, a restart, no
	// Postgres. The connection is dropped rather than answered with a 503,
	// because that is what an outage looks like from inside a cluster and the
	// controller's obligation — play the work out, accumulate the reports — is
	// stated against it.
	down atomic.Bool
}

// testTimings are the contract's parameters, scaled down.
//
// The deadlines are seconds rather than minutes because a test that waits out
// a sixty-second ack timeout is a test somebody deletes. Nothing else changes:
// the code under test reads these from the same place a deployment's values
// would arrive, so what is being exercised is the arithmetic, not a special
// case.
func testTimings() clusterv1.Timings {
	return clusterv1.Timings{
		HeartbeatIntervalSeconds: 1,
		StaleAfterSeconds:        3,
		LeaseTTLSeconds:          2,
		AckTimeoutSeconds:        1,
		MaxWaitSeconds:           2,
		MaxLeasesPerPoll:         10,
		ArtifactTTLMultiplier:    2,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	conn := newDB(t)
	st := store.New(conn)
	if err := st.SetKEK("test", []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("install key encryption key: %v", err)
	}

	store3 := artifacts.NewMemory("haliphron", "http://storage.test.invalid")
	logs := newLogRecorder()

	service := app.New(app.Options{
		Store:     st,
		Artifacts: store3,
		Git:       app.StoredGitToken{Store: st, Name: "git-token"},
		Logger:    slog.New(logs),
		Timings:   testTimings(),
		Defaults: run.Defaults{
			Image:           "ghcr.io/automagicops/agent-runtime@sha256:" + strings.Repeat("a", 64),
			ImagePullPolicy: "IfNotPresent",
			Model:           "anthropic/claude-opus-5",
			Agent:           runv1.AgentClaudeCode,
			TimeoutSeconds:  3600,
			TTLSeconds:      86400,
			MaxInfraRetries: 3,
		},
		Limits: app.Limits{
			LLMAPIKeySecret: "llm-api-key",
			MCPEndpoint:     "http://haliphron-backend.haliphron.svc:8081/mcp",
		},
	})

	h := &harness{t: t, Store: st, Artifacts: store3, App: service, Logs: logs}

	api := clusterapi.New(service, slog.New(logs)).Handler()
	h.Public = httptest.NewServer(restapi.New(service, slog.New(logs)).Handler())
	h.MCP = httptest.NewServer(mcp.New(service, slog.New(logs)).Handler())
	t.Cleanup(h.Public.Close)
	t.Cleanup(h.MCP.Close)

	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.down.Load() {
			panic(http.ErrAbortHandler)
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Server.Close)

	// The model credential every lease carries. Stored rather than configured,
	// so the path under test is the one a deployment uses.
	if _, err := st.PutManagedSecret(context.Background(), "llm-api-key", []byte("sk-test-model-key"), "test"); err != nil {
		t.Fatalf("store the model credential: %v", err)
	}
	if _, err := st.PutManagedSecret(context.Background(), "git-token", []byte("ghs_test_repository_token"), "test"); err != nil {
		t.Fatalf("store the git credential: %v", err)
	}
	return h
}

// Token mints a platform credential with the given scopes, the way an operator
// does before handing one to a client.
func (h *harness) Token(scopes ...string) string {
	h.t.Helper()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "test", Kind: store.TokenKindService, Scopes: scopes, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		h.t.Fatalf("create token: %v", err)
	}
	return token.Secret
}

// SetDown takes the control plane away, or brings it back.
func (h *harness) SetDown(down bool) { h.down.Store(down) }

// BootstrapToken mints a one-time registration token.
func (h *harness) BootstrapToken() string {
	h.t.Helper()
	token, err := h.Store.CreateBootstrapToken(context.Background(), "test", "test", time.Hour, 1)
	if err != nil {
		h.t.Fatalf("create bootstrap token: %v", err)
	}
	return token.Token
}

// Submit admits a run the way the public API does.
func (h *harness) Submit(opts ...func(*run.SubmitRequest)) store.Run {
	h.t.Helper()
	req := run.SubmitRequest{
		Prompt:     "add a health endpoint",
		RepoURL:    "https://github.com/example/repo",
		CreatedBy:  "test",
		CreatedVia: "api",
	}
	for _, opt := range opts {
		opt(&req)
	}
	submitted, err := h.App.Submit(context.Background(), req, app.SubmitOptions{})
	if err != nil {
		h.t.Fatalf("submit run: %v", err)
	}
	return submitted.Run
}

// Run reads a run's current state.
func (h *harness) Run(id runv1.ULID) store.Run {
	h.t.Helper()
	r, err := h.Store.RunByID(context.Background(), id)
	if err != nil {
		h.t.Fatalf("read run %s: %v", id, err)
	}
	return r
}

// Sweep applies whatever the passage of time made due. Called explicitly
// rather than by a background loop: a test racing its own scanner reports
// failures that depend on scheduling.
func (h *harness) Sweep() app.SweepResult {
	h.t.Helper()
	result, err := h.App.Sweep(context.Background())
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
	return result
}

// Audit returns a run's audit records.
func (h *harness) Audit(id runv1.ULID) []store.AuditRecord {
	h.t.Helper()
	records, err := h.Store.AuditForRun(context.Background(), id, 100)
	if err != nil {
		h.t.Fatalf("read audit for %s: %v", id, err)
	}
	return records
}

// HasAudit reports whether a run has a record of this action.
func (h *harness) HasAudit(id runv1.ULID, action string) bool {
	h.t.Helper()
	for _, r := range h.Audit(id) {
		if r.Action == action {
			return true
		}
	}
	return false
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("haliphron_b%d_%d", os.Getpid(), dbSeq.Add(1))

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

func dsnFor(name string) string {
	if i := strings.LastIndex(adminDSN, "/"); i >= 0 {
		return adminDSN[:i+1] + name
	}
	return adminDSN
}
