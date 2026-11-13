package reconcile

import (
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/state"
)

var t0 = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

func exited(code int, oom bool, started bool) *docker.Container {
	c := &docker.Container{Created: t0}
	c.State = docker.ContainerState{Status: docker.StateExited, ExitCode: code, OOMKilled: oom, FinishedAt: t0.Add(time.Hour)}
	if started {
		c.State.StartedAt = t0.Add(time.Minute)
	}
	return c
}

func TestThePhaseTable(t *testing.T) {
	acked := t0
	began := t0
	deadline := 10 * time.Minute
	cases := []struct {
		name   string
		row    func(*state.Run)
		ctr    *docker.Container
		now    time.Time
		phase  runv1.Phase
		reason string
		class  runv1.FailureClass
	}{
		{name: "no container, not acknowledged", ctr: nil, now: t0,
			phase: runv1.PhasePending, reason: ReasonAwaitingAck},
		{name: "no container, acknowledged", row: func(r *state.Run) { r.AckedAt = &acked }, now: t0,
			phase: runv1.PhasePending, reason: ReasonAwaitingContainer},
		{name: "pulling within the startup deadline", row: func(r *state.Run) {
			r.AckedAt = &acked
			r.Status.StartupBeganAt = &began
		}, now: t0.Add(time.Minute), phase: runv1.PhaseStarting, reason: ReasonPullingImage},
		{name: "pull failing past the startup deadline", row: func(r *state.Run) {
			r.AckedAt = &acked
			r.Status.StartupBeganAt = &began
			r.Status.PullError = "manifest unknown"
		}, now: t0.Add(11 * time.Minute), phase: runv1.PhaseFailed, reason: ReasonImagePullFailed, class: runv1.FailureInfra},
		{name: "a recorded container is gone", row: func(r *state.Run) {
			r.AckedAt = &acked
			r.Status.Phase = runv1.PhaseRunning
			r.Status.ContainerName = "haliphron-x-1-a1"
		}, now: t0, phase: runv1.PhaseFailed, reason: ReasonContainerGone, class: runv1.FailureInfra},
		{name: "created and not started past the deadline", ctr: &docker.Container{Created: t0,
			State: docker.ContainerState{Status: docker.StateCreated}}, now: t0.Add(11 * time.Minute),
			phase: runv1.PhaseFailed, reason: ReasonStartupDeadline, class: runv1.FailureInfra},
		{name: "created, runtime refused to start it", ctr: &docker.Container{Created: t0,
			State: docker.ContainerState{Status: docker.StateCreated, Error: "mount source missing"}}, now: t0,
			phase: runv1.PhaseFailed, reason: ReasonStartFailed, class: runv1.FailureInfra},
		{name: "running", ctr: &docker.Container{State: docker.ContainerState{Status: docker.StateRunning,
			Running: true, StartedAt: t0}}, now: t0, phase: runv1.PhaseRunning, reason: "Running"},
		{name: "exit 0 succeeds and nothing more", ctr: exited(0, false, true), now: t0,
			phase: runv1.PhaseSucceeded, reason: ReasonCompleted, class: runv1.FailureNone},
		{name: "exit 10 is the agent's", ctr: exited(10, false, true), now: t0,
			phase: runv1.PhaseFailed, reason: "Failed", class: runv1.FailureAgent},
		{name: "exit 11 timed out", ctr: exited(11, false, true), now: t0,
			phase: runv1.PhaseTimedOut, reason: "TimedOut", class: runv1.FailureAgent},
		{name: "exit 20 is git", ctr: exited(20, false, true), now: t0,
			phase: runv1.PhaseFailed, reason: "Failed", class: runv1.FailureGit},
		{name: "exit 21 is storage, infra", ctr: exited(21, false, true), now: t0,
			phase: runv1.PhaseFailed, reason: "Failed", class: runv1.FailureInfra},
		{name: "OOM killed", ctr: exited(137, true, true), now: t0,
			phase: runv1.PhaseFailed, reason: ReasonOOMKilled, class: runv1.FailureInfra},
		{name: "killed with no stop recorded: the host restarted", ctr: exited(143, false, true), now: t0,
			phase: runv1.PhaseFailed, reason: ReasonHostRestarted, class: runv1.FailureInfra},
		{name: "killed after the controller's deadline stop", row: func(r *state.Run) {
			r.Status.Stop = &state.StopRecord{Reason: state.StopDeadline, At: t0}
		}, ctr: exited(143, false, true), now: t0, phase: runv1.PhaseTimedOut, reason: ReasonDeadline, class: runv1.FailureInfra},
		{name: "killed after a cancel stop is not a host restart", row: func(r *state.Run) {
			r.Status.Stop = &state.StopRecord{Reason: state.StopCancel, At: t0}
		}, ctr: exited(143, false, true), now: t0, phase: runv1.PhaseFailed, reason: "Failed", class: runv1.FailureInfra},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &state.Run{RunID: "01HZRUN0000000000000000000", Epoch: 1}
			if tc.row != nil {
				tc.row(row)
			}
			got := Derive(row, tc.ctr, tc.now, deadline)
			if got.Phase != tc.phase || got.Reason != tc.reason || got.FailureClass != tc.class {
				t.Fatalf("got %s/%s/%s, want %s/%s/%s (%s)",
					got.Phase, got.Reason, got.FailureClass, tc.phase, tc.reason, tc.class, got.Message)
			}
		})
	}
}

