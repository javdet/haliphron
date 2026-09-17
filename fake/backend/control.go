package backend

import (
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The control surface: everything a test does to the fake that is not an HTTP
// request. It is deliberately small and states-not-steps — Enqueue, look, skip
// time, break something — because a fake whose setup requires knowing its
// internals is a second implementation to maintain.

// RunOption adjusts a queued run.
type RunOption func(*run)

// WithSecrets sets the secret material the lease will carry. The defaults are
// placeholders shaped like the real thing; a test that cares what the
// controller does with a git token sets its own and then looks for it in the
// Secret the controller created.
func WithSecrets(s map[string]string) RunOption {
	return func(r *run) { r.secrets = s }
}

// WithRoleConfig sets the role fallback files the controller must turn into a
// ConfigMap — and must not copy into the CR.
func WithRoleConfig(c map[string]string) RunOption {
	return func(r *run) { r.role = c }
}

// WithPriority orders issuance ahead of lower-priority work.
func WithPriority(p int32) RunOption {
	return func(r *run) { r.priority = p }
}

// Enqueue admits a run and returns its ID. It is the equivalent of a request
// arriving at the public API and passing admission: the spec is already
// rendered, the prompt is already in storage.
func (b *Backend) Enqueue(spec runv1.RenderedRunSpec, opts ...RunOption) runv1.ULID {
	b.mu.Lock()
	id := b.ids.next(b.now())
	r := &run{
		id:      id,
		spec:    spec,
		secrets: defaultSecrets(),
		status:  clusterv1.StatusQueued,
		attempt: 1,
		// epoch stays 0 until the first issuance makes it 1. The contract
		// counts the first lease as epoch 1, and a queued run that was never
		// offered to anyone has no ownership to fence.
		epoch:    0,
		excluded: map[runv1.ULID]bool{},
	}
	if r.spec.Prompt.Bucket == "" {
		r.spec.Prompt = runv1.ObjectRef{
			Bucket: b.storage.bucket,
			Key:    storageKey(id, runv1.StorageKeyPrompt),
		}
	}
	for _, opt := range opts {
		opt(r)
	}
	b.runs[id] = r
	b.queue = append(b.queue, id)
	b.logf("enqueued run=%s", id)
	b.mu.Unlock()

	b.broadcast()
	return id
}

func defaultSecrets() map[string]string {
	return map[string]string{
		runv1.SecretKeyGitToken:  "ghs_fake_token_value",
		runv1.SecretKeyLLMAPIKey: "sk-fake-model-key",
	}
}

// RunState is a snapshot. A copy rather than the live struct: a test holding a
// pointer into the fake's state would see changes it never triggered, and the
// failures that produces are the worst kind.
type RunState struct {
	ID      runv1.ULID
	Status  string
	Epoch   int64
	Attempt int32
	Phase   runv1.Phase

	Cluster       runv1.ULID
	Acked         bool
	AckDeadline   time.Time
	LeaseDeadline time.Time
	Excluded      []runv1.ULID

	TerminalPhase runv1.Phase
	FailureClass  runv1.FailureClass
	Reason        string
	Message       string

	Completion *runv1.CompletionReport
	// Charges is one entry per time this run was billed. Length is the
	// assertion that matters.
	Charges  []runv1.MoneyUSD
	Attempts []AttemptRecord
}

// RunState returns the snapshot, and whether the run exists.
func (b *Backend) RunState(id runv1.ULID) (RunState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()
	r, ok := b.runs[id]
	if !ok {
		return RunState{}, false
	}
	return r.snapshot(), true
}

func (r *run) snapshot() RunState {
	s := RunState{
		ID: r.id, Status: r.status, Epoch: r.epoch, Attempt: r.attempt, Phase: r.phase,
		Cluster: r.holder, Acked: r.acked,
		AckDeadline: r.ackDeadline, LeaseDeadline: r.leaseDeadline,
		TerminalPhase: r.terminalPhase, FailureClass: r.failureClass,
		Reason: r.reason, Message: r.message,
		Completion: r.completion,
		Charges:    append([]runv1.MoneyUSD(nil), r.charges...),
		Attempts:   append([]AttemptRecord(nil), r.attempts...),
	}
	for id := range r.excluded {
		s.Excluded = append(s.Excluded, id)
	}
	return s
}

// ClusterState is a registered cluster as the control plane sees it.
type ClusterState struct {
	ID             runv1.ULID
	Name           string
	Revoked        bool
	Unreachable    bool
	FreeSlots      int32
	CapacitySlots  int32
	QuotaExhausted bool
	LastHeartbeat  time.Time
}

// Clusters returns every registered cluster.
func (b *Backend) Clusters() []ClusterState {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ClusterState, 0, len(b.clusters))
	for _, c := range b.clusters {
		out = append(out, ClusterState{
			ID: c.id, Name: c.name, Revoked: c.revoked,
			Unreachable:    !c.lastHeartbeat.IsZero() && b.now().Sub(c.lastHeartbeat) > b.staleAfter(),
			FreeSlots:      c.freeSlots,
			CapacitySlots:  c.capacitySlots,
			QuotaExhausted: c.quotaExhausted,
			LastHeartbeat:  c.lastHeartbeat,
		})
	}
	return out
}

