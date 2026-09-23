package backend

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

func (b *Backend) handleRegister(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r)
	if !ok {
		b.writeProblem(w, http.StatusUnauthorized, clusterv1.Problem{
			Title: "missing bootstrap token", Code: clusterv1.CodeBootstrapTokenInvalid,
			Action: clusterv1.ActionFatal,
		})
		return
	}

	var req clusterv1.RegisterRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	key, err := decodeKey(req.PublicKey.Key)
	if err != nil || len(key) != ed25519.PublicKeySize || req.PublicKey.Alg != clusterv1.KeyAlgorithm {
		b.badRequest(w, "public key must be a raw Ed25519 key, base64url without padding")
		return
	}
	if req.Name == "" || req.AgentNamespace == "" {
		b.badRequest(w, "name and agentNamespace are required")
		return
	}
	if req.CapacitySlots > clusterv1.MaxCapacitySlots {
		b.badRequest(w, fmt.Sprintf("capacitySlots exceeds the maximum of %d", clusterv1.MaxCapacitySlots))
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	spent, known := b.tokens[token]
	if !known {
		b.writeProblem(w, http.StatusUnauthorized, clusterv1.Problem{
			Title: "unknown bootstrap token", Code: clusterv1.CodeBootstrapTokenInvalid,
			Action: clusterv1.ActionFatal,
		})
		return
	}
	if spent != nil {
		// Already used. If the same key is asking, this is the crashed-before-
		// persisting case and the same answer is owed; any other key is a
		// stolen token being replayed.
		if spent.kid != req.PublicKey.KID || spent.key != req.PublicKey.Key {
			b.writeProblem(w, http.StatusUnauthorized, clusterv1.Problem{
				Title: "bootstrap token already spent by another key",
				Code:  clusterv1.CodeBootstrapTokenConsumed, Action: clusterv1.ActionFatal,
			})
			return
		}
		c := b.clusters[spent.clusterID]
		b.logf("register repeat cluster=%s", c.id)
		b.writeSecret(w, http.StatusOK, b.registerResponse(c))
		return
	}

	for _, c := range b.clusters {
		if c.name == req.Name {
			b.writeProblem(w, http.StatusConflict, clusterv1.Problem{
				Title: "cluster name taken", Code: clusterv1.CodeClusterNameTaken,
				Action: clusterv1.ActionFatal,
			})
			return
		}
	}

	c := &cluster{
		id:             b.ids.next(b.now()),
		name:           req.Name,
		labels:         req.Labels,
		kid:            req.PublicKey.KID,
		key:            key,
		runtimes:       req.Runtimes,
		agentNamespace: req.AgentNamespace,
		version:        req.ControllerVersion,
		capacitySlots:  req.CapacitySlots,
	}
	b.clusters[c.id] = c
	b.byKID[c.kid] = c.id
	b.tokens[token] = &spentToken{kid: c.kid, key: req.PublicKey.Key, clusterID: c.id}
	b.logf("registered cluster=%s name=%s", c.id, c.name)

	b.writeSecret(w, http.StatusOK, b.registerResponse(c))
}

func (b *Backend) registerResponse(c *cluster) clusterv1.RegisterResponse {
	return clusterv1.RegisterResponse{
		ClusterID:                   c.id,
		Name:                        c.name,
		KeyID:                       c.kid,
		TokenAudience:               clusterv1.TokenAudience,
		TokenMaxTTLSeconds:          clusterv1.TokenMaxTTLSeconds,
		Timings:                     b.timings,
		SupportedControllerVersions: b.versions,
		ServerTime:                  b.now(),
	}
}

// ---------------------------------------------------------------------------
// leasing
// ---------------------------------------------------------------------------

