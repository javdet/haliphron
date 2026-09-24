package image_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// The plugins phase: what gets installed, from where, and with whose
// credential.
//
// Every row here is a decision that is invisible from the outside once it is
// wrong. A marketplace cloned into the workspace shows up as a pull request
// full of somebody else's repository; a marketplace cloned without the run's
// credential fails only for private catalogues, which is the case the feature
// exists for; and a repository that can choose its own plugins is a repository
// that can choose the code running beside the agent.

// pluginRun is a run with a role, a repository and a scripted agent.
func pluginRun(t *testing.T, req controlplane.RunRequest) *harness {
	t.Helper()
	if req.Prompt == "" {
		req.Prompt = "do the thing"
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if _, set := req.Env[runv1.EnvRole]; !set {
		req.Env[runv1.EnvRole] = "coder"
	}
	h := newHarness(t, req)
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		return agentScript(h, "done", `{"summary":"ok"}`)(c)
	})
	return h
}

// pluginCommands returns the plugin subcommands the run issued, as strings.
func pluginCommands(h *harness) []string {
	var out []string
	for _, path := range []string{"claude", "codex"} {
		for _, c := range h.commander.invocations(path) {
			if len(c.Args) > 0 && c.Args[0] == "plugin" {
				out = append(out, path+" "+strings.Join(c.Args, " "))
			}
		}
	}
	return out
}

// gitClones returns the destination of every clone the run performed.
func gitClones(h *harness) []string {
	var out []string
	for _, c := range h.commander.invocations("git") {
		if gitSubcommand(c.Args) == "clone" {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

func TestAPluginsPhaseIsSkippedWhenNothingDeclaresAny(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{})

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	if got := pluginCommands(h); len(got) != 0 {
		t.Errorf("a run declaring no plugins still ran %v", got)
	}
	if outcome := phaseOutcome(run, runv1.RuntimePhasePlugins); outcome != runv1.PhaseOutcomeSkipped {
		t.Errorf("the plugins phase ended %q, want skipped", outcome)
	}
}

func TestTheRolesPluginsAreInstalledFromAMarketplaceTheEntrypointClonedItself(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"playneta-infra-coder@playneta"},
		}),
	})

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	// The clone is the entrypoint's, not the CLI's. That is what carries the
	// run's credential helper, and therefore what makes a private marketplace
	// work at all.
	clones := gitClones(h)
	var marketplace string
	for _, c := range clones {
		if strings.Contains(c, "playneta/claude-plugin") {
			marketplace = c
		}
	}
	if marketplace == "" {
		t.Fatalf("the marketplace was never cloned by the entrypoint; clones were %v", clones)
	}
	if !strings.Contains(marketplace, "https://github.com/playneta/claude-plugin") {
		t.Errorf("owner/repo was not expanded for git, which reads it as a relative path: %s", marketplace)
	}

	cmds := pluginCommands(h)
	if !containsAll(cmds, "plugin marketplace add", "plugin install playneta-infra-coder@playneta") {
		t.Errorf("the marketplace was not registered and the plugin not installed: %v", cmds)
	}
	// Registered by path. Naming the repository to the CLI would have it clone
	// again, without the credential.
	for _, c := range cmds {
		if strings.Contains(c, "marketplace add") && strings.Contains(c, "playneta/claude-plugin") {
			t.Errorf("the CLI was given the repository rather than the local checkout: %s", c)
		}
	}
}

func TestTheMarketplaceCheckoutStaysOutOfTheWorkspace(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc", CreatePR: true,
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
		}),
	})

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	// A marketplace inside the work tree is a marketplace in the diff, the
	// commit and the pull request.
	checked := 0
	for _, c := range gitClones(h) {
		if !strings.Contains(c, "playneta") {
			continue
		}
		checked++
		dest := lastArg(c)
		if strings.HasPrefix(dest, h.layout.Workspace+string(filepath.Separator)) || dest == h.layout.Workspace {
			t.Errorf("the marketplace was cloned into the workspace at %s", dest)
		}
		if !strings.HasPrefix(dest, h.layout.RunPrivate) {
			t.Errorf("the marketplace landed at %s, outside the entrypoint's private scratch %s",
				dest, h.layout.RunPrivate)
		}
	}
	if checked == 0 {
		t.Fatalf("no marketplace clone was found, so this proved nothing; clones were %v", gitClones(h))
	}
}

