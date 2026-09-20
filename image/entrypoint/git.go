package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Git, and the rule that decides whether a failed push costs one pod run or
// three.
//
// A failure is classified by whoever has the cause, not the effect. The
// controller sees "exit 20"; this process saw "the forge answered 403 to a push
// into a protected branch". So the entrypoint distinguishes, inside the git
// phases (R11):
//
//   - network, 5xx, a ref conflict, an exhausted rate limit → exit 20, retry
//     makes sense;
//   - 401, 403, 404 on clone or push, no permission to open a pull request →
//     exit 30, because a retry would reproduce the same answer three times and
//     spend three pod runs doing it.

// gitFailure classifies what git or the forge said. The output decides the exit
// code, and therefore decides whether the controller spends two more pod runs
// discovering the same 403.
func gitFailure(reason, output, format string, args ...any) *Failure {
	code := runv1.ExitGit
	if isPermanentGitRefusal(output) {
		code = runv1.ExitConfig
	}
	return fail(code, reason, "%s: %s", sprintf(format, args...), firstLines(output, 6))
}

// isPermanentGitRefusal recognises the answers a second attempt would receive
// unchanged. Matching on text is unpleasant and it is what git and the forges
// give us: git exits 128 for almost everything.
func isPermanentGitRefusal(output string) bool {
	lower := strings.ToLower(output)
	permanent := []string{
		"authentication failed",
		"invalid username or password",
		"could not read username",
		"permission denied",
		"403 forbidden",
		"401 unauthorized",
		"repository not found",
		"404 not found",
		"protected branch",
		"pre-receive hook declined",
		"you are not allowed to push",
		"refusing to allow",
	}
	for _, needle := range permanent {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// firstLines trims a command's output to something a 1 KiB message can carry
// while keeping the part that says what happened. The full output is in the
// log, which is where a person goes next.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

// git runs one git command in the workspace and returns its combined output.
//
// The credential helper is passed with -c rather than written into the
// repository's configuration: the token must not end up in .git/config, where
// the agent would read it.
func (r *Run) git(ctx context.Context, args ...string) (string, int32, error) {
	full := []string{
		"-c", "credential.helper=" + r.gitCredentialHelperPath(),
		"-c", "user.name=haliphron",
		"-c", "user.email=haliphron@invalid",
		// Never prompt. A pod waiting on a terminal that does not exist is a
		// pod that burns its whole budget in silence.
		"-c", "core.askpass=",
	}
	full = append(full, args...)

	var out bytes.Buffer
	result, err := r.commander.Run(ctx, Command{
		Path:   "git",
		Args:   full,
		Dir:    r.layout.Workspace,
		Env:    r.gitEnv(),
		Stdout: &out,
		Stderr: &out,
	})
	text := r.redactor.String(out.String())
	if strings.TrimSpace(text) != "" {
		r.logf("git %s: %s", strings.Join(args, " "), firstLines(text, 20))
	}
	return text, result.ExitCode, err
}

// gitEnv is git's environment. The forge CLIs each insist on their own name for
// the one token that arrived, so all of them are set from the single value —
// and none of this reaches the agent, which gets the environment built in the
// auth phase instead.
func (r *Run) gitEnv() []string {
	env := []string{
		"HOME=" + r.layout.Home,
		"PATH=" + orDefault(os.Getenv("PATH"), "/usr/local/bin:/usr/bin:/bin"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if r.secrets.GitToken == "" {
		return env
	}
	switch r.cfg.GitProvider {
	case runv1.GitProviderGitLab:
		return append(env, "GL_TOKEN="+r.secrets.GitToken, "GITLAB_TOKEN="+r.secrets.GitToken)
	default:
		return append(env, "GH_TOKEN="+r.secrets.GitToken, "GITHUB_TOKEN="+r.secrets.GitToken)
	}
}

// phaseClone clones, checks out the base and creates the run's branch.
func phaseClone(ctx context.Context, r *Run) error {
	if !r.cfg.HasRepo() {
		return skip("this run has no repository")
	}

	args := []string{"clone"}
	if r.cfg.CloneDepth > 0 {
		args = append(args, "--depth", strconv.Itoa(r.cfg.CloneDepth))
	}
	if r.cfg.BaseBranch != "" {
		args = append(args, "--branch", r.cfg.BaseBranch)
	}
	if r.cfg.Submodules {
		args = append(args, "--recurse-submodules")
	}
	args = append(args, r.cfg.RepoURL, ".")

	out, code, err := r.git(ctx, args...)
	if err != nil {
		return failWrap(runv1.ExitGit, "CloneFailed", err, "running git clone")
	}
	if code != 0 {
		return gitFailure("CloneFailed", out, "cloning %s", r.cfg.RepoURL)
	}

	// The clone is the first moment .git exists, and the agent must not start
	// before the run's own directory is excluded from it — otherwise the forced
	// commit of the commit phase commits the run's output into the pull request.
	r.excludeRunIO()

	// The branch belongs to the run and the run may be on its second attempt,
	// so an existing branch is checked out rather than treated as a conflict.
	if out, code, err := r.git(ctx, "checkout", "-B", r.cfg.TargetBranch); err != nil {
		return failWrap(runv1.ExitGit, "BranchFailed", err, "creating %s", r.cfg.TargetBranch)
	} else if code != 0 {
		return gitFailure("BranchFailed", out, "creating branch %s", r.cfg.TargetBranch)
	}

	r.repo.TargetBranch = r.cfg.TargetBranch
	if r.cfg.LFS {
		if _, code, _ := r.git(ctx, "lfs", "pull"); code != 0 {
			r.logf("git lfs pull failed; large files may be pointers")
		}
	}
	return nil
}

// phaseCommit commits what the agent left uncommitted.
//
// The agent's own commits are kept as they are; leftover changes are committed
// by force. Otherwise the work of an agent that crashed or was interrupted is
// lost entirely — and the interrupted case is the one the timeout path exists
// to salvage.
func phaseCommit(ctx context.Context, r *Run) error {
	if !r.cfg.HasRepo() {
		return skip("this run has no repository")
	}

	status, code, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return failWrap(runv1.ExitGit, "StatusFailed", err, "running git status")
	}
	if code != 0 {
		return gitFailure("StatusFailed", status, "reading the work tree state")
	}

	if strings.TrimSpace(status) != "" {
		if out, code, err := r.git(ctx, "add", "-A"); err != nil {
			return failWrap(runv1.ExitGit, "CommitFailed", err, "staging changes")
		} else if code != 0 {
			return gitFailure("CommitFailed", out, "staging changes")
		}
		message := sprintf("haliphron: %s run %s\n\nCommitted by the entrypoint: the agent left these changes uncommitted.\n",
			r.cfg.Agent, r.cfg.RunID)
		if out, code, err := r.git(ctx, "commit", "-m", message); err != nil {
			return failWrap(runv1.ExitGit, "CommitFailed", err, "committing")
		} else if code != 0 {
			return gitFailure("CommitFailed", out, "committing the leftover changes")
		}
	}

	head, code, err := r.git(ctx, "rev-parse", "HEAD")
	if err == nil && code == 0 {
		r.repo.CommitSHA = strings.TrimSpace(head)
	}

	// Nothing to push is a legitimate outcome: an analysis run changes no files
	// and should not be made to look like a failure.
	base := "origin/" + orDefault(r.cfg.BaseBranch, "HEAD")
	if diff, code, err := r.git(ctx, "diff", "--quiet", base, "HEAD"); err == nil && code == 0 {
		_ = diff
		return skip("the work tree is identical to %s; there is nothing to push", base)
	}
	return nil
}

// phasePush pushes to the branch the backend named.
//
// --force-with-lease and not --force: the run's branch belongs to the run, but
// overwriting somebody else's work without a single check is not a default
// anyone should have to discover.
func phasePush(ctx context.Context, r *Run) error {
	if !r.cfg.HasRepo() {
		return skip("this run has no repository")
	}
	if r.outcome(runv1.RuntimePhaseCommit) == runv1.PhaseOutcomeSkipped {
		return skip("nothing was committed")
	}

	out, code, err := r.git(ctx, "push", "--force-with-lease", "origin",
		"HEAD:refs/heads/"+r.cfg.TargetBranch)
	if err != nil {
		return failWrap(runv1.ExitGit, "PushFailed", err, "running git push")
	}
	if code != 0 {
		return gitFailure("PushRejected", out, "pushing to %s", r.cfg.TargetBranch)
	}

	r.repo.Pushed = true
	return nil
}

// phasePR creates the pull request, or updates the one a previous attempt
// opened.
//
// create-or-update is what makes the retry safe: on a second attempt the pull
// request already exists, and failing on that would turn every recoverable
// failure into a permanent one.
func phasePR(ctx context.Context, r *Run) error {
	switch {
	case !r.cfg.HasRepo():
		return skip("this run has no repository")
	case !r.cfg.CreatePR:
		return skip("this run did not ask for a pull request")
	case !r.repo.Pushed:
		return skip("nothing was pushed, so there is nothing to open a pull request for")
	}

	tool, args, ok := r.forgeListCommand()
	if !ok {
		return skip("no forge CLI for provider %s", r.cfg.GitProvider)
	}

	var out bytes.Buffer
	result, err := r.commander.Run(ctx, Command{
		Path: tool, Args: args, Dir: r.layout.Workspace, Env: r.gitEnv(),
		Stdout: &out, Stderr: &out,
	})
	if err == nil && result.ExitCode == 0 {
		if url, number, found := parseExistingPR(out.Bytes()); found {
			r.repo.PRURL, r.repo.PRNumber, r.repo.PRAction = url, number, runv1.PRActionUpdated
			r.logf("pull request %d already exists and was updated: %s", number, url)
			return nil
		}
	}

	tool, args = r.forgeCreateCommand()
	out.Reset()
	result, err = r.commander.Run(ctx, Command{
		Path: tool, Args: args, Dir: r.layout.Workspace, Env: r.gitEnv(),
		Stdout: &out, Stderr: &out,
	})
	text := r.redactor.String(out.String())
	if err != nil {
		return failWrap(runv1.ExitGit, "PRFailed", err, "running %s", tool)
	}
	if result.ExitCode != 0 {
		return gitFailure("PRFailed", text, "opening a pull request for %s", r.cfg.TargetBranch)
	}

	r.repo.PRURL = strings.TrimSpace(lastNonEmptyLine(text))
	r.repo.PRAction = runv1.PRActionCreated
	r.logf("pull request opened: %s", r.repo.PRURL)
	return nil
}

func (r *Run) forgeListCommand() (string, []string, bool) {
	switch r.cfg.GitProvider {
	case runv1.GitProviderGitHub:
		return "gh", []string{"pr", "list", "--head", r.cfg.TargetBranch,
			"--state", "open", "--json", "url,number"}, true
	case runv1.GitProviderGitLab:
		return "glab", []string{"mr", "list", "--source-branch", r.cfg.TargetBranch,
			"--output", "json"}, true
	default:
		return "", nil, false
	}
}

func (r *Run) forgeCreateCommand() (string, []string) {
	title := r.prTitle()
	body := sprintf("Opened by haliphron run `%s`, attempt %d, agent `%s`.\n\n%s",
		r.cfg.RunID, r.cfg.Attempt, r.cfg.Agent, truncate(r.summary, 60000))

	if r.cfg.GitProvider == runv1.GitProviderGitLab {
		args := []string{"mr", "create", "--source-branch", r.cfg.TargetBranch,
			"--title", title, "--description", body, "--yes"}
		if r.cfg.BaseBranch != "" {
			args = append(args, "--target-branch", r.cfg.BaseBranch)
		}
		return "glab", args
	}
	args := []string{"pr", "create", "--head", r.cfg.TargetBranch, "--title", title, "--body", body}
	if r.cfg.BaseBranch != "" {
		args = append(args, "--base", r.cfg.BaseBranch)
	}
	return "gh", args
}

// prTitle prefers what the agent proposed. The title does not affect
// idempotency — the branch name does, and that is the backend's — so letting
// the model name its own change costs nothing and reads far better.
func (r *Run) prTitle() string {
	if r.payload != nil && len(r.payload.Data) > 0 {
		var proposed struct {
			Title   string `json:"title"`
			PRTitle string `json:"prTitle"`
		}
		if err := json.Unmarshal(r.payload.Data, &proposed); err == nil {
			if title := orDefault(proposed.PRTitle, proposed.Title); title != "" {
				return truncate(title, 255)
			}
		}
	}
	return sprintf("haliphron: %s run %s", r.cfg.Agent, r.cfg.RunID)
}

// parseExistingPR reads the forge CLI's listing. Both `gh --json url,number`
// and `glab --output json` produce an array of objects with those two fields.
func parseExistingPR(body []byte) (string, int32, bool) {
	var list []struct {
		URL    string `json:"url"`
		WebURL string `json:"web_url"`
		Number int32  `json:"number"`
		IID    int32  `json:"iid"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &list); err != nil || len(list) == 0 {
		return "", 0, false
	}
	first := list[0]
	url := orDefault(first.URL, first.WebURL)
	number := first.Number
	if number == 0 {
		number = first.IID
	}
	if url == "" {
		return "", 0, false
	}
	return url, number, true
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
