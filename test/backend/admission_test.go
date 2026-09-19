package backend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The prompt travels through object storage rather than the lease, because a
// workflow step's prompt absorbs the output of previous steps and has no
// natural ceiling, while a Secret is capped at 1 MiB for all its keys together.
// The digest is what makes "the run executed what was admitted" provable after
// the fact: the pod refuses to run on a mismatch.
func TestAdmissionWritesThePromptAndFreezesItsDigest(t *testing.T) {
	h := newHarness(t)
	const prompt = "add a health endpoint and a test for it"
	admitted := h.Submit(func(r *run.SubmitRequest) { r.Prompt = prompt })

	stored, err := h.Artifacts.Get(context.Background(),
		artifacts.Key(admitted.ID, runv1.StorageKeyPrompt))
	if err != nil {
		t.Fatalf("read the prompt back: %v", err)
	}
	if string(stored) != prompt {
		t.Errorf("stored prompt = %q, want %q", stored, prompt)
	}

	digest := sha256.Sum256([]byte(prompt))
	if fmt.Sprintf("%x", admitted.PromptSHA256) != fmt.Sprintf("%x", digest) {
		t.Errorf("the run's digest does not match what was written")
	}

	var spec runv1.RenderedRunSpec
	if err := json.Unmarshal(admitted.Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec.Prompt.SHA256 != fmt.Sprintf("%x", digest) {
		t.Errorf("spec digest = %s, want %x", spec.Prompt.SHA256, digest)
	}
	if spec.Prompt.Key != artifacts.Key(admitted.ID, runv1.StorageKeyPrompt) {
		t.Errorf("spec points at %s, want the run's own prefix", spec.Prompt.Key)
	}
	if spec.Prompt.Bucket == "" {
		t.Error("the spec does not name a bucket, so the pod cannot address the prompt")
	}
	if spec.Prompt.SizeBytes != int64(len(prompt)) {
		t.Errorf("size = %d, want %d", spec.Prompt.SizeBytes, len(prompt))
	}
}

// The branch name is generated from the run identifier and never invented by
// the agent. A name the agent chooses is a different name on the second
// attempt, so the retry pushes a second branch and opens a second pull request
// instead of updating the first.
func TestTheTargetBranchIsDerivedFromTheRunIdentifier(t *testing.T) {
	h := newHarness(t)
	admitted := h.Submit()

	want := "haliphron/run-" + strings.ToLower(string(admitted.ID))
	if admitted.TargetBranch != want {
		t.Errorf("target branch = %q, want %q", admitted.TargetBranch, want)
	}

	// A retry is a replay of what was admitted: same branch, same prompt, same
	// prefix. That, plus create-or-update on the pull request, is what makes a
	// retry converge instead of forking.
	retried, err := h.App.Retry(context.Background(), admitted.ID, "operator")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.TargetBranch != want {
		t.Errorf("after a retry the branch is %q, want %q unchanged", retried.TargetBranch, want)
	}
	if retried.Epoch != admitted.Epoch+1 {
		t.Errorf("epoch = %d, want %d: a retry revokes ownership", retried.Epoch, admitted.Epoch+1)
	}
	if retried.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", retried.Attempt)
	}
	if retried.ObservedPhase != "" {
		t.Errorf("observed phase = %s, want it cleared so the new attempt can advance",
			retried.ObservedPhase)
	}
}

