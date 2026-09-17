package agentrun

import (
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/launcher"
)

// DefaultStartupDeadline is the budget from Job creation to a running
// container. Without it a nonexistent image or a node selector nothing matches
// waits forever: ImagePullBackOff is not a failure to Kubernetes, and a run
// that hangs in Starting is worse than one that fails, because nobody gets told.
const DefaultStartupDeadline = 600 * time.Second

// Observation is what the controller can see about one attempt. It is the
// entire input to the phase decision, and it holds nothing that is not visible
// in the cluster: the pod's own account of what happened arrives separately,
// through the completion webhook.
type Observation struct {
	Phase        runv1.Phase
	Reason       string
	Message      string
	PodName      string
	NodeName     string
	StartedAt    *metav1.Time
	FinishedAt   *metav1.Time
	ExitCode     *int32
	FailureClass runv1.FailureClass
}

// Reasons the controller produces itself. They are Kubernetes-style CamelCase
// tokens because they end up beside the ones Kubernetes produces, in the same
// field, read by the same person.
const (
	ReasonAwaitingJob     = "AwaitingJob"
	ReasonJobDisappeared  = "JobDisappeared"
	ReasonPodNotCreated   = "PodNotCreated"
	ReasonStartupDeadline = "StartupDeadlineExceeded"
	ReasonDeadline        = "DeadlineExceeded"
	ReasonPodLost         = "PodLost"
	ReasonCancelled       = "Cancelled"
	ReasonCompleted       = "Completed"
)

// waitingReasonsThatNeverResolve are the container waiting states that a
// cluster will retry forever without ever succeeding on its own. They are
// tolerated until the startup deadline — an image pull from a cold registry is
// slow, a node may be a minute away from joining — and then they are a failure
// of class infra, which is the one class the controller retries by itself.
var waitingReasonsThatNeverResolve = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"CrashLoopBackOff":           true,
}

// Derive implements the table in section 7 of the CRD contract. It is a pure
// function of the objects passed in, which is what makes the table testable
// without a cluster — and the table is the one place controller bugs live.
func Derive(cr *agentrunv1alpha1.AgentRun, job *batchv1.Job, pod *corev1.Pod, now time.Time, startupDeadline time.Duration) Observation {
	if startupDeadline <= 0 {
		startupDeadline = DefaultStartupDeadline
	}

	// A Job on its way out is a Job that is gone: its pods are being collected
	// and nothing more will be learned from it. The distinction matters because
	// a deletion with foreground or orphan propagation leaves the object in
	// place, holding a finalizer, for as long as the garbage collector takes —
	// and a controller that kept waiting on it would wait for a pod that is
	// never coming back.
	if job != nil && job.DeletionTimestamp != nil {
		job = nil
	}

	if job == nil {
		// A Job that was there and is not any more, on a run that had not
		// finished, is somebody running kubectl delete. Reporting it as a
		// failure of class infra is both true and useful: the controller
		// retries it, which is what an operator deleting a stuck Job meant.
		if cr.Status.JobName != "" && !cr.Status.Phase.IsTerminal() {
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       ReasonJobDisappeared,
				Message:      fmt.Sprintf("job %s is gone while the run was %s", cr.Status.JobName, cr.Status.Phase),
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(metav1.NewTime(now)),
			}
		}
		return Observation{Phase: runv1.PhasePending, Reason: ReasonAwaitingJob}
	}

	// The Job's own backstop. It fires when the entrypoint itself hung: the
	// agent's timeout is enforced inside the pod and exits 11 long before this.
	if cond, ok := jobCondition(job, batchv1.JobFailed); ok && cond.Reason == "DeadlineExceeded" {
		return Observation{
			Phase:        runv1.PhaseTimedOut,
			Reason:       ReasonDeadline,
			Message:      "the job exceeded activeDeadlineSeconds",
			FailureClass: runv1.FailureInfra,
			PodName:      podName(pod),
			NodeName:     nodeName(pod),
			FinishedAt:   ptr(metav1.NewTime(now)),
		}
	}

	if pod == nil {
		if cond, ok := jobCondition(job, batchv1.JobFailed); ok {
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       orDefault(cond.Reason, ReasonPodNotCreated),
				Message:      cond.Message,
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(metav1.NewTime(now)),
			}
		}
		if expired(job.CreationTimestamp.Time, startupDeadline, now) {
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       ReasonStartupDeadline,
				Message:      "no pod was created within the startup deadline",
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(metav1.NewTime(now)),
			}
		}
		return Observation{Phase: runv1.PhaseStarting, Reason: ReasonPodNotCreated}
	}

	obs := Observation{PodName: pod.Name, NodeName: pod.Spec.NodeName}
	status, hasStatus := containerStatus(pod)

	switch {
	case hasStatus && status.State.Terminated != nil:
		t := status.State.Terminated
		obs.ExitCode = ptr(t.ExitCode)
		obs.FinishedAt = ptr(t.FinishedAt)
		if !t.StartedAt.IsZero() {
			obs.StartedAt = ptr(t.StartedAt)
		}
		obs.Phase = runv1.PhaseForExitCode(t.ExitCode)
		obs.FailureClass = runv1.FailureClassForExitCode(t.ExitCode)
		obs.Reason = orDefault(t.Reason, string(obs.Phase))
		obs.Message = t.Message
		if obs.Phase == runv1.PhaseSucceeded {
			// Success is exit code zero and nothing else. Whether the work was
			// done well is a different question, asked by a different node.
			obs.FailureClass = runv1.FailureNone
			obs.Reason = ReasonCompleted
		}
		return obs

	case pod.Status.Phase == corev1.PodRunning:
		obs.Phase = runv1.PhaseRunning
		obs.Reason = string(runv1.PhaseRunning)
		if hasStatus && status.State.Running != nil {
			obs.StartedAt = ptr(status.State.Running.StartedAt)
		} else if pod.Status.StartTime != nil {
			obs.StartedAt = pod.Status.StartTime
		}
		return obs

	case pod.Status.Phase == corev1.PodSucceeded:
		obs.Phase = runv1.PhaseSucceeded
		obs.Reason = ReasonCompleted
		obs.FailureClass = runv1.FailureNone
		obs.ExitCode = ptr(int32(0))
		obs.FinishedAt = ptr(metav1.NewTime(now))
		return obs

	case pod.Status.Phase == corev1.PodFailed:
		// Evicted, preempted, or killed before the container ever reported.
		// The pod is gone and the work with it, and the cause is always the
		// platform rather than the agent, so the class is infra and the
		// controller is allowed to try again.
		obs.Phase = runv1.PhaseFailed
		obs.Reason = orDefault(pod.Status.Reason, ReasonPodLost)
		obs.Message = pod.Status.Message
		obs.FailureClass = runv1.FailureInfra
		obs.FinishedAt = ptr(metav1.NewTime(now))
		return obs
	}

	// Pending. The question is only whether it is still plausibly making
	// progress.
	reason, message := pendingReason(pod, hasStatus, status)
	if waitingReasonsThatNeverResolve[reason] || reason == "Unschedulable" {
		if expired(job.CreationTimestamp.Time, startupDeadline, now) {
			obs.Phase = runv1.PhaseFailed
			obs.Reason = reason
			obs.Message = fmt.Sprintf("%s for longer than the startup deadline: %s",
				reason, message)
			obs.FailureClass = runv1.FailureInfra
			obs.FinishedAt = ptr(metav1.NewTime(now))
			return obs
		}
	}
	obs.Phase = runv1.PhaseStarting
	obs.Reason = orDefault(reason, string(runv1.PhaseStarting))
	obs.Message = message
	return obs
}