// Cancel asks the cluster to stop a run. Delivery is through the heartbeat and
// repeats until the phase is terminal: cancellation is asynchronous by
// construction, since the backend cannot reach into the cluster.
func (b *Backend) Cancel(id runv1.ULID) {
	b.mu.Lock()
	if r, ok := b.runs[id]; ok {
		r.cancelRequested = true
		b.logf("cancel requested run=%s", id)
	}
	b.mu.Unlock()
	b.broadcast()
}

// InjectCommand queues an arbitrary command for the run's holder, delivered
// once. It exists for the cases the backend would never produce on purpose:
// an unknown command type, which the controller must ignore rather than crash
// on, because a newer control plane will one day send one.
func (b *Backend) InjectCommand(id runv1.ULID, cmd clusterv1.Command) {
	b.mu.Lock()
	if r, ok := b.runs[id]; ok {
		cmd.RunID = id
		r.injected = append(r.injected, cmd)
	}
	b.mu.Unlock()
	b.broadcast()
}

// Reassign takes work away from its current holder and queues it again: the
// operator action behind "the cluster disappeared and the run is Unknown".
// The epoch rises, so the old holder's next report is fenced with an abandon,
// and the attempt resets, because a new ownership starts counting from one.
func (b *Backend) Reassign(id runv1.ULID) {
	b.mu.Lock()
	if r, ok := b.runs[id]; ok {
		b.requeue(r, "reassigned")
	}
	b.mu.Unlock()
	b.broadcast()
}

// Retry is the operator pressing retry in the UI. Mechanically the same as a
// reassignment — new ownership, attempt back to one — and separate because the
// reason is different and the audit trail should say which happened.
func (b *Backend) Retry(id runv1.ULID) {
	b.mu.Lock()
	if r, ok := b.runs[id]; ok {
		r.terminalPhase = ""
		r.completion = nil
		r.completedAttempt = 0
		b.requeue(r, "operator retry")
	}
	b.mu.Unlock()
	b.broadcast()
}

// Revoke marks a cluster revoked. Its next request fails with ClusterRevoked
// and action fatal, within the lifetime of a token it already holds — there is
// no revocation list, because the cluster row is read on every request anyway.
func (b *Backend) Revoke(clusterID runv1.ULID) {
	b.mu.Lock()
	if c, ok := b.clusters[clusterID]; ok {
		c.revoked = true
	}
	b.mu.Unlock()
	b.broadcast()
}

// SetUnavailable makes every endpoint answer 503 with action backoff. The
// controller must keep executing what it already holds and accumulate its
// reports: availability decoupling is the property, and this is how it gets
// tested without stopping a process.
func (b *Backend) SetUnavailable(v bool) {
	b.mu.Lock()
	b.faults.unavailable = v
	b.mu.Unlock()
	b.broadcast()
}

// SetRateLimited makes every endpoint answer 429 with Retry-After.
func (b *Backend) SetRateLimited(v bool) {
	b.mu.Lock()
	b.faults.rateLimited = v
	b.mu.Unlock()
	b.broadcast()
}

// SetDropLongPolls makes /leases close the connection without a response, the
// way a proxy or a rolling restart does. The controller must re-poll at once
// but no faster than once a second; without this it is tested only against a
// backend that is always polite.
func (b *Backend) SetDropLongPolls(v bool) {
	b.mu.Lock()
	b.faults.dropLongPolls = v
	b.mu.Unlock()
	b.broadcast()
}

// SeedStoredCompletion puts a report into the fake's stand-in for storage
// without it having been ingested — the state after a controller crashed
// between the pod's webhook and forwarding it. The backend is supposed to
// recover from exactly this, and does so on the next sweep.
func (b *Backend) SeedStoredCompletion(id runv1.ULID, report runv1.CompletionReport) {
	b.mu.Lock()
	b.stored[id] = &report
	b.sweep()
	b.mu.Unlock()
}

func storageKey(id runv1.ULID, name string) string {
	return "runs/" + string(id) + "/" + name
}
