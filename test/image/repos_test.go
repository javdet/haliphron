package image_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// Several repositories on one run, against real git: the working repository in
// the workspace, the others beside it under /repos, and every one with changes
// committed, pushed and proposed for review.

const (
	appURL  = "https://github.com/acme/app.git"
	libURL  = "https://github.com/acme/lib.git"
	docsURL = "https://github.com/acme/docs.git"
	branch  = "haliphron/run-multi"
)

// multiRepo is a run over three upstreams, each a local repository the run's
// https URLs are redirected to.
type multiRepo struct {
	h         *harness
	upstreams map[string]string // URL → directory
	mu        sync.Mutex
	prCreates []entrypoint.Command
}

func newMultiRepo(t *testing.T, createPR bool) *multiRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("this test needs a real git on PATH: %v", err)
	}
	m := &multiRepo{upstreams: map[string]string{}}
	var gitconfig strings.Builder
	for _, u := range []string{appURL, libURL, docsURL} {
		dir := filepath.Join(t.TempDir(), strings.TrimSuffix(filepath.Base(u), ".git"))
		gitIn(t, "", "init", "--quiet", "--initial-branch=main", dir)
		writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "--quiet", "-m", "initial")
		writeFile(t, filepath.Join(dir, "README.md"), "hello again\n")
		gitIn(t, dir, "commit", "--quiet", "-am", "second")
		m.upstreams[u] = dir
		fmt.Fprintf(&gitconfig, "[url \"file://%s\"]\n\tinsteadOf = %s\n", dir, u)
	}

	m.h = newHarness(t, controlplane.RunRequest{
		Prompt: "change the API and its client", RepoURL: appURL,
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main", TargetBranch: branch,
		CreatePR: createPR,
		ExtraRepos: []runv1.ExtraRepoSpec{
			{URL: libURL, Provider: runv1.GitProviderGitHub, Path: "lib", CloneDepth: 1},
			{URL: docsURL, Provider: runv1.GitProviderGitHub, BaseBranch: "main", Path: "docs"},
		},
	})
	writeFile(t, filepath.Join(m.h.layout.Home, ".gitconfig"), gitconfig.String())
	return m
}

// script runs real git, a scripted gh that has no pull requests yet, and an
// agent that edits the working repository and lib and leaves docs alone.
func (m *multiRepo) script(edit func()) {
	agent := agentScript(m.h, "done", `{"ok":true}`)
	m.h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		switch {
		case c.Path == "git":
			return entrypoint.ExecCommander{}.Run(context.Background(), c)
		case c.Path == "gh" && len(c.Args) > 1 && c.Args[1] == "list":
			fmt.Fprintln(c.Stdout, "[]")
			return entrypoint.CommandResult{}, nil
		case c.Path == "gh" && len(c.Args) > 1 && c.Args[1] == "create":
			m.mu.Lock()
			m.prCreates = append(m.prCreates, c)
			m.mu.Unlock()
			fmt.Fprintf(c.Stdout, "https://github.com/acme/%s/pull/1\n", filepath.Base(c.Dir))
			return entrypoint.CommandResult{}, nil
		case c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p":
			edit()
		}
		return agent(c)
	}
}

func (m *multiRepo) editAppAndLib() {
	writeFile(m.h.t, filepath.Join(m.h.layout.Workspace, "README.md"), "app, changed\n")
	writeFile(m.h.t, filepath.Join(m.h.layout.Repos, "lib", "README.md"), "lib, changed\n")
}

func (m *multiRepo) hasBranch(url string) bool {
	m.h.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = m.upstreams[url]
	return cmd.Run() == nil
}

func extraResult(t *testing.T, report *runv1.CompletionReport, path string) runv1.ExtraRepoResult {
	t.Helper()
	for _, r := range report.ExtraRepos {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("the report has no result for %s: %+v", path, report.ExtraRepos)
	return runv1.ExtraRepoResult{}
}

func TestExtraRepositoriesAreClonedBesideTheWorkspaceOnTheRunsBranch(t *testing.T) {
	t.Parallel()
	m := newMultiRepo(t, false)
	m.script(func() {})

	run, code := m.h.executeRun()
	if got := m.h.phase(run, runv1.RuntimePhaseCloneExtra); got != runv1.PhaseOutcomeOK {
		t.Fatalf("clone-extra is %s, want ok (%v)", got, run.Failure())
	}
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	for _, path := range []string{"lib", "docs"} {
		dir := filepath.Join(m.h.layout.Repos, path)
		if got := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
			t.Errorf("%s is on %q, want the run's branch", path, got)
		}
		if strings.HasPrefix(dir, m.h.layout.Workspace) {
			t.Errorf("%s is inside the workspace", dir)
		}
	}
	if got := gitIn(t, filepath.Join(m.h.layout.Repos, "lib"), "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Errorf("lib, with depth 1, is not shallow: %q", got)
	}
	if got := gitIn(t, filepath.Join(m.h.layout.Repos, "docs"), "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Errorf("docs, with depth 0, is shallow: %q", got)
	}
	// Nothing changed, so nothing is pushed anywhere.
	for _, u := range []string{appURL, libURL, docsURL} {
		if m.hasBranch(u) {
			t.Errorf("%s got a branch from a run that changed nothing", u)
		}
	}
}

