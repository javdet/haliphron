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
	r.logf("storage %s/%s, bundle expires %s",
		r.cfg.StorageBucket, r.cfg.StoragePrefix, r.storage.ExpiresAt().UTC().Format("2006-01-02T15:04:05Z"))

	// The clone occupies all of /workspace, which puts the run's own exchange
	// directory inside the work tree where a forced commit would pick it up.
	// Written here as well as after the clone, because a workspace that already
	// has a .git — a resumed pod, an overlay image — gets no second chance.
	r.excludeRunIO()

	r.log.Start(context.Background(), r.storage, r.checkpoint, r.cfg.LogChunkInterval)
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
	// The capabilities the run cannot finish without. Discovering at the
	// persist phase that the bundle has no PUT for result.md means discovering
	// it after the model has been paid.
	for _, key := range []string{
		runv1.StorageKeyResult, runv1.StorageKeyOutput,
		runv1.StorageKeyState, runv1.StorageKeyCompletion,
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
	if deadline := r.clock().Add(r.cfg.Timeout); r.storage.ExpiresAt().Before(deadline) {
		r.logf("warning: the bundle expires at %s, before this run's own budget runs out at %s; "+
			"a long run will fail its uploads with exit %d",
			r.storage.ExpiresAt().UTC().Format("15:04:05Z"), deadline.UTC().Format("15:04:05Z"),
			runv1.ExitStorage)
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

// phaseFetch downloads the prompt and proves it is the one that was admitted.
//
// The digest check is mandatory (R12). A run that executes something other than
// what the backend posted is worse than a run that does not start, and a
// mismatch is not repaired by trying again — so it is exit 30 and the model is
// never called.
func phaseFetch(ctx context.Context, r *Run) error {
	body, err := r.storage.Get(ctx, runv1.StorageKeyPrompt)
	if errors.Is(err, ErrNotFound) {
		return fail(runv1.ExitStorage, "PromptMissing",
			"%s is not in storage: the backend did not write it, or this bundle points at the wrong prefix",
			runv1.StorageKeyPrompt)
	}
	if err != nil {
		return err
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != r.cfg.PromptSHA256 {
		return fail(runv1.ExitConfig, "PromptDigestMismatch",
			"%s hashes to %s and the controller said %s; refusing to run something other than "+
				"what was admitted", runv1.StorageKeyPrompt, got, r.cfg.PromptSHA256)
	}
	r.prompt = body

	// Into the entrypoint's private directory, not the work tree. A prompt the
	// agent can rewrite is a prompt the next attempt cannot trust.
	path := filepath.Join(r.layout.RunPrivate, "prompt.txt")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", path)
	}
	r.logf("prompt fetched: %d bytes, sha256 %s", len(body), got)
	return nil
}

// phaseCheckpoint reads the previous attempt's state. A 404 is the normal
// answer on a first attempt and is a skip, not a failure.
func phaseCheckpoint(ctx context.Context, r *Run) error {
	prior, note, err := LoadCheckpoint(ctx, r.storage)
	if err != nil {
		return err
	}
	if prior == nil {
		return skip("%s", note)
	}
	r.prior = prior

	// Carried across attempts even when the run is not resumed: numbering the
	// chunks from zero again would overwrite the previous attempt's log, and
	// the reader would see two runs spliced into one without a seam.
	if prior.Log != nil {
		r.checkpoint.Log.NextChunk = prior.Log.NextChunk
		r.checkpoint.Log.BytesUploaded = prior.Log.BytesUploaded
	}

	resumable, why := prior.Resume(r.cfg)
	if !resumable {
		r.logf("not resuming: %s", why)
		return nil
	}
	if err := r.verifyPriorArtifacts(ctx, prior); err != nil {
		r.logf("not resuming: %v", err)
		return nil
	}

	r.resumed = true
	r.agent = AgentResult{ExitCode: prior.Agent.exitCode(), SessionID: prior.Agent.sessionID()}
	if prior.Agent != nil {
		r.agent.Usage = prior.Agent.Usage
	}
	r.resultRef = prior.Artifacts.Result
	r.outputRef = prior.Artifacts.Output
	r.checkpoint.Agent = prior.Agent
	r.checkpoint.Artifacts = prior.Artifacts
	r.logf("resuming: %s; the model will not be called again", why)
	return nil
}

// verifyPriorArtifacts checks that what the checkpoint claims is in storage is
// actually there.
//
// It can only check what the bundle lets it read. The two mandatory GET keys
// are the prompt and the checkpoint itself, so a bundle minted to the minimum
// gives no way to confirm the result object — in which case the claim is taken
// at its word and the log says so. Where the backend grants the read, the claim
// is checked, and that is the difference between skipping a paid phase and
// skipping it on evidence.
func (r *Run) verifyPriorArtifacts(ctx context.Context, prior *Checkpoint) error {
	for _, ref := range []*runv1.ObjectRef{prior.Artifacts.Result, prior.Artifacts.Output} {
		key := strings.TrimPrefix(ref.Key, r.secrets.Bundle.KeyPrefix)
		if r.secrets.Bundle.Get[key].URL == "" {
			r.logf("the bundle grants no read of %s; the checkpoint's claim about it is taken on trust", key)
			continue
		}
		if _, err := r.storage.Get(ctx, key); err != nil {
			return errors.New("the checkpoint points at " + ref.Key + " and it is not readable: " + err.Error())
		}
	}
	return nil
}

func (a *CheckpointAgent) exitCode() int32 {
	if a == nil {
		return 0
	}
	return a.ExitCode
}

func (a *CheckpointAgent) sessionID() string {
	if a == nil {
		return ""
	}
	return a.SessionID
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
