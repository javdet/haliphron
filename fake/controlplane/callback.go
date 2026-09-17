package controlplane

import (
	"encoding/json"
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
