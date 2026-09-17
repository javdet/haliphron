package controller

import (
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// What the controller says, and when it is entitled to stop saying it.

// TestTheFirstHeartbeatClaimsNothingAboutCompleteness. Under reportComplete the
// backend may treat an unmentioned run as lost. A controller that has just come
// up is the one case where silence means "not yet" rather than "gone", and
// getting this wrong takes work away from a cluster that is merely warming up.
func TestTheFirstHeartbeatClaimsNothingAboutCompleteness(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	// The run exists in the cluster, so nothing is missing either way; what is
	// being tested is the flag.
	h.beat()
	h.beat()

	beats := heartbeatLines(h)
	if len(beats) < 2 {
		t.Fatalf("want two heartbeats in the control plane's log, got %d", len(beats))
	}
	if !strings.Contains(beats[0], "complete=false") {
		t.Fatalf("the first heartbeat claimed completeness: %s", beats[0])
	}
	if !strings.Contains(beats[1], "complete=true") {
		t.Fatalf("the second heartbeat is still partial: %s", beats[1])
	}
}

// TestALostRunIsReconciledInOneHeartbeat. The scenario is the agents namespace
// being recreated, or the controller being moved without its state: every CR is
// gone at once. Without the reconciliation the backend waits out staleAfter for
// each of them and hands an operator a batch of Unknowns to triage.
func TestALostRunIsReconciledInOneHeartbeat(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	h.beat() // partial: nothing may be concluded from it
	h.forceDelete(id)
	h.beat() // partial flag has cleared, and now the run really is missing

	beats := heartbeatLines(h)
	if strings.Contains(beats[0], "unknown=1") {
		t.Fatalf("a partial report produced unknownRuns: %s", beats[0])
	}
	if !strings.Contains(beats[len(beats)-1], "unknown=1") {
		t.Fatalf("the lost run was not reported back as unknown: %s", beats[len(beats)-1])
	}
}

// TestTheOutcomeIsNotForgottenUntilTheBackendHasHeardIt. The CR is the only
// copy of what happened that the controller holds; deleting it before the
// control plane accepted the outcome turns a finished run into an Unknown that
// needs a human.
func TestTheOutcomeIsNotForgottenUntilTheBackendHasHeardIt(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podExited(pod, 0, "Completed")
	h.reconcile(id)

	// Past the TTL, but nothing has been delivered: the backend was down.
	h.Backend.SetUnavailable(true)
	h.Clock.Advance(48 * time.Hour)
	h.reconcile(id)
	if h.runGone(id) {
		t.Fatal("the run was reaped before its outcome was delivered")
	}

	h.Backend.SetUnavailable(false)
	h.flush()

	cr := h.run(id)
	if cr.Status.Reported.Phase != runv1.PhaseSucceeded {
		t.Fatalf("delivery was not recorded on the object: %+v", cr.Status.Reported)
	}

	h.reconcile(id)
	if !h.runGone(id) {
		t.Fatal("the run was not reaped after its outcome had been accepted and its TTL passed")
	}
}

// TestAnObservationSurvivesAFailedDelivery. Losing an ingest costs latency, not
// a report: it goes back on the queue, and the heartbeat would carry the same
// fact anyway. What must not happen is a terminal phase disappearing because
// one POST failed.
func TestAnObservationSurvivesAFailedDelivery(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podExited(pod, runv1.ExitAgentError, "Error")
	h.reconcile(id)

	faults.Fail("/ingest/status", true)
	if err := h.Reporter.Flush(h.ctx); err == nil {
		t.Fatal("a failed ingest reported success")
	}
	if h.Reporter.Queue.Depth() == 0 {
		t.Fatal("the observations were dropped when delivery failed")
	}

	faults.Fail("/ingest/status", false)
	h.flush()

	state, _ := h.Backend.RunState(id)
	if state.TerminalPhase != runv1.PhaseFailed {
		t.Fatalf("the terminal phase never arrived: %s", state.TerminalPhase)
	}
	if state.FailureClass != runv1.FailureAgent {
		t.Fatalf("the failure class was lost: %s", state.FailureClass)
	}
}

// TestAStaleObservationIsRefusedRatherThanApplied. Ordering is by (attempt,
// phase rank) and never by time, because cluster clocks are not synchronised.
// A Running that arrives after a Succeeded must not resurrect a run.
func TestAStaleObservationIsRefusedRatherThanApplied(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podExited(pod, 0, "Completed")
	h.reconcile(id)
	h.flush()

	// A report from before the run finished, delivered late.
	h.Reporter.Observed(clusterv1.RunObservation{
		RunID: id, Epoch: 1, Attempt: 1, Phase: runv1.PhaseRunning,
	})
	h.flush()

	state, _ := h.Backend.RunState(id)
	if state.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("a late Running observation moved a finished run: %s", state.TerminalPhase)
	}
}

func heartbeatLines(h *harness) []string {
	var out []string
	for _, line := range h.Backend.Log() {
		if strings.HasPrefix(line, "heartbeat ") {
			out = append(out, line)
		}
	}
	return out
}
