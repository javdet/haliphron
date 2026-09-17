package backend

import (
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The rules from docs/contracts/cluster-api.md, sections 3 to 8. Every caller
// holds b.mu.

func (b *Backend) staleAfter() time.Duration {
	return time.Duration(b.timings.StaleAfterSeconds) * time.Second
}

// sweep applies whatever the passage of time made due. The real backend runs
// two scanners over two indexes; the fake runs both at the top of every request
// and control call, which is equivalent at this scale and removes the question
// of whether a test raced the scanner.
func (b *Backend) sweep() {
	now := b.now()
	for _, r := range b.runs {
		switch {
		// Before the ack the work is guaranteed not to have started: the
		// controller died without creating anything. Safe to hand to someone
		// else immediately, and the epoch rises so the dead controller's late
		// ack is fenced rather than accepted.
		case r.status == clusterv1.StatusLeased && !r.acked && now.After(r.ackDeadline):
			b.logf("ack deadline expired run=%s epoch=%d", r.id, r.epoch)
			b.requeue(r, "ack deadline expired")

		// After the ack the Job may be running this second. Nobody can say
		// whether it is, so the run becomes Unknown and waits for a human or a
		// late report. Requeueing here would be the expensive mistake: two
		// agents on one repository, two PRs, two bills.
		case r.acked && !r.phase.IsTerminal() && now.After(r.leaseDeadline) &&
			r.status != clusterv1.StatusUnknown:
			b.logf("lease deadline expired run=%s epoch=%d", r.id, r.epoch)
			r.status = clusterv1.StatusUnknown

		// The result was in storage before the callback was made, so a
		// completion that never arrived is a read rather than a loss.
		case r.status == clusterv1.StatusCompletedWithoutResult:
			if report, ok := b.stored[r.id]; ok {
				b.applyCompletion(r, report)
				b.logf("recovered completion from storage run=%s", r.id)
			}
		}
	}
}

// requeue starts a new ownership: the epoch rises, the attempt resets to one,
// and the work goes back to the queue. The epoch is what makes this safe — the
// previous holder's next message carries the old one and is told to abandon.
func (b *Backend) requeue(r *run, reason string) {
	r.epoch++
	r.attempt = 1
	r.phase = ""
	r.rank = 0
	r.holder = ""
	r.acked = false
	r.ackDeadline = time.Time{}
	r.leaseDeadline = time.Time{}
	r.status = clusterv1.StatusQueued
	if !b.queued(r.id) {
		b.queue = append(b.queue, r.id)
	}
	b.logf("requeued run=%s epoch=%d reason=%q", r.id, r.epoch, reason)
}

func (b *Backend) queued(id runv1.ULID) bool {
	for _, q := range b.queue {
		if q == id {
			return true
		}
	}
	return false
}

// issue hands work to a cluster. Called under the lock from the lease handler,
// which is what makes "two pollers never receive the same run" true here for
// the same reason FOR UPDATE SKIP LOCKED makes it true in the real backend.
func (b *Backend) issue(c *cluster, limit int, runtimes []runv1.AgentType) []clusterv1.Lease {
	if limit <= 0 || c.quotaExhausted {
		return nil
	}
	now := b.now()
	var out []clusterv1.Lease
	remaining := b.queue[:0:0]

	for _, id := range b.queue {
		r, ok := b.runs[id]
		if !ok || r.status != clusterv1.StatusQueued {
			continue
		}
		if len(out) >= limit || r.excluded[c.id] || !runtimeAllowed(r.spec.Agent, runtimes, c.runtimes) {
			remaining = append(remaining, id)
			continue
		}

		if r.epoch == 0 {
			r.epoch = 1
		}
		r.holder = c.id
		r.status = clusterv1.StatusLeased
		r.acked = false
		r.ackDeadline = now.Add(time.Duration(b.timings.AckTimeoutSeconds) * time.Second)
		r.leaseDeadline = now.Add(time.Duration(b.timings.LeaseTTLSeconds) * time.Second)
		r.attempts = append(r.attempts, AttemptRecord{
			Attempt: r.attempt, Epoch: r.epoch, Cluster: c.id, StartedAt: now,
		})

		out = append(out, clusterv1.Lease{
			RunID:         r.id,
			Epoch:         r.epoch,
			Attempt:       r.attempt,
			AckDeadline:   r.ackDeadline,
			LeaseDeadline: r.leaseDeadline,
			Priority:      r.priority,
			Spec:          r.spec,
			Secrets:       copyMap(r.secrets),
			RoleConfig:    copyMap(r.role),
			Artifacts:     b.mintBundle(r),
		})
	}
	b.queue = remaining
	return out
}

// runtimeAllowed intersects what the poll asked for with what the cluster
// declared at registration. An empty list on either side means no restriction,
// which is the only reading that keeps a controller that declares nothing
// usable.
func runtimeAllowed(agent runv1.AgentType, requested, declared []runv1.AgentType) bool {
	return containsOrEmpty(requested, agent) && containsOrEmpty(declared, agent)
}

func containsOrEmpty(list []runv1.AgentType, want runv1.AgentType) bool {
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// mintBundle produces presigned capabilities. The URLs are not signed and lead
// nowhere: what the controller must get right is the expiry arithmetic and
// keeping the bundle out of the CR, and neither needs a real signature. The
// image track gets real storage from FakeControlPlane instead.
func (b *Backend) mintBundle(r *run) clusterv1.ArtifactBundle {
	multiplier := b.timings.ArtifactTTLMultiplier
	if multiplier <= 0 {
		multiplier = 2
	}
	ttl := time.Duration(float32(r.spec.Runtime.TimeoutSeconds)*multiplier) * time.Second
	expires := b.now().Add(ttl)
	prefix := "runs/" + string(r.id) + "/"

	put := map[string]clusterv1.PresignedURL{}
	for _, key := range []string{
		runv1.StorageKeyOutput, runv1.StorageKeyResult, runv1.StorageKeyState,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		put[key] = clusterv1.PresignedURL{
			URL:       b.storage.endpoint + "/" + b.storage.bucket + "/" + prefix + key + "?sig=fake",
			Method:    methodPUT,
			ExpiresAt: expires,
		}
	}
	get := map[string]clusterv1.PresignedURL{}
	for _, key := range []string{runv1.StorageKeyPrompt, runv1.StorageKeyState} {
		get[key] = clusterv1.PresignedURL{
			URL:       b.storage.endpoint + "/" + b.storage.bucket + "/" + prefix + key + "?sig=fake",
			Method:    methodGET,
			ExpiresAt: expires,
		}
	}
	return clusterv1.ArtifactBundle{
		Bucket:    b.storage.bucket,
		Endpoint:  b.storage.endpoint,
		KeyPrefix: prefix,
		Put:       put,
		Get:       get,
		Post: []clusterv1.PresignedPostPolicy{{
			Prefix:    prefix + runv1.StoragePrefixChunks,
			URL:       b.storage.endpoint + "/" + b.storage.bucket,
			Fields:    map[string]string{"key": prefix + runv1.StoragePrefixChunks + "${filename}"},
			ExpiresAt: expires,
		}},
		ExpiresAt: expires,
	}
}

const (
	methodPUT = "PUT"
	methodGET = "GET"
)

// applyObservation is the monotonicity engine: the table in section 5 of the
// contract, and the single implementation behind both the heartbeat and
// /ingest/status. Two implementations would drift, and the way that drift
// presents is a periodic heartbeat quietly rolling back state the fast path
// delivered.
func (b *Backend) applyObservation(clusterID runv1.ULID, obs clusterv1.RunObservation) clusterv1.StatusIngestResult {
	res := clusterv1.StatusIngestResult{RunID: obs.RunID, Accepted: false}

	r, ok := b.runs[obs.RunID]
	if !ok {
		res.Code = clusterv1.CodeRunNotFound
		res.Action = clusterv1.ActionAbandon
		return res
	}
	res.CurrentEpoch = r.epoch
	res.AppliedStatus = r.status

	switch {
	case obs.Epoch < r.epoch:
		// A zombie: it was disconnected, its work was reassigned, and it has
		// come back with news about a run that is no longer its own.
		res.Code = clusterv1.CodeEpochMismatch
		res.Action = clusterv1.ActionAbandon
		return res
	case obs.Epoch > r.epoch:
		// The backend never issued this epoch. Not a race — a defect.
		res.Code = clusterv1.CodeInvalidRequest
		res.Action = clusterv1.ActionFatal
		return res
	case r.holder != "" && r.holder != clusterID:
		res.Code = clusterv1.CodeRunLeasedByAnotherCluster
		res.Action = clusterv1.ActionAbandon
		return res
	case obs.Attempt < r.attempt:
		res.Code = clusterv1.CodeAttemptRegression
		res.Action = clusterv1.ActionAbandon
		return res
	}

	if obs.Attempt > r.attempt {
		// A new attempt is a fresh lifecycle: the phase rank starts over, or a
		// Pending from attempt 2 would lose to the Succeeded of attempt 1.
		r.attempt = obs.Attempt
		r.rank = 0
		r.phase = ""
		r.terminalPhase = ""
		r.attempts = append(r.attempts, AttemptRecord{
			Attempt: obs.Attempt, Epoch: r.epoch, Cluster: clusterID, StartedAt: b.now(),
		})
	}

	rank := obs.Phase.Rank()
	switch {
	case rank == 0:
		// A phase this build has never heard of. Ignoring it is the
		// compatibility rule: a newer controller must not be able to move a
		// run into a state this backend cannot reason about, and must not be
		// crashed for trying.
		res.Code = clusterv1.CodeInvalidRequest
		res.Action = clusterv1.ActionFatal
		return res

	case rank > r.rank:
		b.acceptPhase(r, clusterID, obs)
		res.Accepted = true

	case rank == r.rank && obs.Phase == r.phase:
		// An idempotent repeat: the same observation delivered twice, which
		// at-least-once delivery guarantees will happen.
		res.Accepted = true

	case rank == r.rank && r.phase.IsTerminal():
		// Two different endings. The first stands — reversing a terminal state
		// on a late message is how a succeeded run becomes failed in the UI
		// while its PR sits open — and the disagreement is recorded.
		b.auditf(r.id, AuditTerminalConflict,
			"cluster %s reported %s after %s", clusterID, obs.Phase, r.terminalPhase)
		res.Code = clusterv1.CodeRunTerminal
		res.Action = clusterv1.ActionAbandon

	default:
		// Almost always reordering within a batch rather than lost ownership,
		// so the controller is told to retry: the next heartbeat carries the
		// current state and settles it.
		res.Code = clusterv1.CodePhaseRegression
		res.Action = clusterv1.ActionRetry
	}

	res.AppliedStatus = r.status
	return res
}

func (b *Backend) acceptPhase(r *run, clusterID runv1.ULID, obs clusterv1.RunObservation) {
	r.phase = obs.Phase
	r.rank = obs.Phase.Rank()
	r.status = clusterv1.StatusForPhase(obs.Phase)
	r.holder = clusterID
	if obs.Reason != "" {
		r.reason = obs.Reason
	}
	if obs.Message != "" {
		r.message = obs.Message
	}
	if obs.FailureClass != "" {
		r.failureClass = obs.FailureClass
	}

	if !obs.Phase.IsTerminal() {
		return
	}
	r.terminalPhase = obs.Phase
	if r.completion == nil || r.completedAttempt != r.attempt {
		// The status arrived and the result did not. Not a failure: the pod
		// writes to storage before it calls back, so the contents exist and
		// this is a read the backend owes itself.
		r.status = clusterv1.StatusCompletedWithoutResult
		if report, ok := b.stored[r.id]; ok {
			b.applyCompletion(r, report)
		}
	}
}

// applyCompletion charges the run once and resolves it to its terminal status.
func (b *Backend) applyCompletion(r *run, report *runv1.CompletionReport) {
	r.completion = report
	r.completedAttempt = report.Attempt
	if report.Usage != nil && report.Usage.TotalCostUSD != "" {
		r.charges = append(r.charges, report.Usage.TotalCostUSD)
	}
	if report.FailureClass != "" {
		r.failureClass = report.FailureClass
	}
	if report.Reason != "" {
		r.reason = report.Reason
	}
	if report.Message != "" {
		r.message = report.Message
	}
	if r.terminalPhase != "" {
		r.status = clusterv1.StatusForPhase(r.terminalPhase)
	}
	delete(b.stored, r.id)
}

// pendingCommands is what the cluster holding this run should be told to do.
//
// Commands carry no acknowledgement on purpose: an acknowledgement would have
// to be stored and expired, and re-sending cancel to an already cancelled run
// costs nothing. So cancel is regenerated on every call until the phase is
// terminal, and the observed state is the acknowledgement.
func (b *Backend) pendingCommands(r *run) []clusterv1.Command {
	var out []clusterv1.Command
	if r.cancelRequested && !r.phase.IsTerminal() {
		issued := b.now()
		out = append(out, clusterv1.Command{
			Type:               clusterv1.CommandCancel,
			RunID:              r.id,
			Epoch:              r.epoch,
			Reason:             "cancelled by operator",
			IssuedAt:           &issued,
			GracePeriodSeconds: 30,
		})
	}
	out = append(out, r.injected...)
	r.injected = nil
	return out
}

func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
