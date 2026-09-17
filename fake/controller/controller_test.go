package controller_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
	"github.com/automagicops/haliphron/fake/controller"
)

// FakeController is exercised against FakeBackend, which makes every test here
// do two jobs: it checks the controller's obligations, and it checks that the
// two independent readings of the same contract agree. A rule one side
// implements from the document and the other does not is the failure these
// fakes exist to find, and it is cheaper to find it between two fakes than
// between two teams.

func setup(t *testing.T, opts ...controller.Option) (*backend.Backend, *controller.Controller) {
	t.Helper()
	b := backend.New()
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)

	// The controller shares the backend's clock: a test that skips an ack
	// deadline would otherwise also expire every token minted afterwards.
	opts = append([]controller.Option{controller.WithClock(b.Now)}, opts...)
	c, err := controller.New(srv.URL, opts...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := c.Register(context.Background(), b.BootstrapToken()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return b, c
}

func sampleSpec() runv1.RenderedRunSpec {
	return runv1.RenderedRunSpec{
		Agent:  runv1.AgentClaudeCode,
		Prompt: runv1.ObjectRef{Bucket: "haliphron", Key: "runs/x/prompt.txt", SHA256: strings.Repeat("a", 64)},
		Model:  "anthropic/claude-opus-5",
		Image:  "ghcr.io/automagicops/agent-runtime@sha256:" + strings.Repeat("b", 64),
		Repo: runv1.RepoSpec{
			URL: "https://github.com/acme/widgets.git", Provider: runv1.GitProviderGitHub,
			BaseBranch: "main", TargetBranch: "haliphron/01j8-add-rds",
		},
		Runtime: runv1.RuntimeSpec{
			TimeoutSeconds: 3600,
			Resources:      runv1.Resources{CPU: "2", Memory: "4Gi", EphemeralStorage: "20Gi"},
			MCPServers:     []runv1.MCPServer{{Name: "helm", Transport: "http", URL: "http://mcp-helm:8080"}},
		},
	}
}

// sync takes work and expects exactly n leases.
func sync(t *testing.T, c *controller.Controller, n int) {
	t.Helper()
	got, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got != n {
		t.Fatalf("took %d leases, want %d", got, n)
	}
}

// ---------------------------------------------------------------------------
// materialisation: the boundary between spec and materials
// ---------------------------------------------------------------------------

func TestNoSecretValueReachesTheCR(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	const token = "ghs_secret_value_that_must_not_leak"
	id := b.Enqueue(sampleSpec(), backend.WithSecrets(map[string]string{
		runv1.SecretKeyGitToken:  token,
		runv1.SecretKeyLLMAPIKey: "sk-model-key",
	}))
	sync(t, c, 1)

	cr, ok := c.AgentRun(id)
	if !ok {
		t.Fatal("no AgentRun was created")
	}
	raw, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// `get agentruns` must not be a way to read tokens. The structural schema
	// is what makes this hold — there is nowhere in the type to put one — but
	// runtime.env[].value is free-form, so the check is a test and not a policy
	// rule: no rule can inspect the contents of a string.
	if strings.Contains(string(raw), token) {
		t.Fatal("the git token appears in the AgentRun")
	}
	if strings.Contains(string(raw), "?sig=") {
		t.Fatal("a presigned URL appears in the AgentRun")
	}

	var secret *corev1.Secret
	for i, s := range c.Objects().Secrets {
		if s.Name == agentrunv1alpha1.SecretName(id) {
			secret = &c.Objects().Secrets[i]
		}
	}
	if secret == nil {
		t.Fatal("no per-run Secret was created")
	}
	if string(secret.Data[runv1.SecretKeyGitToken]) != token {
		t.Fatal("the git token did not reach the Secret")
	}
	// Minted by the controller, not the backend: callbackURL is cluster-local,
	// and without a token any pod in this namespace could post a forged
	// completion for somebody else's run.
	if len(secret.Data[runv1.SecretKeyCallbackToken]) == 0 {
		t.Fatal("no callback token was minted")
	}
	if len(secret.OwnerReferences) != 1 {
		t.Fatal("the Secret has no ownerReference: it would outlive the run")
	}
}

func TestRoleConfigBecomesAConfigMapAndOnlyItsNameRides(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec(), backend.WithRoleConfig(map[string]string{
		"settings.json": `{"permissions":{"allow":["Bash"]}}`,
	}))
	sync(t, c, 1)

	cr, _ := c.AgentRun(id)
	if cr.Spec.Materials.ConfigMapName != agentrunv1alpha1.ConfigMapName(id) {
		t.Fatalf("materials.configMapName = %q", cr.Spec.Materials.ConfigMapName)
	}
	raw, _ := json.Marshal(cr.Spec)
	if strings.Contains(string(raw), "permissions") {
		t.Fatal("the role config contents reached the spec")
	}

	cms := c.Objects().ConfigMaps
	if len(cms) != 1 || cms[0].Data["settings.json"] == "" {
		t.Fatalf("the role config did not become a ConfigMap: %+v", cms)
	}
}

