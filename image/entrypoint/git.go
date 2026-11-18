package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// git runs one git command in the workspace, as the working repository, and
// returns its combined output.
func (r *Run) git(ctx context.Context, args ...string) (string, int32, error) {
	return r.gitAt(ctx, r.layout.Workspace, r.primaryRepo(), args...)
}

// gitAt runs one git command in dir with the environment of repository w.
//
// The credential helper is passed with -c rather than written into the
// repository's configuration: the token must not end up in .git/config, where
// the agent would read it.
func (r *Run) gitAt(ctx context.Context, dir string, w *repoWork, args ...string) (string, int32, error) {
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
		Dir:    dir,
		Env:    r.gitEnvFor(w),
		Stdout: &out,
		Stderr: &out,
	})
	text := r.redactor.String(out.String())
	if strings.TrimSpace(text) != "" {
		r.logf("git %s: %s", strings.Join(args, " "), firstLines(text, 20))
	}
	return text, result.ExitCode, err
}

// gitEnv is the working repository's git environment.
func (r *Run) gitEnv() []string { return r.gitEnvFor(r.primaryRepo()) }

// gitEnvFor is git's environment for one repository. The forge CLIs each insist
// on their own name for the token, so all of them are set from the token of
// that repository's host — and none of this reaches the agent, which gets the
// environment built in the auth phase instead.
func (r *Run) gitEnvFor(w *repoWork) []string {
	env := []string{
		"HOME=" + r.layout.Home,
		"PATH=" + orDefault(os.Getenv("PATH"), "/usr/local/bin:/usr/bin:/bin"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	env = append(env, r.safeDirectoryEnv()...)
	token := r.tokenFor(w)
	if token == "" {
		return env
	}
	switch w.Provider {
	case runv1.GitProviderGitLab:
		return append(env, "GL_TOKEN="+token, "GITLAB_TOKEN="+token)
	default:
		return append(env, "GH_TOKEN="+token, "GITHUB_TOKEN="+token)
	}
}

// tokenFor is the token for a repository's host: its entry in
// SecretKeyGitTokens, or — for the working repository, and for a run whose
// backend wrote no such key — SecretKeyGitToken.
func (r *Run) tokenFor(w *repoWork) string {
	if w == nil {
		return r.secrets.GitToken
	}
	if entry, ok := r.secrets.GitTokens[w.Host]; ok && w.Host != "" {
		return entry.Token
	}
	if w.Path == "" {
		return r.secrets.GitToken
	}
	return ""
}

// safeDirectoryEnv marks the workspace, and every extra repository, as a
// repository git may work in.
//
// The root of an emptyDir belongs to root: fsGroup changes its group and never
// its owner, and a pod with every capability dropped cannot chown it. git
// refuses a work tree owned by anyone but the caller ("detected dubious
// ownership"), so without this the clone succeeds — git created .git itself —
// and the checkout that follows fails on every attempt.
//
// Set through GIT_CONFIG_COUNT rather than -c because the same environment
// reaches gh, glab and the agent, all of which run git in the workspace too.
// It is command scope, which git honours for safe.directory where it ignores
// the repository's own configuration, and it names the run's own work trees
// rather than "*".
func (r *Run) safeDirectoryEnv() []string {
	dirs := []string{r.layout.Workspace}
	for _, extra := range r.cfg.ExtraRepos {
		dirs = append(dirs, filepath.Join(r.layout.Repos, extra.Path))
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(dirs))}
	for i, dir := range dirs {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"=safe.directory", "GIT_CONFIG_VALUE_"+n+"="+dir)
	}
	return env
}

// repoWork is one repository the git phases act on: the working one, at the
// workspace, or one of the extras, under Layout.Repos.
type repoWork struct {
	Dir string
	URL string
	// Path is the directory name under Layout.Repos; empty for the working
	// repository.
	Path       string
	BaseBranch string
	Provider   runv1.GitProvider
	// Host is what git names to the credential helper; empty when the URL
	// names none.
	Host  string
	Depth int
	// Result is what the report says about the repository.
	Result *runv1.RepoResult

	// cloned is whether this attempt's clone of it succeeded. An extra that
	// was never cloned is not reported and not committed.
	cloned bool
	// changed is whether the commit phase of this attempt found anything to
	// push in it.
	changed bool
}

// label names the repository in a log line or a failure message.
func (w *repoWork) label() string {
	if w.Path == "" {
		return "the working repository"
	}
	return w.Dir
}

