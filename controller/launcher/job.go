// Package launcher turns an AgentRun into the Job that executes it.
//
// Section 11 of the CRD contract is a table of values with a reason attached to
// each, and the reasons are not stylistic: backoffLimit zero is what makes an
// exit code mean what the failure-class table says it means, the absent
// ttlSecondsAfterFinished is what stops Kubernetes deleting the evidence before
// the controller has read it, and requests equal to limits is what keeps an
// agent from being evicted an hour and one model bill into its work. This
// package is that table, executable.
package launcher

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/kmeta"
)

const (
	// ContainerName is what the controller looks for in the pod status, so it
	// is a constant and not a convention.
	ContainerName = "agent"

	// DefaultDeadlineSlackSeconds separates the agent's own budget from the
	// Job's backstop. The entrypoint stops the agent at timeoutSeconds and then
	// still has to upload the partial result, commit, push and report; if the
	// Job's deadline fired at the same moment, a normal timeout would lose
	// everything the run had already produced.
	DefaultDeadlineSlackSeconds int64 = 600

	// DefaultGraceSeconds is how long the pod gets between SIGTERM and SIGKILL.
	// It is passed to the entrypoint as well, so that the shutdown budget is a
	// number both sides know rather than one of them guesses.
	DefaultGraceSeconds int64 = 60

	// agentUID is the uid the image's USER line sets. The pod must agree with
	// it: run the container as anyone else and the CLI caches that were moved
	// into $HOME to make readOnlyRootFilesystem possible land in a directory
	// that user does not own.
	agentUID int64 = 1000
	agentGID int64 = 1000

	// secretMode is 0400: readable by the agent user and nobody else. The
	// files here are an hour-long git token, a model key and bearer
	// capabilities on a bucket prefix.
	secretMode int32 = 0400
)

// Volume names, shared between the mounts and the volumes so that a rename
// cannot desynchronise the two halves.
const (
	volWorkspace  = "workspace"
	volRunPrivate = "run-private"
	volHome       = "home"
	volTmp        = "tmp"
	volSecrets    = "secrets"
	volRole       = "role"
)

// Builder holds the cluster-local facts the spec cannot carry.
type Builder struct {
	Namespace string
	ClusterID runv1.ULID
	// GraceSeconds and DeadlineSlackSeconds are configurable because a cluster
	// with slow image pulls or a large monorepo needs different numbers, and
	// the alternative is an operator editing a constant.
	GraceSeconds         int64
	DeadlineSlackSeconds int64
	// ServiceAccountName is the agent pod's identity. It is not the
	// controller's: the pod gets no API access at all, and the token is not
	// mounted.
	ServiceAccountName string
}

