package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The controller's side of the runtime API: one endpoint, one report, and four
// ways to say no. The image is obliged to distinguish them — a 401 and a 503
// look the same from a distance and mean opposite things about whether to try
// again — and this is where that distinction is made observable.
//
// The rule the endpoint exists to enforce: without a token, any pod in the
// agents namespace could post a forged completion for somebody else's run.
// So the token is checked, the run it is bound to is checked, and the runID in
// the body is checked against both.

// maxReportBytes is the controller's body ceiling. The image's answer to 413 is
// to truncate summary and retry once, which is a behaviour and therefore needs
// a fake that actually refuses something.
const maxReportBytes = 1 << 20

func (c *ControlPlane) handleCompletion(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReportBytes+1))
	if err != nil {
		c.callbackError(w, http.StatusBadRequest, "MalformedBody", err.Error())
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.faults.callbackAttempts++
	attempt := c.faults.callbackAttempts

	// An injected failure is answered before anything else is looked at, so a
	// test can make the controller unreachable without also having to make the
	// report wrong.
	if status := c.callbackFault(); status != 0 {
		c.logf("completion attempt %d -> injected %d", attempt, status)
		c.callbackError(w, status, "Injected", "injected fault")
		return
	}

	if len(body) > maxReportBytes {
		c.logf("completion attempt %d -> 413 (%d bytes)", attempt, len(body))
		c.callbackError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge",
			"report exceeds 1 MiB; truncate summary")
		return
	}

	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		c.logf("completion attempt %d -> 401 no token", attempt)
		c.callbackError(w, http.StatusUnauthorized, "TokenMissing", "no bearer token")
		return
	}
	boundTo, known := c.byToken[token]
	if !known {
		c.logf("completion attempt %d -> 401 unknown token", attempt)
		c.callbackError(w, http.StatusUnauthorized, "TokenMismatch", "token is not one this controller minted")
		return
	}

	var report runv1.CompletionReport
	if err := json.Unmarshal(body, &report); err != nil {
		c.logf("completion attempt %d -> 400 %v", attempt, err)
		c.callbackError(w, http.StatusBadRequest, "MalformedBody", err.Error())
		return
	}
	// The third check, and the one a fake is tempted to skip: a valid token for
	// run A must not carry a report about run B.
	if report.RunID != boundTo {
		c.logf("completion attempt %d -> 409 token bound to %s, body says %s", attempt, boundTo, report.RunID)
		c.callbackError(w, http.StatusConflict, "RunMismatch",
			"token is bound to "+string(boundTo)+", body reports "+string(report.RunID))
		return
	}

	state := c.runs[boundTo]
	received := ReceivedReport{
		Report:      report,
		Body:        append([]byte(nil), body...),
		Attempt:     attempt,
		ReceivedAt:  c.now(),
		Idempotency: r.Header.Get("Idempotency-Key"),
		Status:      http.StatusAccepted,
	}

	// Idempotent on (runID, attempt), as the contract says the receiver must
	// be. The repeat is recorded rather than dropped: an image that sends the
	// same report twice is behaving correctly under at-least-once delivery, and
	// a test that cannot see the repeat cannot tell that apart from a second
	// run of the agent.
	for _, prev := range state.reports {
		if prev.Status == http.StatusAccepted && prev.Report.Attempt == report.Attempt {
			received.Duplicate = true
			break
		}
	}
	// A report for an attempt older than one already accepted is not a
	// duplicate, it is a message from the past, and accepting it would move the
	// run backwards.
	for _, prev := range state.reports {
		if prev.Status == http.StatusAccepted && prev.Report.Attempt > report.Attempt {
			received.Status = http.StatusConflict
			state.reports = append(state.reports, received)
			c.logf("completion attempt %d -> 409 stale (attempt %d after %d)",
				attempt, report.Attempt, prev.Report.Attempt)
			c.callbackError(w, http.StatusConflict, "StaleAttempt", "a later attempt has already reported")
			return
		}
	}

	state.reports = append(state.reports, received)
	c.logf("completion attempt %d -> 202 run=%s runAttempt=%d status=%s exit=%d duplicate=%t",
		attempt, report.RunID, report.Attempt, report.Status, report.ExitCode, received.Duplicate)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{
		"accepted": true, "duplicate": received.Duplicate,
	})
}