// primaryRepo is the working repository as a repoWork.
func (r *Run) primaryRepo() *repoWork {
	if r.primary == nil {
		host, _ := gitHost(r.cfg.RepoURL)
		r.primary = &repoWork{
			Dir: r.layout.Workspace, URL: r.cfg.RepoURL, BaseBranch: r.cfg.BaseBranch,
			Provider: r.cfg.GitProvider, Host: host, Depth: r.cfg.CloneDepth, Result: &r.repo,
			cloned: r.cfg.HasRepo(),
		}
	}
	return r.primary
}

// extraRepos are the extra repositories as repoWorks, in the order the spec
// lists them.
func (r *Run) extraRepos() []*repoWork {
	if r.extras == nil && len(r.cfg.ExtraRepos) > 0 {
		for _, spec := range r.cfg.ExtraRepos {
			host, _ := gitHost(spec.URL)
			r.extras = append(r.extras, &repoWork{
				Dir: filepath.Join(r.layout.Repos, spec.Path), URL: spec.URL, Path: spec.Path,
				BaseBranch: spec.BaseBranch, Provider: spec.Provider, Host: host,
				Depth: int(spec.CloneDepth), Result: &runv1.RepoResult{},
			})
		}
	}
	return r.extras
}

// clonedRepos are the repositories this attempt has a work tree for, the
// working one first.
func (r *Run) clonedRepos() []*repoWork {
	var out []*repoWork
	if primary := r.primaryRepo(); primary.cloned {
		out = append(out, primary)
	}
	for _, w := range r.extraRepos() {
		if w.cloned {
			out = append(out, w)
		}
	}
	return out
}

// repoFailure is one repository's failure inside a git phase that goes on to
// the others.
type repoFailure struct {
	repo *repoWork
	err  error
}

// joinRepoFailures is the outcome of a git phase that went through every
// repository, failures and all: a 403 in one repository must not leave the
// work in the others unpushed.
//
// One failure is returned as it is, named by repository when it is not the
// working one. Several become one, classified as config only when every one
// of them is — a retry is pointless only if it would be pointless for all of
// them — and git otherwise.
func joinRepoFailures(fails []repoFailure) error {
	if len(fails) == 0 {
		return nil
	}
	if len(fails) == 1 && fails[0].repo.Path == "" {
		return fails[0].err
	}
	code := runv1.ExitConfig
	messages := make([]string, 0, len(fails))
	var reason string
	for _, rf := range fails {
		f := classify(rf.err, runv1.ExitGit)
		if f.Code != runv1.ExitConfig {
			code = runv1.ExitGit
		}
		if reason == "" {
			reason = f.Reason
		}
		messages = append(messages, rf.repo.label()+": "+f.Error())
	}
	if len(fails) > 1 {
		return fail(code, reason, "%d repositories failed: %s", len(fails), strings.Join(messages, "; "))
	}
	return fail(code, reason, "%s", messages[0])
}

