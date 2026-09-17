// Package kmeta builds the metadata every object the controller creates
// carries: the labels that make a run selectable, and the owner reference that
// makes cleanup a property of the AgentRun rather than a job for the
// controller.
//
// It is a package of its own because the same labels go on the AgentRun, the
// Secret, the ConfigMap, the Job and the pod template, and a set of labels that
// differs between the object and the thing that selects it is a bug that only
// shows up as an empty `kubectl get -l`.
package kmeta

import (
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ComponentAgentRun marks the objects the controller owns in the agents
// namespace, so an operator can select them apart from whatever else was
// installed there.
const ComponentAgentRun = "agent-run"

// Labels are the selectable facts about one run. Everything else is an
// annotation: a label value is an entry in an etcd index, and indexing a
// timestamp or a URL buys nothing.
func Labels(runID runv1.ULID, epoch int64, attempt int32, clusterID runv1.ULID, agent runv1.AgentType) map[string]string {
	return map[string]string{
		agentrunv1alpha1.LabelRunID:     strings.ToLower(string(runID)),
		agentrunv1alpha1.LabelEpoch:     strconv.FormatInt(epoch, 10),
		agentrunv1alpha1.LabelAttempt:   strconv.Itoa(int(attempt)),
		agentrunv1alpha1.LabelClusterID: strings.ToLower(string(clusterID)),
		agentrunv1alpha1.LabelAgent:     string(agent),
		agentrunv1alpha1.LabelComponent: ComponentAgentRun,
	}
}

// LabelsFor is Labels for an AgentRun that already exists. The attempt is taken
// from the status rather than the spec: the spec's is the attempt the lease was
// issued at, and the controller may have started several since.
func LabelsFor(cr *agentrunv1alpha1.AgentRun, clusterID runv1.ULID) map[string]string {
	attempt := cr.Status.Attempt
	if attempt < 1 {
		attempt = 1
	}
	return Labels(cr.Spec.RunID, cr.Spec.LeaseEpoch, attempt, clusterID, cr.Spec.Agent)
}

// RunSelector matches every object belonging to one run, whatever attempt it
// came from.
func RunSelector(runID runv1.ULID) map[string]string {
	return map[string]string{agentrunv1alpha1.LabelRunID: strings.ToLower(string(runID))}
}

// OwnerRef makes the AgentRun the owner of the objects built from it. This is
// the entire cleanup mechanism: deleting the AgentRun collects the Job, the
// pod, the Secret and the ConfigMap, and nothing has to remember to do it.
//
// BlockOwnerDeletion is set so that foreground deletion of the AgentRun waits
// for them, which is what keeps a Secret holding a live git token from
// outliving the run by however long the garbage collector takes.
func OwnerRef(cr *agentrunv1alpha1.AgentRun) metav1.OwnerReference {
	controller, block := true, true
	return metav1.OwnerReference{
		APIVersion:         agentrunv1alpha1.GroupVersion.String(),
		Kind:               "AgentRun",
		Name:               cr.Name,
		UID:                cr.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &block,
	}
}
