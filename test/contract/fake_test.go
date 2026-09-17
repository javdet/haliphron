package contract

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"
	fakecontroller "github.com/automagicops/haliphron/fake/controller"
)

// FakeController builds AgentRuns that the backend's tests will assert on. If
// the real API server would reject one of them, those assertions are about an
// object that can never exist, and the backend team spends a sprint agreeing
// with a fiction.
//
// This is the only place in the repository where all three meet: a lease issued
// by FakeBackend, materialised by FakeController, applied to a real API server
// running the generated CRD.

func TestFakeControllerBuildsACRTheAPIServerAccepts(t *testing.T) {
	ctx := context.Background()
	b := fakebackend.New()
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)

	fc, err := fakecontroller.New(srv.URL, fakecontroller.WithClock(b.Now))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := fc.Register(ctx, b.BootstrapToken()); err != nil {
		t.Fatalf("register: %v", err)
	}

	const token = "ghs_token_that_must_stay_in_the_secret"
	id := b.Enqueue(fullSpec(), fakebackend.WithSecrets(map[string]string{
		runv1.SecretKeyGitToken: token,
	}), fakebackend.WithRoleConfig(map[string]string{
		"settings.json": `{"permissions":{"allow":["Bash(kubectl:*)"]}}`,
	}))
	if _, err := fc.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	built, ok := fc.AgentRun(id)
	if !ok {
		t.Fatal("the controller built no AgentRun")
	}

	// The fake's own store does not validate. This one does.
	c := newClient(t)
	built.ResourceVersion = ""
	built.UID = ""
	built.Namespace = testNamespace
	if err := c.Create(ctx, built); err != nil {
		t.Fatalf("the API server rejected the CR the controller built: %v", err)
	}

	var stored agentrunv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(built), &stored); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Nothing was pruned: the spec hash the controller recorded still describes
	// what the API server kept. A mismatch here means the generated CRD and the
	// Go types have drifted apart in a way only a live schema can show.
	if stored.Annotations[agentrunv1alpha1.AnnotationSpecHash] == "" {
		t.Fatal("no spec hash annotation: pruning would be undetectable")
	}
	if stored.Spec.Runtime.MCPServers == nil {
		t.Fatal("mcpServers was pruned by the current CRD")
	}
	if stored.Spec.Materials.SecretName != agentrunv1alpha1.SecretName(id) {
		t.Fatalf("materials.secretName = %q", stored.Spec.Materials.SecretName)
	}
	if stored.Spec.CallbackURL == "" {
		t.Fatal("callbackURL is empty: the pod would have nowhere to report")
	}

	// The invariant the structural schema is supposed to make unnecessary to
	// police — checked here against what the API server actually stored,
	// rather than against what the controller intended to send.
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("a secret value survived into the stored AgentRun")
	}
	if strings.Contains(string(raw), "permissions") {
		t.Fatal("the role config contents survived into the stored AgentRun")
	}
}

// fullSpec exercises the fields most likely to be pruned or to fail validation:
// the bounded map, the explicit toleration schema, and the quantity strings.
func fullSpec() runv1.RenderedRunSpec {
	return runv1.RenderedRunSpec{
		Agent:  runv1.AgentClaudeCode,
		Prompt: runv1.ObjectRef{Bucket: "haliphron", Key: "runs/x/prompt.txt", SHA256: strings.Repeat("a", 64)},
		Model:  "anthropic/claude-opus-5",
		Role:   "coder",
		Image:  "ghcr.io/automagicops/agent-runtime@sha256:" + strings.Repeat("b", 64),
		Repo: runv1.RepoSpec{
			URL: "https://github.com/acme/widgets.git", Provider: runv1.GitProviderGitHub,
			BaseBranch: "main", TargetBranch: "haliphron/01j8-add-rds",
		},
		Runtime: runv1.RuntimeSpec{
			TimeoutSeconds: 3600,
			Resources:      runv1.Resources{CPU: "2", Memory: "4Gi", EphemeralStorage: "20Gi"},
			NodeSelector:   map[string]runv1.LabelValue{"workload": "agents"},
			Tolerations:    []runv1.Toleration{{Key: "agents", Operator: "Exists", Effect: "NoSchedule"}},
			MCPServers:     []runv1.MCPServer{{Name: "helm", Transport: "http", URL: "http://mcp-helm:8080"}},
		},
		ToolPolicy: &runv1.ToolPolicy{Allow: []string{"Bash(kubectl:*)"}, Deny: []string{"Bash(rm:*)"}},
	}
}
