package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Several repositories on one run: the limit set in the UI, the gate on the
// lease, the token for every host, and what the run view says afterwards.

type runLimitsBody struct {
	MaxReposPerRun        int    `json:"max_repos_per_run"`
	DefaultMaxReposPerRun int    `json:"default_max_repos_per_run"`
	Ceiling               int    `json:"max_repos_per_run_ceiling"`
	Source                string `json:"source"`
	UpdatedBy             string `json:"updated_by"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}

func reposBody(urls ...string) map[string]any {
	repos := make([]map[string]any, len(urls))
	for i, u := range urls {
		repos[i] = map[string]any{"url": u}
	}
	return map[string]any{"prompt": "change the API and its client", "repos": repos}
}

// leaseDeclaring polls the way a controller of this release does: with the
// features it carries out.
func (p *probe) leaseDeclaring(features []clusterv1.Feature) []clusterv1.Lease {
	p.t.Helper()
	var out clusterv1.LeaseResponse
	_, problem := p.post("/clusters/"+string(p.clusterID)+"/leases", p.token(),
		clusterv1.LeaseRequest{FreeSlots: 5, WaitSeconds: 1, Features: features}, &out)
	if problem != nil {
		p.t.Fatalf("lease: %+v", problem)
	}
	return out.Leases
}

func withRepos(repos ...string) func(*run.SubmitRequest) {
	return func(req *run.SubmitRequest) {
		req.RepoURL = ""
		list := make([]run.RepoRequest, len(repos))
		for i, u := range repos {
			list[i] = run.RepoRequest{URL: u}
		}
		if err := req.SetRepos(list); err != nil {
			panic(err)
		}
	}
}

func TestALimitSavedInTheUIAppliesToTheNextAdmission(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin, store.ScopeRunsWrite, store.ScopeRunsRead)

	var limits runLimitsBody
	if status := h.call(t, admin, http.MethodGet, "/api/v1/run-limits", nil, &limits); status != http.StatusOK {
		t.Fatalf("GET /run-limits = %d", status)
	}
	if limits.MaxReposPerRun != run.DefaultMaxRepos || limits.Source != "default" ||
		limits.DefaultMaxReposPerRun != run.DefaultMaxRepos || limits.Ceiling != run.MaxReposCeiling {
		t.Fatalf("before any save: %+v, want the default of %d", limits, run.DefaultMaxRepos)
	}

	three := reposBody("https://github.com/acme/app", "https://github.com/acme/lib", "https://github.com/acme/infra")
	if status := h.call(t, admin, http.MethodPost, "/api/v1/runs", three, nil); status != http.StatusAccepted {
		t.Fatalf("three repositories under the default limit = %d, want 202", status)
	}

	if status := h.call(t, admin, http.MethodPut, "/api/v1/run-limits",
		map[string]any{"max_repos_per_run": 2}, &limits); status != http.StatusOK {
		t.Fatalf("PUT /run-limits = %d", status)
	}
	if limits.MaxReposPerRun != 2 || limits.Source != "database" || limits.UpdatedBy == "" {
		t.Errorf("after the save: %+v", limits)
	}

	var refused errorResponse
	if status := h.call(t, admin, http.MethodPost, "/api/v1/runs", three, &refused); status != http.StatusUnprocessableEntity {
		t.Fatalf("three repositories over a limit of two = %d, want 422", status)
	}
	if refused.Error.Field != "repos" || !strings.Contains(refused.Error.Message, "limit of 2") {
		t.Errorf("the refusal does not name the limit: %+v", refused.Error)
	}

	two := reposBody("https://github.com/acme/app", "https://github.com/acme/lib")
	if status := h.call(t, admin, http.MethodPost, "/api/v1/runs", two, nil); status != http.StatusAccepted {
		t.Fatalf("two repositories under a limit of two = %d, want 202", status)
	}
}

func TestPuttingRunLimitsNeedsTheAdminScope(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)

	if status := h.call(t, reader, http.MethodGet, "/api/v1/run-limits", nil, nil); status != http.StatusOK {
		t.Errorf("GET /run-limits with runs:read = %d, want 200: the run forms need the limit", status)
	}
	if status := h.call(t, reader, http.MethodPut, "/api/v1/run-limits",
		map[string]any{"max_repos_per_run": 32}, nil); status != http.StatusForbidden {
		t.Errorf("PUT /run-limits without admin = %d, want 403", status)
	}
	if _, err := h.Store.RunLimits(context.Background()); err == nil {
		t.Error("a refused PUT saved a limit")
	}
}

func TestARunLimitOutsideOneToThirtyTwoIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	for _, n := range []int{0, -1, 33} {
		var refused errorResponse
		if status := h.call(t, admin, http.MethodPut, "/api/v1/run-limits",
			map[string]any{"max_repos_per_run": n}, &refused); status != http.StatusUnprocessableEntity {
			t.Errorf("PUT max_repos_per_run=%d = %d, want 422", n, status)
		}
	}
	if status := h.call(t, admin, http.MethodPut, "/api/v1/run-limits", map[string]any{}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("PUT without max_repos_per_run = %d, want 422", status)
	}

	// The table's CHECK is the last word, past every layer above it.
	if _, err := h.Store.PutRunLimits(context.Background(), store.RunLimits{MaxReposPerRun: 33}, "test"); err == nil {
		t.Error("the store saved 33; the CRD would refuse every run it admits")
	}
}

func TestARunWithExtraRepositoriesIsNotLeasedToAControllerThatDoesNotDeclareTheFeature(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit(withRepos("https://github.com/acme/app", "https://github.com/acme/lib"))

	if leases := p.leaseDeclaring(nil); len(leases) != 0 {
		t.Fatalf("a controller that does not carry out extraRepos was leased %d run(s); it would push one "+
			"repository of a change that spans two", len(leases))
	}
	if got := h.Run(admitted.ID).Status; got != clusterv1.StatusQueued {
		t.Errorf("the run is %s, want it waiting in Queued", got)
	}

	leases := p.leaseDeclaring(clusterv1.ControllerFeatures)
	if len(leases) != 1 || leases[0].RunID != admitted.ID {
		t.Fatalf("a controller declaring the feature got %d lease(s)", len(leases))
	}
	if extras := leases[0].Spec.ExtraRepos; len(extras) != 1 || extras[0].Path != "lib" || extras[0].CloneDepth != 1 {
		t.Errorf("the lease carries extra repositories %+v", extras)
	}
}

func TestARunWithOneRepositoryIsLeasedWhateverTheControllerDeclares(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	leases := p.leaseDeclaring(nil)
	if len(leases) != 1 || leases[0].RunID != admitted.ID {
		t.Fatalf("an ordinary run was held back from an older controller: %d lease(s)", len(leases))
	}
}

func TestATokenIsMintedForEveryHostTheRunClones(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Store.PutManagedSecret(context.Background(), "git-token-gitlab",
		[]byte("glpat-test-gitlab-token"), "", "test"); err != nil {
		t.Fatalf("store the gitlab token: %v", err)
	}
	p := newProbe(t, h, "east")
	h.Submit(withRepos(
		"https://github.com/acme/app",
		"https://gitlab.example.com/ops/infra",
		"https://github.com/acme/lib",
	))

	leases := p.leaseDeclaring(clusterv1.ControllerFeatures)
	if len(leases) != 1 {
		t.Fatalf("got %d leases", len(leases))
	}
	secrets := leases[0].Secrets
	if secrets[runv1.SecretKeyGitToken] != "ghs_test_repository_token" {
		t.Errorf("git-token is %q, want the working repository's", secrets[runv1.SecretKeyGitToken])
	}
	var tokens runv1.GitHostTokens
	if err := json.Unmarshal([]byte(secrets[runv1.SecretKeyGitTokens]), &tokens); err != nil {
		t.Fatalf("%s does not parse: %v", runv1.SecretKeyGitTokens, err)
	}
	if len(tokens) != 2 {
		t.Errorf("tokens for %d hosts, want one per host: %v", len(tokens), hostsOf(tokens))
	}
	if got := tokens["github.com"]; got.Token != "ghs_test_repository_token" || got.Provider != runv1.GitProviderGitHub {
		t.Errorf("github.com: %+v", got)
	}
	if got := tokens["gitlab.example.com"]; got.Token != "glpat-test-gitlab-token" || got.Provider != runv1.GitProviderGitLab {
		t.Errorf("gitlab.example.com: %+v", got)
	}
}

func hostsOf(m runv1.GitHostTokens) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestTheRunViewListsEveryRepositoryWithItsPullRequest(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead)
	p := newProbe(t, h, "east")
	admitted := h.Submit(withRepos("https://github.com/acme/app", "https://github.com/acme/lib",
		"https://github.com/acme/docs"))

	lease := p.leaseDeclaring(clusterv1.ControllerFeatures)[0]
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	report := completionFor(lease.RunID, 1, "0.500000", "https://github.com/acme/app/pull/3")
	report.ExtraRepos = []runv1.ExtraRepoResult{
		{Path: "lib", RepoResult: runv1.RepoResult{Pushed: true, CommitSHA: "abc123",
			PRURL: "https://github.com/acme/lib/pull/9", PRNumber: 9, PRAction: runv1.PRActionCreated}},
		{Path: "docs", RepoResult: runv1.RepoResult{TargetBranch: lease.Spec.Repo.TargetBranch}},
	}
	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1, report); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}

	var view struct {
		PRURL string `json:"pr_url"`
		Repos []struct {
			URL      string `json:"url"`
			Path     string `json:"path"`
			Pushed   bool   `json:"pushed"`
			PRURL    string `json:"pr_url"`
			PRNumber *int32 `json:"pr_number"`
		} `json:"repos"`
	}
	if status := h.call(t, reader, http.MethodGet, "/api/v1/runs/"+string(admitted.ID), nil, &view); status != http.StatusOK {
		t.Fatalf("GET run = %d", status)
	}
	if view.PRURL != "https://github.com/acme/app/pull/3" {
		t.Errorf("pr_url %q, want the working repository's, as it always was", view.PRURL)
	}
	if len(view.Repos) != 3 {
		t.Fatalf("repos has %d entries, want the working one and two extras: %+v", len(view.Repos), view.Repos)
	}
	if view.Repos[0].Path != runv1.MountWorkspace || view.Repos[0].PRURL != view.PRURL {
		t.Errorf("first entry %+v, want the working repository", view.Repos[0])
	}
	lib := view.Repos[1]
	if lib.Path != "/repos/lib" || !lib.Pushed || lib.PRURL != "https://github.com/acme/lib/pull/9" ||
		lib.PRNumber == nil || *lib.PRNumber != 9 {
		t.Errorf("lib: %+v", lib)
	}
	if docs := view.Repos[2]; docs.Path != "/repos/docs" || docs.Pushed || docs.PRURL != "" {
		t.Errorf("docs, which had no changes: %+v", docs)
	}
}
