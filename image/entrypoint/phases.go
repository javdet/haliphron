package entrypoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The phases that come before the model and cost nothing but time. Between
// them they establish that this pod can do the job at all — and every one of
// them that fails does so with exit 30, because a retry would reproduce it
// exactly.

// phaseInit establishes identity and the writable layout, and opens the log.
//
// There are exactly four writable directories in the image and all four are
// emptyDir; the container root is read-only. That is achievable rather than
// aspirational only because every CLI cache is redirected into $HOME, and it is
// verified by running the image under `docker run --read-only`, not by argument.
func phaseInit(_ context.Context, r *Run) error {
	for _, dir := range []string{
		r.layout.Workspace, r.layout.RunIO, r.layout.Artifacts,
		r.layout.Inputs, r.layout.RunPrivate, r.layout.Home,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return failWrap(runv1.ExitConfig, "LayoutUnwritable", err,
				"%s must be writable and is not; the pod is missing an emptyDir", dir)
		}
	}

	log, err := NewLogSink(filepath.Join(r.layout.RunPrivate, "agent.log"))
	if err != nil {
		return err
	}
	r.log = log

	r.logf("haliphron entrypoint: contract %s, image %s", runv1.ContractVersion, r.cfg.ImageVersion)
	r.logf("run %s attempt %d, agent %s, model %s, role %q",
		r.cfg.RunID, r.cfg.Attempt, r.cfg.Agent, r.cfg.Model, r.cfg.Role)
	r.logf("artifacts: %s, prefix %s", r.uploader.Describe(), r.cfg.StoragePrefix)

	// The clone occupies all of /workspace, which puts the run's own exchange
	// directory inside the work tree where a forced commit would pick it up.
	// Written here as well as after the clone, because a workspace that already
	// has a .git — a resumed pod, an overlay image — gets no second chance.
	r.excludeRunIO()

	r.log.Start(context.Background(), r.uploader, r.checkpoint, r.cfg.LogChunkInterval)
	return nil
}

