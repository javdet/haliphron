package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The phase table from section 7 of the CRD contract, and the decisions that
// hang off it. This is where controller bugs live, so the tests are literal.

// TestTheExitCodeDecidesThePhaseAndTheClass. Success is exit zero and nothing
// else; everything else is classified by the shared table, because the
// controller decides whether to retry and the backend has to explain the same
// failure in the UI.
func TestTheExitCodeDecidesThePhaseAndTheClass(t *testing.T) {
	cases := []struct {
		name  string
		code  int32
		phase runv1.Phase
		class runv1.FailureClass
	}{
		{"success", 0, runv1.PhaseSucceeded, runv1.FailureNone},
		{"agent error", runv1.ExitAgentError, runv1.PhaseFailed, runv1.FailureAgent},
		{"agent timeout", runv1.ExitAgentTimeout, runv1.PhaseTimedOut, runv1.FailureAgent},
		{"git", runv1.ExitGit, runv1.PhaseFailed, runv1.FailureGit},
		{"config", runv1.ExitConfig, runv1.PhaseFailed, runv1.FailureConfig},
		{"oom killed", 137, runv1.PhaseFailed, runv1.FailureInfra},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id := h.Backend.Enqueue(sampleSpec())
			h.poll()
			h.reconcile(id)

			pod := h.startPod(id, 1)
			h.podRunning(pod)
			h.reconcile(id)
			if cr := h.run(id); cr.Status.Phase != runv1.PhaseRunning {
				t.Fatalf("want Running while the pod runs, got %s", cr.Status.Phase)
			}

			h.podExited(pod, tc.code, "")
			h.reconcile(id)

			cr := h.run(id)
			if cr.Status.Phase != tc.phase {
				t.Fatalf("exit %d gave phase %s, want %s", tc.code, cr.Status.Phase, tc.phase)
			}
			if cr.Status.FailureClass != tc.class {
				t.Fatalf("exit %d gave class %s, want %s", tc.code, cr.Status.FailureClass, tc.class)
			}
			if cr.Status.ExitCode == nil || *cr.Status.ExitCode != tc.code {
				t.Fatalf("the exit code was not recorded: %v", cr.Status.ExitCode)
			}
		})
	}
}

// TestAnImagePullBackOffFailsOnlyAfterTheStartupDeadline. To Kubernetes an
// image that does not exist is a state, not an error, and it will retry the
// pull until somebody notices. The deadline is what turns "notices" into a
// report.
func TestAnImagePullBackOffFailsOnlyAfterTheStartupDeadline(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podWaiting(pod, "ImagePullBackOff")
	h.reconcile(id)

	if cr := h.run(id); cr.Status.Phase != runv1.PhaseStarting {
		t.Fatalf("a slow pull is not a failure yet: %s", cr.Status.Phase)
	}

	h.Clock.Advance(11 * time.Minute)
	h.reconcile(id)

	cr := h.run(id)
	if cr.Status.Phase != runv1.PhaseFailed || cr.Status.FailureClass != runv1.FailureInfra {
		t.Fatalf("want Failed/infra past the startup deadline, got %s/%s", cr.Status.Phase, cr.Status.FailureClass)
	}
	if cr.Status.Reason != "ImagePullBackOff" {
		t.Fatalf("the reason was lost: %q", cr.Status.Reason)
	}
}

// TestAJobDeletedWhileRunningIsAnInfrastructureFailure. Somebody ran kubectl
// delete on a Job that looked stuck. That is a decision to retry, and the
// controller should read it as one rather than wait forever for a pod that is
// not coming back.
func TestAJobDeletedWhileRunningIsAnInfrastructureFailure(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)

	job := h.job(id, 1)
	if err := h.K8s.Delete(h.ctx, job,
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	h.deletePod(pod)
	h.reconcileOnce(id)

	cr := h.run(id)
	if cr.Status.Phase != runv1.PhaseFailed || cr.Status.FailureClass != runv1.FailureInfra {
		t.Fatalf("want Failed/infra when the Job vanishes, got %s/%s", cr.Status.Phase, cr.Status.FailureClass)
	}
}

