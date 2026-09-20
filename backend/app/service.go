// Package app is the backend's use cases: the layer between the three
// listeners and the store, where the Cluster API's semantics live.
//
// The listeners are transport. The store is SQL. Everything that the contract
// calls a rule — how a lease is assembled, what a heartbeat means, which
// commands are owed to a cluster, what happens when a deadline passes — is
// here, once, so that the REST path and the MCP path cannot admit runs by
// different rules and the ingest path and the heartbeat path cannot apply
// reports by different ones.
package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Service is the use cases. One per process; safe for concurrent use.
type Service struct {
	store     *store.Store
	artifacts artifacts.Store
	git       GitTokenMinter
	log       *slog.Logger

	timings  clusterv1.Timings
	versions clusterv1.VersionRange
	defaults run.Defaults
	limits   Limits

	// work releases every waiting long poll at once. A condition variable
	// would do as well, but a channel composes with the request context and
	// the poll deadline in one select.
	work *notifier

	now func() time.Time
}

// Limits are the operational values that are neither contract constants nor
// per-run settings.
type Limits struct {
	// LLMAPIKeySecret and GitTokenSecret name the secrets the lease resolves
	// its material from. Named rather than hard-coded because an installation
	// with two model providers rotates them independently.
	LLMAPIKeySecret string
	GitTokenSecret  string

	// MCPEndpoint is the URL an agent's per-run MCP token is used against. It
	// is the control plane's own address as reached from a cluster, so the
	// backend cannot derive it and the deployment states it.
	MCPEndpoint string

	// RunTokenTTLMultiplier sizes a per-run MCP token against the run's own
	// timeout. A token that outlives its run is a way to start work charged to
	// a budget nobody is watching.
	RunTokenTTLMultiplier float32

	IdempotencyTTL time.Duration

	// MaxArtifactBytesPerRun is artifacts.maxBytesPerRun, handed to the
	// controller in every bundle. It is enforced there rather than here so the
	// transfer is not paid for twice — once into the controller's spool and
	// once into a refusal by the backend that already received it.
	//
	// The backend still bounds one object at clusterv1.MaxArtifactBytes: the
	// per-run budget is an agreement with a controller, and a control plane
	// does not stake its volume on an agreement.
	MaxArtifactBytesPerRun int64
}

// Options assembles a Service.
type Options struct {
	Store     *store.Store
	Artifacts artifacts.Store
	Git       GitTokenMinter
	Logger    *slog.Logger

	Timings  clusterv1.Timings
	Versions clusterv1.VersionRange
	Defaults run.Defaults
	Limits   Limits
}

// New builds the service, filling in the contract's stated defaults for
// anything the deployment did not set.
//
// The defaults are the contract's own numbers rather than fresh choices: a
// controller tested against one set of timings and deployed against another
// has its entire expiry arithmetic silently rescaled.
func New(opts Options) *Service {
	timings := opts.Timings
	if timings.HeartbeatIntervalSeconds == 0 {
		timings = clusterv1.DefaultTimings()
	}
	if opts.Versions.Min == "" {
		opts.Versions = clusterv1.VersionRange{Min: "0.1.0", Max: "99.0.0"}
	}
	limits := opts.Limits
	if limits.RunTokenTTLMultiplier <= 0 {
		limits.RunTokenTTLMultiplier = 2
	}
	if limits.IdempotencyTTL <= 0 {
		limits.IdempotencyTTL = 24 * time.Hour
	}
	if limits.MaxArtifactBytesPerRun <= 0 {
		limits.MaxArtifactBytesPerRun = clusterv1.DefaultMaxBytesPerRun
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store:     opts.Store,
		artifacts: opts.Artifacts,
		git:       opts.Git,
		log:       logger,
		timings:   timings,
		versions:  opts.Versions,
		defaults:  opts.Defaults,
		limits:    limits,
		work:      newNotifier(),
		now:       time.Now,
	}
}

// Timings are the operational parameters handed out at registration. They come
// from the control plane rather than each cluster's values.yaml, so that
// staleAfter means the same thing in every installation.
func (s *Service) Timings() clusterv1.Timings { return s.timings }

// Versions is the controller SemVer window this control plane speaks.
func (s *Service) Versions() clusterv1.VersionRange { return s.versions }

// Store exposes the store for the listeners' own reads — authentication, the
// run list, the operator endpoints — which have no use case to go through.
func (s *Service) Store() *store.Store { return s.store }

// Artifacts exposes the object store for the endpoints that hand out a
// presigned link instead of proxying bytes through the control plane.
func (s *Service) Artifacts() artifacts.Store { return s.artifacts }

func (s *Service) ackTimeout() time.Duration {
	return time.Duration(s.timings.AckTimeoutSeconds) * time.Second
}

func (s *Service) leaseTTL() time.Duration {
	return time.Duration(s.timings.LeaseTTLSeconds) * time.Second
}

func (s *Service) staleAfter() time.Duration {
	return time.Duration(s.timings.StaleAfterSeconds) * time.Second
}

// notifier releases every waiting long poll.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

// wait returns the channel to select on. It must be taken under the same lock
// as the check for work that follows it: fetching it afterwards loses a
// broadcast that lands in between, and the symptom is a poll that sits out its
// full wait while work is queued — which reads as latency rather than as a bug.
func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}

// Notify releases waiting polls. Exported for the paths outside this package
// that produce work — the scheduler, once there is one.
func (s *Service) Notify() { s.work.broadcast() }

// ctxDone is a small helper for the several loops that wait on three things.
func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