// Slack and n8n retry on a timeout, and a timeout is exactly when a run has
// already started. The same key must return the first run rather than start a
// second one.
func TestAnIdempotencyKeyReturnsTheFirstRun(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"prompt":"add a health endpoint"}`)
	opts := app.SubmitOptions{Key: "slack-1", Body: body}
	req := run.SubmitRequest{Prompt: "add a health endpoint", CreatedBy: "slack", CreatedVia: "slack"}

	first, err := h.App.Submit(context.Background(), req, opts)
	if err != nil {
		t.Fatalf("first submission: %v", err)
	}
	if err := h.App.CompleteSubmission(context.Background(), opts, first.Run.ID, 202,
		json.RawMessage(`{"run_id":"`+string(first.Run.ID)+`"}`)); err != nil {
		t.Fatalf("record the answer: %v", err)
	}

	second, err := h.App.Submit(context.Background(), req, opts)
	if err != nil {
		t.Fatalf("repeat submission: %v", err)
	}
	if !second.Replayed {
		t.Error("the repeat was not recognised as one")
	}
	if second.Run.ID != first.Run.ID {
		t.Errorf("repeat produced %s, first produced %s", second.Run.ID, first.Run.ID)
	}

	runs, err := h.Store.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("%d runs exist, want 1", len(runs))
	}

	// The same key with a different body is a client defect: answering it with
	// the first run would hand back the result of work nobody ordered.
	_, err = h.App.Submit(context.Background(), req,
		app.SubmitOptions{Key: "slack-1", Body: []byte(`{"prompt":"delete production"}`)})
	if err == nil {
		t.Fatal("the same key with a different body was accepted")
	}
}

// Admission refuses what the schema would refuse anyway, so that a caller gets
// a field name instead of a constraint violation.
func TestAdmissionRefusesRequestsWithAField(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name  string
		req   run.SubmitRequest
		field string
	}{
		{
			name:  "a run without a prompt has no task",
			req:   run.SubmitRequest{Prompt: "  "},
			field: "prompt",
		},
		{
			name:  "an unknown runtime",
			req:   run.SubmitRequest{Prompt: "x", Agent: runv1.AgentType("emacs")},
			field: "agent",
		},
		{
			name:  "a timeout outside the CRD's own bounds",
			req:   run.SubmitRequest{Prompt: "x", TimeoutSeconds: 10},
			field: "timeout_seconds",
		},
		{
			name:  "a negative cost is a broken or hostile caller",
			req:   run.SubmitRequest{Prompt: "x", MaxCostUSD: runv1.MoneyUSD("-5")},
			field: "max_cost_usd",
		},
		{
			name:  "a branch name git would not take",
			req:   run.SubmitRequest{Prompt: "x", RepoURL: "https://github.com/e/r", BaseBranch: "no spaces"},
			field: "base_branch",
		},
		{
			name:  "nesting beyond the depth limit costs money rather than correctness",
			req:   run.SubmitRequest{Prompt: "x", Depth: 9},
			field: "depth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.App.Submit(context.Background(), tc.req, app.SubmitOptions{})
			var invalid *run.InvalidRequestError
			if !asInvalid(err, &invalid) {
				t.Fatalf("error = %v, want an InvalidRequestError", err)
			}
			if invalid.Field != tc.field {
				t.Errorf("field = %q, want %q", invalid.Field, tc.field)
			}
		})
	}
}

// A role is a set of defaults for a run, and its files are material: the
// controller turns them into a ConfigMap and only the ConfigMap's name rides in
// the CR. The rule is mechanical — whatever the controller materialises never
// goes into the spec.
func TestARolesFilesTravelBesideTheSpecAndNotInsideIt(t *testing.T) {
	h := newHarness(t)
	roleSpec := `{
		"model": "anthropic/claude-sonnet-5",
		"permissionMode": "acceptEdits",
		"maxTurns": 40,
		"toolPolicy": {"allow": ["Bash", "Edit", "Read"], "deny": ["WebFetch"]},
		"configFiles": {".claude/settings.json": "{\"permissions\":{}}"}
	}`
	if _, err := h.Store.UpsertRole(context.Background(), "coder", json.RawMessage(roleSpec), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}

	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.Role = "coder" })
	lease := p.LeaseOne()

	if lease.Spec.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("model = %s, want the role's", lease.Spec.Model)
	}
	if lease.Spec.Runtime.PermissionMode != "acceptEdits" || lease.Spec.Runtime.MaxTurns != 40 {
		t.Errorf("the role's runtime settings did not reach the spec: %+v", lease.Spec.Runtime)
	}
	if lease.Spec.ToolPolicy == nil || len(lease.Spec.ToolPolicy.Allow) != 3 {
		t.Errorf("tool policy = %+v, want the role's allow list", lease.Spec.ToolPolicy)
	}
	if lease.RoleConfig[".claude/settings.json"] == "" {
		t.Error("the role's files did not travel with the lease")
	}

	// The config file's contents must not be anywhere in the spec: `kubectl
	// get agentrun -o yaml` is not a way to read what the controller mounted.
	encoded, err := json.Marshal(lease.Spec)
	if err != nil {
		t.Fatalf("encode spec: %v", err)
	}
	if strings.Contains(string(encoded), "permissions") {
		t.Error("a role config file's contents reached the spec")
	}
}

// A run with no repository is legal — an analysis, a question, a report — and
// it gets no git token, because there is nothing for one to open.
func TestARunWithoutARepositoryCarriesNoGitToken(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.RepoURL = "" })

	lease := p.LeaseOne()
	if lease.Spec.Repo.Provider != runv1.GitProviderNone {
		t.Errorf("provider = %s, want none", lease.Spec.Repo.Provider)
	}
	if lease.Spec.Repo.TargetBranch != "" {
		t.Errorf("target branch = %q, want none for a run with no repository", lease.Spec.Repo.TargetBranch)
	}
	if token := lease.Secrets[runv1.SecretKeyGitToken]; token != "" {
		t.Errorf("a run with no repository was given a git token")
	}
}

// A run admitted while no cluster can take it waits rather than failing: a
// request that arrives during a controller rollout is a request that should
// have waited a few seconds.
func TestARunAdmittedWithNoClusterWaitsForOne(t *testing.T) {
	h := newHarness(t)
	admitted := h.Submit()

	if admitted.ClusterID != "" {
		t.Fatalf("placed on %s, but no cluster is registered", admitted.ClusterID)
	}
	if admitted.Status != "Queued" {
		t.Errorf("status = %s, want Queued", admitted.Status)
	}

	p := newProbe(t, h, "east")
	if placed := h.Sweep().Placed; placed != 1 {
		t.Fatalf("the sweep placed %d runs, want 1", placed)
	}
	lease := p.LeaseOne()
	if lease.RunID != admitted.ID {
		t.Errorf("leased %s, want %s", lease.RunID, admitted.ID)
	}
}

func asInvalid(err error, target **run.InvalidRequestError) bool {
	for err != nil {
		if v, ok := err.(*run.InvalidRequestError); ok {
			*target = v
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
