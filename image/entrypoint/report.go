package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The completion report, and why failing to deliver it is not a failure.
//
// It leaves in two copies: as a webhook to the controller, and as the
// runs/{runID}/completion.json object. Byte for byte identical — either may
// turn out to be the one that survived. The durable copy costs one upload and
// closes the one scenario in which the cost and the pull request link are lost
// for good: the controller crashed between accepting the webhook and forwarding
// it, and neither result.md nor output.json carries either field.
//
// Both copies go to the controller in relay mode, on different paths and with
// different meanings. The artifact upload is acknowledged when the bytes are
// durable, so it is the copy that survives; the webhook is a notification the
// pod may fail to deliver without failing the run.

// buildReport assembles what this pod says happened.
//
// Nothing in it is authoritative. It is produced by the least trusted component
// in the system: the cost and the token counts come out of the agent CLI, and a
// compromised agent can understate them. The backend cross-checks the duration
// against the Job duration the controller observed; metering at a proxy in front
// of the model is the real answer and is not in v1.
func (r *Run) buildReport() *runv1.CompletionReport {
	report := &runv1.CompletionReport{
		RunID:    r.cfg.RunID,
		Attempt:  r.cfg.Attempt,
		Status:   r.completionStatus(),
		ExitCode: r.exitCode(),
		Agent:    r.cfg.Agent,
		Model:    r.cfg.Model,
		Runtime: &runv1.RuntimeInfo{
			ImageVersion:    r.cfg.ImageVersion,
			ContractVersion: runv1.ContractVersion,
			AgentVersion:    r.agentVersion(),
		},
		ResultRef: r.resultRef,
		OutputRef: r.outputRef,
		LogRef:    r.logRef,
		// What this attempt and every earlier one got through. The per-phase
		// reports are the primary channel and this is the copy that survives a
		// pod whose last few reports did not get through — a summary rather
		// than the only record, which is why losing one costs a repeated phase
		// and not a repeated run.
		CompletedPhases: r.checkpoint.Completed(),
		// Everything up to but not including finalize, because this runs
		// inside finalize. A report cannot carry the duration of the phase
		// that is assembling it, nor of the one that will send it, and the
		// alternative — sending the report before it is complete — trades a
		// fact nobody needs for the one thing the report exists to deliver.
		// The checkpoint, written a few lines later, does have all eighteen.
		PhaseTimings: r.timings,
	}
	report.FailureClass = runv1.FailureClassForExitCode(report.ExitCode)

	if r.failure != nil && r.failure.Phase != runv1.RuntimePhaseNotify {
		// The three fields that are the difference between "Failed, config" in
		// the UI and a user who can fix their run. The evidence exists once, in
		// this pod, at the moment of failure.
		report.FailedPhase = r.failure.Phase
		report.Reason = r.failure.Reason
		report.Message = r.redactor.String(r.failure.Message())
	}

	// The first 64 KiB of result.md, redacted like everything else that leaves.
	// A secret that reached the summary would otherwise travel to the backend
	// and into the run list in the UI.
	report.Summary = truncateSummary(r.redactor.String(r.summary))

	if r.cfg.HasRepo() {
		repo := r.repo
		report.Repo = &repo
	}
	if r.agent.Usage != nil {
		report.Usage = r.agent.Usage
	}
	return report
}

// completionStatus is the pod's own verdict. It is derivable from the exit code
// and is sent anyway, for one reason: cancellation and eviction are
// indistinguishable by code — both arrive as signal 143 — and only this process
// knows which of them it was, unless the controller was the one that asked.
func (r *Run) completionStatus() runv1.CompletionStatus {
	switch {
	case r.cancelled:
		return runv1.CompletionCancelled
	case r.failure == nil, r.failure.Phase == runv1.RuntimePhaseNotify:
		return runv1.CompletionSuccess
	case r.failure.Code == runv1.ExitAgentTimeout:
		return runv1.CompletionTimeout
	default:
		return runv1.CompletionFailure
	}
}

// agentVersion asks the CLI what it is. Without it, a behaviour change across a
// fleet of clusters on different image tags is diagnosed by guesswork.
func (r *Run) agentVersion() string {
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := r.commander.Run(ctx, Command{
		Path: string(r.cfg.Agent), Args: []string{"--version"},
		Dir: r.layout.Workspace, Env: r.agentEnv, Stdout: &out, Stderr: io.Discard,
	})
	if err != nil || result.ExitCode != 0 {
		return ""
	}
	return truncate(lastNonEmptyLine(out.String()), 64)
}

