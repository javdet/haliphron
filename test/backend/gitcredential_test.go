package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// The git token is not refused at admission, so its absence is found at clone
// or push. Its endpoint says which of the names a lease tries are stored, so
// the UI can ask for one before a run is submitted, and never says what any of
// them holds.

func TestTheGitCredentialListsEveryNameALeaseTriesAndNoValue(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead)
	admin := h.Token(store.ScopeAdmin)

	var got struct {
		SecretName string `json:"secret_name"`
		Configured bool   `json:"configured"`
		Tokens     []struct {
			Provider   string `json:"provider"`
			SecretName string `json:"secret_name"`
			Configured bool   `json:"configured"`
			Kind       string `json:"kind"`
			Problem    string `json:"problem"`
		} `json:"tokens"`
	}
	if status := h.call(t, reader, http.MethodGet, "/api/v1/git-credential", nil, &got); status != http.StatusOK {
		t.Fatalf("get: status %d; a caller who submits runs is owed the reason they fail", status)
	}
	if got.SecretName != "git-token" || !got.Configured || len(got.Tokens) != 3 {
		t.Fatalf("the harness's stored token reads back as %+v", got)
	}
	for i, want := range []struct{ provider, name string }{
		{"", "git-token"}, {"github", "git-token-github"}, {"gitlab", "git-token-gitlab"},
	} {
		if tok := got.Tokens[i]; tok.Provider != want.provider || tok.SecretName != want.name {
			t.Errorf("token %d is %+v, want %s under %s", i, tok, want.provider, want.name)
		}
	}
	if !got.Tokens[0].Configured || got.Tokens[0].Kind != "managed" || got.Tokens[0].Problem != "" {
		t.Errorf("the fallback reads as %+v", got.Tokens[0])
	}
	if got.Tokens[1].Configured || got.Tokens[2].Configured {
		t.Errorf("a per-forge token nobody stored reads as stored: %+v", got.Tokens)
	}

	const glpat = "glpat-a-token-for-this-test"
	var raw json.RawMessage
	if status := h.call(t, admin, http.MethodPut, "/api/v1/git-credential",
		map[string]any{"provider": "gitlab", "value": glpat + "\n"}, &raw); status != http.StatusOK {
		t.Fatalf("put: status %d", status)
	}
	if contains(string(raw), glpat) {
		t.Fatal("the token's value came back from the API")
	}

	// Stored trimmed, under the name StoredGitToken tries first for GitLab.
	value, err := h.Store.ResolveSecret(context.Background(), "git-token-gitlab")
	if err != nil || string(value) != glpat {
		t.Errorf("stored %q (%v), want the token without its newline", value, err)
	}
	minted, err := app.StoredGitToken{Store: h.Store, Name: "git-token"}.
		MintToken(context.Background(), "https://gitlab.example.com/a/b", runv1.GitProviderGitLab, 0)
	if err != nil || minted != glpat {
		t.Errorf("a GitLab lease resolves %q (%v), want the token just stored", minted, err)
	}
}

func TestAGitTokenThatCannotBeUsedIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no value", map[string]any{}},
		{"a reference, which a lease fails on", map[string]any{"ref": "vault://kv#git"}},
		{"a pasted header", map[string]any{"value": "Authorization: token ghp_abc"}},
		{"an unknown forge", map[string]any{"provider": "bitbucket", "value": "abc"}},
		{"a GitHub token stored as the GitLab one", map[string]any{"provider": "gitlab", "value": "github_pat_abc"}},
		{"a GitLab token stored as the GitHub one", map[string]any{"provider": "github", "value": "glpat-abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status := h.call(t, admin, http.MethodPut, "/api/v1/git-credential", tc.body, nil); status != http.StatusUnprocessableEntity {
				t.Errorf("status %d, want 422", status)
			}
		})
	}

	value, err := h.Store.ResolveSecret(context.Background(), "git-token")
	if err != nil || string(value) != "ghs_test_repository_token" {
		t.Errorf("a refused request replaced the stored token: %q (%v)", value, err)
	}
}

func TestOnlyAnAdminSetsTheGitCredential(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)
	if status := h.call(t, token, http.MethodPut, "/api/v1/git-credential",
		map[string]any{"value": "ghp_abc"}, nil); status != http.StatusForbidden {
		t.Errorf("status %d, want 403", status)
	}
}

// A reference stored through the generic endpoint fails every lease that meets
// it, so it is reported as a problem rather than as a stored token.
func TestAReferencedGitTokenIsReportedAsAProblem(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Store.PutReferencedSecret(context.Background(), "git-token-github", "vault://kv#git", "test"); err != nil {
		t.Fatalf("store the reference: %v", err)
	}

	cred, err := h.App.GitCredential(context.Background())
	if err != nil {
		t.Fatalf("git credential: %v", err)
	}
	github := cred.Tokens[1]
	if !github.Configured || github.Kind != "referenced" || github.Problem == "" {
		t.Errorf("a referenced token reads as %+v", github)
	}
	if cred.Tokens[0].Problem != "" {
		t.Errorf("the healthy fallback reads as %+v", cred.Tokens[0])
	}
}

// Absent is a state the UI prompts on before anybody submits a run.
func TestAnAbsentGitCredentialIsReportedAsAbsent(t *testing.T) {
	h := newHarness(t)
	service := app.New(app.Options{Store: h.Store, Limits: app.Limits{GitTokenSecret: "never-stored"}})

	cred, err := service.GitCredential(context.Background())
	if err != nil {
		t.Fatalf("git credential: %v", err)
	}
	if cred.Configured() || cred.SecretName != "never-stored" || len(cred.Tokens) != 3 {
		t.Errorf("an unstored credential reads as %+v", cred)
	}
}
