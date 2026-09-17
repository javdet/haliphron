// Package backend is FakeBackend: the control plane's side of the Cluster API,
// implemented in memory, faithfully enough to develop and regression-test a
// controller against.
//
// It exists because the two sides of that contract are written in parallel, and
// they diverge on the rules rather than on the shape of the fields: fencing by
// epoch, the two deadlines, report monotonicity, what a 409 obliges the caller
// to do. Those rules fire only in failure, so a controller developed against a
// permissive stub is a controller whose failure paths have never run.
//
// What it is not: a reference implementation. There is no database, no
// placement, no workflow engine and no real object storage, and no behaviour
// here should be copied into the backend. Where the contract states a rule this
// implements the rule; where the contract is silent this does the simplest
// thing that keeps a test honest, and says so.
package backend

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Backend is the fake. Safe for concurrent use: the controller under test polls
// from several goroutines, and "two pollers never get the same run" is one of
// the properties worth testing.
type Backend struct {
	mu sync.Mutex

	timings  clusterv1.Timings
	versions clusterv1.VersionRange
	storage  storageConfig

	clusters map[runv1.ULID]*cluster
	byKID    map[string]runv1.ULID
	// tokens maps an unspent bootstrap token to nothing, and a spent one to the
	// key that spent it. Registration is idempotent on (token, key), which is
	// what saves a controller that got a 200 and crashed before writing the
	// clusterID into its Secret: without it the token is gone, the identity is
	// lost, and only a human with the UI can repair it.
	tokens map[string]*spentToken

	runs  map[runv1.ULID]*run
	queue []runv1.ULID

	// stored stands in for completion.json in object storage. The report is
	// there before the callback is made (ADR 15), so a run whose terminal
	// status arrived without a completion is recoverable rather than lost.
	stored map[runv1.ULID]*runv1.CompletionReport

	ids    ulidGen
	offset time.Duration

	// wake is closed and replaced to release every long poll at once. A
	// condition variable would do, but a channel composes with the context and
	// the timer in a select.
	wake chan struct{}

	faults faults
	log    []string
	audit  []AuditEntry
}

type storageConfig struct {
	bucket   string
	endpoint string
}

type faults struct {
	unavailable   bool
	rateLimited   bool
	dropLongPolls bool
}

type spentToken struct {
	kid       string
	key       string
	clusterID runv1.ULID
}

type cluster struct {
	id       runv1.ULID
	name     string
	labels   map[string]string
	kid      string
	key      []byte
	revoked  bool
	runtimes []runv1.AgentType

	agentNamespace string
	version        string

	freeSlots      int32
	capacitySlots  int32
	quotaExhausted bool
	lastHeartbeat  time.Time
}

// run is one unit of work, as the control plane sees it.
type run struct {
	id       runv1.ULID
	spec     runv1.RenderedRunSpec
	secrets  map[string]string
	role     map[string]string
	priority int32

	status string
	// epoch is ownership, attempt is tries within an ownership. They are
	// confused constantly, which is why they are raised in different places:
	// epoch only here, attempt only by the controller.
	epoch   int64
	attempt int32
	phase   runv1.Phase
	rank    int

	holder        runv1.ULID
	acked         bool
	ackDeadline   time.Time
	leaseDeadline time.Time

	// excluded are clusters that said they could not materialise this work. A
	// negative ack that did not exclude would re-offer the same lease to the
	// same cluster forever.
	excluded map[runv1.ULID]bool

	// terminalPhase is the first terminal phase accepted. The first one wins:
	// a run that ended twice with different outcomes is a defect to audit, not
	// a value to overwrite.
	terminalPhase runv1.Phase

	completion       *runv1.CompletionReport
	completedAttempt int32
	// charges records each time this run was billed. The list rather than a
	// sum, because the property under test is "charged once", and a sum that
	// is right by luck is not evidence.
	charges []runv1.MoneyUSD

	failureClass runv1.FailureClass
	reason       string
	message      string

	cancelRequested bool
	injected        []clusterv1.Command
	attempts        []AttemptRecord
}

// AttemptRecord is one row of what the real backend keeps in run_attempts.
type AttemptRecord struct {
	Attempt   int32
	Epoch     int64
	Cluster   runv1.ULID
	StartedAt time.Time
}

// AuditEntry is something the control plane must not silently drop. In the real
// backend these are rows an operator can read; here they are assertable.
type AuditEntry struct {
	RunID  runv1.ULID
	Kind   string
	Detail string
	At     time.Time
}

