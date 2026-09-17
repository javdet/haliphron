// Package controller is FakeController: a cluster's controller, implemented in
// memory, faithfully enough to develop and regression-test a backend against.
//
// It is the counterpart of fake/backend, and it exists for the other half of
// the same problem. The backend's tests have to assert about things that happen
// inside a cluster it cannot see: that no secret value reached the AgentRun,
// that the role config became a ConfigMap and only its name rode in the CR,
// that a cluster whose CRD prunes a field refuses the work instead of running
// it with the field missing. Those are the controller's obligations, and
// without something that discharges them the backend can only be tested against
// a peer that agrees with it by construction.
//
// So this speaks the real Cluster API over HTTP, mints its own Ed25519 tokens,
// and materialises leases into real Kubernetes object types. What it does not
// have is a cluster: the Job never runs, and a test says how the pod ended.
//
// The object-building code is the closest thing to a reference for the
// controller's JobLauncher — the table in section 11 of the CRD contract is
// implemented here, once, and asserted on in this package's tests.
package controller

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Controller is the fake. One instance is one cluster.
type Controller struct {
	mu sync.Mutex

	baseURL string
	http    *http.Client

	name        string
	namespace   string
	version     string
	k8sVersion  string
	runtimes    []runv1.AgentType
	capacity    int32
	crdVersions []string

	callbackURL string

	key       keyPair
	clusterID runv1.ULID
	tokenTTL  time.Duration
	timings   clusterv1.Timings

	// objects is the cluster's API server, reduced to what this contract
	// touches: AgentRuns, Secrets, ConfigMaps and Jobs.
	objects *objectStore

	runs map[runv1.ULID]*runState

	// abandoned records work this controller was told to drop. Nothing about a
	// run in here may ever be reported again — that is the whole meaning of the
	// abandon action, and the property is worth being able to assert.
	abandoned map[runv1.ULID]bool

	quotaExhausted bool
	// pruned are spec paths this cluster's CRD does not know, simulating an
	// older chart. The most interesting failure in the CRD contract, and
	// otherwise reproducible only by installing an old CRD.
	pruned []string

	clock func() time.Time
	sent  []SentReport
	notes []string
}

// runState is what the controller knows about one run it holds.
type runState struct {
	runID   runv1.ULID
	epoch   int64
	attempt int32
	lease   clusterv1.Lease

	phase runv1.Phase
	// reportedPhase is the last phase the backend actually accepted. It is not
	// the same as phase, and the difference is the whole of the availability
	// story: a terminal phase reported into an outage is still owed to the
	// backend, and the heartbeat is what owes it.
	reportedPhase runv1.Phase
	// terminal is the first terminal phase observed. It is not overwritten: a
	// pod that exited zero while a cancellation was in flight succeeded.
	terminal runv1.Phase

	acked        bool
	infraRetries int32
	cancelled    bool
	// completionDelivered gates cleanup: the CR is held until the backend has
	// heard the outcome.
	completionDelivered bool

	callbackToken string
	specHash      string

	// rejection is set when materialisation failed before anything started. It
	// rides on the ack instead of the run being burned: create the Job, let it
	// fail, report Failed was the only way to say "I cannot" before delta D16.
	rejection *clusterv1.AckRejection
}

// SentReport is one thing this controller told the backend. Tests assert on the
// sequence, because the properties that matter most are about what was *not*
// sent after an abandon.
type SentReport struct {
	Kind    string // status | completion | ack
	RunID   runv1.ULID
	Epoch   int64
	Attempt int32
	Phase   runv1.Phase
	At      time.Time
}

// Report kinds.
const (
	ReportStatus     = "status"
	ReportCompletion = "completion"
	ReportAck        = "ack"
)

// Option configures the fake controller.
type Option func(*Controller)

// WithName sets the cluster name presented at registration.
func WithName(name string) Option { return func(c *Controller) { c.name = name } }

// WithNamespace sets the namespace agent Jobs are created in.
func WithNamespace(ns string) Option { return func(c *Controller) { c.namespace = ns } }

