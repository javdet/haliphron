package backend

import (
	"context"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Under reportComplete the absence of a run means something: the controller is
// telling the backend this is everything it holds. A run the backend believes
// is there and the controller did not mention has been lost — the agent
// namespace was recreated, or the controller was moved with its PV.
//
// Answering in the same heartbeat is what turns that from a batch of manual
// triage after staleAfter into one round trip.
func TestACompleteHeartbeatThatOmitsARunReportsItUnknown(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("report running: %+v", problem)
	}

	resp := p.Heartbeat(true)

	if len(resp.UnknownRuns) != 1 || resp.UnknownRuns[0] != admitted.ID {
		t.Fatalf("unknownRuns = %v, want exactly %s", resp.UnknownRuns, admitted.ID)
	}
	lost := h.Run(admitted.ID)
	if lost.Status != clusterv1.StatusUnknown {
		t.Errorf("status = %s, want Unknown", lost.Status)
	}
	if lost.ObservedPhase != runv1.PhaseRunning {
		t.Errorf("observed phase = %s, want Running left where it was", lost.ObservedPhase)
	}
	if lost.Epoch != lease.Epoch {
		t.Errorf("epoch = %d, want %d: losing sight of a run revokes nothing", lost.Epoch, lease.Epoch)
	}
}

// Without the flag, "I do not have this run" and "I have not told you about it
// yet" are the same message. A controller whose informer cache is still warming
// sends the second, and reacting to it is how a warming controller has its work
// taken away.
func TestAPartialHeartbeatDrawsNoConclusionFromSilence(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("report running: %+v", problem)
	}

	resp := p.Heartbeat(false)

	if len(resp.UnknownRuns) != 0 {
		t.Errorf("unknownRuns = %v, want none under a partial report", resp.UnknownRuns)
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusRunning {
		t.Errorf("status = %s, want Running: silence said nothing", status)
	}
}