func (b *Backend) handleLeases(w http.ResponseWriter, r *http.Request) {
	c, ok := b.authenticate(w, r, runv1.ULID(r.PathValue("clusterID")))
	if !ok {
		return
	}
	var req clusterv1.LeaseRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}

	b.mu.Lock()
	if b.faults.dropLongPolls {
		b.mu.Unlock()
		// No status, no body: the connection simply goes away, the way it does
		// through a proxy with a shorter read timeout or during a rolling
		// restart. A controller that treats this as a fatal error will spin.
		panic(http.ErrAbortHandler)
	}
	wait := req.WaitSeconds
	if wait <= 0 || wait > b.timings.MaxWaitSeconds {
		wait = b.timings.MaxWaitSeconds
	}
	c.freeSlots = req.FreeSlots
	if req.CapacitySlots > 0 && req.CapacitySlots <= clusterv1.MaxCapacitySlots {
		c.capacitySlots = req.CapacitySlots
	}
	b.mu.Unlock()

	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		// The wake channel is captured under the same lock as the check for
		// work. Fetching it afterwards loses a broadcast that lands in
		// between, and the symptom is a long poll that sits out its full wait
		// while work is queued — which reads as latency, not as a bug.
		b.mu.Lock()
		wake := b.wake
		b.sweep()
		limit := int(req.FreeSlots)
		if max := int(b.timings.MaxLeasesPerPoll); limit > max {
			limit = max
		}
		if headroom := b.headroom(c); limit > headroom {
			limit = headroom
		}
		leases := b.issue(c, limit, req.Runtimes)
		b.mu.Unlock()

		if len(leases) > 0 {
			b.mu.Lock()
			// The body carries a git token, a model key and presigned URLs.
			// Identifiers and a count, never the body — at any level.
			b.logf("leases issued cluster=%s count=%d", c.id, len(leases))
			b.mu.Unlock()
			b.writeSecret(w, http.StatusOK, clusterv1.LeaseResponse{
				Leases: leases, ServerTime: b.now(),
			})
			return
		}

		select {
		case <-wake:
			// Something changed; look again.
		case <-deadline:
			// No work and the poll expired normally. 204 rather than an empty
			// list so the controller can re-poll at once without parsing, and
			// without backoff: backoff here turns a long poll into polling.
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (b *Backend) handleAck(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.PathValue("runID"))
	var req clusterv1.AckRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	c, ok := b.authenticate(w, r, req.ClusterID)
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	r2, ok := b.runs[runID]
	if !ok {
		b.runNotFound(w, runID)
		return
	}
	if req.Epoch != r2.epoch {
		b.epochProblem(w, r2, req.Epoch)
		return
	}
	if r2.holder != c.id {
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "run is leased by another cluster", Code: clusterv1.CodeRunLeasedByAnotherCluster,
			Action: clusterv1.ActionAbandon, RunID: runID, CurrentEpoch: r2.epoch,
		})
		return
	}

	if !req.IsAccepted() {
		b.rejectAck(w, r2, c, req)
		return
	}

	// Idempotent on (runID, epoch): a repeat changes nothing and answers the
	// same. The controller retries this call after a restart, and a second ack
	// must not look like a second dispatch.
	if !r2.acked {
		r2.acked = true
		r2.status = clusterv1.StatusDispatched
		r2.leaseDeadline = b.now().Add(time.Duration(b.timings.LeaseTTLSeconds) * time.Second)
		b.logf("ack run=%s epoch=%d cluster=%s", runID, r2.epoch, c.id)
	}

	b.writeJSON(w, http.StatusOK, clusterv1.AckResponse{
		RunID: runID, Epoch: r2.epoch, Status: r2.status,
		LeaseDeadline: r2.leaseDeadline,
		// Anything that queued up while the controller was materialising goes
		// out now: a cancellation that arrived in that window would otherwise
		// wait a full heartbeat, having started a Job that must be killed.
		Commands: b.pendingCommands(r2),
	})
}

// rejectAck handles "I cannot materialise this". The work never started, so
// there is nothing to clean up and nothing was spent — the whole reason a
// negative ack exists instead of letting the run burn.
func (b *Backend) rejectAck(w http.ResponseWriter, r2 *run, c *cluster, req clusterv1.AckRequest) {
	reason := "materialization refused"
	if req.Rejection != nil {
		reason = string(req.Rejection.Code) + ": " + req.Rejection.Message
	}
	r2.excluded[c.id] = true
	b.logf("ack rejected run=%s cluster=%s reason=%q", r2.id, c.id, reason)

	if b.everyClusterExcluded(r2) {
		// Queueing forever would be the silent failure: nobody can run this,
		// and the operator needs to be told why rather than watch it sit.
		r2.epoch++
		r2.status = clusterv1.StatusFailed
		r2.holder = ""
		r2.acked = false
		r2.failureClass = runv1.FailureConfig
		r2.reason = "NoEligibleCluster"
		r2.message = reason
		r2.terminalPhase = runv1.PhaseFailed
		b.auditf(r2.id, AuditNoClusterForRun, "%s", reason)
	} else {
		b.requeue(r2, reason)
	}

	b.writeJSON(w, http.StatusOK, clusterv1.AckResponse{
		RunID: r2.id, Epoch: r2.epoch, Status: r2.status, LeaseDeadline: r2.leaseDeadline,
	})
}

