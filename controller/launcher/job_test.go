package launcher

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Section 11 of the CRD contract is a table of values with a reason attached to
// each. These are those reasons, as assertions: every one of them changes what
// an exit code means, and a pod built without them produces a run that reports
// something other than what happened.

const testRunID = runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8CD")

func testRun() *agentrunv1alpha1.AgentRun {
	createPR := true
	return &agentrunv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentrunv1alpha1.ObjectName(testRunID),
			Namespace: "agents",
			UID:       types.UID("uid-1"),
		},
		Spec: agentrunv1alpha1.AgentRunSpec{
			RunID:      testRunID,
			LeaseEpoch: 1,
			RenderedRunSpec: runv1.RenderedRunSpec{
				Agent:        runv1.AgentClaudeCode,
				PromptSHA256: strings.Repeat("a", 64),
				Model:        "anthropic/claude-opus-5",
				Image:        "ghcr.io/automagicops/agent:1.0.0",
				Repo: runv1.RepoSpec{
					URL: "https://github.com/acme/widgets", Provider: runv1.GitProviderGitHub,
					BaseBranch: "main", TargetBranch: "haliphron/run-x", CreatePR: &createPR,
				},
				Runtime: runv1.RuntimeSpec{
					TimeoutSeconds: 3600,
					Resources:      runv1.Resources{CPU: "2", Memory: "4Gi", EphemeralStorage: "20Gi"},
				},
			},
			Materials:   agentrunv1alpha1.MaterialsRef{SecretName: agentrunv1alpha1.SecretName(testRunID)},
			CallbackURL: "http://haliphron-controller.haliphron.svc:8083",
		},
		Status: agentrunv1alpha1.AgentRunStatus{Attempt: 1},
	}
}

func build(t *testing.T, cr *agentrunv1alpha1.AgentRun, attempt int32) *corev1.PodSpec {
	t.Helper()
	b := Builder{Namespace: "agents", ClusterID: "01J8X4K2ZQ7YB3M9F0R5W6T8CE"}
	job, err := b.Job(cr, attempt, nil)
	if err != nil {
		t.Fatalf("build the job: %v", err)
	}
	return &job.Spec.Template.Spec
}

// TestEveryAttemptIsADecisionOfTheController. A restart performed by Kubernetes
// would not increment the attempt, would not appear in any report and would
// bypass the rule that only infrastructure failures are repeated.
func TestEveryAttemptIsADecisionOfTheController(t *testing.T) {
	b := Builder{Namespace: "agents"}
	job, err := b.Job(testRun(), 2, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit is %v; Kubernetes would retry behind the controller's back", job.Spec.BackoffLimit)
	}
	if job.Name != agentrunv1alpha1.JobName(testRunID, 2) {
		t.Fatalf("the attempt is not in the Job's name: %s", job.Name)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("a restart inside the pod is invisible from outside")
	}
}

// TestTheJobHasNoTTL. The TTL controller would delete the Job, and with it the
// pod and its exit code, before the controller had observed and reported them —
// turning a success into an Unknown that needs a human.
func TestTheJobHasNoTTL(t *testing.T) {
	b := Builder{Namespace: "agents"}
	job, err := b.Job(testRun(), 1, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("the Job carries a TTL of %ds", *job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 3600 {
		t.Fatalf("the backstop must outlast the agent's own budget, got %v", job.Spec.ActiveDeadlineSeconds)
	}
	owners := job.OwnerReferences
	if len(owners) != 1 || owners[0].UID != "uid-1" {
		t.Fatal("the Job is not owned by its AgentRun, so nothing will ever collect it")
	}
}

// TestThePodIsGuaranteed. An agent evicted half an hour into its work costs an
// hour of wall clock and a second bill for the model, against a saving of
// nothing: the pod uses what it asked for.
func TestThePodIsGuaranteed(t *testing.T) {
	spec := build(t, testRun(), 1)
	resources := spec.Containers[0].Resources
	for name, want := range map[corev1.ResourceName]string{
		corev1.ResourceCPU: "2", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "20Gi",
	} {
		limit, ok := resources.Limits[name]
		if !ok {
			t.Fatalf("no limit for %s", name)
		}
		if limit.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("%s limit is %s, want %s", name, limit.String(), want)
		}
		request, ok := resources.Requests[name]
		if !ok || request.Cmp(limit) != 0 {
			t.Fatalf("%s request %v does not equal its limit %v: the pod is not Guaranteed", name, request, limit)
		}
	}
}

// TestAnUnparsableQuantityIsRefusedRatherThanDropped. Silently omitting a
// memory limit produces a run that works until the node is busy.
func TestAnUnparsableQuantityIsRefusedRatherThanDropped(t *testing.T) {
	cr := testRun()
	cr.Spec.Runtime.Resources.Memory = "four gigabytes"
	b := Builder{Namespace: "agents"}
	_, err := b.Job(cr, 1, nil)
	if err == nil {
		t.Fatal("a malformed quantity was accepted")
	}
	var invalid *InvalidFieldError
	if !errorAs(err, &invalid) || invalid.Path != "runtime.resources.memory" {
		t.Fatalf("the error does not name the field the backend has to be told about: %v", err)
	}
}

// TestTheSecretIsAVolumeAndNotAnEnvironment. Two of its keys are file-shaped
// and none of them is a valid variable name, so envFrom would skip every one of
// them in silence: the pod would start with no token, no MCP configuration and
// no link to storage, and the first intelligible symptom would be "could not
// download the prompt".
func TestTheSecretIsAVolumeAndNotAnEnvironment(t *testing.T) {
	spec := build(t, testRun(), 1)
	container := spec.Containers[0]

	if len(container.EnvFrom) != 0 {
		t.Fatal("the Secret is mounted through envFrom, which silently drops every key it has")
	}

	var mount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].MountPath == runv1.MountSecrets {
			mount = &container.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly {
		t.Fatalf("the secrets are not mounted read-only at %s", runv1.MountSecrets)
	}

	var volume *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == mount.Name {
			volume = &spec.Volumes[i]
		}
	}
	if volume == nil || volume.Secret == nil {
		t.Fatal("the secrets volume does not come from a Secret")
	}
	if volume.Secret.SecretName != agentrunv1alpha1.SecretName(testRunID) {
		t.Fatalf("the wrong Secret is mounted: %s", volume.Secret.SecretName)
	}
	if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0400 {
		t.Fatalf("the secret files are not 0400: %v", volume.Secret.DefaultMode)
	}
}