// phaseClone clones, checks out the base and creates the run's branch.
func phaseClone(ctx context.Context, r *Run) error {
	if !r.cfg.HasRepo() {
		return skip("this run has no repository")
	}

	// git refuses to clone into a directory that is not empty, and the
	// workspace never is by now: phaseInit has already created DirRunIO inside
	// it. So the repository goes straight to the workspace's .git, the work
	// tree git insists on creating is a throwaway under RunPrivate, and the
	// checkout is done in place afterwards. Everything else — depth, branch,
	// origin/HEAD — comes out exactly as a plain clone would leave it.
	scratch := filepath.Join(r.layout.RunPrivate, "clone")
	_ = os.RemoveAll(scratch)

	args := []string{"clone", "--no-checkout",
		"--separate-git-dir", filepath.Join(r.layout.Workspace, ".git")}
	if r.cfg.CloneDepth > 0 {
		args = append(args, "--depth", strconv.Itoa(r.cfg.CloneDepth))
	}
	if r.cfg.BaseBranch != "" {
		args = append(args, "--branch", r.cfg.BaseBranch)
	}
	args = append(args, r.cfg.RepoURL, scratch)

	out, code, err := r.git(ctx, args...)
	if err != nil {
		return failWrap(runv1.ExitGit, "CloneFailed", err, "running git clone")
	}
	if code != 0 {
		return gitFailure("CloneFailed", out, "cloning %s", r.cfg.RepoURL)
	}
	// All the scratch tree holds is a .git file pointing back at the workspace.
	if err := os.RemoveAll(scratch); err != nil {
		r.logf("could not remove %s: %v", scratch, err)
	}

	// Untracked files are left alone, which is what keeps DirRunIO intact.
	if out, code, err := r.git(ctx, "reset", "--hard", "--quiet"); err != nil {
		return failWrap(runv1.ExitGit, "CheckoutFailed", err, "running git reset")
	} else if code != 0 {
		return gitFailure("CheckoutFailed", out, "checking out %s", r.cfg.RepoURL)
	}
	if r.cfg.Submodules {
		if out, code, err := r.git(ctx, "submodule", "update", "--init", "--recursive"); err != nil {
			return failWrap(runv1.ExitGit, "SubmodulesFailed", err, "running git submodule update")
		} else if code != 0 {
			return gitFailure("SubmodulesFailed", out, "cloning the submodules of %s", r.cfg.RepoURL)
		}
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

// phaseCloneExtra clones the extra repositories beside the workspace, each onto
// the run's branch, with the submodules and LFS objects the working repository
// was given.
//
// A failure stops at the repository that failed and abandons the run: nothing
// has been paid for yet, and an agent started without a repository its task
// spans would do part of the work and report success.
func phaseCloneExtra(ctx context.Context, r *Run) error {
	extras := r.extraRepos()
	if len(extras) == 0 {
		return skip("this run has no extra repositories")
	}
	if err := os.MkdirAll(r.layout.Repos, 0o755); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "creating %s", r.layout.Repos)
	}

	for _, w := range extras {
		args := []string{"clone"}
		if w.Depth > 0 {
			args = append(args, "--depth", strconv.Itoa(w.Depth))
		}
		if w.BaseBranch != "" {
			args = append(args, "--branch", w.BaseBranch)
		}
		args = append(args, w.URL, w.Dir)

		out, code, err := r.gitAt(ctx, r.layout.Repos, w, args...)
		if err != nil {
			return failWrap(runv1.ExitGit, "ExtraCloneFailed", err, "running git clone for %s", w.Dir)
		}
		if code != 0 {
			return gitFailure("ExtraCloneFailed", out, "cloning %s into %s", w.URL, w.Dir)
		}
		if r.cfg.Submodules {
			if out, code, err := r.gitAt(ctx, w.Dir, w, "submodule", "update", "--init", "--recursive"); err != nil {
				return failWrap(runv1.ExitGit, "SubmodulesFailed", err, "running git submodule update in %s", w.Dir)
			} else if code != 0 {
				return gitFailure("SubmodulesFailed", out, "cloning the submodules of %s", w.URL)
			}
		}
		if out, code, err := r.gitAt(ctx, w.Dir, w, "checkout", "-B", r.cfg.TargetBranch); err != nil {
			return failWrap(runv1.ExitGit, "BranchFailed", err, "creating %s in %s", r.cfg.TargetBranch, w.Dir)
		} else if code != 0 {
			return gitFailure("BranchFailed", out, "creating branch %s in %s", r.cfg.TargetBranch, w.Dir)
		}
		w.cloned = true
		w.Result.TargetBranch = r.cfg.TargetBranch
		if r.cfg.LFS {
			if _, code, _ := r.gitAt(ctx, w.Dir, w, "lfs", "pull"); code != 0 {
				r.logf("git lfs pull failed in %s; large files may be pointers", w.Dir)
			}
		}
		r.logf("cloned %s into %s on %s", w.URL, w.Dir, r.cfg.TargetBranch)
	}
	return nil
}

// phaseCommit commits what the agent left uncommitted, in every repository.
//
// The agent's own commits are kept as they are; leftover changes are committed
// by force. Otherwise the work of an agent that crashed or was interrupted is
// lost entirely — and the interrupted case is the one the timeout path exists
// to salvage.
//
// Every repository is gone through even when one fails, so that the push phase
// still has the others to push. The phase is skipped only when no repository
// has anything to push.
func phaseCommit(ctx context.Context, r *Run) error {
	if !r.cfg.HasRepo() {
		return skip("this run has no repository")
	}

	var fails []repoFailure
	changed := 0
	for _, w := range r.clonedRepos() {
		if err := r.commitOne(ctx, w); err != nil {
			fails = append(fails, repoFailure{w, err})
			continue
		}
		if w.changed {
			changed++
		}
	}
	if err := joinRepoFailures(fails); err != nil {
		return err
	}
	if changed == 0 {
		// Nothing to push is a legitimate outcome: an analysis run changes
		// no files and should not be made to look like a failure.
		if len(r.extraRepos()) == 0 {
			return skip("the work tree is identical to %s; there is nothing to push", baseRef(r.primaryRepo()))
		}
		return skip("no repository differs from its base; there is nothing to push")
	}
	return nil
}