// WithVersion sets the controller SemVer sent on every request.
func WithVersion(v string) Option { return func(c *Controller) { c.version = v } }

// WithRuntimes declares which agent runtimes this cluster will execute.
func WithRuntimes(rt ...runv1.AgentType) Option {
	return func(c *Controller) { c.runtimes = rt }
}

// WithCapacity declares how many concurrent runs the cluster accepts.
func WithCapacity(n int32) Option { return func(c *Controller) { c.capacity = n } }

// WithClock makes the controller share a test's clock. A fake backend whose
// clock has been advanced will refuse tokens minted against wall time, so a
// test that skips a deadline must pass backend.Now here.
func WithClock(now func() time.Time) Option {
	return func(c *Controller) { c.clock = now }
}

// WithPrunedFields makes this cluster's CRD drop the named spec paths, the way
// an older chart silently does. Paths are dotted, e.g. "runtime.mcpServers".
//
// This is the trap the CRD contract calls its main one: a structural schema
// does not ignore an unknown field, it deletes it and answers 201. Without a
// way to provoke it, the backend's handling of SpecFieldsPruned is written
// against an imagined failure.
func WithPrunedFields(paths ...string) Option {
	return func(c *Controller) { c.pruned = paths }
}

// New builds a controller pointed at a Cluster API base URL.
func New(baseURL string, opts ...Option) (*Controller, error) {
	key, err := newKeyPair("kid-" + fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		return nil, err
	}
	c := &Controller{
		baseURL:     baseURL,
		http:        &http.Client{Timeout: 60 * time.Second},
		name:        "fake-cluster",
		namespace:   "haliphron-agents",
		version:     "1.0.0",
		k8sVersion:  "v1.34.1",
		capacity:    10,
		crdVersions: []string{"v1alpha1"},
		callbackURL: "http://haliphron-controller.haliphron.svc:8080/completion",
		key:         key,
		tokenTTL:    120 * time.Second,
		timings:     clusterv1.DefaultTimings(),
		objects:     newObjectStore(),
		runs:        map[runv1.ULID]*runState{},
		abandoned:   map[runv1.ULID]bool{},
		clock:       time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.objects.prune = c.pruned
	return c, nil
}

// ClusterID is the identity the backend assigned at registration.
func (c *Controller) ClusterID() runv1.ULID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clusterID
}

// Timings are the operational parameters the control plane handed out.
func (c *Controller) Timings() clusterv1.Timings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timings
}

// Sent returns everything this controller told the backend, in order.
func (c *Controller) Sent() []SentReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SentReport(nil), c.sent...)
}

// SentFor returns the reports about one run.
func (c *Controller) SentFor(id runv1.ULID) []SentReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []SentReport
	for _, r := range c.sent {
		if r.RunID == id {
			out = append(out, r)
		}
	}
	return out
}

// Notes are the lines this controller would have logged. Tests read them
// instead of a log sink, and one test reads them to prove a secret is not there.
func (c *Controller) Notes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.notes...)
}

// Abandoned reports whether this run was dropped on the backend's instruction.
func (c *Controller) Abandoned(id runv1.ULID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.abandoned[id]
}

// SetQuotaExhausted makes the next heartbeat declare the agent namespace full,
// so the backend can stop assigning here before it sees a run of failures with
// "exceeded quota" in the message.
func (c *Controller) SetQuotaExhausted(v bool) {
	c.mu.Lock()
	c.quotaExhausted = v
	c.mu.Unlock()
}

func (c *Controller) now() time.Time { return c.clock() }

func (c *Controller) note(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteLocked(format, args...)
}

// noteLocked records a line. The rule is the backend's: identifiers, counts and
// decisions, never a lease body — the secrets in it are why the response is
// no-store in the first place.
func (c *Controller) noteLocked(format string, args ...any) {
	c.notes = append(c.notes, fmt.Sprintf(format, args...))
}

func (c *Controller) record(kind string, r *runState, phase runv1.Phase) {
	c.sent = append(c.sent, SentReport{
		Kind: kind, RunID: r.runID, Epoch: r.epoch, Attempt: r.attempt,
		Phase: phase, At: c.now(),
	})
}