// TestThePodRunsAsTheUserTheImageBuilds. The image moves the CLI caches into
// $HOME so that the root filesystem can be read-only; running as anybody else
// puts them in a directory that user does not own, and the failure reads as a
// broken agent CLI.
func TestThePodRunsAsTheUserTheImageBuilds(t *testing.T) {
	spec := build(t, testRun(), 1)

	if spec.SecurityContext == nil || spec.SecurityContext.RunAsUser == nil {
		t.Fatal("no user is set")
	}
	if *spec.SecurityContext.RunAsUser != agentUID {
		t.Fatalf("the pod runs as %d and the image's USER is %d", *spec.SecurityContext.RunAsUser, agentUID)
	}
	if spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Fatal("runAsNonRoot is not set")
	}
	if spec.SecurityContext.SeccompProfile == nil ||
		spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("the seccomp profile is not RuntimeDefault")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("the agent pod is given a service account token it has no use for")
	}

	sc := spec.Containers[0].SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatal("the root filesystem is writable")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation is allowed")
	}

	// readOnlyRootFilesystem is only achievable because four directories are
	// writable volumes. Dropping any of them turns it back into an aspiration.
	mounted := map[string]bool{}
	for _, m := range spec.Containers[0].VolumeMounts {
		mounted[m.MountPath] = true
	}
	for _, path := range []string{runv1.MountWorkspace, runv1.DirRunPrivate, runv1.HomeDir, "/tmp"} {
		if !mounted[path] {
			t.Fatalf("%s is not writable; the read-only root will fail the first write", path)
		}
	}
}

// TestTheImageGetsEverythingItRefusesToStartWithout. The entrypoint treats
// these as required and exits with a configuration error otherwise, which is a
// run that fails before it costs anything — but also before anybody learns what
// the controller forgot.
func TestTheImageGetsEverythingItRefusesToStartWithout(t *testing.T) {
	spec := build(t, testRun(), 3)
	env := envMap(spec.Containers[0].Env)

	for _, name := range []string{
		runv1.EnvContract, runv1.EnvRunID, runv1.EnvCallbackURL, runv1.EnvModel,
		runv1.EnvPromptSHA256, runv1.EnvAgent, runv1.EnvAttempt,
		runv1.EnvTimeoutSeconds, runv1.EnvGraceSeconds,
	} {
		if env[name] == "" {
			t.Fatalf("%s is not set; the entrypoint refuses to start without it", name)
		}
	}
	if env[runv1.EnvAttempt] != "3" {
		t.Fatalf("the attempt is wrong: %s", env[runv1.EnvAttempt])
	}
}

// TestABooleanTheImageReadsAsFalseWhenAbsentIsAlwaysSet. The image reads an
// unset HALIPHRON_CREATE_PR as false, so omitting it is not "use the default" —
// it is "do not open a pull request", on a run whose spec said to open one.
func TestABooleanTheImageReadsAsFalseWhenAbsentIsAlwaysSet(t *testing.T) {
	spec := build(t, testRun(), 1)
	env := envMap(spec.Containers[0].Env)

	if env[runv1.EnvCreatePR] != "true" {
		t.Fatalf("%s is %q for a spec that asks for a pull request", runv1.EnvCreatePR, env[runv1.EnvCreatePR])
	}
	if env[runv1.EnvSubmodules] != "false" || env[runv1.EnvLFS] != "false" {
		t.Fatal("the repository booleans are not stated explicitly")
	}

	// A run without a repository sets none of them, because there is nothing
	// to clone and the image checks the URL first.
	cr := testRun()
	cr.Spec.Repo = runv1.RepoSpec{}
	env = envMap(build(t, cr, 1).Containers[0].Env)
	if _, ok := env[runv1.EnvRepoURL]; ok {
		t.Fatal("a run without a repository was given a repository URL")
	}
}