func TestARunWithoutRoleConfigGetsNoConfigMap(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	sync(t, c, 1)

	cr, _ := c.AgentRun(id)
	if cr.Spec.Materials.ConfigMapName != "" {
		t.Fatalf("configMapName = %q for a run with no role files", cr.Spec.Materials.ConfigMapName)
	}
	if len(c.Objects().ConfigMaps) != 0 {
		t.Fatal("an empty ConfigMap was created")
	}
}

func TestTheJobFollowsTheContract(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	sync(t, c, 1)

	jobs := c.Objects().Jobs
	if len(jobs) != 1 {
		t.Fatalf("want 1 Job, got %d", len(jobs))
	}
	job := jobs[0]
	if job.Name != agentrunv1alpha1.JobName(id, 1) {
		t.Fatalf("job name %q", job.Name)
	}

	// Every one of these changes how an exit code is interpreted, which is why
	// the table is contract and not an implementation choice.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("backoffLimit must be 0: a restart by Kubernetes never increments attempt and never reaches a report")
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Error("the Job must not have a TTL: the TTL controller would delete it before the outcome was reported, turning a success into Unknown")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 3600 {
		t.Error("activeDeadlineSeconds must exceed the agent's own timeout, or a normal timeout has no time to upload its partial result")
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the agent pod does not need the cluster API")
	}

	container := pod.Containers[0]
	limits, requests := container.Resources.Limits, container.Resources.Requests
	if len(limits) == 0 || limits.Cpu().Cmp(*requests.Cpu()) != 0 || limits.Memory().Cmp(*requests.Memory()) != 0 {
		t.Error("requests must equal limits: an agent evicted halfway costs an hour of work and a second model bill")
	}
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be set")
	}

	// A volume and not envFrom. Not one of the Secret's keys is a valid
	// environment variable name, so envFrom would skip all of them silently and
	// the pod would start with no token, no MCP config and no link to storage.
	var mounted bool
	for _, m := range container.VolumeMounts {
		if m.MountPath == runv1.MountSecrets && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("the Secret is not mounted read-only at %s", runv1.MountSecrets)
	}
	for _, e := range container.Env {
		if strings.Contains(strings.ToLower(e.Name), "token") || strings.HasPrefix(e.Value, "ghs_") {
			t.Errorf("secret-looking variable %s in the container environment", e.Name)
		}
	}
	if envOf(container.Env, runv1.EnvRunID) != string(id) {
		t.Errorf("%s = %q", runv1.EnvRunID, envOf(container.Env, runv1.EnvRunID))
	}
	if envOf(container.Env, runv1.EnvCallbackURL) == "" {
		t.Error("the pod has nowhere to post its completion")
	}
}