func TestTheRepositorysRoleSettingsWinOverTheRolesOwnList(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "fallback", URL: "org/fallback"}},
			Enabled:      []string{"from-the-role@fallback"},
		}),
	})
	writeRepoSettings(t, h, "settings.coder.json", `{
	  "extraKnownMarketplaces": {"playneta": {"source": {"source": "github", "repo": "playneta/claude-plugin"}}},
	  "enabledPlugins": {"from-the-repo@playneta": true}
	}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	cmds := strings.Join(pluginCommands(h), " | ")
	if !strings.Contains(cmds, "from-the-repo@playneta") {
		t.Errorf("the repository's role settings were not used: %s", cmds)
	}
	// Not merged. A repository that could add to the role's list is a
	// repository choosing code the operator did not.
	if strings.Contains(cmds, "from-the-role@fallback") {
		t.Errorf("the two sources were merged rather than resolved: %s", cmds)
	}
}

func TestTheRolesListIsUsedWhenTheRepositoryHasNoRoleSettings(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"from-the-role@playneta"},
		}),
	})
	// The repository's *default* settings rank below the role's own list.
	writeRepoSettings(t, h, "settings.json", `{
	  "extraKnownMarketplaces": {"other": {"source": {"source": "github", "repo": "org/other"}}},
	  "enabledPlugins": {"from-the-default@other": true}
	}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	cmds := strings.Join(pluginCommands(h), " | ")
	if !strings.Contains(cmds, "from-the-role@playneta") {
		t.Errorf("the role's own list was not used: %s", cmds)
	}
	if strings.Contains(cmds, "from-the-default@other") {
		t.Errorf("the repository's default settings outranked the role: %s", cmds)
	}
}

func TestTheRepositorysDefaultSettingsAreTheLastResort(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
	})
	writeRepoSettings(t, h, "settings.json", `{
	  "extraKnownMarketplaces": {"playneta": {"source": {"source": "github", "repo": "playneta/claude-plugin"}}},
	  "enabledPlugins": {"from-the-default@playneta": true}
	}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	if cmds := strings.Join(pluginCommands(h), " | "); !strings.Contains(cmds, "from-the-default@playneta") {
		t.Errorf("a run with no role list ignored the repository's settings: %s", cmds)
	}
}

func TestRepositorySourcesAreIgnoredWhenTheRoleDoesNotTrustThem(t *testing.T) {
	t.Parallel()
	no := false
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		RoleConfig: pluginConfig(t, runv1.PluginSpec{TrustRepositorySources: &no}),
	})
	writeRepoSettings(t, h, "settings.coder.json", `{
	  "extraKnownMarketplaces": {"evil": {"source": {"source": "github", "repo": "attacker/plugins"}}},
	  "enabledPlugins": {"backdoor@evil": true}
	}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	// This is the switch that decides whether a repository — untrusted input,
	// by this system's own reckoning — may choose code that runs beside the
	// agent with its privileges.
	if cmds := pluginCommands(h); len(cmds) != 0 {
		t.Errorf("a role that refuses repository sources still installed %v", cmds)
	}
	for _, c := range gitClones(h) {
		if strings.Contains(c, "attacker/plugins") {
			t.Errorf("the repository's marketplace was cloned anyway: %s", c)
		}
	}
}