// Job builds the Job for one attempt. It returns an error only for things the
// cluster would reject anyway — an unparsable quantity above all — so that the
// failure is reported as a rejection before anything is created rather than as
// a pod that never schedules.
func (b Builder) Job(cr *agentrunv1alpha1.AgentRun, attempt int32) (*batchv1.Job, error) {
	if attempt < 1 {
		return nil, fmt.Errorf("launcher: attempt %d is not a counting number", attempt)
	}
	spec := cr.Spec.RenderedRunSpec

	resources, err := Resources(spec.Runtime.Resources)
	if err != nil {
		return nil, err
	}

	labels := kmeta.Labels(cr.Spec.RunID, cr.Spec.LeaseEpoch, attempt, b.ClusterID, spec.Agent)

	backoffLimit := int32(0)
	activeDeadline := int64(spec.Runtime.TimeoutSeconds) + b.deadlineSlack()
	grace := b.grace()
	automount := false
	runAsNonRoot, readOnlyRoot, allowEscalation := true, true, false
	uid, gid := agentUID, agentGID

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            agentrunv1alpha1.JobName(cr.Spec.RunID, attempt),
			Namespace:       b.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{kmeta.OwnerRef(cr)},
		},
		Spec: batchv1.JobSpec{
			// Every attempt is a decision of the controller: counted in the
			// status, reported to the backend, and subject to the rule that
			// only infrastructure failures are repeated. A restart performed by
			// Kubernetes would be none of those things.
			BackoffLimit: &backoffLimit,
			// A backstop for an entrypoint that hung, not the agent's timeout.
			ActiveDeadlineSeconds: &activeDeadline,
			// Deliberately no TTLSecondsAfterFinished. The TTL controller would
			// delete the Job, and with it the pod and its exit code, before this
			// controller had observed and reported them — turning a success
			// into Unknown. Cleanup belongs to the AgentRun's ownerReference.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  &automount,
					ServiceAccountName:            b.ServiceAccountName,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &runAsNonRoot,
						RunAsUser:      &uid,
						RunAsGroup:     &gid,
						FSGroup:        &gid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            ContainerName,
						Image:           spec.Image,
						ImagePullPolicy: corev1.PullPolicy(spec.ImagePullPolicy),
						Env:             b.env(cr, attempt),
						Resources:       resources,
						VolumeMounts:    volumeMounts(cr),
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							AllowPrivilegeEscalation: &allowEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes(cr),
				},
			},
		},
	}

	for _, name := range spec.ImagePullSecrets {
		job.Spec.Template.Spec.ImagePullSecrets = append(
			job.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
	if len(spec.Runtime.NodeSelector) > 0 {
		sel := make(map[string]string, len(spec.Runtime.NodeSelector))
		for k, v := range spec.Runtime.NodeSelector {
			sel[k] = string(v)
		}
		job.Spec.Template.Spec.NodeSelector = sel
	}
	job.Spec.Template.Spec.Tolerations = tolerations(spec.Runtime.Tolerations)
	return job, nil
}

func (b Builder) grace() int64 {
	if b.GraceSeconds > 0 {
		return b.GraceSeconds
	}
	return DefaultGraceSeconds
}

func (b Builder) deadlineSlack() int64 {
	if b.DeadlineSlackSeconds > 0 {
		return b.DeadlineSlackSeconds
	}
	return DefaultDeadlineSlackSeconds
}

// env is the non-secret half of the runtime contract. Everything secret is a
// file under MountSecrets instead: a variable lands in /proc/self/environ,
// which every child process inherits — including the agent, the one process in
// this system explicitly assumed to be capable of exfiltrating what it reads.
//
// Optional variables are set whenever the spec has an answer, including the
// boolean ones. The image reads an absent boolean as false, so leaving
// HALIPHRON_CREATE_PR unset is not "use the default" — it is "do not open a
// pull request", on a run whose spec said to open one.
func (b Builder) env(cr *agentrunv1alpha1.AgentRun, attempt int32) []corev1.EnvVar {
	spec := cr.Spec.RenderedRunSpec
	vals := map[string]string{
		runv1.EnvContract:       strconv.Itoa(runv1.ContractMajor),
		runv1.EnvRunID:          string(cr.Spec.RunID),
		runv1.EnvAttempt:        strconv.Itoa(int(attempt)),
		runv1.EnvClusterID:      string(b.ClusterID),
		runv1.EnvCallbackURL:    cr.Spec.CallbackURL,
		runv1.EnvGraceSeconds:   strconv.FormatInt(b.grace(), 10),
		runv1.EnvAgent:          string(spec.Agent),
		runv1.EnvModel:          spec.Model,
		runv1.EnvTimeoutSeconds: strconv.Itoa(int(spec.Runtime.TimeoutSeconds)),
		runv1.EnvPromptSHA256:   spec.Prompt.SHA256,
		runv1.EnvStorageBucket:  spec.Prompt.Bucket,
		runv1.EnvStoragePrefix:  fmt.Sprintf(runv1.StoragePrefixRun, cr.Spec.RunID),
	}
	setIf(vals, runv1.EnvRole, spec.Role)
	setIf(vals, runv1.EnvPermissionMode, string(spec.Runtime.PermissionMode))
	if spec.Runtime.MaxTurns > 0 {
		vals[runv1.EnvMaxTurns] = strconv.Itoa(int(spec.Runtime.MaxTurns))
	}
	if spec.ToolPolicy != nil {
		setIf(vals, runv1.EnvAllowedTools, strings.Join(spec.ToolPolicy.Allow, ","))
		setIf(vals, runv1.EnvDeniedTools, strings.Join(spec.ToolPolicy.Deny, ","))
	}
	if spec.Repo.URL != "" {
		vals[runv1.EnvRepoURL] = spec.Repo.URL
		vals[runv1.EnvGitProvider] = string(spec.Repo.Provider)
		vals[runv1.EnvBaseBranch] = spec.Repo.BaseBranch
		vals[runv1.EnvTargetBranch] = spec.Repo.TargetBranch
		vals[runv1.EnvCreatePR] = strconv.FormatBool(spec.Repo.CreatePR == nil || *spec.Repo.CreatePR)
		vals[runv1.EnvSubmodules] = strconv.FormatBool(spec.Repo.Submodules)
		vals[runv1.EnvLFS] = strconv.FormatBool(spec.Repo.LFS)
		if spec.Repo.CloneDepth > 0 {
			vals[runv1.EnvCloneDepth] = strconv.Itoa(int(spec.Repo.CloneDepth))
		}
	}
	if o := spec.Observability; o != nil {
		setIf(vals, runv1.EnvOTLPEndpoint, o.OTLPEndpoint)
		setIf(vals, runv1.EnvTraceparent, o.Traceparent)
		if o.LogChunkIntervalSeconds != nil && *o.LogChunkIntervalSeconds > 0 {
			vals[runv1.EnvLogChunkSeconds] = strconv.Itoa(int(*o.LogChunkIntervalSeconds))
		}
	}

	names := make([]string, 0, len(vals))
	for name := range vals {
		names = append(names, name)
	}
	// Sorted, so that two builds of the same attempt produce byte-identical
	// pod specs and a diff against a live Job means something changed.
	sort.Strings(names)

	out := make([]corev1.EnvVar, 0, len(vals)+len(spec.Runtime.Env))
	for _, name := range names {
		out = append(out, corev1.EnvVar{Name: name, Value: vals[name]})
	}
	// Free-form extras from the spec, last so that a run cannot quietly
	// redefine a contract variable ahead of it. The backend is obliged not to
	// put secrets here; no schema can check the contents of a string, so the
	// obligation is held by a contract test instead.
	for _, e := range spec.Runtime.Env {
		out = append(out, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	return out
}

func setIf(vals map[string]string, key, value string) {
	if value != "" {
		vals[key] = value
	}
}

// volumeMounts: four writable emptyDirs and two read-only mounts. The Secret is
// a volume rather than envFrom because its keys — git-token, mcp.json,
// presigned.json — are not valid environment variable names, and envFrom skips
// such keys silently. The pod would start with no token, no MCP configuration
// and no link to storage, and the first intelligible symptom would be "could
// not download the prompt".
func volumeMounts(cr *agentrunv1alpha1.AgentRun) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: volWorkspace, MountPath: runv1.MountWorkspace},
		{Name: volRunPrivate, MountPath: runv1.DirRunPrivate},
		{Name: volHome, MountPath: runv1.HomeDir},
		{Name: volTmp, MountPath: "/tmp"},
		{Name: volSecrets, MountPath: runv1.MountSecrets, ReadOnly: true},
	}
	if cr.Spec.Materials.ConfigMapName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name: volRole, MountPath: runv1.MountRoleConfig, ReadOnly: true,
		})
	}
	return mounts
}

