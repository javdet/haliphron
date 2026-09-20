// Package contract runs the AgentRun CRD against a real API server.
//
// The generated manifest is only half a contract: CEL transition rules, cost
// estimation, structural pruning and defaulting are all enforced by the API
// server and by nothing else. A CRD that controller-gen produces happily can
// still be rejected on apply, and a field the backend sends can still be
// dropped without a word. These tests are how that is found out here rather
// than in a customer's cluster.
package contract

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

const (
	testNamespace = "default"
	testRunID     = runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8CD")
)

var (
	restCfg *rest.Config
	scheme  = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if err := agentrunv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	restCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// sampleRun is a spec with every field populated, because the immutability rule
// is priced against the largest object the schema allows, not against a
// minimal one.
func sampleRun(name string) *agentrunv1alpha1.AgentRun {
	createPR := true
	retries := int32(3)
	ttl := int32(86400)
	return &agentrunv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				agentrunv1alpha1.LabelRunID: "01j8x4k2zq7yb3m9f0r5w6t8cd",
			},
			Annotations: map[string]string{
				agentrunv1alpha1.AnnotationSpecHash: "sha256:deadbeef",
			},
		},
		Spec: agentrunv1alpha1.AgentRunSpec{
			RunID:      testRunID,
			LeaseEpoch: 1,
			RenderedRunSpec: runv1.RenderedRunSpec{
				Agent:        runv1.AgentClaudeCode,
				PromptSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Model:        "anthropic/claude-opus-5",
				Role:         "coder",
				Image:        "ghcr.io/automagicops/agent-runtime@sha256:abc",
				Repo: runv1.RepoSpec{
					URL:          "https://github.com/org/repo.git",
					Provider:     runv1.GitProviderGitHub,
					BaseBranch:   "main",
					TargetBranch: "haliphron/01j8x4k2-add-rds",
					CreatePR:     &createPR,
				},
				Runtime: runv1.RuntimeSpec{
					TimeoutSeconds: 3600,
					MaxTurns:       150,
					PermissionMode: "acceptEdits",
					Env:            []runv1.EnvVar{{Name: "HALIPHRON_RUN_ID", Value: string(testRunID)}},
					Resources: runv1.Resources{
						CPU: "2", Memory: "4Gi", EphemeralStorage: "20Gi",
					},
					NodeSelector: map[string]runv1.LabelValue{"workload": "agents"},
					Tolerations: []runv1.Toleration{
						{Key: "agents", Operator: "Equal", Value: "true", Effect: "NoSchedule"},
					},
					MCPServers: []runv1.MCPServer{{Name: "helm", Transport: "http", URL: "http://mcp-helm:8080"}},
				},
				ToolPolicy:              &runv1.ToolPolicy{Allow: []string{"Bash"}, Deny: []string{"WebFetch"}},
				Budget:                  &runv1.BudgetSpec{MaxCostUSD: "5.00"},
				Retry:                   &runv1.RetrySpec{MaxInfraRetries: &retries},
				Observability:           &runv1.ObservabilitySpec{Traceparent: "00-0af7-00f0-01"},
				TTLSecondsAfterFinished: &ttl,
			},
			Materials: agentrunv1alpha1.MaterialsRef{
				SecretName:    agentrunv1alpha1.SecretName(testRunID),
				ConfigMapName: agentrunv1alpha1.ConfigMapName(testRunID),
			},
			CallbackURL: "http://haliphron-controller.haliphron-agents.svc:8080/completion",
		},
	}
}

// TestCRDInstalls is the load-bearing assertion: envtest refuses to start if
// the API server rejects the manifest, which is what a CEL rule over the budget
// looks like. The whole-spec immutability rule survives only because every
// string and map in the spec is bounded.
func TestCRDInstallsAndAcceptsAFullSpec(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	run := sampleRun("ar-full")
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	var got agentrunv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Runtime.Resources.EphemeralStorage != "20Gi" {
		t.Fatalf("ephemeralStorage round-trip: %q", got.Spec.Runtime.Resources.EphemeralStorage)
	}
}