func TestEveryRepositoryWithChangesIsCommittedPushedAndProposed(t *testing.T) {
	t.Parallel()
	m := newMultiRepo(t, true)
	m.script(m.editAppAndLib)

	run, code := m.h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if !m.hasBranch(appURL) || !m.hasBranch(libURL) {
		t.Fatalf("pushed: app %v, lib %v; want both", m.hasBranch(appURL), m.hasBranch(libURL))
	}
	if got := gitIn(t, m.upstreams[libURL], "show", branch+":README.md"); got != "lib, changed" {
		t.Errorf("lib's branch holds %q, want the agent's change", got)
	}

	report := run.Report()
	if report.Repo == nil || !report.Repo.Pushed || report.Repo.PRURL != "https://github.com/acme/workspace/pull/1" {
		t.Errorf("the working repository's result: %+v", report.Repo)
	}
	lib := extraResult(t, report, "lib")
	if !lib.Pushed || lib.PRURL != "https://github.com/acme/lib/pull/1" || lib.PRAction != runv1.PRActionCreated ||
		lib.CommitSHA == "" || lib.TargetBranch != branch {
		t.Errorf("lib's result: %+v", lib)
	}

	// The extra repository's pull request first, so the working one can list it.
	if len(m.prCreates) != 2 {
		t.Fatalf("%d pull requests opened, want two", len(m.prCreates))
	}
	if filepath.Base(m.prCreates[0].Dir) != "lib" || m.prCreates[1].Dir != m.h.layout.Workspace {
		t.Errorf("pull requests opened in %s then %s; the extras go first",
			m.prCreates[0].Dir, m.prCreates[1].Dir)
	}
	body := func(c entrypoint.Command) string {
		i := slices.Index(c.Args, "--body")
		return c.Args[i+1]
	}
	if !strings.Contains(body(m.prCreates[0]), appURL) {
		t.Errorf("lib's pull request does not say which change it is part of: %s", body(m.prCreates[0]))
	}
	if !strings.Contains(body(m.prCreates[1]), "https://github.com/acme/lib/pull/1") {
		t.Errorf("the working repository's pull request does not list lib's: %s", body(m.prCreates[1]))
	}
}

func TestARepositoryWithoutChangesIsNotPushed(t *testing.T) {
	t.Parallel()
	m := newMultiRepo(t, true)
	m.script(m.editAppAndLib)

	run, code := m.h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if m.hasBranch(docsURL) {
		t.Error("docs, which the agent did not touch, was pushed")
	}
	docs := extraResult(t, run.Report(), "docs")
	if docs.Pushed || docs.PRURL != "" {
		t.Errorf("docs' result: %+v", docs)
	}
	for _, c := range m.prCreates {
		if filepath.Base(c.Dir) == "docs" {
			t.Error("a pull request was opened for a repository with nothing pushed")
		}
	}
}

// declinePushes makes an upstream refuse every push, the way a protected
// branch rule does.
func declinePushes(t *testing.T, dir string) {
	t.Helper()
	hook := filepath.Join(dir, ".git", "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused by policy >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestARefusedPushInOneRepositoryDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	m := newMultiRepo(t, true)
	declinePushes(t, m.upstreams[libURL])
	m.script(m.editAppAndLib)

	run, code := m.h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d: a declined push is permanent (%v)", code, runv1.ExitConfig, run.Failure())
	}
	if f := run.Failure(); f == nil || f.Phase != runv1.RuntimePhasePush || !strings.Contains(f.Message(), "lib") {
		t.Errorf("failure %+v, want the push phase naming lib", f)
	}
	if !m.hasBranch(appURL) {
		t.Error("the working repository was not pushed because lib refused")
	}
	report := run.Report()
	if report.Repo == nil || report.Repo.PRURL == "" {
		t.Errorf("the working repository's pull request was not opened: %+v", report.Repo)
	}
	if lib := extraResult(t, report, "lib"); lib.Pushed {
		t.Errorf("lib reported as pushed: %+v", lib)
	}
}

func TestOnlyPermanentRefusalsInEveryRepositoryAreAConfigFailure(t *testing.T) {
	t.Parallel()

	t.Run("every refusal permanent", func(t *testing.T) {
		m := newMultiRepo(t, false)
		declinePushes(t, m.upstreams[appURL])
		declinePushes(t, m.upstreams[libURL])
		m.script(m.editAppAndLib)
		run, code := m.h.executeRun()
		if code != runv1.ExitConfig {
			t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitConfig, run.Failure())
		}
		if f := run.Failure(); f == nil || !strings.Contains(f.Message(), "2 repositories failed") {
			t.Errorf("the failure does not say both failed: %v", f)
		}
	})

	t.Run("one refusal a retry could fix", func(t *testing.T) {
		m := newMultiRepo(t, false)
		declinePushes(t, m.upstreams[appURL])
		// lib's remote disappears while the agent works: a network failure,
		// as far as git can tell, and worth a retry.
		m.script(func() {
			m.editAppAndLib()
			if err := os.RemoveAll(m.upstreams[libURL]); err != nil {
				t.Error(err)
			}
		})
		run, code := m.h.executeRun()
		if code != runv1.ExitGit {
			t.Fatalf("exit %d, want %d: one of the failures is not permanent (%v)", code, runv1.ExitGit, run.Failure())
		}
	})
}