// A run that has been leased but not yet acknowledged is one the controller has
// not had a chance to mention. Concluding it lost would take work away from a
// controller that is materialising it at that moment.
func TestALeasedButUnacknowledgedRunIsNotConcludedLost(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	if lease := p.LeaseOne(); lease.RunID != admitted.ID {
		t.Fatalf("leased %s, want %s", lease.RunID, admitted.ID)
	}

	resp := p.Heartbeat(true)
	if len(resp.UnknownRuns) != 0 {
		t.Errorf("unknownRuns = %v, want none: the ack has not had its deadline yet", resp.UnknownRuns)
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusLeased {
		t.Errorf("status = %s, want Leased", status)
	}
}

// Every heartbeat that carries an observation renews that run's lease. Without
// the renewal the lease expiry scanner would mark a perfectly healthy run
// Unknown one TTL after it started.
func TestAHeartbeatRenewsTheLeasesItReports(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	before := h.Run(admitted.ID)

	time.Sleep(1100 * time.Millisecond)
	resp := p.Heartbeat(true, observation(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning))

	if len(resp.Leases) != 1 || resp.Leases[0].RunID != admitted.ID {
		t.Fatalf("renewals = %+v, want one for %s", resp.Leases, admitted.ID)
	}
	after := h.Run(admitted.ID)
	if before.LeaseDeadline == nil || after.LeaseDeadline == nil {
		t.Fatal("a dispatched run must carry a lease deadline")
	}
	if !after.LeaseDeadline.After(*before.LeaseDeadline) {
		t.Errorf("deadline went from %s to %s; the heartbeat did not extend it",
			before.LeaseDeadline, after.LeaseDeadline)
	}

	// And with the renewal, the scanner leaves it alone.
	if swept := h.Sweep(); swept.LeaseExpired != 0 {
		t.Errorf("the scanner expired %d renewed leases", swept.LeaseExpired)
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusRunning {
		t.Errorf("status = %s, want Running", status)
	}
}

// Commands carry no acknowledgement, on purpose: one would have to be stored
// and expired, while re-sending cancel to an already cancelled run costs
// nothing. So the command is regenerated from the run's state on every
// heartbeat until the phase is terminal, and the observed state is the
// acknowledgement.
func TestCancelIsRedeliveredUntilThePhaseIsTerminal(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("report running: %+v", problem)
	}

	if _, err := h.App.Cancel(context.Background(), admitted.ID, "operator", "changed my mind"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	for i := 0; i < 3; i++ {
		resp := p.Heartbeat(true, observation(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning))
		if len(resp.Commands) != 1 {
			t.Fatalf("heartbeat %d carried %d commands, want the cancel repeated", i, len(resp.Commands))
		}
		cmd := resp.Commands[0]
		if cmd.Type != clusterv1.CommandCancel || cmd.RunID != admitted.ID {
			t.Fatalf("command = %+v, want a cancel for %s", cmd, admitted.ID)
		}
		if cmd.GracePeriodSeconds <= 0 {
			t.Error("a cancel without a grace period gives the pod no chance to upload its partial result")
		}
	}

	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseCancelled); problem != nil {
		t.Fatalf("report cancelled: %+v", problem)
	}
	resp := p.Heartbeat(true)
	if len(resp.Commands) != 0 {
		t.Errorf("the cancel was still being sent after the run ended: %+v", resp.Commands)
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusCancelled {
		t.Errorf("status = %s, want Cancelled", status)
	}
}

// A cancellation that arrives between the lease and the ack must go out on the
// ack. Waiting for the next heartbeat would first start a Job that has to be
// killed immediately.
func TestACancellationInTheMaterialisationWindowRidesOnTheAck(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, err := h.App.Cancel(context.Background(), admitted.ID, "operator", "too expensive"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	resp, problem := p.Ack(lease.RunID, lease.Epoch)
	fatalIfProblem(t, "ack", problem)
	if len(resp.Commands) != 1 || resp.Commands[0].Type != clusterv1.CommandCancel {
		t.Fatalf("the ack carried %+v, want a cancel", resp.Commands)
	}
}

// A run cancelled before it was ever handed out has no Job to kill and nothing
// to send a command to. Leaving it Queued would hand it out afterwards.
func TestCancellingAQueuedRunEndsItImmediately(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	if _, err := h.App.Cancel(context.Background(), admitted.ID, "operator", "wrong repo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	cancelled := h.Run(admitted.ID)
	if cancelled.Status != clusterv1.StatusCancelled {
		t.Errorf("status = %s, want Cancelled", cancelled.Status)
	}
	if leases, _ := p.Lease(1, 1); len(leases) != 0 {
		t.Errorf("a cancelled run was handed out: %+v", leases)
	}
}

// A cluster that stops reporting becomes Unreachable, and placement stops
// choosing it. Its runs are a separate question with a different deadline.
func TestAClusterThatStopsReportingBecomesUnreachable(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	p.Heartbeat(true)

	time.Sleep(3200 * time.Millisecond)
	if swept := h.Sweep(); swept.StaleClusters != 1 {
		t.Fatalf("the sweep marked %d clusters stale, want 1", swept.StaleClusters)
	}

	cluster, err := h.Store.ClusterByID(context.Background(), p.clusterID)
	if err != nil {
		t.Fatalf("read cluster: %v", err)
	}
	if cluster.Status != "Unreachable" {
		t.Errorf("status = %s, want Unreachable", cluster.Status)
	}

	// It comes back by itself on the next heartbeat: unreachable is a
	// statement about the last interval, not a decision about the cluster.
	p.Heartbeat(true)
	cluster, err = h.Store.ClusterByID(context.Background(), p.clusterID)
	if err != nil {
		t.Fatalf("read cluster: %v", err)
	}
	if cluster.Status != "Active" {
		t.Errorf("status = %s, want Active after a heartbeat", cluster.Status)
	}
}

// An exhausted ResourceQuota is declared by the controller. Not issuing into it
// is the whole value of the flag: otherwise the fact surfaces as a run of
// failures with "exceeded quota" in the message.
func TestNoWorkIsIssuedIntoAnExhaustedQuota(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit()

	var resp clusterv1.HeartbeatResponse
	_, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", p.token(),
		clusterv1.HeartbeatRequest{
			FreeSlots: 10, CapacitySlots: 10, ReportComplete: true,
			Cluster: &clusterv1.ClusterFacts{QuotaExhausted: true},
		}, &resp)
	fatalIfProblem(t, "heartbeat", problem)

	if leases, _ := p.Lease(1, 1); len(leases) != 0 {
		t.Errorf("work was issued into an exhausted quota: %+v", leases)
	}
}