func TestAnUnreachableMarketplaceIsRetryableAndARefusedOneIsNot(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		output string
		want   int32
	}{
		"a forge that is briefly down": {
			output: "fatal: unable to access 'https://github.com/playneta/claude-plugin/': 503\n",
			want:   runv1.ExitGit,
		},
		"a private repository this token cannot see": {
			output: "remote: Repository not found.\nfatal: repository not found\n",
			want:   runv1.ExitConfig,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := pluginRun(t, controlplane.RunRequest{
				RoleConfig: pluginConfig(t, runv1.PluginSpec{
					Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
				}),
			})
			h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				return agentScript(h, "done", `{"summary":"ok"}`)(c)
			})
			inner := h.commander.handle
			h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				if c.Path == "git" && gitSubcommand(c.Args) == "clone" &&
					strings.Contains(strings.Join(c.Args, " "), "playneta") {
					fmt.Fprint(c.Stdout, tc.output)
					return entrypoint.CommandResult{ExitCode: 128}, nil
				}
				return inner(c)
			}

			run, code := h.executeRun()
			if code != tc.want {
				t.Fatalf("exit %d, want %d (%v)", code, tc.want, run.Failure())
			}
			// The model must not have been paid for: the phase is before run.
			if h.commander.called("claude") && agentWasLaunched(h) {
				t.Error("the agent was launched after the plugins phase failed")
			}
		})
	}
}

func TestAnOfficialMarketplaceIsRegisteredBySourceWhenItsNameIsReserved(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "official", URL: "anthropics/claude-code"}},
		}),
	})

	// claude-code reserves the names of Anthropic's own marketplaces and will
	// not let a local directory claim one, however the directory got there.
	var offered []string
	inner := h.commander.handle
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 2 && c.Args[1] == "marketplace" && c.Args[2] == "add" {
			source := c.Args[3]
			offered = append(offered, source)
			if !strings.Contains(source, "anthropics/claude-code") {
				c.Stdout.Write([]byte("Failed to add marketplace: The name 'claude-code-plugins' " +
					"is reserved for official Anthropic marketplaces and can only be used with " +
					"GitHub sources from the 'anthropics' organization.\n"))
				return entrypoint.CommandResult{ExitCode: 1}, nil
			}
		}
		return inner(c)
	}

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0: a reserved name is not a reason to fail the run (%v)", code, run.Failure())
	}
	if len(offered) != 2 {
		t.Fatalf("the CLI was offered %v, want the local checkout and then the source", offered)
	}
	// The local checkout is always tried first: it is the only form that works
	// for a private catalogue, which is the case this feature exists for.
	if strings.Contains(offered[0], "anthropics/claude-code") {
		t.Errorf("the source was offered first; a private marketplace would be fetched without the credential: %v", offered)
	}
	if offered[1] != "anthropics/claude-code" {
		t.Errorf("the fallback offered %q, want the original source", offered[1])
	}
}

func TestAMarketplaceRefusedForAnyOtherReasonFailsTheRun(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
		}),
	})
	var attempts int
	inner := h.commander.handle
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 2 && c.Args[1] == "marketplace" && c.Args[2] == "add" {
			attempts++
			c.Stdout.Write([]byte("Failed to add marketplace: no marketplace.json found\n"))
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		return inner(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitConfig, run.Failure())
	}
	// Retrying by source would ask the CLI to fetch a private catalogue with a
	// credential it does not have, and report that instead of the real problem.
	if attempts != 1 {
		t.Errorf("the registration was attempted %d times, want 1", attempts)
	}
}

func TestAFailedInstallStopsTheRunBeforeTheModelIsPaidFor(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"missing@playneta"},
		}),
	})
	inner := h.commander.handle
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 1 && c.Args[0] == "plugin" && c.Args[1] == "install" {
			fmt.Fprintln(c.Stdout, "Plugin not found in marketplace: missing")
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		return inner(c)
	}

	run, code := h.executeRun()
	// A name that is not in the catalogue is not going to appear on a retry.
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitConfig, run.Failure())
	}
	if agentWasLaunched(h) {
		t.Error("the agent ran although its role's plugin was missing; " +
			"it would have done the work without the tools it was promised")
	}
}

