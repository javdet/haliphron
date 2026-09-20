package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// componentAgentRun marks the objects this controller owns, so that an operator
// can select them apart from anything else in the agents namespace.
const componentAgentRun = "agent-run"

// Turning a lease into cluster objects. The boundary rule is mechanical and is
// the whole reason this code is worth having twice: whatever the controller
// materialises does not go into the spec. Secrets, presigned links and role
// files become objects; only their names ride in the CR.

// materialize builds the Secret, the ConfigMap and the AgentRun, writes them,
// and reads the CR back. It returns a rejection when the read-back does not
// match what was written — the only way to learn that this cluster's CRD is
// older than the spec it was handed.
func (c *Controller) materialize(lease clusterv1.Lease) (*runState, *clusterv1.AckRejection) {
	name := agentrunv1alpha1.ObjectName(lease.RunID)

	// A new epoch is new ownership, so the old CR goes rather than being
	// edited: the spec is immutable, and a status carried across epochs would
	// describe a run that never started. At most one AgentRun per runID.
	if existing, ok := c.objects.getAgentRun(name); ok && existing.Spec.LeaseEpoch != lease.Epoch {
		c.noteLocked("replacing agentrun %s: epoch %d superseded by %d",
			name, existing.Spec.LeaseEpoch, lease.Epoch)
		c.objects.deleteAgentRun(name)
	}

	state := &runState{
		runID:   lease.RunID,
		epoch:   lease.Epoch,
		attempt: lease.Attempt,
		lease:   lease,
		phase:   runv1.PhasePending,
		// The callback token is minted here, not by the backend: callbackURL
		// is cluster-local, and without a token any pod in this namespace could
		// post a forged completion for someone else's run.
		callbackToken: mintCallbackToken(),
		specHash:      hashSpec(lease.Spec),
	}

	cr := c.buildAgentRun(lease, state)
	stored := c.objects.createAgentRun(cr)

	// The API server answered 201 either way. The hash is the only evidence.
	if got := hashSpec(stored.Spec.RenderedRunSpec); got != state.specHash {
		lost := lostPaths(lease.Spec, stored.Spec.RenderedRunSpec)
		c.objects.deleteAgentRun(name)
		return nil, &clusterv1.AckRejection{
			Code: clusterv1.RejectSpecFieldsPruned,
			Message: fmt.Sprintf(
				"this cluster's AgentRun CRD dropped %d spec field(s) on write", len(lost)),
			Fields: lost,
		}
	}

	owner := ownerRef(stored)
	c.objects.putSecret(c.buildSecret(lease, state, owner))
	if len(lease.RoleConfig) > 0 {
		c.objects.putConfigMap(c.buildConfigMap(lease, owner))
	}
	return state, nil
}

// buildAgentRun adds the four fields the backend cannot know and nothing else.
func (c *Controller) buildAgentRun(lease clusterv1.Lease, state *runState) *agentrunv1alpha1.AgentRun {
	name := agentrunv1alpha1.ObjectName(lease.RunID)
	materials := agentrunv1alpha1.MaterialsRef{
		SecretName: agentrunv1alpha1.SecretName(lease.RunID),
	}
	if len(lease.RoleConfig) > 0 {
		materials.ConfigMapName = agentrunv1alpha1.ConfigMapName(lease.RunID)
	}

	return &agentrunv1alpha1.AgentRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: agentrunv1alpha1.GroupVersion.String(),
			Kind:       "AgentRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
			UID:       types.UID(name),
			Labels:    c.labelsFor(lease),
			Annotations: map[string]string{
				// Annotations are not pruned, which is what makes the hash
				// survive a write that drops half the spec.
				agentrunv1alpha1.AnnotationSpecHash: state.specHash,
				agentrunv1alpha1.AnnotationLeasedAt: c.now().UTC().Format("2006-01-02T15:04:05Z07:00"),
			},
			// The finalizer holds the CR until the Job and pod have stopped.
			Finalizers: []string{agentrunv1alpha1.FinalizerTerminateJob},
		},
		Spec: agentrunv1alpha1.AgentRunSpec{
			RunID:           lease.RunID,
			LeaseEpoch:      lease.Epoch,
			RenderedRunSpec: lease.Spec,
			Materials:       materials,
			CallbackURL:     c.callbackURL,
		},
	}
}