// TestAnInfrastructureFailureIsRetriedAndTheBudgetSurvivesARestart. The retry
// budget lives in the status precisely so that a controller restart does not
// hand a failing run a fresh set of attempts — which is how a broken node turns
// into an unbounded bill.
func TestAnInfrastructureFailureIsRetriedAndTheBudgetSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)
	h.podExited(pod, 137, "OOMKilled")
	h.reconcile(id)

	cr := h.run(id)
	if cr.Status.Retry.InfraRetries != 1 || cr.Status.Retry.NextAttemptAt == nil {
		t.Fatalf("no retry was scheduled: %+v", cr.Status.Retry)
	}
	if h.job(id, 2) != nil {
		t.Fatal("the next attempt started before its backoff elapsed")
	}

	next := h.restart()
	next.Clock.Advance(10 * time.Minute)
	next.reconcile(id)

	cr = next.run(id)
	if cr.Status.Attempt != 2 {
		t.Fatalf("want attempt 2 after the backoff, got %d", cr.Status.Attempt)
	}
	if cr.Status.Retry.InfraRetries != 1 {
		t.Fatalf("the restart reset the retry budget: %d", cr.Status.Retry.InfraRetries)
	}
	if next.job(id, 2) == nil {
		t.Fatal("the second attempt has no Job")
	}
	if next.job(id, 1) == nil {
		t.Fatal("the first attempt's Job was removed; its exit code is the evidence for the retry")
	}
}

// TestAnAgentFailureIsNotRetried. Replaying an agent that already pushed a
// branch spends the budget twice and can produce conflicting commits, and no
// number of repeats fixes a prompt.
func TestAnAgentFailureIsNotRetried(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podExited(pod, runv1.ExitAgentError, "Error")
	h.reconcile(id)
	h.Clock.Advance(10 * time.Minute)
	h.reconcile(id)

	if h.job(id, 2) != nil {
		t.Fatal("an agent failure was retried")
	}
	if cr := h.run(id); cr.Status.Attempt != 1 {
		t.Fatalf("the attempt counter moved on an agent failure: %d", cr.Status.Attempt)
	}
}

// TestCancelDoesNotOverrideASuccessfulExit. If the pod exited zero while the
// cancellation was still travelling, the run succeeded. The backend applies the
// same rule to the report, so the two sides cannot end up disagreeing about
// what happened.
func TestCancelDoesNotOverrideASuccessfulExit(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podExited(pod, 0, "Completed")
	h.reconcile(id)

	h.Backend.Cancel(id)
	h.beat()
	h.reconcile(id)

	cr := h.run(id)
	if cr.Status.Phase != runv1.PhaseSucceeded {
		t.Fatalf("a cancellation overrode a successful exit: %s", cr.Status.Phase)
	}
}

// TestACancelStopsARunningPod is the other half: while the run is still going,
// the command is carried out, and the pod gets its grace period to upload
// whatever it already has.
func TestACancelStopsARunningPod(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)

	h.Backend.Cancel(id)
	h.beat()
	h.reconcile(id)

	if job := h.job(id, 1); job != nil && job.DeletionTimestamp == nil {
		t.Fatal("the Job was not deleted on a cancellation")
	}
	h.deletePod(pod)
	h.Clock.Advance(5 * time.Minute)
	h.reconcile(id)

	if cr := h.run(id); cr.Status.Phase != runv1.PhaseCancelled {
		t.Fatalf("want Cancelled once the pod is gone, got %s", cr.Status.Phase)
	}
}

// TestAbandonRemovesEverythingAndSaysNothing. An abandoned run belongs to
// another cluster now. Reporting on it is how a controller that came back from
// a disconnect appends its state to somebody else's run — the one behaviour the
// epoch is there to prevent.
func TestAbandonRemovesEverythingAndSaysNothing(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)

	// The operator reassigned it: the epoch rises and this cluster's next word
	// on the subject is fenced.
	h.Backend.Reassign(id)
	h.flush()

	cr := h.run(id)
	if _, abandoned := cr.Annotations[agentrunv1alpha1.GroupName+"/abandoned-at"]; !abandoned {
		t.Fatal("the run was not marked abandoned after the control plane fenced it")
	}

	h.reconcile(id)
	if !h.runGone(id) {
		t.Fatal("the AgentRun survived an abandon")
	}
	if job := h.job(id, 1); job != nil && job.DeletionTimestamp == nil {
		t.Fatal("the Job survived an abandon")
	}

	// Nothing more is said about it, whatever happens in the cluster next.
	before, _ := h.Backend.RunState(id)
	h.podExited(pod, 0, "Completed")
	h.reconcileOnce(id)
	h.flush()
	after, _ := h.Backend.RunState(id)
	if after.Phase != before.Phase || after.Status != before.Status {
		t.Fatalf("an abandoned run was reported on: %s/%s became %s/%s",
			before.Status, before.Phase, after.Status, after.Phase)
	}
}