func (b *Backend) everyClusterExcluded(r2 *run) bool {
	eligible := 0
	for id, c := range b.clusters {
		if c.revoked || r2.excluded[id] {
			continue
		}
		eligible++
	}
	return eligible == 0
}

func (b *Backend) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.PathValue("runID"))
	var req clusterv1.ArtifactBundleRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	c, ok := b.authenticate(w, r, "")
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	r2, ok := b.runs[runID]
	if !ok {
		b.runNotFound(w, runID)
		return
	}
	if req.Epoch != r2.epoch {
		b.epochProblem(w, r2, req.Epoch)
		return
	}
	if r2.holder != c.id {
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "run is leased by another cluster", Code: clusterv1.CodeRunLeasedByAnotherCluster,
			Action: clusterv1.ActionAbandon, RunID: runID, CurrentEpoch: r2.epoch,
		})
		return
	}

	// Deliberately not idempotent: the point of the call is a later expiry
	// than the one the caller already has.
	bundle := b.mintBundle(r2)
	b.logf("artifact bundle minted run=%s epoch=%d attempt=%d", runID, r2.epoch, req.Attempt)
	b.writeSecret(w, http.StatusOK, bundle)
}

// ---------------------------------------------------------------------------
// heartbeat
// ---------------------------------------------------------------------------