func TestAPhaseNeverMovesBackwardsAndTheFirstTerminalWins(t *testing.T) {
	row := &state.Run{Status: state.Status{Attempt: 1, Phase: runv1.PhaseSucceeded, Reason: ReasonCompleted}}
	got := Monotonic(row, 1, Observation{Phase: runv1.PhaseCancelled, Reason: ReasonCancelled})
	if got.Phase != runv1.PhaseSucceeded {
		t.Fatalf("a cancel overwrote a success: %s", got.Phase)
	}
	row.Status = state.Status{Attempt: 1, Phase: runv1.PhaseRunning}
	if got := Monotonic(row, 1, Observation{Phase: runv1.PhaseStarting}); got.Phase != runv1.PhaseRunning {
		t.Fatalf("Running went back to %s", got.Phase)
	}
	if got := Monotonic(row, 2, Observation{Phase: runv1.PhasePending}); got.Phase != runv1.PhasePending {
		t.Fatalf("a new attempt must start over, got %s", got.Phase)
	}
}

func TestOnlyADeliberateExitOutranksACancellation(t *testing.T) {
	code := int32(0)
	if !outranksCancellation(Observation{Phase: runv1.PhaseSucceeded, ExitCode: &code}) {
		t.Error("an exit 0 that raced the cancel is a success")
	}
	sig := int32(143)
	if outranksCancellation(Observation{Phase: runv1.PhaseFailed, ExitCode: &sig}) {
		t.Error("the SIGTERM the cancellation sent does not outrank it")
	}
}

func TestOnlyInfraAndGitAreRetriedWithinTheBudget(t *testing.T) {
	r := &Reconciler{}
	row := &state.Run{Status: state.Status{Phase: runv1.PhaseFailed, FailureClass: runv1.FailureInfra}}
	if !r.shouldRetry(row) {
		t.Error("infra is retried")
	}
	row.Status.FailureClass = runv1.FailureAgent
	if r.shouldRetry(row) {
		t.Error("an agent failure is never replayed")
	}
	row.Status.FailureClass = runv1.FailureGit
	row.Status.Retry.InfraRetries = DefaultMaxInfraRetries
	if r.shouldRetry(row) {
		t.Error("the budget is spent")
	}
	row.Status.Retry.InfraRetries = 0
	now := t0
	row.CancelRequestedAt = &now
	if r.shouldRetry(row) {
		t.Error("a cancelled run is not retried")
	}
}

func TestTheReaperWaitsForTheReportAndTheRelay(t *testing.T) {
	row := &state.Run{Status: state.Status{Attempt: 1, Phase: runv1.PhaseSucceeded}}
	if ReportSettled(row) {
		t.Fatal("nothing has been delivered")
	}
	row.Status.Reported = state.Reported{Attempt: 1, Phase: runv1.PhaseSucceeded}
	if !ReportSettled(row) {
		t.Fatal("the phase was delivered and nothing else is owed")
	}
	f := false
	row.Status.ArtifactsRelayed = &f
	if ReportSettled(row) {
		t.Fatal("artifacts are still in the spool")
	}
	tr := true
	row.Status.ArtifactsRelayed = &tr
	row.Status.ResultReported = true
	if ReportSettled(row) {
		t.Fatal("the completion was received and not delivered")
	}
	row.Status.Reported.CompletionDelivered = true
	if !ReportSettled(row) {
		t.Fatal("everything was delivered")
	}
}