func (c *Controller) labelsFor(lease clusterv1.Lease) map[string]string {
	return map[string]string{
		agentrunv1alpha1.LabelRunID:     strings.ToLower(string(lease.RunID)),
		agentrunv1alpha1.LabelEpoch:     strconv.FormatInt(lease.Epoch, 10),
		agentrunv1alpha1.LabelAttempt:   strconv.Itoa(int(lease.Attempt)),
		agentrunv1alpha1.LabelClusterID: strings.ToLower(string(c.clusterID)),
		agentrunv1alpha1.LabelAgent:     string(lease.Spec.Agent),
		agentrunv1alpha1.LabelComponent: componentAgentRun,
	}
}

// buildSecret carries the lease's secret material plus the controller's own
// callback token. The keys are fixed by the Cluster API contract; unknown ones
// pass through untouched.
func (c *Controller) buildSecret(lease clusterv1.Lease, state *runState, owner metav1.OwnerReference) *corev1.Secret {
	data := map[string][]byte{}
	for k, v := range lease.Secrets {
		data[k] = []byte(v)
	}
	// The prompt is a Secret key rather than a CR field. It is not a
	// credential — it is the task — but prompts carry customer context, and in
	// the spec they would land in `kubectl get agentrun -o yaml` and in every
	// GitOps diff. It reaches the container through valueFrom.secretKeyRef,
	// which keeps it out of the Job's spec as well.
	data[runv1.SecretKeyPrompt] = []byte(lease.Prompt)

	// The presigned bundle is object-store mode only. In relay mode there is
	// nothing to sign — the pod posts to the controller — and writing an empty
	// bundle would give the entrypoint a mode to misread.
	if !lease.Artifacts.Relay() {
		if bundle, err := json.Marshal(lease.Artifacts); err == nil {
			data[runv1.SecretKeyPresigned] = bundle
		}
	}
	data[runv1.SecretKeyCallbackToken] = []byte(state.callbackToken)

	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: c.objectMeta(agentrunv1alpha1.SecretName(lease.RunID), lease, owner),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func (c *Controller) buildConfigMap(lease clusterv1.Lease, owner metav1.OwnerReference) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: c.objectMeta(agentrunv1alpha1.ConfigMapName(lease.RunID), lease, owner),
		Data:       lease.RoleConfig,
	}
}

func (c *Controller) objectMeta(name string, lease clusterv1.Lease, owner metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       c.namespace,
		Labels:          c.labelsFor(lease),
		OwnerReferences: []metav1.OwnerReference{owner},
	}
}

func ownerRef(cr *agentrunv1alpha1.AgentRun) metav1.OwnerReference {
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

// buildJob implements the table in section 11 of the CRD contract. Every value
// here changes how an exit code is interpreted, which is why the table is part
// of the contract rather than an implementation choice.
func (c *Controller) buildJob(state *runState) *batchv1.Job {
	lease := state.lease
	spec := lease.Spec
	cr, _ := c.objects.getAgentRun(agentrunv1alpha1.ObjectName(state.runID))

	var backoffLimit int32 // 0: every attempt is a controller decision
	// A backstop larger than the agent's own budget, so that a normal timeout
	// still has time to upload its partial result before the Job is killed.
	activeDeadline := int64(spec.Runtime.TimeoutSeconds) + 600
	grace := int64(30)
	runAsNonRoot, readOnlyRoot, noEscalation := true, true, false
	// 1000, matching the image's USER line. The CLI caches live in /home/agent
	// owned by uid 1000, so a pod that runs as anyone else finds them in a
	// directory it does not own — and the real controller uses 1000, so a fake
	// that used anything else would have the backend track asserting on a pod
	// nobody builds.
	var runAsUser int64 = 1000
	automount := false
	var secretMode int32 = 0400

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentrunv1alpha1.JobName(state.runID, state.attempt),
			Namespace: c.namespace,
			Labels:    c.labelsFor(lease),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &activeDeadline,
			// Deliberately no TTLSecondsAfterFinished: the Kubernetes TTL
			// controller would delete the Job and pod before this controller
			// observed and reported them, and a success would finish as
			// Unknown. Cleanup runs solely through the CR's ownerReference.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: c.labelsFor(lease)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  &automount,
					TerminationGracePeriodSeconds: &grace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &runAsNonRoot,
						RunAsUser:      &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: spec.Image,
						Env:   c.containerEnv(state),
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							AllowPrivilegeEscalation: &noEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: resourcesFor(spec.Runtime.Resources),
						// A volume, not envFrom: the Secret's keys are fixed by
						// contract 1 and not one of them is a valid environment
						// variable name. envFrom would skip them all silently
						// and the pod would start with no token, no MCP config
						// and no link to storage.
						VolumeMounts: volumeMounts(lease),
					}},
					Volumes: c.volumes(state, secretMode),
				},
			},
		},
	}
	if cr != nil {
		job.OwnerReferences = []metav1.OwnerReference{ownerRef(cr)}
	}
	if len(spec.ImagePullSecrets) > 0 {
		for _, name := range spec.ImagePullSecrets {
			job.Spec.Template.Spec.ImagePullSecrets = append(
				job.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
		}
	}
	if len(spec.Runtime.NodeSelector) > 0 {
		sel := map[string]string{}
		for k, v := range spec.Runtime.NodeSelector {
			sel[k] = string(v)
		}
		job.Spec.Template.Spec.NodeSelector = sel
	}
	return job
}