func (c *ControlPlane) callbackError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// writeCallbackJSON answers a callback with a JSON body.
func (c *ControlPlane) writeCallbackJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		c.logf("writing a callback response: %v", err)
	}
}

// bearer extracts the token, case-insensitively on the scheme as RFC 7235
// requires. An image that sends "bearer" lowercase is not wrong, and a fake
// that rejects it sends its author hunting for a bug that is the fake's.
func bearer(header string) string {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// Reports returns every delivery the fake saw for a run, in arrival order.
func (c *ControlPlane) Reports(id runv1.ULID) []ReceivedReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.runs[id]
	if !ok {
		return nil
	}
	return append([]ReceivedReport(nil), state.reports...)
}

// AcceptedReport returns the last report the fake accepted for a run.
func (c *ControlPlane) AcceptedReport(id runv1.ULID) (runv1.CompletionReport, bool) {
	for _, r := range reverse(c.Reports(id)) {
		if r.Status == http.StatusAccepted {
			return r.Report, true
		}
	}
	return runv1.CompletionReport{}, false
}

// CallbackAttempts counts every delivery the endpoint saw, injected failures
// included. It is how a test checks that the image gave up inside its sixty
// second budget instead of retrying forever.
func (c *ControlPlane) CallbackAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.faults.callbackAttempts
}

