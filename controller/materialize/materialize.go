// Package materialize turns a lease into cluster objects.
//
// One rule shapes everything here, and it is mechanical: whatever the
// controller materialises does not go into the spec. The secret values, the
// presigned bundle and the role's config files become a Secret and a ConfigMap,
// and only their names ride in the AgentRun. The invariant "there is not a
// single secret in `kubectl get agentrun -o yaml`" is then held by the
// structural schema — there is no field to put one in — rather than by anyone
// remembering.
//
// The second thing this package exists for is the trap in section 6 of the CRD
// contract. A structural schema does not ignore a field it does not know: it
// deletes it and answers 201. An older chart against a newer control plane
// therefore runs a spec with a setting silently missing, and nobody finds out.
// So every create is followed by a comparison against the hash of what was
// sent, and a loss is refused with a negative ack before the run costs
// anything.
package materialize

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/kmeta"
	"github.com/automagicops/haliphron/controller/launcher"
)

// Materializer writes the objects one lease becomes.
type Materializer struct {
	Client      client.Client
	Namespace   string
	ClusterID   runv1.ULID
	CallbackURL string
	// RunURLTemplate renders the link back into the backend's UI, with %s for
	// the run ID. Empty means no annotation.
	RunURLTemplate string
	// Builder is used for the dry-run preflight only; the Job itself is created
	// by the reconciler.
	Builder launcher.Builder
	// PreflightJob asks the API server to admit the Job without persisting it.
	// That one call answers every remaining cause of a negative ack —
	// exhausted quota, tolerations the cluster will not accept, an image the
	// policy forbids — while the alternative is to learn them from a pod that
	// never schedules, after the run has been acknowledged as started.
	PreflightJob bool
	Clock        func() time.Time
	Log          *slog.Logger
}

// Result is the outcome of materialising one lease. Exactly one of the three
// states holds.
type Result struct {
	// AgentRun is set when the work is durable in the cluster and the lease may
	// be acknowledged.
	AgentRun *agentrunv1alpha1.AgentRun
	// Rejection is set when the work could not be materialised and nothing was
	// started. It rides back on a negative ack: the backend raises the epoch,
	// requeues the run and excludes this cluster, which is strictly better than
	// the alternative that existed before delta D16 — create the Job, let it
	// fail, report Failed, and bill somebody for it.
	Rejection *clusterv1.AckRejection
	// Superseded means a previous epoch's AgentRun is still terminating. The
	// lease is neither acknowledged nor rejected: if the deletion does not
	// finish inside the ack deadline the backend reissues the work at epoch+1,
	// which is a self-healing path that needs no code of its own.
	Superseded bool
}