// Delivery budget. Five attempts, exponential backoff, sixty seconds in total —
// and then the pod gives up and exits with its own code rather than a delivery
// error.
const (
	notifyAttempts = 5
	notifyBudget   = 60 * time.Second
	notifyBackoff  = 500 * time.Millisecond
)

// phaseNotify posts the report to the controller.
//
// It runs last, and it is deliberately unable to change the exit code (R10).
// The report is not the result of the work but a message about it: the result
// is already in storage, the outcome is visible to the controller through the
// Job's exit code, and the contents will be lifted by the backend from the
// run's prefix. Failing a successful run because the controller restarted would
// be substituting the means for the end.
func phaseNotify(ctx context.Context, r *Run) error {
	if r.cfg.CallbackURL == "" {
		return skip("no callback URL was configured")
	}
	if len(r.reportBody) == 0 {
		return skip("finalize produced no report to send")
	}

	deadline := time.Now().Add(notifyBudget)
	backoff := notifyBackoff
	var last error

	for attempt := 1; attempt <= notifyAttempts; attempt++ {
		if time.Now().After(deadline) {
			break
		}
		status, err := r.postReport(ctx)
		switch {
		case err == nil && status == http.StatusAccepted:
			r.logf("completion report delivered on attempt %d", attempt)
			return nil

		case err == nil && status == http.StatusRequestEntityTooLarge:
			// The one answer with a remedy the pod can apply. Truncating the
			// summary and retrying once is cheaper than losing the cost and the
			// pull request link over a verbose agent.
			if r.report.Summary == "" {
				return fail(runv1.ExitStorage, "ReportTooLarge",
					"the controller refused the report as too large and there is nothing left to drop")
			}
			r.logf("the controller refused the report as too large; dropping the summary and retrying")
			r.report.Summary = ""
			body, marshalErr := json.Marshal(r.report)
			if marshalErr != nil {
				return failWrap(runv1.ExitStorage, "ReportUnserialisable", marshalErr,
					"re-marshalling the truncated report")
			}
			r.reportBody = body
			continue

		case err == nil && (status == http.StatusBadRequest || status == http.StatusUnauthorized ||
			status == http.StatusConflict):
			// A retry would give the same answer. Saying so once is more useful
			// than saying it five times over sixty seconds.
			return fail(runv1.ExitStorage, "ReportRejected",
				"the controller answered %d and will answer it again; the report is in storage regardless",
				status)

		case err == nil:
			last = errorf("the controller answered %d", status)
		default:
			last = err
		}

		r.logf("completion delivery attempt %d failed: %v", attempt, last)
		select {
		case <-ctx.Done():
			return failWrap(runv1.ExitStorage, "ReportUndelivered", ctx.Err(), "delivering the report")
		case <-time.After(backoff):
		}
		backoff *= 2
	}

	return failWrap(runv1.ExitStorage, "ReportUndelivered", last,
		"the report was not delivered within %s; it is in storage as %s and the exit code is unchanged",
		notifyBudget, runv1.StorageKeyCompletion)
}

// postReport makes one delivery attempt.
func (r *Run) postReport(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(r.cfg.CallbackURL, "/")+runv1.CallbackPathCompletion,
		bytes.NewReader(r.reportBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Bearer, and the token is the controller's rather than the backend's:
	// callbackURL is cluster-local, and without a token any pod in the agent
	// namespace could post a forged completion for somebody else's run.
	req.Header.Set("Authorization", "Bearer "+r.secrets.CallbackToken)
	req.Header.Set("X-Haliphron-Contract", runv1.ContractVersion)
	// Duplicates what is already in the body, and is worth it: the controller
	// drops a repeat without parsing the payload.
	req.Header.Set("Idempotency-Key", string(r.cfg.RunID)+"/"+itoa32(r.cfg.Attempt))
	if r.cfg.Traceparent != "" {
		req.Header.Set("traceparent", r.cfg.Traceparent)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return 0, errorf("%s", r.redactor.String(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, nil
}