// TestSpecIsImmutable pins the guarantee the controller relies on when it
// reports status: the run described by the spec is the run that started.
func TestSpecIsImmutable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	run := sampleRun("ar-immutable")
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}
	run.Spec.Model = "anthropic/claude-haiku-4-5"
	if err := c.Update(ctx, run); err == nil {
		t.Fatal("expected the API server to reject a spec update, it accepted one")
	}
}

// TestUnknownSpecFieldIsPruned is the reason the controller carries a spec hash
// and re-reads what it wrote.
//
// A newer backend may render a field that this cluster's CRD has never heard
// of. The API server does not reject it — structural schemas prune silently —
// so the run would execute with the field missing and nobody would know. This
// test states that behaviour as a fact of the contract, so the mitigation is
// designed for rather than discovered.
func TestUnknownSpecFieldIsPruned(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(agentrunv1alpha1.GroupVersion.WithKind("AgentRun"))
	obj.SetNamespace(testNamespace)
	obj.SetName("ar-pruned")
	spec := map[string]any{
		"runID":        string(testRunID),
		"leaseEpoch":   int64(1),
		"agent":        "claude-code",
		"promptSHA256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"model":        "anthropic/claude-opus-5",
		"image":        "ghcr.io/automagicops/agent-runtime:1",
		"repo":         map[string]any{},
		"runtime":      map[string]any{"timeoutSeconds": int64(3600)},
		"materials":    map[string]any{"secretName": "ar-x-s"},
		"callbackURL":  "http://haliphron-controller:8080",
		// A field from a future version of the contract.
		"sandboxProfile": "strict",
	}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		t.Fatalf("set spec: %v", err)
	}
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}

	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, found, _ := unstructured.NestedString(stored.Object, "spec", "sandboxProfile"); found {
		t.Fatal("unknown field survived: the pruning hazard this contract is built around is gone, " +
			"and the spec-hash read-back check can be dropped")
	}
}

// TestDefaultsApply keeps the defaults in one place. If the CRD stops applying
// them, the backend and the controller start disagreeing about what an omitted
// field means.
func TestDefaultsApply(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(agentrunv1alpha1.GroupVersion.WithKind("AgentRun"))
	obj.SetNamespace(testNamespace)
	obj.SetName("ar-defaults")
	spec := map[string]any{
		"runID":        string(testRunID),
		"leaseEpoch":   int64(1),
		"agent":        "codex",
		"promptSHA256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"model":        "openai/gpt-5",
		"image":        "ghcr.io/automagicops/agent-runtime:1",
		"repo":         map[string]any{},
		"runtime":      map[string]any{},
		"materials":    map[string]any{"secretName": "ar-x-s"},
		"callbackURL":  "http://haliphron-controller:8080",
	}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		t.Fatalf("set spec: %v", err)
	}
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}

	var got agentrunv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Runtime.TimeoutSeconds != 3600 {
		t.Errorf("timeoutSeconds default = %d, want 3600", got.Spec.Runtime.TimeoutSeconds)
	}
	if got.Spec.Repo.BaseBranch != "main" {
		t.Errorf("baseBranch default = %q, want main", got.Spec.Repo.BaseBranch)
	}
	if got.Spec.Repo.CreatePR == nil || !*got.Spec.Repo.CreatePR {
		t.Errorf("createPR default = %v, want true", got.Spec.Repo.CreatePR)
	}
	if got.Spec.TTLSecondsAfterFinished == nil || *got.Spec.TTLSecondsAfterFinished != 86400 {
		t.Errorf("ttlSecondsAfterFinished default = %v, want 86400", got.Spec.TTLSecondsAfterFinished)
	}
}