func (b *Backend) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	c, ok := b.authenticate(w, r, runv1.ULID(r.PathValue("clusterID")))
	if !ok {
		return
	}
	var req clusterv1.HeartbeatRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	if len(req.Runs) > clusterv1.MaxHeartbeatRuns {
		b.badRequest(w, "runs exceeds the maximum of 500")
		return
	}
	if req.CapacitySlots > clusterv1.MaxCapacitySlots {
		b.badRequest(w, fmt.Sprintf("capacitySlots exceeds the maximum of %d", clusterv1.MaxCapacitySlots))
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	now := b.now()
	c.lastHeartbeat = now
	c.freeSlots = req.FreeSlots
	if req.CapacitySlots > 0 {
		c.capacitySlots = req.CapacitySlots
	}
	if req.Cluster != nil {
		c.quotaExhausted = req.Cluster.QuotaExhausted
		if len(req.Cluster.Runtimes) > 0 {
			c.runtimes = req.Cluster.Runtimes
		}
	}

	resp := clusterv1.HeartbeatResponse{ServerTime: now, Leases: []clusterv1.LeaseRenewal{}, Commands: []clusterv1.Command{}}
	mentioned := map[runv1.ULID]bool{}

	// The same code as /ingest/status, on purpose: a heartbeat is a batch of
	// observations plus a reconciliation, and two implementations of the
	// monotonicity rules would drift until the slow path started undoing the
	// fast one.
	for _, obs := range req.Runs {
		mentioned[obs.RunID] = true
		res := b.applyObservation(c.id, obs)
		if !res.Accepted {
			resp.Observations = append(resp.Observations, res)
			continue
		}
		r2 := b.runs[obs.RunID]
		if r2.phase.IsTerminal() {
			continue
		}
		r2.leaseDeadline = now.Add(time.Duration(b.timings.LeaseTTLSeconds) * time.Second)
		resp.Leases = append(resp.Leases, clusterv1.LeaseRenewal{
			RunID: r2.id, Epoch: r2.epoch, LeaseDeadline: r2.leaseDeadline,
		})
	}

	for _, r2 := range b.runs {
		if r2.holder != c.id || r2.phase.IsTerminal() {
			continue
		}
		if cmds := b.pendingCommands(r2); len(cmds) > 0 {
			resp.Commands = append(resp.Commands, cmds...)
		}
		// Only under reportComplete does silence mean anything. Without the
		// flag, "I do not have this run" and "I have not told you yet" are the
		// same message, and reacting to the second is how a warming controller
		// gets its work taken away.
		if req.ReportComplete && !mentioned[r2.id] && r2.acked {
			resp.UnknownRuns = append(resp.UnknownRuns, r2.id)
		}
	}

	b.logf("heartbeat cluster=%s runs=%d complete=%t unknown=%d",
		c.id, len(req.Runs), req.ReportComplete, len(resp.UnknownRuns))
	b.writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// ingest
// ---------------------------------------------------------------------------

func (b *Backend) handleIngestStatus(w http.ResponseWriter, r *http.Request) {
	var req clusterv1.StatusIngestRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	c, ok := b.authenticate(w, r, req.ClusterID)
	if !ok {
		return
	}
	if len(req.Reports) == 0 || len(req.Reports) > clusterv1.MaxStatusReports {
		b.badRequest(w, "reports must hold between 1 and 100 items")
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	resp := clusterv1.StatusIngestResponse{Results: make([]clusterv1.StatusIngestResult, 0, len(req.Reports))}
	for _, obs := range req.Reports {
		// Row by row: partial success is the normal outcome of a batch, and
		// failing the whole request over one stale row would lose the rest.
		resp.Results = append(resp.Results, b.applyObservation(c.id, obs))
	}
	b.logf("ingest status cluster=%s rows=%d", c.id, len(req.Reports))
	b.writeJSON(w, http.StatusOK, resp)
}

func (b *Backend) handleIngestCompletion(w http.ResponseWriter, r *http.Request) {
	var req clusterv1.CompletionIngestRequest
	if err := decode(r, &req); err != nil {
		b.badRequest(w, "malformed body")
		return
	}
	if _, ok := b.authenticate(w, r, req.ClusterID); !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	r2, ok := b.runs[req.RunID]
	if !ok {
		b.runNotFound(w, req.RunID)
		return
	}
	if req.Epoch != r2.epoch {
		b.epochProblem(w, r2, req.Epoch)
		return
	}
	if req.Completion.RunID != "" && req.Completion.RunID != req.RunID {
		// The report names a different run than the envelope. The duplication
		// exists so completion.json can be read from storage without an
		// envelope; a mismatch means the wrong object was read.
		b.badRequest(w, "completion.runID does not match the envelope")
		return
	}

	// Idempotent on (runID, attempt). The cost is charged once: this call is
	// retried on any network error, and a sum that grows per retry is a bill
	// that grows per retry.
	if r2.completion != nil && r2.completedAttempt == req.Attempt {
		b.writeJSON(w, http.StatusOK, clusterv1.CompletionIngestResponse{
			RunID: req.RunID, Accepted: true, Duplicate: true, AppliedStatus: r2.status,
		})
		return
	}

	report := req.Completion
	b.applyCompletion(r2, &report)
	b.auditUsage(r2, req)
	b.logf("ingest completion run=%s attempt=%d status=%s", req.RunID, req.Attempt, report.Status)

	b.writeJSON(w, http.StatusOK, clusterv1.CompletionIngestResponse{
		RunID: req.RunID, Accepted: true, AppliedStatus: r2.status,
		Commands: b.pendingCommands(r2),
	})
}

// auditUsage records the pod's self-declared duration against the window the
// controller observed. The phase 1 mitigation for self-reported cost is not a
// block — a wrong threshold would refuse honest runs — it is a record an
// operator can go and read.
func (b *Backend) auditUsage(r2 *run, req clusterv1.CompletionIngestRequest) {
	if req.Completion.Usage == nil || len(r2.attempts) == 0 {
		return
	}
	observed := b.now().Sub(r2.attempts[len(r2.attempts)-1].StartedAt)
	declared := time.Duration(req.Completion.Usage.DurationMs) * time.Millisecond
	if declared > observed+time.Minute {
		b.auditf(r2.id, AuditUsageDivergence,
			"declared %s, observed at most %s", declared, observed)
	}
}

// ---------------------------------------------------------------------------
// shared problem responses
// ---------------------------------------------------------------------------

func (b *Backend) badRequest(w http.ResponseWriter, detail string) {
	b.writeProblem(w, http.StatusBadRequest, clusterv1.Problem{
		Title: "invalid request", Detail: detail,
		Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
	})
}

func (b *Backend) runNotFound(w http.ResponseWriter, id runv1.ULID) {
	b.writeProblem(w, http.StatusNotFound, clusterv1.Problem{
		Title: "unknown run", Code: clusterv1.CodeRunNotFound,
		Action: clusterv1.ActionAbandon, RunID: id,
	})
}

// epochProblem answers the two epoch failures, which look alike and are not.
// Lower means the work moved on without this controller; higher means the
// backend never issued it, which no retry can fix.
func (b *Backend) epochProblem(w http.ResponseWriter, r2 *run, got int64) {
	if got > r2.epoch {
		b.writeProblem(w, http.StatusBadRequest, clusterv1.Problem{
			Title: "epoch was never issued", Code: clusterv1.CodeInvalidRequest,
			Action: clusterv1.ActionFatal, RunID: r2.id,
			CurrentEpoch: r2.epoch, CurrentStatus: r2.status,
		})
		return
	}
	b.writeProblem(w, http.StatusConflict, clusterv1.Problem{
		Title: "stale epoch", Code: clusterv1.CodeEpochMismatch,
		Action: clusterv1.ActionAbandon, RunID: r2.id,
		CurrentEpoch: r2.epoch, CurrentStatus: r2.status,
	})
}

// handleIngestArtifacts accepts one relayed object.
//
// The fake keeps the bytes in a map rather than writing them anywhere. What a
// controller track needs to assert is that the object arrived under the right
// key, with the digest the controller claimed, exactly once — and none of that
// needs a filesystem. The real backend writes to a volume; the difference is
// below the property under test.
func (b *Backend) handleIngestArtifacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID := runv1.ULID(q.Get(clusterv1.QueryRunID))
	key := q.Get(clusterv1.QueryKey)
	epoch, _ := strconv.ParseInt(q.Get(clusterv1.QueryEpoch), 10, 64)

	if runID == "" || key == "" {
		b.badRequest(w, "an artifact must name its run and its key")
		return
	}
	// The cluster comes from the token rather than from a parameter. Every
	// other endpoint cross-checks a decoded body; here there is no body to
	// read it from, and taking it from the query would let a caller name a
	// cluster it cannot sign for.
	c, ok := b.authenticate(w, r, "")
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			b.writeProblem(w, http.StatusRequestEntityTooLarge, clusterv1.Problem{
				Title: "the artifact exceeds 256 MiB", Code: clusterv1.CodePayloadTooLarge,
				Action: clusterv1.ActionFatal, RunID: runID,
			})
			return
		}
		b.badRequest(w, "the artifact body could not be read")
		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if want := r.Header.Get(clusterv1.HeaderArtifactSHA256); want != "" && !strings.EqualFold(want, digest) {
		// Retry rather than fatal: the likely cause is a connection cut
		// mid-transfer, and the controller still holds the whole object.
		b.writeProblem(w, http.StatusBadRequest, clusterv1.Problem{
			Title:  "the artifact does not match its digest",
			Detail: fmt.Sprintf("%s hashes to %s and the controller said %s", key, digest, want),
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionRetry, RunID: runID,
		})
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()

	run, ok := b.runs[runID]
	if !ok {
		b.runNotFound(w, runID)
		return
	}
	if epoch != run.epoch {
		// A cluster that lost this run must not overwrite the result of the
		// cluster that now holds it.
		b.epochProblem(w, run, epoch)
		return
	}
	if run.holder != "" && run.holder != c.id {
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "run is leased by another cluster", Code: clusterv1.CodeRunLeasedByAnotherCluster,
			Action: clusterv1.ActionAbandon, RunID: runID, CurrentEpoch: run.epoch,
		})
		return
	}

	// The run prefix is stamped here, from the envelope this call
	// authenticated, exactly as the controller stamped it from the CR. The
	// producer of the bytes never gets to choose the prefix they land under.
	full := "runs/" + string(runID) + "/" + strings.TrimPrefix(key, "/")
	_, duplicate := b.artifacts[full]
	b.artifacts[full] = body
	if !run.sawArtifact {
		run.sawArtifact = true
		run.artifactsFirst = run.completion == nil
	}
	b.logf("artifact relayed run=%s key=%s bytes=%d", runID, key, len(body))

	b.writeJSON(w, http.StatusOK, clusterv1.ArtifactIngestResponse{
		RunID:     runID,
		Duplicate: duplicate,
		Ref: runv1.ObjectRef{
			Key:         full,
			SizeBytes:   int64(len(body)),
			SHA256:      digest,
			ContentType: r.Header.Get("Content-Type"),
			Uploaded:    true,
		},
	})
}

// Artifact is what a controller relayed under one key, for a test to assert on.
// The key is relative to the run, as the pod names it.
func (b *Backend) Artifact(id runv1.ULID, key string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.artifacts["runs/"+string(id)+"/"+key]
	return body, ok
}

// ArtifactPrecededCompletion reports whether the run's first artifact reached
// the control plane before its completion did.
//
// The property, not the timestamps: what matters is that a report never arrives
// describing objects the backend does not have, and the controller's flush
// order — artifacts, then completions, then observations — is what guarantees
// it.
func (b *Backend) ArtifactPrecededCompletion(id runv1.ULID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	run, ok := b.runs[id]
	return ok && run.artifactsFirst
}

// Artifacts lists what a controller has relayed for a run, by relative key.
func (b *Backend) Artifacts(id runv1.ULID) map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := "runs/" + string(id) + "/"
	out := map[string][]byte{}
	for key, body := range b.artifacts {
		if strings.HasPrefix(key, prefix) {
			out[strings.TrimPrefix(key, prefix)] = body
		}
	}
	return out
}