func volumes(cr *agentrunv1alpha1.AgentRun) []corev1.Volume {
	mode := secretMode
	vols := []corev1.Volume{
		emptyDir(volWorkspace),
		emptyDir(volRunPrivate),
		emptyDir(volHome),
		emptyDir(volTmp),
		{Name: volSecrets, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  cr.Spec.Materials.SecretName,
				DefaultMode: &mode,
			},
		}},
	}
	if name := cr.Spec.Materials.ConfigMapName; name != "" {
		vols = append(vols, corev1.Volume{Name: volRole, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		}})
	}
	return vols
}

func emptyDir(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}}
}

func tolerations(in []runv1.Toleration) []corev1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(in))
	for _, t := range in {
		out = append(out, corev1.Toleration{
			Key:               t.Key,
			Operator:          corev1.TolerationOperator(t.Operator),
			Value:             t.Value,
			Effect:            corev1.TaintEffect(t.Effect),
			TolerationSeconds: t.TolerationSeconds,
		})
	}
	return out
}

// Resources sets requests equal to limits, which puts the pod in the Guaranteed
// QoS class. Burstable would let the kubelet evict an agent under node pressure
// half an hour into a run, and the cost of that is an hour of wall clock and a
// second bill for the model — against a saving of nothing, since the pod uses
// what it asked for.
//
// An unparsable quantity is an error rather than a skipped field: silently
// dropping a memory limit produces a run that works until the node is busy.
func Resources(r runv1.Resources) (corev1.ResourceRequirements, error) {
	list := corev1.ResourceList{}
	for _, q := range []struct {
		path  string
		name  corev1.ResourceName
		value string
	}{
		{"runtime.resources.cpu", corev1.ResourceCPU, r.CPU},
		{"runtime.resources.memory", corev1.ResourceMemory, r.Memory},
		{"runtime.resources.ephemeralStorage", corev1.ResourceEphemeralStorage, r.EphemeralStorage},
	} {
		if q.value == "" {
			continue
		}
		parsed, err := resource.ParseQuantity(q.value)
		if err != nil {
			return corev1.ResourceRequirements{}, &InvalidFieldError{Path: q.path, Value: q.value, Err: err}
		}
		list[q.name] = parsed
	}
	if len(list) == 0 {
		return corev1.ResourceRequirements{}, nil
	}
	return corev1.ResourceRequirements{Limits: list, Requests: list.DeepCopy()}, nil
}

// InvalidFieldError names the spec path at fault, because that path is what
// travels back to the backend in a negative ack and from there into the UI.
type InvalidFieldError struct {
	Path  string
	Value string
	Err   error
}

func (e *InvalidFieldError) Error() string {
	return fmt.Sprintf("%s=%q is not a valid quantity: %v", e.Path, e.Value, e.Err)
}

func (e *InvalidFieldError) Unwrap() error { return e.Err }