// CallbackToken is minted here, not by the backend, and is what stops any pod
// in the agents namespace from posting a forged completion for another run.
// The callback URL is cluster-local, so the backend could not have supplied
// either half.
func mintCallbackToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("materialize: mint callback token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (m *Materializer) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

func (m *Materializer) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Apply makes the lease durable: the AgentRun, then the objects it owns. It is
// idempotent — a controller that crashed between creating the CR and
// acknowledging the lease calls it again on the next poll and gets the same
// answer, because the object's name is a pure function of the run ID.
func (m *Materializer) Apply(ctx context.Context, lease clusterv1.Lease) (Result, error) {
	name := agentrunv1alpha1.ObjectName(lease.RunID)
	key := types.NamespacedName{Namespace: m.Namespace, Name: name}

	var existing agentrunv1alpha1.AgentRun
	err := m.Client.Get(ctx, key, &existing)
	switch {
	case err == nil:
		if res, done := m.reconcileExisting(ctx, &existing, lease); done {
			return res, nil
		}
		// Superseded and now deleted, or still going: either way the caller
		// waits. A create against an object the API server still holds would
		// fail on the name.
		return Result{Superseded: true}, nil
	case !apierrors.IsNotFound(err):
		return Result{}, fmt.Errorf("materialize: get %s: %w", name, err)
	}

	if rejection := m.preflight(lease); rejection != nil {
		return Result{Rejection: rejection}, nil
	}

	token, err := mintCallbackToken()
	if err != nil {
		return Result{}, err
	}

	cr := m.buildAgentRun(lease)
	if err := m.Client.Create(ctx, cr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race with our own previous attempt. The next poll finds
			// the object and takes the idempotent path.
			return Result{Superseded: true}, nil
		}
		if rejection := rejectionForAPIError(err); rejection != nil {
			return Result{Rejection: rejection}, nil
		}
		return Result{}, fmt.Errorf("materialize: create %s: %w", name, err)
	}

	// Create returns what the API server stored, which is the read-back: an
	// older CRD has already dropped whatever it did not recognise by the time
	// this line runs, and the annotation holding the hash of what was sent is
	// the only surviving evidence.
	if lost := m.prunedFields(lease.Spec, cr.Spec.RenderedRunSpec); len(lost) > 0 {
		m.log().Error("this cluster's AgentRun CRD dropped spec fields on write",
			"runID", lease.RunID, "fields", lost)
		m.deleteQuietly(ctx, cr)
		return Result{Rejection: &clusterv1.AckRejection{
			Code: clusterv1.RejectSpecFieldsPruned,
			Message: fmt.Sprintf("this cluster's AgentRun CRD dropped %d spec field(s) on write: %s",
				len(lost), strings.Join(lost, ", ")),
			Fields: lost,
		}}, nil
	}

	if err := m.ensureMaterials(ctx, cr, lease, token); err != nil {
		return Result{}, err
	}

	if rejection, err := m.preflightJob(ctx, cr); err != nil {
		return Result{}, err
	} else if rejection != nil {
		m.deleteQuietly(ctx, cr)
		return Result{Rejection: rejection}, nil
	}

	m.log().Info("lease materialised",
		"runID", lease.RunID, "epoch", lease.Epoch, "attempt", lease.Attempt,
		"agentRun", cr.Name, "namespace", m.Namespace)
	return Result{AgentRun: cr}, nil
}

// reconcileExisting handles the three ways an AgentRun for this run can already
// be there. The bool reports whether the Result is final.
func (m *Materializer) reconcileExisting(ctx context.Context, existing *agentrunv1alpha1.AgentRun, lease clusterv1.Lease) (Result, bool) {
	switch {
	case existing.DeletionTimestamp != nil:
		// A previous epoch on its way out; the finalizer is waiting for its Job.
		return Result{}, false

	case existing.Spec.LeaseEpoch == lease.Epoch:
		// The restart case: the CR was created and the ack never went out. The
		// ack is idempotent on (runID, epoch), so repeating it is exactly what
		// the contract asks for. The materials are re-ensured because the crash
		// may equally have landed between the CR and the Secret.
		if err := m.ensureMaterials(ctx, existing, lease, ""); err != nil {
			m.log().Error("re-ensuring materials for an existing run", "runID", lease.RunID, "error", err)
			return Result{}, false
		}
		m.log().Info("lease already materialised; acknowledging again",
			"runID", lease.RunID, "epoch", lease.Epoch)
		return Result{AgentRun: existing}, true

	case existing.Spec.LeaseEpoch > lease.Epoch:
		// The backend never issues a lower epoch than one it has already handed
		// out, so this is a defect rather than a race. Refusing is the safe
		// answer: accepting would let an old lease replace newer ownership.
		return Result{Rejection: &clusterv1.AckRejection{
			Code: clusterv1.RejectMaterializationFailed,
			Message: fmt.Sprintf("this cluster already holds run %s at epoch %d, newer than the offered %d",
				lease.RunID, existing.Spec.LeaseEpoch, lease.Epoch),
		}}, true

	default:
		// A new epoch is new ownership. The spec is immutable and a status
		// carried over from the previous epoch would describe a run that never
		// started, so the object is replaced rather than edited.
		m.log().Info("replacing a superseded AgentRun",
			"runID", lease.RunID, "was", existing.Spec.LeaseEpoch, "now", lease.Epoch)
		if err := m.Client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			m.log().Error("deleting a superseded AgentRun", "runID", lease.RunID, "error", err)
		}
		return Result{}, false
	}
}