// TestOnlyThePromptIsReadFromASecret. A literal variable lands in
// /proc/self/environ, which every child process inherits — including the agent,
// the one process in this system explicitly assumed to be capable of
// exfiltrating what it reads.
//
// Exactly one entry is allowed to come from a Secret, and it is named here
// rather than described by a rule: the prompt is the task, the one value the
// agent is meant to read, and nothing is protected by withholding it from the
// process whose purpose is to act on it. Every other Secret key stays a file
// under MountSecrets.
func TestOnlyThePromptIsReadFromASecret(t *testing.T) {
	spec := build(t, testRun(), 1)

	var fromSecret []string
	for _, e := range spec.Containers[0].Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		fromSecret = append(fromSecret, e.Name)
		if e.Name != runv1.EnvPrompt {
			t.Errorf("%s is read from a Secret into the environment", e.Name)
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		switch {
		case ref.Key != runv1.SecretKeyPrompt:
			t.Errorf("the prompt is read from key %q, want %q", ref.Key, runv1.SecretKeyPrompt)
		case ref.Name != agentrunv1alpha1.SecretName(testRunID):
			t.Errorf("the prompt is read from Secret %q, want the run's own", ref.Name)
		case e.Value != "":
			// A literal would put customer text into the Job's spec and into
			// `kubectl describe pod`, which is the whole reason for the
			// reference.
			t.Errorf("the prompt also has a literal value of %d bytes", len(e.Value))
		case ref.Optional == nil || !*ref.Optional:
			// Without optional, a Secret written by an older controller gives a
			// pod stuck in CreateContainerConfigError rather than one that
			// starts and fails at validate with a message about the prompt.
			t.Error("the prompt reference is not optional")
		}
	}
	if len(fromSecret) != 1 {
		t.Errorf("entries read from a Secret: %v, want exactly the prompt", fromSecret)
	}

	for _, e := range spec.Containers[0].Env {
		if e.ValueFrom != nil {
			continue
		}
		for _, forbidden := range []string{"token", "secret", "key", "presigned"} {
			if strings.Contains(strings.ToLower(e.Name), forbidden) &&
				!strings.EqualFold(e.Name, runv1.EnvPromptSHA256) {
				t.Fatalf("%s looks like secret material in the environment", e.Name)
			}
		}
	}
}

// The checkpoint reaches the next attempt as a variable, in the contract's
// execution order rather than the order the reports arrived in: the entrypoint's
// resume rule is "every phase before the first unfinished one is done", which is
// only meaningful against a fixed sequence.
func TestTheCheckpointReachesTheNextAttempt(t *testing.T) {
	// Absent on a first attempt, which the entrypoint reads as "nothing is done
	// yet" rather than as a failure.
	env := envMap(build(t, testRun(), 1).Containers[0].Env)
	if v, ok := env[runv1.EnvCompletedPhases]; ok {
		t.Errorf("a first attempt was handed a checkpoint of %q", v)
	}

	b := Builder{Namespace: "agents", ClusterID: "01J8X4K2ZQ7YB3M9F0R5W6T8CE"}
	job, err := b.Job(testRun(), 2, []runv1.RuntimePhase{
		// Deliberately out of order, and with a repeat.
		runv1.RuntimePhasePersist, runv1.RuntimePhaseInit, runv1.RuntimePhaseRun,
		runv1.RuntimePhaseInit,
	})
	if err != nil {
		t.Fatalf("build the job: %v", err)
	}
	env = envMap(job.Spec.Template.Spec.Containers[0].Env)
	if got, want := env[runv1.EnvCompletedPhases], "init,run,persist"; got != want {
		t.Errorf("%s = %q, want %q", runv1.EnvCompletedPhases, got, want)
	}
}

// The mode is told rather than inferred, and an unset one is relay: a CR
// written before the field existed belongs to an installation that had no other
// mode, and defaulting to the one that needs no configuration is the only safe
// direction.
func TestArtifactModeDefaultsToRelay(t *testing.T) {
	env := envMap(build(t, testRun(), 1).Containers[0].Env)
	if got := env[runv1.EnvArtifactMode]; got != string(runv1.ArtifactModeRelay) {
		t.Errorf("%s = %q, want relay", runv1.EnvArtifactMode, got)
	}

	cr := testRun()
	cr.Spec.ArtifactMode = runv1.ArtifactModeObjectStore
	env = envMap(build(t, cr, 1).Containers[0].Env)
	if got := env[runv1.EnvArtifactMode]; got != string(runv1.ArtifactModeObjectStore) {
		t.Errorf("%s = %q, want object-store", runv1.EnvArtifactMode, got)
	}

	// And there is no bucket variable in either: the pod addresses no bucket by
	// name, so a name in its environment is one it could leak.
	if _, ok := env["HALIPHRON_STORAGE_BUCKET"]; ok {
		t.Error("the pod was told a bucket name")
	}
}

func envMap(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		out[e.Name] = e.Value
	}
	return out
}

func errorAs(err error, target **InvalidFieldError) bool {
	for err != nil {
		if e, ok := err.(*InvalidFieldError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