func TestAResumedAttemptDoesNotFetchAMarketplaceTwice(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
		}),
	})

	// Stand in for the checkout a previous attempt left behind: the directory
	// is in the pod's own scratch, which survives a local retry.
	dir := filepath.Join(h.layout.Marketplaces, "playneta", ".git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("staging the previous attempt's checkout: %v", err)
	}

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	for _, c := range gitClones(h) {
		if strings.Contains(c, "playneta") {
			t.Errorf("the marketplace was fetched again although it was already on disk: %s", c)
		}
	}
}

func TestCodexInstallsThroughItsOwnCommands(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		Env: map[string]string{runv1.EnvAgent: string(runv1.AgentCodex), runv1.EnvRole: "coder"},
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"playneta-infra-coder@playneta"},
		}),
	})

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	cmds := strings.Join(pluginCommands(h), " | ")
	// The same role must reach the same plugins on both runtimes, through each
	// one's own spelling.
	if !strings.Contains(cmds, "codex plugin marketplace add") {
		t.Errorf("codex did not register the marketplace: %s", cmds)
	}
	if !strings.Contains(cmds, "codex plugin add playneta-infra-coder@playneta") {
		t.Errorf("codex did not install the plugin: %s", cmds)
	}
	if strings.Contains(cmds, "claude plugin") {
		t.Errorf("a codex run issued claude-code's commands: %s", cmds)
	}
}

// ---------------------------------------------------------------------------

func pluginConfig(t *testing.T, spec runv1.PluginSpec) map[string]string {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal the plugin spec: %v", err)
	}
	return map[string]string{runv1.RoleConfigKeyPlugins: string(body)}
}

// writeRepoSettings puts a settings file where the clone would have left one.
func writeRepoSettings(t *testing.T, h *harness, name, body string) {
	t.Helper()
	dir := filepath.Join(h.layout.Workspace, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("preparing %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func phaseOutcome(run *entrypoint.Run, phase runv1.RuntimePhase) runv1.PhaseOutcome {
	for _, timing := range run.Timings() {
		if timing.Phase == phase {
			return timing.Outcome
		}
	}
	return ""
}

// agentWasLaunched distinguishes the model run from the plugin and mcp
// subcommands, which use the same binary.
func agentWasLaunched(h *harness) bool {
	for _, path := range []string{"claude", "codex"} {
		for _, c := range h.commander.invocations(path) {
			if len(c.Args) == 0 {
				continue
			}
			switch c.Args[0] {
			case "plugin", "mcp", "--version":
				continue
			}
			return true
		}
	}
	return false
}

func containsAll(haystack []string, needles ...string) bool {
	joined := strings.Join(haystack, " | ")
	for _, n := range needles {
		if !strings.Contains(joined, n) {
			return false
		}
	}
	return true
}

func lastArg(command string) string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func TestTheGitCredentialIsOfferedOnlyToTheRunsOwnForge(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://github.com/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		Secrets: map[string]string{runv1.SecretKeyGitToken: "ghs_the_runs_token"},
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "evil", URL: "https://attacker.example/plugins.git"}},
		}),
	})

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	// Every clone this pod makes goes through the one helper, and the plugins
	// phase clones a URL that may have come out of the repository. A helper that
	// answered whoever asked would hand an hour-long git token to any host a
	// repository cared to name — answer a 401 and read it out of the Basic
	// header.
	helper, err := os.ReadFile(filepath.Join(h.layout.RunPrivate, "git-credential-haliphron"))
	if err != nil {
		t.Fatalf("reading the credential helper: %v", err)
	}
	script := string(helper)
	if !strings.Contains(script, "github.com") {
		t.Fatalf("the helper is not bound to the run's forge:\n%s", script)
	}
	if !strings.Contains(script, "host") {
		t.Errorf("the helper does not read the host git asks about:\n%s", script)
	}
	if strings.Contains(script, "attacker.example") {
		t.Errorf("the helper mentions a repository-supplied host:\n%s", script)
	}
}