// containerEnv sets only the non-secret contract variables. Anything secret is
// a file under MountSecrets: an environment variable lands in
// /proc/self/environ, which every child process inherits — including the agent,
// the one process here assumed capable of exfiltrating what it can read.
func (c *Controller) containerEnv(state *runState) []corev1.EnvVar {
	spec := state.lease.Spec
	vals := map[string]string{
		runv1.EnvContract:       strconv.Itoa(runv1.ContractMajor),
		runv1.EnvRunID:          string(state.runID),
		runv1.EnvAttempt:        strconv.Itoa(int(state.attempt)),
		runv1.EnvClusterID:      string(c.clusterID),
		runv1.EnvCallbackURL:    c.callbackURL,
		runv1.EnvGraceSeconds:   "30",
		runv1.EnvAgent:          string(spec.Agent),
		runv1.EnvModel:          spec.Model,
		runv1.EnvRole:           spec.Role,
		runv1.EnvTimeoutSeconds: strconv.Itoa(int(spec.Runtime.TimeoutSeconds)),
		runv1.EnvRepoURL:        spec.Repo.URL,
		runv1.EnvGitProvider:    string(spec.Repo.Provider),
		runv1.EnvBaseBranch:     spec.Repo.BaseBranch,
		runv1.EnvTargetBranch:   spec.Repo.TargetBranch,
		runv1.EnvPromptSHA256:   spec.PromptSHA256,
		runv1.EnvArtifactMode:   string(artifactMode(state.lease)),
		runv1.EnvStoragePrefix:  fmt.Sprintf(runv1.StoragePrefixRun, state.runID),
		// The image reads an absent boolean as false, so leaving these unset is
		// not "use the default" — it is "do not open a pull request", on a run
		// whose spec said to open one.
		runv1.EnvCreatePR:   strconv.FormatBool(spec.Repo.CreatePR == nil || *spec.Repo.CreatePR),
		runv1.EnvSubmodules: strconv.FormatBool(spec.Repo.Submodules),
		runv1.EnvLFS:        strconv.FormatBool(spec.Repo.LFS),
	}
	if spec.Repo.CloneDepth > 0 {
		vals[runv1.EnvCloneDepth] = strconv.Itoa(int(spec.Repo.CloneDepth))
	}
	if len(state.lease.CompletedPhases) > 0 {
		names := make([]string, 0, len(state.lease.CompletedPhases))
		for _, phase := range state.lease.CompletedPhases {
			names = append(names, string(phase))
		}
		vals[runv1.EnvCompletedPhases] = strings.Join(names, ",")
	}
	if spec.Runtime.MaxTurns > 0 {
		vals[runv1.EnvMaxTurns] = strconv.Itoa(int(spec.Runtime.MaxTurns))
	}
	if spec.Runtime.PermissionMode != "" {
		vals[runv1.EnvPermissionMode] = string(spec.Runtime.PermissionMode)
	}
	if spec.ToolPolicy != nil {
		vals[runv1.EnvAllowedTools] = strings.Join(spec.ToolPolicy.Allow, ",")
		vals[runv1.EnvDeniedTools] = strings.Join(spec.ToolPolicy.Deny, ",")
	}
	if spec.Observability != nil {
		vals[runv1.EnvOTLPEndpoint] = spec.Observability.OTLPEndpoint
		vals[runv1.EnvTraceparent] = spec.Observability.Traceparent
	}

	out := make([]corev1.EnvVar, 0, len(vals)+len(spec.Runtime.Env))
	names := make([]string, 0, len(vals))
	for name := range vals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, corev1.EnvVar{Name: name, Value: vals[name]})
	}
	// Non-secret extras from the spec. The backend is obliged not to put
	// secrets here, and that obligation is checked by a test rather than by a
	// policy: no rule can inspect the contents of a string.
	for _, e := range spec.Runtime.Env {
		out = append(out, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	// Last, and the only entry with no Value: the prompt, from one named key of
	// the per-run Secret. Naming one key is what separates this from the envFrom
	// failure mode ADR 33 describes.
	optional := true
	out = append(out, corev1.EnvVar{
		Name: runv1.EnvPrompt,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: agentrunv1alpha1.SecretName(state.runID),
				},
				Key:      runv1.SecretKeyPrompt,
				Optional: &optional,
			},
		},
	})
	return out
}