// preflight refuses what the cluster would refuse anyway, before an object
// exists. Every check here names a spec path, because that path is what the
// operator sees in the UI when the run comes back as a configuration failure.
func (m *Materializer) preflight(lease clusterv1.Lease) *clusterv1.AckRejection {
	if m.CallbackURL == "" {
		// A controller misconfiguration, not a bad spec: without a callback URL
		// the pod has nowhere to report and the run would finish as
		// CompletedWithoutResult every time.
		return &clusterv1.AckRejection{
			Code:    clusterv1.RejectMaterializationFailed,
			Message: "this controller has no callback URL configured",
		}
	}
	if _, err := launcher.Resources(lease.Spec.Runtime.Resources); err != nil {
		var invalid *launcher.InvalidFieldError
		if errors.As(err, &invalid) {
			return &clusterv1.AckRejection{
				Code:    clusterv1.RejectInvalidSpec,
				Message: invalid.Error(),
				Fields:  []string{invalid.Path},
			}
		}
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: err.Error()}
	}
	if lease.Spec.Image == "" {
		return &clusterv1.AckRejection{
			Code: clusterv1.RejectInvalidSpec, Message: "spec.image is empty", Fields: []string{"image"},
		}
	}
	if lease.Spec.Prompt.Key == "" {
		return &clusterv1.AckRejection{
			Code: clusterv1.RejectInvalidSpec, Message: "spec.prompt has no key", Fields: []string{"prompt.key"},
		}
	}
	return nil
}

// preflightJob asks the API server to admit the Job without persisting it. A
// dry-run create runs the same admission chain as the real one — quota,
// policies, the structural schema — and consumes nothing, so the causes the CRD
// contract lists as "detectable before the Job" actually get detected there.
func (m *Materializer) preflightJob(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (*clusterv1.AckRejection, error) {
	if !m.PreflightJob {
		return nil, nil
	}
	job, err := m.Builder.Job(cr, 1)
	if err != nil {
		var invalid *launcher.InvalidFieldError
		if errors.As(err, &invalid) {
			return &clusterv1.AckRejection{
				Code: clusterv1.RejectInvalidSpec, Message: invalid.Error(), Fields: []string{invalid.Path},
			}, nil
		}
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: err.Error()}, nil
	}
	if err := m.Client.Create(ctx, job, client.DryRunAll); err != nil {
		if rejection := rejectionForAPIError(err); rejection != nil {
			return rejection, nil
		}
		// Anything else — a webhook that timed out, an API server restart — is
		// not evidence about this spec, and refusing the run over it would turn
		// a blip into a burned lease.
		m.log().Warn("job preflight was inconclusive; continuing",
			"runID", cr.Spec.RunID, "error", err)
	}
	return nil, nil
}

// rejectionForAPIError translates the deterministic refusals. Forbidden with a
// quota message is the one worth separating: an exhausted ResourceQuota is
// otherwise visible only as a series of failures with "exceeded quota" buried
// in the message, and the backend cannot see the namespace to know better.
func rejectionForAPIError(err error) *clusterv1.AckRejection {
	switch {
	case apierrors.IsForbidden(err):
		if strings.Contains(err.Error(), "exceeded quota") {
			return &clusterv1.AckRejection{
				Code: clusterv1.RejectQuotaExhausted, Message: err.Error(),
			}
		}
		return &clusterv1.AckRejection{
			Code: clusterv1.RejectImageNotAllowed, Message: err.Error(),
		}
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return &clusterv1.AckRejection{
			Code: clusterv1.RejectInvalidSpec, Message: err.Error(),
		}
	}
	return nil
}

// prunedFields compares what came back with what was sent. The hash is the fast
// path; the field walk runs only when they differ, and only a *loss* counts —
// a field the API server added is a default being applied, not a setting going
// missing.
func (m *Materializer) prunedFields(sent, stored runv1.RenderedRunSpec) []string {
	if HashSpec(sent) == HashSpec(stored) {
		return nil
	}
	return LostFields(sent, stored)
}