// TestStatusIsASubresource proves that a status write cannot smuggle a spec
// change, which is what lets the controller hold write access to status without
// also being able to rewrite the work it was given.
func TestStatusIsASubresource(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	run := sampleRun("ar-status")
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	run.Status.Phase = runv1.PhaseRunning
	run.Status.Attempt = 1
	run.Status.StartedAt = &metav1.Time{Time: time.Now()}
	run.Spec.Model = "anthropic/claude-haiku-4-5" // must be ignored, not rejected
	if err := c.Status().Update(ctx, run); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var got agentrunv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != runv1.PhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if got.Spec.Model != "anthropic/claude-opus-5" {
		t.Errorf("status write changed the spec: model = %q", got.Spec.Model)
	}
}

// TestPhaseRankIsMonotonic guards the ordering rule that both sides implement
// from this one function. Reports are ordered by (attempt, rank) because
// cluster clocks are not synchronised; if the ranks ever stop being ordered,
// a late Running silently resurrects a finished run.
func TestPhaseRankIsMonotonic(t *testing.T) {
	ordered := []runv1.Phase{runv1.PhasePending, runv1.PhaseStarting, runv1.PhaseRunning, runv1.PhaseSucceeded}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Rank() >= ordered[i].Rank() {
			t.Fatalf("rank(%s) >= rank(%s)", ordered[i-1], ordered[i])
		}
	}
	for _, p := range []runv1.Phase{runv1.PhaseSucceeded, runv1.PhaseFailed, runv1.PhaseTimedOut, runv1.PhaseCancelled} {
		if !p.IsTerminal() {
			t.Errorf("%s should be terminal", p)
		}
	}
	if runv1.Phase("SomethingNewer").Rank() != 0 {
		t.Error("an unknown phase must rank below every known one, not above")
	}
}

// TestFailureClassTable is the exit-code contract, asserted where both the
// controller and the backend can see it fail.
func TestFailureClassTable(t *testing.T) {
	cases := map[int32]runv1.FailureClass{
		0: runv1.FailureNone, 10: runv1.FailureAgent, 11: runv1.FailureAgent,
		12: runv1.FailureAgent, 20: runv1.FailureGit, 30: runv1.FailureConfig,
		137: runv1.FailureInfra, 143: runv1.FailureInfra, 1: runv1.FailureAgent,
	}
	for code, want := range cases {
		if got := runv1.FailureClassForExitCode(code); got != want {
			t.Errorf("exit %d: class %s, want %s", code, got, want)
		}
	}
	phases := map[int32]runv1.Phase{
		0: runv1.PhaseSucceeded, 11: runv1.PhaseTimedOut, 10: runv1.PhaseFailed,
		20: runv1.PhaseFailed, 137: runv1.PhaseFailed,
	}
	for code, want := range phases {
		if got := runv1.PhaseForExitCode(code); got != want {
			t.Errorf("exit %d: phase %s, want %s", code, got, want)
		}
	}
	if !runv1.FailureInfra.Retriable() || !runv1.FailureGit.Retriable() {
		t.Error("infra and git must be retriable")
	}
	for _, f := range []runv1.FailureClass{runv1.FailureAgent, runv1.FailureConfig, runv1.FailureBudget} {
		if f.Retriable() {
			t.Errorf("%s must not be retried automatically", f)
		}
	}
}

// TestNamesDoNotCollideWithinASecond is the reason object names carry the whole
// ULID. The first eight characters encode the timestamp to about a second, so
// truncating there makes two runs submitted together share a Secret.
func TestNamesDoNotCollideWithinASecond(t *testing.T) {
	a := runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8CD")
	b := runv1.ULID("01J8X4K2ZQ0000000000000001")
	if agentrunv1alpha1.ObjectName(a) == agentrunv1alpha1.ObjectName(b) {
		t.Fatal("two runs from the same millisecond produced the same object name")
	}
	if got := agentrunv1alpha1.JobName(a, 2); got != "ar-01j8x4k2zq7yb3m9f0r5w6t8cd-j2" {
		t.Fatalf("job name = %q", got)
	}
	if len(agentrunv1alpha1.JobName(a, 10)) > 63 {
		t.Fatal("job name exceeds 63 characters, pod names generated from it will be rejected")
	}
}