func envOf(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// pruning: the main trap of the CRD contract
// ---------------------------------------------------------------------------

func TestPrunedFieldsAreRefusedBeforeAnyJobIsCreated(t *testing.T) {
	t.Parallel()
	b, c := setup(t, controller.WithPrunedFields("runtime.mcpServers"))
	id := b.Enqueue(sampleSpec())

	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// A structural schema does not ignore an unknown field, it deletes it and
	// answers 201. Running the agent without the MCP servers it was promised
	// would succeed at something nobody asked for.
	if len(c.Objects().Jobs) != 0 {
		t.Fatal("a Job was created for a spec this cluster cannot store")
	}
	if len(c.Objects().AgentRuns) != 0 {
		t.Fatal("the AgentRun was left behind after the refusal")
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("want Failed with only one cluster, got %s", st.Status)
	}
	if st.FailureClass != runv1.FailureConfig {
		t.Fatalf("want class config, got %q", st.FailureClass)
	}
	if !strings.Contains(st.Message, string(clusterv1.RejectSpecFieldsPruned)) {
		t.Fatalf("the reason did not survive to the backend: %q", st.Message)
	}
}

// ---------------------------------------------------------------------------
// the run's lifecycle
// ---------------------------------------------------------------------------

func TestASuccessfulRunEndToEnd(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()

	sync(t, c, 1)
	if st, _ := b.RunState(id); st.Status != clusterv1.StatusDispatched {
		t.Fatalf("after the ack, want Dispatched, got %s", st.Status)
	}

	if err := c.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st, _ := b.RunState(id); st.Status != clusterv1.StatusRunning {
		t.Fatalf("want Running, got %s", st.Status)
	}

	if err := c.Finish(ctx, id, controller.Outcome{
		ExitCode: runv1.ExitSuccess, Summary: "added the rds module",
		CostUSD: "0.4231", PRURL: "https://github.com/acme/widgets/pull/42",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusSucceeded {
		t.Fatalf("want Succeeded, got %s", st.Status)
	}
	if st.Completion == nil || st.Completion.Repo == nil || st.Completion.Repo.PRURL == "" {
		t.Fatal("the PR link did not reach the backend")
	}
	if len(st.Charges) != 1 {
		t.Fatalf("charged %d times", len(st.Charges))
	}
}

func TestAnInfraFailureIsRetriedLocallyWithoutANewEpoch(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()

	sync(t, c, 1)
	before, _ := b.RunState(id)
	if err := c.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 137 is OOMKilled: the platform stopped the process, so it is infra and
	// retriable, and the controller does not ask permission.
	if err := c.Finish(ctx, id, controller.Outcome{ExitCode: 137, Reason: "OOMKilled"}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	st, _ := b.RunState(id)
	if st.Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", st.Attempt)
	}
	if st.Epoch != before.Epoch {
		t.Fatalf("epoch moved to %d: a local retry is not a change of ownership", st.Epoch)
	}
	if st.Status == clusterv1.StatusFailed {
		t.Fatal("a retriable failure was reported as terminal")
	}

	// Each attempt is its own Job, and the old one is gone.
	jobs := c.Objects().Jobs
	if len(jobs) != 1 || jobs[0].Name != agentrunv1alpha1.JobName(id, 2) {
		t.Fatalf("want only the attempt-2 Job, got %+v", jobNames(c))
	}
}

func TestAnAgentFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	// Exit 10 is the agent CLI failing. Replaying it spends the budget again on
	// the same outcome.
	if err := c.Finish(ctx, id, controller.Outcome{ExitCode: runv1.ExitAgentError}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("want Failed, got %s", st.Status)
	}
	if st.Attempt != 1 {
		t.Fatalf("the run was retried: attempt %d", st.Attempt)
	}
}

func TestTheRetryBudgetIsBounded(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	budget := int32(2)
	spec := sampleSpec()
	spec.Retry = &runv1.RetrySpec{MaxInfraRetries: &budget}
	id := b.Enqueue(spec)
	ctx := context.Background()
	sync(t, c, 1)

	for i := 0; i < 3; i++ {
		if err := c.Finish(ctx, id, controller.Outcome{ExitCode: 137, Reason: "OOMKilled"}); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("want Failed once the budget ran out, got %s", st.Status)
	}
	if st.Attempt != 3 {
		t.Fatalf("want 3 attempts from a budget of 2 retries, got %d", st.Attempt)
	}
}

func TestStartupFailuresAreTheControllersToClassify(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	budget := int32(0)
	spec := sampleSpec()
	spec.Retry = &runv1.RetrySpec{MaxInfraRetries: &budget}
	id := b.Enqueue(spec)
	sync(t, c, 1)

	// The container never ran, so there is no exit code and no pod to classify
	// the cause. An image that does not exist otherwise hangs at Starting
	// forever.
	if err := c.ObserveFailure(context.Background(), id, "ImagePullBackOff", runv1.FailureInfra); err != nil {
		t.Fatalf("observe: %v", err)
	}
	st, _ := b.RunState(id)
	if st.TerminalPhase != runv1.PhaseFailed || st.FailureClass != runv1.FailureInfra {
		t.Fatalf("want a Failed/infra terminal phase, got %q/%s", st.TerminalPhase, st.FailureClass)
	}
}

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

func TestCancelDoesNotOverwriteASuccessAlreadyObserved(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	// The pod exited zero while the cancellation was still travelling. The
	// first terminal phase wins, on both sides.
	if err := c.Finish(ctx, id, controller.Outcome{ExitCode: runv1.ExitSuccess, CostUSD: "0.10"}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	b.Cancel(id)
	if _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusSucceeded {
		t.Fatalf("a late cancel rewrote the outcome: %s", st.Status)
	}
}

func TestCancelStopsARunningJob(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	b.Cancel(id)
	if _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if len(c.Objects().Jobs) != 0 {
		t.Fatal("the Job survived the cancellation")
	}
	st, _ := b.RunState(id)
	if st.TerminalPhase != runv1.PhaseCancelled {
		t.Fatalf("want a Cancelled terminal phase, got %q", st.TerminalPhase)
	}
	// A cancelled pod may or may not have uploaded a result, so the backend
	// records the terminal phase and goes looking in storage. The outcome is
	// carried by the phase, not by the status.
	if st.Status != clusterv1.StatusCompletedWithoutResult {
		t.Fatalf("want CompletedWithoutResult pending the storage read, got %s", st.Status)
	}
}

func TestAbandonDeletesEverythingAndSilencesTheRun(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	b.InjectCommand(id, clusterv1.Command{Type: clusterv1.CommandAbandon, Reason: "reassigned"})
	if _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	objects := c.Objects()
	if len(objects.AgentRuns) != 0 || len(objects.Jobs) != 0 || len(objects.Secrets) != 0 {
		t.Fatalf("objects survived the abandon: %+v", objects)
	}
	if !c.Abandoned(id) {
		t.Fatal("the run was not marked abandoned")
	}

	before := len(c.SentFor(id))
	// Nothing further, ever. A report after an abandon appends this cluster's
	// state to a run that now belongs to another one.
	_ = c.Finish(ctx, id, controller.Outcome{ExitCode: runv1.ExitSuccess})
	_, _ = c.Heartbeat(ctx)
	if after := len(c.SentFor(id)); after != before {
		t.Fatalf("%d further reports after the abandon", after-before)
	}
}

func TestAnUnknownCommandIsIgnored(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	// A newer control plane will send one. Crashing here would take down every
	// older controller in the fleet during an upgrade.
	b.InjectCommand(id, clusterv1.Command{Type: "quarantine"})
	if _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("an unknown command broke the heartbeat: %v", err)
	}
	if st, _ := b.RunState(id); st.Status != clusterv1.StatusRunning {
		t.Fatalf("the run changed state on an unknown command: %s", st.Status)
	}
}

// ---------------------------------------------------------------------------
// fencing and restarts
// ---------------------------------------------------------------------------

func TestAStaleEpochLeadsToAbandon(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	// The work was reassigned while this controller was busy.
	b.Reassign(id)

	if err := c.Finish(ctx, id, controller.Outcome{ExitCode: runv1.ExitSuccess}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !c.Abandoned(id) {
		t.Fatal("a stale-epoch rejection did not lead to abandoning the run")
	}
	if st, _ := b.RunState(id); st.TerminalPhase != "" {
		t.Fatalf("the zombie drove the run terminal: %s", st.TerminalPhase)
	}
}

func TestARestartBeforeTheAckRepeatsItUnderTheSameEpoch(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)

	before, _ := b.RunState(id)
	if err := c.Restart(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	after, _ := b.RunState(id)
	// Idempotent on (runID, epoch): a second ack is not a second dispatch, and
	// without the repeat the ack deadline expires on work sitting right here.
	if after.Epoch != before.Epoch {
		t.Fatalf("epoch moved from %d to %d across a restart", before.Epoch, after.Epoch)
	}
	if after.Status != clusterv1.StatusDispatched {
		t.Fatalf("want Dispatched, got %s", after.Status)
	}
	if len(c.Held()) != 1 {
		t.Fatalf("the run was not recovered from the cluster: %v", c.Held())
	}
}

func TestARestartDoesNotRefillTheRetryBudget(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	budget := int32(2)
	spec := sampleSpec()
	spec.Retry = &runv1.RetrySpec{MaxInfraRetries: &budget}
	id := b.Enqueue(spec)
	ctx := context.Background()
	sync(t, c, 1)

	for i := 0; i < 2; i++ {
		if err := c.Finish(ctx, id, controller.Outcome{ExitCode: 137}); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	if err := c.Restart(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	// The budget lives on the CR's status, not in memory. A run that has been
	// failing all afternoon must not get a fresh set of attempts because the
	// controller was rescheduled.
	if err := c.Finish(ctx, id, controller.Outcome{ExitCode: 137}); err != nil {
		t.Fatalf("finish after restart: %v", err)
	}
	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("the budget was refilled by the restart: status %s at attempt %d", st.Status, st.Attempt)
	}
}

func TestANewEpochReplacesTheCRAndLeavesOneJob(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	sync(t, c, 1)

	// The lease expired and came back to the same cluster. A new epoch is new
	// ownership: the spec is immutable and a status carried across would
	// describe a run that never started.
	b.Reassign(id)
	sync(t, c, 1)

	objects := c.Objects()
	if len(objects.AgentRuns) != 1 {
		t.Fatalf("want exactly one AgentRun per runID, got %d", len(objects.AgentRuns))
	}
	if objects.AgentRuns[0].Spec.LeaseEpoch != 2 {
		t.Fatalf("the CR still carries epoch %d", objects.AgentRuns[0].Spec.LeaseEpoch)
	}
	if len(objects.Jobs) != 1 {
		t.Fatalf("want one Job, got %v", jobNames(c))
	}
}

// ---------------------------------------------------------------------------
// availability decoupling
// ---------------------------------------------------------------------------

func TestWorkIsPlayedOutWhileTheBackendIsAway(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	b.SetUnavailable(true)
	// The agent finishes regardless. Killing an hour of work because the
	// control plane is restarting is the failure ADR 6 exists to prevent.
	if err := c.Finish(ctx, id, controller.Outcome{
		ExitCode: runv1.ExitSuccess, CostUSD: "0.9", Summary: "done",
	}); err != nil {
		t.Fatalf("finish during an outage: %v", err)
	}
	b.SetUnavailable(false)

	// The heartbeat is the reconciliation path: the state the lost ingest
	// carried is delivered an interval later.
	if _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	st, _ := b.RunState(id)
	if st.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("the outcome never reached the backend: %s", st.Status)
	}
}

func TestADroppedLongPollIsNotFatal(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	b.SetDropLongPolls(true)

	// Proxies and rolling restarts do this. A controller that treats it as
	// fatal stops polling; one that retries without a floor spins.
	if _, err := c.Sync(context.Background()); err == nil {
		t.Fatal("a dropped connection was not reported to the caller")
	}
	b.SetDropLongPolls(false)

	b.Enqueue(sampleSpec())
	sync(t, c, 1)
}

func TestACompletionThatNeverArrivesLeavesTheResultRecoverable(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)
	_ = c.Start(ctx, id)

	// The controller died between the pod's webhook and forwarding it.
	if err := c.Finish(ctx, id, controller.Outcome{
		ExitCode: runv1.ExitSuccess, SkipCompletion: true,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusCompletedWithoutResult {
		t.Fatalf("want CompletedWithoutResult, got %s", st.Status)
	}

	// The CR is held rather than reaped: dropping it now would lose the only
	// record the cluster still has.
	if c.Cleanup(id) {
		t.Fatal("the CR was reaped before the outcome was delivered")
	}
}

// ---------------------------------------------------------------------------
// reconciliation and housekeeping
// ---------------------------------------------------------------------------

func TestAPartialHeartbeatSaysNothingAboutWhatIsMissing(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	b.Enqueue(sampleSpec())
	ctx := context.Background()
	sync(t, c, 1)

	full, err := c.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(full.Leases) != 1 {
		t.Fatalf("want 1 renewed lease, got %d", len(full.Leases))
	}

	// A controller whose informer cache has not warmed must not have its work
	// taken away for not having mentioned it yet.
	if _, err := c.HeartbeatPartial(ctx); err != nil {
		t.Fatalf("partial heartbeat: %v", err)
	}
}

func TestTheFinalizerIsReleasedAfterABoundedWait(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())
	sync(t, c, 1)

	cr, _ := c.AgentRun(id)
	if len(cr.Finalizers) != 1 {
		t.Fatalf("want the terminate-job finalizer, got %v", cr.Finalizers)
	}

	// Released whether or not the pod actually stopped: a finalizer that can
	// wedge makes the namespace undeletable, and the cure is editing objects by
	// hand in production.
	c.ReleaseFinalizer(id)
	cr, _ = c.AgentRun(id)
	if len(cr.Finalizers) != 0 {
		t.Fatalf("the finalizer was not released: %v", cr.Finalizers)
	}
}

func TestQuotaExhaustedIsAnnouncedNotDiscovered(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	b.Enqueue(sampleSpec())
	c.SetQuotaExhausted(true)

	if _, err := c.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Otherwise an exhausted ResourceQuota shows up as a series of Failed runs
	// with "exceeded quota" in the message.
	got, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got != 0 {
		t.Fatalf("the backend assigned %d runs to a cluster with no quota", got)
	}
}

func TestTheControllerNeverLogsTheLeaseBody(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	const token = "ghs_controller_side_secret"
	b.Enqueue(sampleSpec(), backend.WithSecrets(map[string]string{runv1.SecretKeyGitToken: token}))
	sync(t, c, 1)

	for _, line := range c.Notes() {
		if strings.Contains(line, token) || strings.Contains(line, "?sig=") {
			t.Fatalf("secret material reached the controller's log: %q", line)
		}
	}
}

func TestRegistrationAdoptsTheControlPlanesTimings(t *testing.T) {
	t.Parallel()
	_, c := setup(t)
	timings := c.Timings()

	// The intervals come from the control plane rather than the cluster's
	// values.yaml, or staleAfter drifts per installation and no two clusters
	// agree on what it means.
	if timings.AckTimeoutSeconds != clusterv1.DefaultTimings().AckTimeoutSeconds {
		t.Fatalf("ackTimeoutSeconds = %d", timings.AckTimeoutSeconds)
	}
	if c.ClusterID() == "" {
		t.Fatal("no cluster identity was adopted")
	}
}

func TestTheAckDeadlineExpiresWhenNothingIsMaterialised(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec())

	// Take the work and never acknowledge it, the way a controller that died
	// mid-materialisation would.
	if _, err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	b.AdvanceClock(time.Duration(clusterv1.DefaultTimings().AckTimeoutSeconds+1) * time.Second)

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusQueued || st.Epoch != 2 {
		t.Fatalf("want Queued at epoch 2, got %s at %d", st.Status, st.Epoch)
	}
}

func jobNames(c *controller.Controller) []string {
	var out []string
	for _, j := range c.Objects().Jobs {
		out = append(out, j.Name)
	}
	return out
}