// excludeRunIO keeps DirRunIO out of git. .git/info/exclude and not .gitignore:
// the latter is a repository file, and editing it is a change that would ride
// along into the pull request.
func (r *Run) excludeRunIO() {
	gitDir := filepath.Join(r.layout.Workspace, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return
	}
	rel, err := filepath.Rel(r.layout.Workspace, r.layout.RunIO)
	if err != nil {
		return
	}
	pattern := "/" + filepath.ToSlash(rel) + "/\n"

	path := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.logf("could not prepare %s: %v", path, err)
		return
	}
	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), pattern) {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.logf("could not write %s: %v", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(pattern); err != nil {
		r.logf("could not write %s: %v", path, err)
		return
	}
	r.logf("excluded %s from the work tree", pattern[:len(pattern)-1])
}

// phaseValidate checks everything that can be checked before anything costs
// money — and, just as importantly, before anything is charged to a model.
func phaseValidate(_ context.Context, r *Run) error {
	if err := r.validateUploader(); err != nil {
		return err
	}

	// One credential arrives under one name and three tools insist on their
	// own. The combination that cannot work is a gitlab run whose MCP
	// configuration talks to GitHub: the server would be handed a gitlab token
	// and would fail in a way the agent reports as "the tool did not work".
	if err := r.checkProviderAgainstMCP(); err != nil {
		return err
	}

	r.logf("configuration validated: %d allowed tools, %d denied, permission mode %q",
		len(r.cfg.AllowedTools), len(r.cfg.DeniedTools), r.cfg.PermissionMode)
	return nil
}

// validateUploader checks that this run can persist what it is about to pay
// for, before it pays for it.
//
// Discovering at the persist phase that there is nowhere to write result.md
// means discovering it after the model has been billed. What there is to check
// depends on the mode, and the asymmetry is itself an argument: relay mode has
// one thing that can be wrong and object-store mode has four capabilities and
// an expiry.
func (r *Run) validateUploader() error {
	if r.cfg.ArtifactMode != runv1.ArtifactModeObjectStore {
		// The relay's single precondition, and NewUploader already refused a
		// run without it. Restated here so that the phase which is supposed to
		// check preconditions is the phase that checks them.
		if r.callback == nil {
			return fail(runv1.ExitConfig, "MissingConfiguration",
				"relay mode needs a callback URL and none was configured")
		}
		return nil
	}

	for _, key := range []string{
		runv1.StorageKeyResult, runv1.StorageKeyOutput,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		if r.secrets.Bundle.Put[key].URL == "" {
			return fail(runv1.ExitConfig, "MalformedSecret",
				"the bundle has no presigned PUT for %s: this run could not persist its result", key)
		}
	}

	// A bundle that expires before the run's own budget is a result lost on
	// work that succeeded. The controller reissues before attempt two; there is
	// nothing this pod can do about it except say so while the log is still
	// being read for something else.
	expires := r.secrets.Bundle.ExpiresAt
	if deadline := r.clock().Add(r.cfg.Timeout); expires.Before(deadline) {
		r.logf("warning: the bundle expires at %s, before this run's own budget runs out at %s; "+
			"a long run will fail its uploads with exit %d",
			expires.UTC().Format("15:04:05Z"), deadline.UTC().Format("15:04:05Z"),
			runv1.ExitStorage)
	}
	return nil
}

// checkProviderAgainstMCP refuses a combination that would be discovered as a
// baffled agent rather than as a configuration error.
func (r *Run) checkProviderAgainstMCP() error {
	if len(r.secrets.MCPConfig) == 0 || r.cfg.GitProvider == runv1.GitProviderNone {
		return nil
	}
	var cfg struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(r.secrets.MCPConfig, &cfg); err != nil {
		return failWrap(runv1.ExitConfig, "MalformedSecret", err,
			"%s does not parse", runv1.SecretKeyMCPConfig)
	}
	foreign := map[runv1.GitProvider]string{
		runv1.GitProviderGitHub: "gitlab",
		runv1.GitProviderGitLab: "github",
	}[r.cfg.GitProvider]
	if foreign == "" {
		return nil
	}
	for name, raw := range cfg.Servers {
		haystack := strings.ToLower(name + " " + string(raw))
		if strings.Contains(haystack, foreign) {
			return fail(runv1.ExitConfig, "ProviderMismatch",
				"the run's provider is %s and the MCP server %q is configured for %s; "+
					"it would be handed the wrong token and the agent would report a broken tool",
				r.cfg.GitProvider, name, foreign)
		}
	}
	return nil
}

// phaseFetch reads the prompt out of the environment and proves it is the one
// that was admitted.
//
// It used to be a presigned GET against runs/{id}/prompt.txt. It is now a
// variable, and the phase survives the change deliberately: the names here key
// the checkpoint, they are the phase field of PhaseTiming, and the UI groups a
// run's timeline by them. Deleting a phase to save two lines would have been a
// breaking change to three contracts in exchange for nothing.
//
// The digest check is mandatory and is the one property the presigned GET
// bought that was worth keeping (R12). A run that executes something other than
// what the backend admitted is worse than a run that does not start, and a
// mismatch is not repaired by trying again — so it is exit 30 and the model is
// never called.
func phaseFetch(_ context.Context, r *Run) error {
	body := []byte(r.cfg.Prompt)

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != r.cfg.PromptSHA256 {
		return fail(runv1.ExitConfig, "PromptDigestMismatch",
			"the prompt in %s hashes to %s and the controller said %s; refusing to run something "+
				"other than what was admitted", runv1.EnvPrompt, got, r.cfg.PromptSHA256)
	}
	r.prompt = body

	// Written into the entrypoint's private directory, not the work tree. Two
	// reasons: an agent CLI that takes its prompt from a file needs one, and a
	// prompt the agent can rewrite is a prompt the next attempt cannot trust.
	path := filepath.Join(r.layout.RunPrivate, "prompt.txt")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", path)
	}
	r.logf("prompt read from the environment: %d bytes, sha256 %s", len(body), got)
	return nil
}