// artifactMode is what the pod is told about where its results go. An empty
// mode reads as relay, matching the real controller.
func artifactMode(lease clusterv1.Lease) runv1.ArtifactMode {
	if lease.Artifacts.Mode == runv1.ArtifactModeObjectStore {
		return runv1.ArtifactModeObjectStore
	}
	return runv1.ArtifactModeRelay
}

func volumeMounts(lease clusterv1.Lease) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: runv1.MountWorkspace},
		{Name: "run-private", MountPath: runv1.DirRunPrivate},
		{Name: "home", MountPath: runv1.HomeDir},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "secrets", MountPath: runv1.MountSecrets, ReadOnly: true},
	}
	if len(lease.RoleConfig) > 0 {
		mounts = append(mounts, corev1.VolumeMount{
			Name: "role", MountPath: runv1.MountRoleConfig, ReadOnly: true,
		})
	}
	return mounts
}

// volumes are the four writable emptyDirs that make readOnlyRootFilesystem
// achievable rather than aspirational, plus the two read-only mounts.
func (c *Controller) volumes(state *runState, secretMode int32) []corev1.Volume {
	vols := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "run-private", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "secrets", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  agentrunv1alpha1.SecretName(state.runID),
			DefaultMode: &secretMode,
		}}},
	}
	if len(state.lease.RoleConfig) > 0 {
		vols = append(vols, corev1.Volume{Name: "role", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: agentrunv1alpha1.ConfigMapName(state.runID),
				},
			},
		}})
	}
	return vols
}

// resourcesFor sets requests equal to limits: QoS class Guaranteed, because an
// agent evicted halfway through costs an hour of work and a second model bill,
// and Burstable buys nothing in return.
func resourcesFor(r runv1.Resources) corev1.ResourceRequirements {
	list := corev1.ResourceList{}
	if q, err := resource.ParseQuantity(r.CPU); err == nil && r.CPU != "" {
		list[corev1.ResourceCPU] = q
	}
	if q, err := resource.ParseQuantity(r.Memory); err == nil && r.Memory != "" {
		list[corev1.ResourceMemory] = q
	}
	if q, err := resource.ParseQuantity(r.EphemeralStorage); err == nil && r.EphemeralStorage != "" {
		list[corev1.ResourceEphemeralStorage] = q
	}
	if len(list) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: list, Requests: list.DeepCopy()}
}

// hashSpec digests the spec as it arrived. Go marshals struct fields in
// declaration order and map keys sorted, so the encoding is stable without a
// canonicaliser.
func hashSpec(spec runv1.RenderedRunSpec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// lostPaths names what the write dropped. A rejection that says "something was
// pruned" sends an operator reading CRD YAML; one that names
// runtime.mcpServers does not.
func lostPaths(sent, stored runv1.RenderedRunSpec) []string {
	var a, b map[string]any
	rawSent, _ := json.Marshal(sent)
	rawStored, _ := json.Marshal(stored)
	_ = json.Unmarshal(rawSent, &a)
	_ = json.Unmarshal(rawStored, &b)

	var out []string
	diffPaths("", a, b, &out)
	sort.Strings(out)
	return out
}

func diffPaths(prefix string, sent, stored map[string]any, out *[]string) {
	for key, value := range sent {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		other, ok := stored[key]
		if !ok {
			*out = append(*out, path)
			continue
		}
		sentChild, sentIsMap := value.(map[string]any)
		storedChild, storedIsMap := other.(map[string]any)
		if sentIsMap && storedIsMap {
			diffPaths(path, sentChild, storedChild, out)
			continue
		}
		if !reflect.DeepEqual(value, other) {
			*out = append(*out, path)
		}
	}
}

func mintCallbackToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}