func TestTheCredentialHelperAnswersEveryHostOfTheRunAndNoOther(t *testing.T) {
	t.Parallel()
	tokens, _ := json.Marshal(runv1.GitHostTokens{
		"github.com":         {Provider: runv1.GitProviderGitHub, Token: "ghs_working_repository"},
		"gitlab.example.com": {Provider: runv1.GitProviderGitLab, Token: "glpat-extra-repository"},
		"evil.example":       {Provider: runv1.GitProviderGitHub, Token: "ghs_not_a_host_of_this_run"},
	})
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "x", RepoURL: appURL, GitProvider: runv1.GitProviderGitHub, TargetBranch: branch,
		ExtraRepos: []runv1.ExtraRepoSpec{{URL: "https://gitlab.example.com/ops/infra", Provider: runv1.GitProviderGitLab, Path: "infra"}},
		Secrets:    map[string]string{runv1.SecretKeyGitTokens: string(tokens)},
	})
	h.commander.handle = agentScript(h, "done", `{"ok":true}`)
	run, _ := h.executeRun()
	if got := h.phase(run, runv1.RuntimePhaseAuth); got != runv1.PhaseOutcomeOK {
		t.Fatalf("auth is %s (%v)", got, run.Failure())
	}

	ask := func(host string) string {
		cmd := exec.Command(filepath.Join(h.layout.RunPrivate, "git-credential-haliphron"), "get")
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the helper failed for %s: %v", host, err)
		}
		return string(out)
	}
	if got := ask("github.com"); !strings.Contains(got, "password=ghs_working_repository") {
		t.Errorf("github.com: %q", got)
	}
	if got := ask("gitlab.example.com"); !strings.Contains(got, "password=glpat-extra-repository") {
		t.Errorf("gitlab.example.com: %q", got)
	}
	for _, host := range []string{"evil.example", "example.com", "*"} {
		if got := ask(host); got != "" {
			t.Errorf("the helper answered %s, which no repository of the run is on: %q", host, got)
		}
	}

	// git in each repository gets its own host's token.
	gotToken := false
	for _, c := range h.commander.invocations("git") {
		if c.Dir == filepath.Join(h.layout.Repos, "infra") && slices.Contains(c.Env, "GL_TOKEN=glpat-extra-repository") {
			gotToken = true
		}
	}
	if !gotToken {
		t.Error("git in the gitlab repository did not get the gitlab token")
	}
	if log := h.mustObject(runv1.StorageKeyAgentLog); bytes.Contains(log, []byte("glpat-extra-repository")) {
		t.Error("a per-host token reached the uploaded log")
	}
}

func TestEveryRuntimeIsGivenTheExtraRepositoriesAsWritableDirectories(t *testing.T) {
	t.Parallel()
	for _, agent := range []runv1.AgentType{runv1.AgentClaudeCode, runv1.AgentCodex} {
		t.Run(string(agent), func(t *testing.T) {
			h := newHarness(t, controlplane.RunRequest{
				Prompt: "x", Agent: agent, RepoURL: appURL, GitProvider: runv1.GitProviderGitHub,
				TargetBranch: branch, PermissionMode: "acceptEdits",
				ExtraRepos: []runv1.ExtraRepoSpec{{URL: libURL, Provider: runv1.GitProviderGitHub, Path: "lib"}},
			})
			h.commander.handle = agentScript(h, "done", `{"ok":true}`)
			h.executeRun()

			lib := filepath.Join(h.layout.Repos, "lib")
			path := map[runv1.AgentType]string{runv1.AgentClaudeCode: "claude", runv1.AgentCodex: "codex"}[agent]
			for _, c := range h.commander.invocations(path) {
				i := slices.Index(c.Args, "--add-dir")
				if i < 0 {
					continue
				}
				if c.Args[i+1] != lib {
					t.Errorf("--add-dir %s, want %s", c.Args[i+1], lib)
				}
				prompt := strings.Join(c.Args, " ")
				if c.Stdin != nil {
					var b bytes.Buffer
					_, _ = b.ReadFrom(c.Stdin)
					prompt += b.String()
				}
				if !strings.Contains(prompt, lib+" — "+libURL) {
					t.Errorf("the agent is not told where lib is")
				}
				return
			}
			t.Fatalf("%s was launched without --add-dir for the extra repository", path)
		})
	}
}