// WaitForReport blocks until a run has an accepted report or the deadline
// passes. The webhook arrives from the pod's own process, so a test that
// asserts immediately after `docker run` returns is racing the network, not the
// image.
func (c *ControlPlane) WaitForReport(id runv1.ULID, within time.Duration) (runv1.CompletionReport, bool) {
	deadline := time.Now().Add(within)
	for {
		if report, ok := c.AcceptedReport(id); ok {
			return report, true
		}
		if time.Now().After(deadline) {
			return runv1.CompletionReport{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reverse[T any](s []T) []T {
	out := make([]T, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// handlePhase records one entrypoint phase the pod got through.
//
// This is what replaced the image writing state.json, and a fake that did not
// serve it would let the image ship without the one path that makes an
// idempotent retry possible. What a test asserts on is Phases: that the phases
// the image claims to have completed are the ones it actually completed, in the
// order the contract fixes.
func (c *ControlPlane) handlePhase(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		c.callbackError(w, http.StatusBadRequest, "MalformedBody", err.Error())
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	run, ok := c.authenticatedRun(w, r)
	if !ok {
		return
	}

	var report runv1.PhaseReport
	if err := json.Unmarshal(body, &report); err != nil {
		c.callbackError(w, http.StatusBadRequest, "MalformedBody", err.Error())
		return
	}
	if report.RunID != run.id {
		c.callbackError(w, http.StatusConflict, "RunMismatch",
			"the token is bound to "+string(run.id)+" and the report names "+string(report.RunID))
		return
	}

	// Only an ok outcome goes on the record, as in the real controller: a
	// failed or skipped phase is not something a later attempt may assume was
	// done, and the one phase whose replay costs money is exactly where getting
	// that wrong would skip a model call that never happened.
	if report.Outcome == runv1.PhaseOutcomeOK {
		run.phases = append(run.phases, report.Phase)
		c.logf("phase %s reported by run %s", report.Phase, run.id)
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleRelay accepts one artifact in relay mode.
//
// It answers what a controller answers, including the two refusals the image
// has to distinguish: a 413 when the run has spent its artifact budget, which
// the image drops the object over and carries on, and a 409 when the run is
// configured for object storage and should not be relaying at all, which is a
// defect a retry repeats.
//
// The run prefix is stamped here, from the token this call authenticated —
// exactly as the real controller stamps it from the CR. A pod that could name
// an absolute key could name another run's.
func (c *ControlPlane) handleRelay(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get(runv1.QueryKey)
	if key == "" {
		c.callbackError(w, http.StatusBadRequest, "MissingKey", "an artifact must name its key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxObjectBytes+1))
	if err != nil {
		c.callbackError(w, http.StatusBadRequest, "MalformedBody", err.Error())
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	run, ok := c.authenticatedRun(w, r)
	if !ok {
		return
	}
	if c.artifactModeOrRelay() == runv1.ArtifactModeObjectStore {
		c.callbackError(w, http.StatusConflict, "ArtifactModeMismatch",
			"this run uploads to object storage, not to the controller")
		return
	}
	// Storage faults, not callback faults. FailCallback is about the completion
	// webhook, and a relayed artifact is the pod's *write* path — the same path
	// a presigned PUT is in the other mode. Keeping one control for both is
	// what lets a checklist row like "a refused upload is infra, not config"
	// run unchanged in either mode, which is the whole point of the port.
	if status, code := c.storageFault(http.MethodPut, key); status != 0 {
		c.logf("relay of %s -> injected %d", key, status)
		c.callbackError(w, status, code, "injected fault")
		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	// Verified before anything is stored, as the real relay does: a truncated
	// transfer under the right key is worse than none, because the recovery
	// path would read it and believe it.
	if want := r.Header.Get(runv1.HeaderSHA256); want != "" && !strings.EqualFold(want, digest) {
		c.callbackError(w, http.StatusBadRequest, "DigestMismatch",
			"the body hashes to "+digest+" and the pod said "+want)
		return
	}

	if c.maxBytesPerRun > 0 && run.relayed+int64(len(body)) > c.maxBytesPerRun {
		c.logf("run %s reached its artifact budget at %d bytes", run.id, run.relayed)
		c.callbackError(w, http.StatusRequestEntityTooLarge, "ArtifactBudgetSpent",
			"this run has spent its artifact budget")
		return
	}
	run.relayed += int64(len(body))

	// The same map the presigned path writes into, under the same key. That is
	// the point of the fake serving both halves: a test asserts on what was
	// stored without knowing which mode put it there, which is exactly the
	// property the port is supposed to give.
	full := fmt.Sprintf(runv1.StoragePrefixRun, run.id) + strings.TrimPrefix(key, "/")
	c.store(full, body, r.Header.Get("Content-Type"))
	c.logf("relayed %s (%d bytes) for run %s", full, len(body), run.id)

	c.writeCallbackJSON(w, http.StatusOK, runv1.ArtifactAck{
		Ref: runv1.ObjectRef{
			Key:         full,
			SizeBytes:   int64(len(body)),
			SHA256:      digest,
			ContentType: r.Header.Get("Content-Type"),
			Uploaded:    true,
		},
		BytesRemaining: c.remainingBudget(run),
	})
}

// authenticatedRun resolves the bearer token to the run it was minted for.
//
// The check is the same for all three callback paths and is shared for the
// reason the real controller shares it: a phase endpoint that authenticated by
// namespace rather than by run would let any pod mark another run's expensive
// phase as done, and that run's next attempt would skip its model call. The
// caller holds the lock.
func (c *ControlPlane) authenticatedRun(w http.ResponseWriter, r *http.Request) (*runState, bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		c.callbackError(w, http.StatusUnauthorized, "TokenMissing", "no bearer token")
		return nil, false
	}
	id, known := c.byToken[token]
	if !known {
		c.callbackError(w, http.StatusUnauthorized, "TokenMismatch",
			"token is not one this controller minted")
		return nil, false
	}
	run, ok := c.runs[id]
	if !ok {
		c.callbackError(w, http.StatusConflict, "RunNotFound", "no such run in this cluster")
		return nil, false
	}
	return run, true
}

func (c *ControlPlane) remainingBudget(run *runState) int64 {
	if c.maxBytesPerRun <= 0 {
		return 0
	}
	if left := c.maxBytesPerRun - run.relayed; left > 0 {
		return left
	}
	return 0
}

// Phases is what a run reported getting through, in the order it reported them.
// This is the assertion surface that replaced reading state.json out of the
// bucket.
func (c *ControlPlane) Phases(id runv1.ULID) []runv1.RuntimePhase {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, ok := c.runs[id]
	if !ok {
		return nil
	}
	return append([]runv1.RuntimePhase(nil), run.phases...)
}