func (m *Materializer) buildAgentRun(lease clusterv1.Lease) *agentrunv1alpha1.AgentRun {
	materials := agentrunv1alpha1.MaterialsRef{
		SecretName: agentrunv1alpha1.SecretName(lease.RunID),
	}
	if len(lease.RoleConfig) > 0 {
		materials.ConfigMapName = agentrunv1alpha1.ConfigMapName(lease.RunID)
	}

	annotations := map[string]string{
		agentrunv1alpha1.AnnotationSpecHash: HashSpec(lease.Spec),
		agentrunv1alpha1.AnnotationLeasedAt: m.now().UTC().Format(time.RFC3339),
	}
	if m.RunURLTemplate != "" {
		annotations[agentrunv1alpha1.AnnotationRunURL] = fmt.Sprintf(m.RunURLTemplate, lease.RunID)
	}

	return &agentrunv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:        agentrunv1alpha1.ObjectName(lease.RunID),
			Namespace:   m.Namespace,
			Labels:      kmeta.Labels(lease.RunID, lease.Epoch, lease.Attempt, m.ClusterID, lease.Spec.Agent),
			Annotations: annotations,
			// The finalizer goes on at creation rather than with the Job: a
			// controller that crashes between the two would otherwise leave a
			// deletable CR with a live pod under it.
			Finalizers: []string{agentrunv1alpha1.FinalizerTerminateJob},
		},
		Spec: agentrunv1alpha1.AgentRunSpec{
			RunID:           lease.RunID,
			LeaseEpoch:      lease.Epoch,
			RenderedRunSpec: lease.Spec,
			Materials:       materials,
			CallbackURL:     m.CallbackURL,
		},
	}
}

// ensureMaterials writes the Secret and, if the role contributed files, the
// ConfigMap. Both carry an owner reference to the AgentRun, so no long-lived
// secret is left behind in the agents namespace when the run is collected.
//
// An empty token means "keep whatever is already there": on the restart path
// the pod may already be holding the callback token from the first attempt, and
// minting a new one would make its report unauthenticatable.
func (m *Materializer) ensureMaterials(ctx context.Context, cr *agentrunv1alpha1.AgentRun, lease clusterv1.Lease, token string) error {
	owner := kmeta.OwnerRef(cr)
	labels := kmeta.Labels(lease.RunID, lease.Epoch, lease.Attempt, m.ClusterID, lease.Spec.Agent)

	bundle, err := json.Marshal(lease.Artifacts)
	if err != nil {
		return fmt.Errorf("materialize: encode artifact bundle: %w", err)
	}

	data := make(map[string][]byte, len(lease.Secrets)+2)
	for k, v := range lease.Secrets {
		data[k] = []byte(v)
	}
	data[runv1.SecretKeyPresigned] = bundle

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.Spec.Materials.SecretName,
			Namespace:       m.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Type: corev1.SecretTypeOpaque,
	}

	var current corev1.Secret
	err = m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: secret.Name}, &current)
	switch {
	case apierrors.IsNotFound(err):
		if token == "" {
			if token, err = mintCallbackToken(); err != nil {
				return err
			}
		}
		data[runv1.SecretKeyCallbackToken] = []byte(token)
		secret.Data = data
		if err := m.Client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("materialize: create secret %s: %w", secret.Name, err)
		}
	case err != nil:
		return fmt.Errorf("materialize: get secret %s: %w", secret.Name, err)
	default:
		if existing := current.Data[runv1.SecretKeyCallbackToken]; len(existing) > 0 {
			data[runv1.SecretKeyCallbackToken] = existing
		} else if token != "" {
			data[runv1.SecretKeyCallbackToken] = []byte(token)
		} else if token, err = mintCallbackToken(); err == nil {
			data[runv1.SecretKeyCallbackToken] = []byte(token)
		} else {
			return err
		}
		current.Data = data
		current.Labels = labels
		current.OwnerReferences = []metav1.OwnerReference{owner}
		if err := m.Client.Update(ctx, &current); err != nil {
			return fmt.Errorf("materialize: update secret %s: %w", secret.Name, err)
		}
	}

	if len(lease.RoleConfig) == 0 {
		return nil
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.Spec.Materials.ConfigMapName,
			Namespace:       m.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Data: lease.RoleConfig,
	}
	if err := m.Client.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("materialize: create configmap %s: %w", cm.Name, err)
		}
		if err := m.Client.Update(ctx, cm); err != nil {
			return fmt.Errorf("materialize: update configmap %s: %w", cm.Name, err)
		}
	}
	return nil
}

func (m *Materializer) deleteQuietly(ctx context.Context, cr *agentrunv1alpha1.AgentRun) {
	if err := m.Client.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		m.log().Error("deleting a rejected AgentRun", "agentRun", cr.Name, "error", err)
	}
}