// pendingReason digs out why a pod has not started. The container's waiting
// reason is the specific answer; an unschedulable condition is the other one,
// and it is the shape an exhausted quota or an unsatisfiable nodeSelector takes.
func pendingReason(pod *corev1.Pod, hasStatus bool, status corev1.ContainerStatus) (string, string) {
	if hasStatus && status.State.Waiting != nil {
		return status.State.Waiting.Reason, status.State.Waiting.Message
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return orDefault(c.Reason, "Unschedulable"), c.Message
		}
	}
	return "", ""
}

// Monotonic applies the ordering rule: within one attempt a phase never moves
// backwards, and the first terminal phase wins.
//
// The second half is what makes a cancellation racing a successful exit
// harmless. If the pod exited zero while the cancel was travelling to the
// cluster, the run succeeded — and the backend applies exactly the same rule to
// the report, so the two sides cannot disagree about which it was.
func Monotonic(cr *agentrunv1alpha1.AgentRun, attempt int32, obs Observation) Observation {
	if cr.Status.Attempt != attempt {
		return obs
	}
	current := cr.Status.Phase
	if current == "" {
		return obs
	}
	if current.IsTerminal() || obs.Phase.Rank() < current.Rank() {
		return Observation{
			Phase:        current,
			Reason:       cr.Status.Reason,
			Message:      cr.Status.Message,
			PodName:      orDefault(obs.PodName, cr.Status.PodName),
			NodeName:     orDefault(obs.NodeName, cr.Status.NodeName),
			StartedAt:    cr.Status.StartedAt,
			FinishedAt:   cr.Status.FinishedAt,
			ExitCode:     cr.Status.ExitCode,
			FailureClass: cr.Status.FailureClass,
		}
	}
	return obs
}

func containerStatus(pod *corev1.Pod) (corev1.ContainerStatus, bool) {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == launcher.ContainerName {
			return s, true
		}
	}
	return corev1.ContainerStatus{}, false
}

func jobCondition(job *batchv1.Job, want batchv1.JobConditionType) (batchv1.JobCondition, bool) {
	for _, c := range job.Status.Conditions {
		if c.Type == want && c.Status == corev1.ConditionTrue {
			return c, true
		}
	}
	return batchv1.JobCondition{}, false
}

func expired(start time.Time, budget time.Duration, now time.Time) bool {
	return !start.IsZero() && now.Sub(start) > budget
}

func podName(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	return pod.Name
}

func nodeName(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	return pod.Spec.NodeName
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func ptr[T any](v T) *T { return &v }