// TestANewEpochReplacesTheAgentRun. A new epoch is new ownership: the spec is
// immutable and a status carried across would describe a run that never
// started, so the object is replaced rather than edited — and there is never a
// moment with two Jobs.
func TestANewEpochReplacesTheAgentRun(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	if h.job(id, 1) == nil {
		t.Fatal("the first attempt has no Job")
	}

	// An operator pressed retry: the epoch rises and the work returns to the
	// queue.
	h.Backend.Retry(id)

	h.poll() // the superseded object is deleted and the lease is left unacknowledged
	cr := h.run(id)
	if cr.DeletionTimestamp == nil {
		t.Fatal("the superseded AgentRun was not deleted")
	}
	h.reconcile(id) // the finalizer releases once the Job is gone
	if !h.runGone(id) {
		t.Fatal("the superseded AgentRun is still here")
	}
	if h.job(id, 1) != nil {
		t.Fatal("the superseded epoch's Job outlived its AgentRun")
	}

	// Nothing acknowledged that lease, so the ack deadline returns the work to
	// the queue at a further epoch. This is the self-healing path the contract
	// describes, and it needs no code of its own.
	h.Backend.AdvanceClock(2 * time.Minute)

	h.poll()
	cr = h.run(id)
	if cr.Spec.LeaseEpoch < 2 {
		t.Fatalf("want the AgentRun at a later epoch, got %d", cr.Spec.LeaseEpoch)
	}
	if cr.DeletionTimestamp != nil {
		t.Fatal("the new AgentRun is already being deleted")
	}
	h.reconcile(id)
	if h.job(id, 1) == nil {
		t.Fatal("the new epoch never got a Job")
	}
	if !ownedByRun(h.job(id, 1), cr.UID) {
		t.Fatal("the new epoch adopted the previous epoch's Job")
	}
}

func ownedByRun(job *batchv1.Job, uid types.UID) bool {
	for _, ref := range job.OwnerReferences {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// TestTheFinalizerIsReleasedOnABoundedWait. A finalizer that can wedge is worse
// than the leak it prevents: it makes the namespace undeletable, and the cure
// is an operator editing objects by hand in production.
func TestTheFinalizerIsReleasedOnABoundedWait(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)

	cr := h.run(id)
	if err := h.K8s.Delete(h.ctx, cr); err != nil {
		t.Fatalf("delete the AgentRun: %v", err)
	}

	// The pod is still there and, with no kubelet, will stay there.
	h.reconcileOnce(id)
	if _, err := h.runErr(id); err != nil {
		t.Fatal("the finalizer released while the pod was still running")
	}

	h.Clock.Advance(10 * time.Minute)
	h.reconcileOnce(id)
	if _, err := h.runErr(id); err == nil {
		t.Fatal("the finalizer never released; the namespace is now undeletable")
	}
	_ = pod
}

// TestAnUnknownCommandIsIgnored. The control plane may be newer than the
// controller, and a command type this build has never heard of must not take
// the cluster down.
func TestAnUnknownCommandIsIgnored(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	h.Backend.InjectCommand(id, clusterCommand("defenestrate", id))
	h.beat()
	h.reconcile(id)

	cr := h.run(id)
	if _, abandoned := cr.Annotations[agentrunv1alpha1.GroupName+"/abandoned-at"]; abandoned {
		t.Fatal("an unknown command was treated as an abandon")
	}
	if cr.Status.Phase.IsTerminal() {
		t.Fatalf("an unknown command ended the run: %s", cr.Status.Phase)
	}
}

// clusterCommand builds a command of an arbitrary type, which the typed
// constructors deliberately cannot.
func clusterCommand(kind string, id runv1.ULID) clusterv1.Command {
	return clusterv1.Command{Type: clusterv1.CommandType(kind), RunID: id}
}