// phaseCheckpoint reads what an earlier attempt of this run got through.
//
// An absent HALIPHRON_COMPLETED_PHASES is the normal answer on a first attempt
// and is a skip, not a failure — the same non-event the old 404 on state.json
// was, minus the round trip that could fail for unrelated reasons. That round
// trip is why this used to be the phase that could kill a run before it
// started: a presigned GET whose signature had expired answered 403, and 403
// is indistinguishable from a forged link.
func phaseCheckpoint(_ context.Context, r *Run) error {
	if len(r.cfg.CompletedPhases) == 0 {
		return skip("no checkpoint: this is the first attempt of this run under this lease")
	}
	r.logf("an earlier attempt reported: %s", joinPhaseNames(r.cfg.CompletedPhases))

	resumable, why := r.checkpoint.Resume(r.cfg)
	if !resumable {
		r.logf("not resuming: %s", why)
		return nil
	}

	// Resuming means skipping the one phase whose repetition costs money, and
	// it is done on the control plane's word rather than on evidence this pod
	// can gather. That is a deliberate narrowing from what the object-based
	// checkpoint claimed to do: it recorded where the previous attempt's result
	// went and this pod verified the object was readable. It could only ever
	// verify that in object-store mode, and only when the bundle happened to
	// grant a read — so the check succeeded on trust more often than not.
	//
	// What replaces it is a stricter rule, enforced above: resume only when the
	// previous attempt completed *both* run and persist. persist is what made
	// the model's product durable, so an attempt that reached it has a result
	// in the store under a key this attempt's report will name, whether or not
	// this pod can read it back.
	r.resumed = true
	// The refs the report will name. They are reconstructed from the layout
	// rather than carried over, and that is possible only because the keys are
	// fixed: runs/{id}/result.md is where the previous attempt's persist phase
	// put the result, whichever mode it used and whichever store it reached.
	//
	// Uploaded is set because an earlier attempt completed persist, which is
	// exactly the condition Resume checks. The backend checks the same flag
	// before believing a ref, so claiming it on any weaker evidence would be
	// claiming something this pod cannot know.
	prefix := sprintf(runv1.StoragePrefixRun, r.cfg.RunID)
	r.resultRef = &runv1.ObjectRef{
		Key: prefix + runv1.StorageKeyResult, ContentType: "text/markdown", Uploaded: true}
	r.outputRef = &runv1.ObjectRef{
		Key: prefix + runv1.StorageKeyOutput, ContentType: "application/json", Uploaded: true}

	r.logf("resuming: %s; the model will not be called again", why)
	return nil
}

// joinPhaseNames renders a checkpoint for a log line.
func joinPhaseNames(phases []runv1.RuntimePhase) string {
	names := make([]string, 0, len(phases))
	for _, p := range phases {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

// phaseRole resolves the role config chain and picks up the node's output
// schema.
//
// The chain is: the repository's own settings for this role, the ConfigMap's
// fallback for this role, the repository's default settings, the built-in
// defaults. The first two do not exist before the clone, which is why this
// phase runs after it rather than before.
func phaseRole(_ context.Context, r *Run) error {
	// The reserved key. A role that ships a file under this name loses it; that
	// is the price of the reservation, and the reason it is named in the
	// contract rather than agreed informally.
	schemaPath := filepath.Join(r.layout.RoleConfig, runv1.RoleConfigKeyOutputSchema)
	switch schema, err := os.ReadFile(schemaPath); {
	case err == nil:
		r.nodeSchema = schema
		r.logf("the node declared an output schema: %s is now mandatory and validated", r.layout.Output)
	case !errors.Is(err, fs.ErrNotExist):
		return failWrap(runv1.ExitConfig, "RoleConfigUnreadable", err, "reading %s", schemaPath)
	}

	chain := r.roleChain()
	for _, candidate := range chain {
		if _, err := os.Stat(candidate); err == nil {
			r.logf("role settings: %s", candidate)
			return nil
		}
	}
	// Not a failure. A run with no role files gets the built-in defaults, and
	// the tool ceiling has already been applied by the backend regardless: the
	// permission mode and the allow and deny lists in the environment are the
	// intersection, and this phase does not recompute them.
	r.logf("no role settings found in %d candidates; using the built-in defaults", len(chain))
	return nil
}

// roleChain is the resolution order, most specific first.
func (r *Run) roleChain() []string {
	var chain []string
	if r.cfg.Role != "" {
		chain = append(chain,
			filepath.Join(r.layout.Workspace, ".claude", "settings."+r.cfg.Role+".json"),
			filepath.Join(r.layout.RoleConfig, "settings."+r.cfg.Role+".json"),
		)
	}
	return append(chain, filepath.Join(r.layout.Workspace, ".claude", "settings.json"))
}