// baseRef is what a repository's branch is compared with to decide whether
// there is anything to push.
func baseRef(w *repoWork) string { return "origin/" + orDefault(w.BaseBranch, "HEAD") }

// commitOne is the commit phase for one repository.
func (r *Run) commitOne(ctx context.Context, w *repoWork) error {
	status, code, err := r.gitAt(ctx, w.Dir, w, "status", "--porcelain")
	if err != nil {
		return failWrap(runv1.ExitGit, "StatusFailed", err, "running git status")
	}
	if code != 0 {
		return gitFailure("StatusFailed", status, "reading the work tree state")
	}

	if strings.TrimSpace(status) != "" {
		if out, code, err := r.gitAt(ctx, w.Dir, w, "add", "-A"); err != nil {
			return failWrap(runv1.ExitGit, "CommitFailed", err, "staging changes")
		} else if code != 0 {
			return gitFailure("CommitFailed", out, "staging changes")
		}
		message := sprintf("haliphron: %s run %s\n\nCommitted by the entrypoint: the agent left these changes uncommitted.\n",
			r.cfg.Agent, r.cfg.RunID)
		if out, code, err := r.gitAt(ctx, w.Dir, w, "commit", "-m", message); err != nil {
			return failWrap(runv1.ExitGit, "CommitFailed", err, "committing")
		} else if code != 0 {
			return gitFailure("CommitFailed", out, "committing the leftover changes")
		}
	}

	head, code, err := r.gitAt(ctx, w.Dir, w, "rev-parse", "HEAD")
	if err == nil && code == 0 {
		w.Result.CommitSHA = strings.TrimSpace(head)
	}

	base := baseRef(w)
	if _, code, err := r.gitAt(ctx, w.Dir, w, "diff", "--quiet", base, "HEAD"); err == nil && code == 0 {
		r.logf("%s is identical to %s; nothing to push from it", w.label(), base)
		return nil
	}
	w.changed = true
	return nil
}

// phasePush pushes every repository with something to push to the branch the
// backend named.
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

	var fails []repoFailure
	pushed := 0
	for _, w := range r.clonedRepos() {
		if !w.changed {
			continue
		}
		out, code, err := r.gitAt(ctx, w.Dir, w, "push", "--force-with-lease", "origin",
			"HEAD:refs/heads/"+r.cfg.TargetBranch)
		switch {
		case err != nil:
			fails = append(fails, repoFailure{w, failWrap(runv1.ExitGit, "PushFailed", err, "running git push")})
		case code != 0:
			fails = append(fails, repoFailure{w, gitFailure("PushRejected", out, "pushing to %s", r.cfg.TargetBranch)})
		default:
			w.Result.Pushed = true
			pushed++
		}
	}
	if err := joinRepoFailures(fails); err != nil {
		return err
	}
	if pushed == 0 {
		return skip("nothing was committed")
	}
	return nil
}

// phasePR creates a pull request in every repository that was pushed, or
// updates the one a previous attempt opened.
//
// create-or-update is what makes the retry safe: on a second attempt the pull
// request already exists, and failing on that would turn every recoverable
// failure into a permanent one.
//
// The extra repositories go first, so that the working repository's pull
// request — the one a person is pointed at — can link every other.
func phasePR(ctx context.Context, r *Run) error {
	switch {
	case !r.cfg.HasRepo():
		return skip("this run has no repository")
	case !r.cfg.CreatePR:
		return skip("this run did not ask for a pull request")
	}

	var order []*repoWork
	for _, w := range r.extraRepos() {
		if w.Result.Pushed {
			order = append(order, w)
		}
	}
	if primary := r.primaryRepo(); primary.Result.Pushed {
		order = append(order, primary)
	}
	if len(order) == 0 {
		return skip("nothing was pushed, so there is nothing to open a pull request for")
	}

	var fails []repoFailure
	opened := 0
	for _, w := range order {
		err := r.prOne(ctx, w)
		switch {
		case err == nil:
			opened++
		case errors.Is(err, errSkip):
			r.logf("no pull request for %s: %v", w.label(), err)
		default:
			fails = append(fails, repoFailure{w, err})
		}
	}
	if err := joinRepoFailures(fails); err != nil {
		return err
	}
	if opened == 0 {
		if len(order) == 1 {
			return skip("no forge CLI for provider %s", order[0].Provider)
		}
		return skip("no pushed repository is on a forge this image can open a pull request on")
	}
	return nil
}

