package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// TestAPrunedSpecFieldIsRefusedRatherThanRun is the test this whole contract
// was designed around.
//
// The control plane and the chart are upgraded independently, so a backend that
// renders a field an older CRD has never heard of is a normal state, not a
// fault. What is not normal is what Kubernetes does about it: a structural
// schema does not ignore the unknown field, it deletes it, answers 201, and the
// run executes with a setting silently missing. Here the cluster's CRD has no
// mcpServers, the lease has three, and the correct outcome is that the run does
// not start at all — the control plane raises the epoch, excludes this cluster
// and, with nowhere else to put it, fails the run with a reason a person can
// read.
func TestAPrunedSpecFieldIsRefusedRatherThanRun(t *testing.T) {
	cfg, stop := startClusterWithoutMCPServers(t)
	defer stop()

	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	h := newHarness(t, withClient(k8s))

	spec := sampleSpec()
	spec.Runtime.MCPServers = []runv1.MCPServer{
		{Name: "jira", Transport: "http", URL: "https://mcp.example/jira"},
	}
	id := h.Backend.Enqueue(spec)

	h.poll()

	if !h.runGone(id) {
		t.Fatal("the run was materialised although the cluster's CRD dropped part of its spec")
	}
	if h.job(id, 1) != nil {
		t.Fatal("a Job was created for a spec this cluster had already mangled")
	}
	state, ok := h.Backend.RunState(id)
	if !ok {
		t.Fatal("the run vanished from the control plane")
	}
	if !strings.Contains(state.Message, string(clusterv1.RejectSpecFieldsPruned)) {
		t.Fatalf("the refusal did not say what was wrong: %q", state.Message)
	}
	if !strings.Contains(state.Message, "runtime.mcpServers") {
		t.Fatalf("the refusal did not name the lost field: %q", state.Message)
	}
	if len(state.Excluded) == 0 {
		t.Fatal("the cluster was not excluded from selection for this run")
	}
}

// TestADefaultedFieldIsNotMistakenForAPrunedOne is the other half, and the one
// a naive implementation gets wrong. The API server *adds* fields on write —
// ttlSecondsAfterFinished, baseBranch, createPR — so a comparison that demanded
// the stored spec equal the sent one would refuse every lease that omitted an
// optional value.
func TestADefaultedFieldIsNotMistakenForAPrunedOne(t *testing.T) {
	h := newHarness(t)

	spec := sampleSpec()
	spec.TTLSecondsAfterFinished = nil
	spec.Repo.CreatePR = nil
	spec.Repo.BaseBranch = ""
	id := h.Backend.Enqueue(spec)

	h.poll()

	cr := h.run(id)
	if cr.Spec.TTLSecondsAfterFinished == nil || *cr.Spec.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("the TTL default was not applied: %v", cr.Spec.TTLSecondsAfterFinished)
	}
	if cr.Spec.Repo.BaseBranch != "main" {
		t.Fatalf("the base branch default was not applied: %q", cr.Spec.Repo.BaseBranch)
	}
	if state, _ := h.Backend.RunState(id); state.Status != clusterv1.StatusDispatched {
		t.Fatalf("a defaulted field was refused as a pruned one: %s %q", state.Status, state.Message)
	}
}

// startClusterWithoutMCPServers brings up an API server whose AgentRun CRD is a
// version behind: everything else is as generated, and one field is missing.
func startClusterWithoutMCPServers(t *testing.T) (*rest.Config, func()) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "haliphron.io_agentruns.yaml"))
	if err != nil {
		t.Fatalf("read the CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse the CRD: %v", err)
	}

	removed := false
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := version.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			continue
		}
		runtimeProps, ok := spec.Properties["runtime"]
		if !ok {
			continue
		}
		if _, ok := runtimeProps.Properties["mcpServers"]; ok {
			delete(runtimeProps.Properties, "mcpServers")
			removed = true
		}
	}
	if !removed {
		t.Fatal("runtime.mcpServers is no longer in the CRD; this test needs a field to remove")
	}

	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&crd}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start an API server with the older CRD: %v", err)
	}
	return cfg, func() { _ = env.Stop() }
}
