package image_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// The git phases against real git.
//
// Everywhere else git is scripted, and a scripted clone succeeds whatever the
// directory it is pointed at looks like. The real one refuses a destination
// that is not empty — and the workspace never is by the time the clone runs,
// because phaseInit has already created the run's exchange directory inside it.
// Every run with a repository failed on that, on every attempt, and no scripted
// test could have noticed.

func TestTheCloneSucceedsIntoAWorkspaceTheInitPhaseAlreadyPopulated(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("this test needs a real git on PATH: %v", err)
	}

	upstream := filepath.Join(t.TempDir(), "upstream")
	gitIn(t, "", "init", "--quiet", "--initial-branch=main", upstream)
	writeFile(t, filepath.Join(upstream, "README.md"), "hello\n")
	gitIn(t, upstream, "add", "-A")
	gitIn(t, upstream, "commit", "--quiet", "-m", "initial")

	const repoURL = "https://forge.invalid/org/repo.git"
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "change something", RepoURL: repoURL,
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main", CloneDepth: 1,
		TargetBranch: "haliphron/abc-change",
	})
	// git's HOME is the layout's, so this is the one place a test can point the
	// run's https URL at a local repository without the entrypoint knowing.
	writeFile(t, filepath.Join(h.layout.Home, ".gitconfig"),
		"[url \"file://"+upstream+"\"]\n\tinsteadOf = "+repoURL+"\n")

	agent := agentScript(h, "done", `{"ok":true}`)
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "git" {
			return entrypoint.ExecCommander{}.Run(context.Background(), c)
		}
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			writeFile(t, filepath.Join(h.layout.Workspace, "README.md"), "hello, changed\n")
		}
		return agent(c)
	}

	run, code := h.executeRun()
	if got := h.phase(run, runv1.RuntimePhaseClone); got != runv1.PhaseOutcomeOK {
		t.Fatalf("the clone phase is %s, want ok (%v)", got, run.Failure())
	}
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	// The clone is the workspace itself, and the exchange directory survived it.
	if _, err := os.Stat(filepath.Join(h.layout.Workspace, ".git")); err != nil {
		t.Errorf("the workspace is not the repository: %v", err)
	}
	if _, err := os.Stat(h.layout.Output); err != nil {
		t.Errorf("the agent's output did not survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.layout.RunPrivate, "clone")); !os.IsNotExist(err) {
		t.Errorf("the clone's scratch work tree was left behind: %v", err)
	}
	if got := gitIn(t, h.layout.Workspace, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Errorf("a clone with depth 1 is not shallow: %q", got)
	}

	// What reached the forge is the agent's change and nothing of the run's own.
	pushed := gitIn(t, upstream, "ls-tree", "-r", "--name-only", "haliphron/abc-change")
	if pushed != "README.md" {
		t.Errorf("the pushed branch holds %q, want only README.md", pushed)
	}
	if got := gitIn(t, upstream, "show", "haliphron/abc-change:README.md"); got != "hello, changed" {
		t.Errorf("the pushed README is %q, want the agent's change", got)
	}
}

// gitIn runs the test's own git — the upstream's, never the run's — isolated
// from whatever configuration the machine running the suite has.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@invalid",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