// Audit kinds.
const (
	// AuditTerminalConflict is a second, different terminal phase for a run
	// that already ended. The first stands; this records that the cluster
	// disagreed.
	AuditTerminalConflict = "TerminalConflict"
	// AuditUsageDivergence is the pod's self-declared duration disagreeing with
	// the Job duration the controller observed. Cost and tokens come from the
	// least trusted component in the system, and this is the phase 1
	// mitigation: not a block, a record.
	AuditUsageDivergence = "UsageDivergence"
	// AuditNoClusterForRun is a run every registered cluster has refused.
	AuditNoClusterForRun = "NoClusterForRun"
)

// Option configures the fake.
type Option func(*Backend)

// WithTimings overrides the operational parameters handed out at registration.
// Tests use it to make a long poll or an ack window short enough to wait on.
func WithTimings(t clusterv1.Timings) Option {
	return func(b *Backend) { b.timings = t }
}

// WithVersionRange sets the supported controller SemVer window.
func WithVersionRange(min, max string) Option {
	return func(b *Backend) { b.versions = clusterv1.VersionRange{Min: min, Max: max} }
}

// WithStorage sets the bucket and endpoint the presigned bundles point at.
func WithStorage(bucket, endpoint string) Option {
	return func(b *Backend) { b.storage = storageConfig{bucket: bucket, endpoint: endpoint} }
}

// New builds a fake with the contract's defaults and one unspent bootstrap
// token, which BootstrapToken returns.
func New(opts ...Option) *Backend {
	b := &Backend{
		timings:  clusterv1.DefaultTimings(),
		versions: clusterv1.VersionRange{Min: "0.1.0", Max: "99.0.0"},
		storage:  storageConfig{bucket: "haliphron", endpoint: "http://fake-storage.invalid"},
		clusters: map[runv1.ULID]*cluster{},
		byKID:    map[string]runv1.ULID{},
		tokens:   map[string]*spentToken{defaultBootstrapToken: nil},
		runs:     map[runv1.ULID]*run{},
		stored:   map[runv1.ULID]*runv1.CompletionReport{},
		wake:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

const defaultBootstrapToken = clusterv1.BootstrapTokenPrefix + "fake0000000000000000000000000000"

// BootstrapToken returns the token a controller may register with.
func (b *Backend) BootstrapToken() string { return defaultBootstrapToken }

// IssueBootstrapToken mints another one-time token, for tests that register two
// clusters or exercise a spent token.
func (b *Backend) IssueBootstrapToken() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tok := clusterv1.BootstrapTokenPrefix + string(b.ids.next(b.now()))
	b.tokens[tok] = nil
	return tok
}

// Handler returns the Cluster API. Mount it on an httptest.Server, or serve it
// directly for docker-compose.
func (b *Backend) Handler() http.Handler { return b.routes() }

// now is the fake's clock: wall time plus whatever the test has skipped. The
// long poll still waits in real time — a test that wants a 204 asks for a short
// waitSeconds — but every deadline in the protocol is computed from here, so
// expiry can be provoked in a microsecond instead of ninety seconds.
func (b *Backend) now() time.Time { return time.Now().Add(b.offset) }

// Now is the fake's current time, including whatever a test has skipped. A
// caller minting tokens against this fake must use it: the backend checks
// lifetimes against its own clock, and a client still on wall time starts
// failing authentication the moment the test skips forward — correctly, and
// confusingly.
func (b *Backend) Now() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now()
}

// AdvanceClock moves the fake's clock forward and applies whatever that made
// due. This is the only way to test the deadlines: the alternative is a test
// that sleeps for the ack timeout, which is a minute per case and the first
// thing anyone deletes.
func (b *Backend) AdvanceClock(d time.Duration) {
	b.mu.Lock()
	b.offset += d
	b.sweep()
	b.mu.Unlock()
	b.broadcast()
}

// broadcast releases every waiting long poll. Called after anything that could
// have produced work or changed what a poller would see.
func (b *Backend) broadcast() {
	b.mu.Lock()
	close(b.wake)
	b.wake = make(chan struct{})
	b.mu.Unlock()
}

// logf records a line. What is recorded matters as much as what is not: the
// lease and artifact bodies carry a git token, a model key and presigned URLs,
// and this fake is where a controller's own logging habits get reviewed. The
// rule from the contract is runID, epoch and counts — never a body — and
// TestLeaseBodyNeverReachesTheLog holds the fake to it.
func (b *Backend) logf(format string, args ...any) {
	b.log = append(b.log, fmt.Sprintf(format, args...))
}

// Log returns the recorded lines.
func (b *Backend) Log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.log...)
}

// Audit returns what the control plane recorded rather than dropped.
func (b *Backend) Audit() []AuditEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]AuditEntry(nil), b.audit...)
}

func (b *Backend) auditf(id runv1.ULID, kind, format string, args ...any) {
	b.audit = append(b.audit, AuditEntry{
		RunID: id, Kind: kind, Detail: fmt.Sprintf(format, args...), At: b.now(),
	})
}