// prOne is the pull request phase for one repository.
func (r *Run) prOne(ctx context.Context, w *repoWork) error {
	tool, args, ok := r.forgeListCommand(w)
	if !ok {
		return skip("no forge CLI for provider %s", w.Provider)
	}

	var out bytes.Buffer
	result, err := r.commander.Run(ctx, Command{
		Path: tool, Args: args, Dir: w.Dir, Env: r.gitEnvFor(w),
		Stdout: &out, Stderr: &out,
	})
	if err == nil && result.ExitCode == 0 {
		if url, number, found := parseExistingPR(out.Bytes()); found {
			w.Result.PRURL, w.Result.PRNumber, w.Result.PRAction = url, number, runv1.PRActionUpdated
			r.logf("pull request %d in %s already exists and was updated: %s", number, w.label(), url)
			return nil
		}
	}

	tool, args = r.forgeCreateCommand(w)
	out.Reset()
	result, err = r.commander.Run(ctx, Command{
		Path: tool, Args: args, Dir: w.Dir, Env: r.gitEnvFor(w),
		Stdout: &out, Stderr: &out,
	})
	text := r.redactor.String(out.String())
	if err != nil {
		return failWrap(runv1.ExitGit, "PRFailed", err, "running %s", tool)
	}
	if result.ExitCode != 0 {
		return gitFailure("PRFailed", text, "opening a pull request for %s", r.cfg.TargetBranch)
	}

	w.Result.PRURL = strings.TrimSpace(lastNonEmptyLine(text))
	w.Result.PRAction = runv1.PRActionCreated
	r.logf("pull request opened in %s: %s", w.label(), w.Result.PRURL)
	return nil
}

func (r *Run) forgeListCommand(w *repoWork) (string, []string, bool) {
	switch w.Provider {
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

func (r *Run) forgeCreateCommand(w *repoWork) (string, []string) {
	title := r.prTitle()
	body := r.prBody(w)

	if w.Provider == runv1.GitProviderGitLab {
		args := []string{"mr", "create", "--source-branch", r.cfg.TargetBranch,
			"--title", title, "--description", body, "--yes"}
		if w.BaseBranch != "" {
			args = append(args, "--target-branch", w.BaseBranch)
		}
		return "glab", args
	}
	args := []string{"pr", "create", "--head", r.cfg.TargetBranch, "--title", title, "--body", body}
	if w.BaseBranch != "" {
		args = append(args, "--base", w.BaseBranch)
	}
	return "gh", args
}

// prBody is the description of a repository's pull request. For a run with
// several repositories, an extra one's says which change it is part of, and the
// working repository's lists the others — so a reviewer who opens any of them
// finds the rest.
func (r *Run) prBody(w *repoWork) string {
	head := sprintf("Opened by haliphron run `%s`, attempt %d, agent `%s`.", r.cfg.RunID, r.cfg.Attempt, r.cfg.Agent)
	extras := r.extraRepos()
	if len(extras) == 0 {
		return head + "\n\n" + truncate(r.summary, 60000)
	}
	if w.Path != "" {
		return head + sprintf("\n\nPart of a change that spans several repositories; the description is on the "+
			"pull request in %s, branch `%s`.", r.cfg.RepoURL, r.cfg.TargetBranch)
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\nThis change spans several repositories:\n")
	for _, x := range extras {
		switch {
		case x.Result.PRURL != "":
			b.WriteString(sprintf("- %s: %s\n", x.URL, x.Result.PRURL))
		case x.Result.Pushed:
			b.WriteString(sprintf("- %s: branch `%s`\n", x.URL, r.cfg.TargetBranch))
		default:
			b.WriteString(sprintf("- %s: no changes\n", x.URL))
		}
	}
	b.WriteString("\n")
	b.WriteString(truncate(r.summary, 56000))
	return b.String()
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