func TestACredentialHelperAnswersOnlyItsOwnHost(t *testing.T) {
	t.Parallel()

	// The script is shell, so the only honest check is to run it. Skipped where
	// there is no /bin/sh, which is nowhere this image runs.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell")
	}

	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://github.com/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		Secrets: map[string]string{runv1.SecretKeyGitToken: "ghs_the_runs_token"},
	})
	if _, code := h.executeRun(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	helper := filepath.Join(h.layout.RunPrivate, "git-credential-haliphron")

	ask := func(host string) string {
		cmd := exec.Command(sh, helper, "get")
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("running the helper for %s: %v", host, err)
		}
		return string(out)
	}

	if answer := ask("github.com"); !strings.Contains(answer, "ghs_the_runs_token") {
		t.Errorf("the helper did not answer for the run's own forge: %q", answer)
	}
	for _, host := range []string{"attacker.example", "github.com.attacker.example", "gitlab.com"} {
		if answer := ask(host); strings.Contains(answer, "ghs_the_runs_token") {
			t.Errorf("the helper gave the run's token to %s: %q", host, answer)
		}
	}
}

func TestARepositoryCannotSpendTheLeaseOnMarketplaces(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
	})

	// Well past the ceiling. The settings file is the one source that did not
	// come through the backend's validation, so the ceiling has to be applied
	// here or it is not applied at all.
	var entries []string
	for i := 0; i < runv1.MaxPluginMarketplaces*4; i++ {
		entries = append(entries, fmt.Sprintf(
			`"m%d": {"source": {"source": "github", "repo": "attacker/p%d"}}`, i, i))
	}
	writeRepoSettings(t, h, "settings.json",
		`{"extraKnownMarketplaces": {`+strings.Join(entries, ",")+`}}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0: an unusable settings file is ignored, not fatal (%v)",
			code, run.Failure())
	}
	for _, c := range gitClones(h) {
		if strings.Contains(c, "attacker/") {
			t.Fatalf("a marketplace past the ceiling was cloned: %s", c)
		}
	}
}

func TestACodexRunDoesNotInheritARepositorysClaudeCodeSettings(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		Env:     map[string]string{runv1.EnvAgent: string(runv1.AgentCodex), runv1.EnvRole: "coder"},
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"from-the-role@playneta"},
		}),
	})
	// `.claude/settings.<role>.json` is claude-code's format and says nothing
	// about what codex should load. Reading it here would have a codex run
	// issue `codex plugin add` for a list written for the other runtime.
	writeRepoSettings(t, h, "settings.coder.json", `{
	  "extraKnownMarketplaces": {"other": {"source": {"source": "github", "repo": "org/other"}}},
	  "enabledPlugins": {"for-claude-code@other": true}
	}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	cmds := strings.Join(pluginCommands(h), " | ")
	if strings.Contains(cmds, "for-claude-code@other") {
		t.Errorf("a codex run installed from claude-code's settings file: %s", cmds)
	}
	if !strings.Contains(cmds, "from-the-role@playneta") {
		t.Errorf("the role's own list did not reach codex: %s", cmds)
	}
}

func TestARepositoryThatNamesNoMarketplaceIsIgnoredRatherThanFatal(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc",
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"from-the-role@playneta"},
		}),
	})
	// A very ordinary settings file: the plugins are enabled here and the
	// marketplaces were added once, by hand, on a developer's machine. Nothing
	// in this pod can register them, so acting on it would win the chain,
	// discard the role's list and then fail the run at the install.
	writeRepoSettings(t, h, "settings.json", `{"enabledPlugins": {"installed-at-home@somewhere": true}}`)

	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	cmds := strings.Join(pluginCommands(h), " | ")
	if strings.Contains(cmds, "installed-at-home@somewhere") {
		t.Errorf("a plugin with no registrable marketplace was attempted: %s", cmds)
	}
	if !strings.Contains(cmds, "from-the-role@playneta") {
		t.Errorf("the role's list was discarded by an unusable settings file: %s", cmds)
	}
}
