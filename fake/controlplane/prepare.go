package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Preparing a run is everything the backend and the controller do between a
// request arriving and a pod starting, collapsed into one call: the prompt goes
// into storage, a bundle of presigned capabilities is minted over that run's
// prefix, a callback token is minted and bound to the run, and the whole lot is
// rendered as the environment and the two directories the pod expects to find
// mounted.
//
// The collapse is the point. The image cannot tell which of the two components
// produced any given input, and it must not be able to: everything it receives
// arrives as files and variables, and a harness that reproduced the division of
// labour would be testing the division rather than the image.

// RunRequest describes the run to prepare. Every field has a defensible zero:
// the smallest useful call is Prepare(RunRequest{Prompt: "say hello"}), which
// is a run with no repository — a legal case that the checklist requires to
// succeed with four phases skipped.
type RunRequest struct {
	// RunID is generated when empty.
	RunID runv1.ULID
	// Attempt defaults to 1. An attempt above 1 is what makes the image read
	// the checkpoint at all.
	Attempt int32

	Agent runv1.AgentType
	Model string
	Role  string

	// Prompt is uploaded to runs/{id}/prompt.txt and its digest is put in
	// HALIPHRON_PROMPT_SHA256.
	Prompt string
	// PromptSHA256 overrides that digest without touching what was uploaded.
	// It exists for one row of the checklist — the prompt did not match, the
	// model was never called — and there is no other way to reach that row.
	PromptSHA256 string

	RepoURL      string
	GitProvider  runv1.GitProvider
	BaseBranch   string
	TargetBranch string
	CreatePR     bool
	CloneDepth   int32

	TimeoutSeconds  int32
	GraceSeconds    int32
	MaxTurns        int32
	PermissionMode  string
	AllowedTools    []string
	DeniedTools     []string
	LogChunkSeconds int32

	// Secrets overrides individual keys of the per-run Secret. Unset keys get
	// placeholders shaped like the real thing; a test that cares what the image
	// does with a git token sets its own and then looks for it, or for its
	// absence, in what was uploaded.
	Secrets map[string]string
	// RoleConfig becomes the per-run ConfigMap. The reserved
	// output.schema.json key is how a node declares a schema for the structured
	// output, which is what makes output.json mandatory.
	RoleConfig map[string]string

	// ContractMajor overrides HALIPHRON_CONTRACT. Set it to one above the
	// image's own to check that the image refuses to start.
	ContractMajor int
	// Env adds or overrides environment variables verbatim, after everything
	// else has been computed. A variable set to the empty string here is
	// removed, which is how "a required variable is missing" is arranged.
	Env map[string]string
}

// Prepared is the input side of one run: what would have been the Secret, the
// ConfigMap and the container's environment.
type Prepared struct {
	RunID   runv1.ULID
	Attempt int32
	Prefix  string
	Bucket  string

	Env        map[string]string
	Secrets    map[string]string
	RoleConfig map[string]string

	Bundle        clusterv1.ArtifactBundle
	CallbackToken string
	CallbackURL   string
}

// Prepare admits a run and returns everything the pod needs to start.
func (c *ControlPlane) Prepare(req RunRequest) *Prepared {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	id := req.RunID
	if id == "" {
		id = c.ids.next(now)
	}
	attempt := req.Attempt
	if attempt < 1 {
		attempt = 1
	}
	prefix := fmt.Sprintf(runv1.StoragePrefixRun, id)

	// The prompt is in storage before the pod exists, exactly as it is in the
	// real system: the backend writes it at admission.
	c.store(prefix+runv1.StorageKeyPrompt, []byte(req.Prompt), "text/plain")
	digest := req.PromptSHA256
	if digest == "" {
		sum := sha256.Sum256([]byte(req.Prompt))
		digest = hex.EncodeToString(sum[:])
	}

	state, existing := c.runs[id]
	if !existing {
		state = &runState{id: id, callbackToken: randomToken()}
		c.runs[id] = state
		c.byToken[state.callbackToken] = id
	}
	state.attempt = attempt
	state.bundle = c.mintBundle(prefix, now)

	secrets := map[string]string{
		runv1.SecretKeyGitToken:      "ghs_fake_token_for_" + string(id),
		runv1.SecretKeyLLMAPIKey:     "sk-fake-key-for-" + string(id),
		runv1.SecretKeyMCPConfig:     `{"mcpServers":{}}`,
		runv1.SecretKeyCallbackToken: state.callbackToken,
	}
	for k, v := range req.Secrets {
		secrets[k] = v
	}
	// presigned.json is assembled last and is not overridable: it is the fake's
	// own capability document, and a test that replaced it would be testing a
	// bundle this control plane will not honour.
	bundleJSON, err := json.Marshal(state.bundle)
	if err != nil {
		panic("controlplane: bundle does not marshal: " + err.Error())
	}
	secrets[runv1.SecretKeyPresigned] = string(bundleJSON)
	state.secrets = secrets

	p := &Prepared{
		RunID:         id,
		Attempt:       attempt,
		Prefix:        prefix,
		Bucket:        c.bucket,
		Secrets:       secrets,
		RoleConfig:    copyMap(req.RoleConfig),
		Bundle:        state.bundle,
		CallbackToken: state.callbackToken,
		CallbackURL:   c.baseURL + "/runtime/v1/completion",
	}
	p.Env = c.renderEnv(req, p, digest)
	c.logf("prepared run %s attempt %d (%d secret keys, %d role files)",
		id, attempt, len(secrets), len(p.RoleConfig))
	return p
}

// Reissue mints a fresh bundle for an existing run, as the controller does
// before any attempt past the first. Without it, the second attempt of a run
// whose first one took most of the TTL inherits links that expire mid-flight —
// which is the failure the contract classifies as 21 rather than 30 precisely
// because the cluster knows how to repair it.
func (c *ControlPlane) Reissue(p *Prepared, attempt int32) *Prepared {
	c.mu.Lock()
	state, ok := c.runs[p.RunID]
	if !ok {
		c.mu.Unlock()
		panic("controlplane: Reissue for a run that was never prepared: " + string(p.RunID))
	}
	state.attempt = attempt
	state.bundle = c.mintBundle(p.Prefix, c.now())
	bundleJSON, err := json.Marshal(state.bundle)
	if err != nil {
		c.mu.Unlock()
		panic("controlplane: bundle does not marshal: " + err.Error())
	}
	next := &Prepared{
		RunID:         p.RunID,
		Attempt:       attempt,
		Prefix:        p.Prefix,
		Bucket:        p.Bucket,
		Secrets:       copyMap(p.Secrets),
		RoleConfig:    copyMap(p.RoleConfig),
		Bundle:        state.bundle,
		CallbackToken: p.CallbackToken,
		CallbackURL:   p.CallbackURL,
	}
	next.Secrets[runv1.SecretKeyPresigned] = string(bundleJSON)
	next.Env = copyMap(p.Env)
	next.Env[runv1.EnvAttempt] = strconv.Itoa(int(attempt))
	c.logf("reissued bundle for run %s attempt %d", p.RunID, attempt)
	c.mu.Unlock()
	return next
}

// mintBundle produces the pod's whole access to storage. The caller holds the
// lock.
//
// The Put keys are the ones known in advance; the Post policy covers the two
// prefixes whose object names are not — log chunks, numbered as the run goes,
// and artifacts, named by the agent. The Get keys are prompt.txt, without which
// there is no task, and state.json, without which an idempotent retry is
// impossible.
func (c *ControlPlane) mintBundle(prefix string, now time.Time) clusterv1.ArtifactBundle {
	expires := now.Add(c.ttl)

	put := map[string]clusterv1.PresignedURL{}
	for _, key := range []string{
		runv1.StorageKeyOutput, runv1.StorageKeyResult, runv1.StorageKeyState,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		put[key] = c.presign("PUT", prefix+key, expires)
	}
	// prompt.txt and state.json are the two the contract makes mandatory:
	// without the first the pod has no task, and without the second an
	// idempotent retry is impossible.
	//
	// result.md and output.json are granted on top of them, and that is a
	// deliberate choice rather than an accident of implementation. The resume
	// rule requires the previous attempt's artifacts to be "present and
	// readable", and with only the two mandatory reads a pod can check presence
	// and nothing else — so the checklist row about a checkpoint that points at
	// a result which is no longer there could not be reached at all. A bundle
	// minted to the bare minimum is still handled: the entrypoint takes the
	// claim on trust and says so.
	get := map[string]clusterv1.PresignedURL{}
	for _, key := range []string{
		runv1.StorageKeyPrompt, runv1.StorageKeyState,
		runv1.StorageKeyResult, runv1.StorageKeyOutput,
	} {
		get[key] = c.presign("GET", prefix+key, expires)
	}
	return clusterv1.ArtifactBundle{
		Bucket:    c.bucket,
		Endpoint:  c.baseURL,
		KeyPrefix: prefix,
		Put:       put,
		Get:       get,
		Post: []clusterv1.PresignedPostPolicy{
			c.presignPost(prefix+runv1.StoragePrefixChunks, expires, DefaultMaxPostBytes),
			c.presignPost(prefix+"artifacts/", expires, DefaultMaxPostBytes),
		},
		ExpiresAt: expires,
	}
}

// renderEnv builds the container environment. Everything here is non-secret,
// and that is a property rather than a coincidence: these variables are visible
// in `kubectl describe pod` and inherited by every child process, the agent
// included. The caller holds the lock.
func (c *ControlPlane) renderEnv(req RunRequest, p *Prepared, promptDigest string) map[string]string {
	major := req.ContractMajor
	if major == 0 {
		major = runv1.ContractMajor
	}
	agent := req.Agent
	if agent == "" {
		agent = runv1.AgentClaudeCode
	}
	model := req.Model
	if model == "" {
		model = "anthropic/claude-opus-5"
	}
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = 900
	}
	grace := req.GraceSeconds
	if grace == 0 {
		grace = 120
	}

	env := map[string]string{
		runv1.EnvContract:    strconv.Itoa(major),
		runv1.EnvRunID:       string(p.RunID),
		runv1.EnvAttempt:     strconv.Itoa(int(p.Attempt)),
		runv1.EnvClusterID:   "FAKECONTROLPLANE000000000",
		runv1.EnvCallbackURL: p.CallbackURL,

		runv1.EnvGraceSeconds:   strconv.Itoa(int(grace)),
		runv1.EnvAgent:          string(agent),
		runv1.EnvModel:          model,
		runv1.EnvTimeoutSeconds: strconv.Itoa(int(timeout)),

		runv1.EnvPromptSHA256:  promptDigest,
		runv1.EnvStorageBucket: c.bucket,
		runv1.EnvStoragePrefix: p.Prefix,
	}
	setIf := func(key, value string) {
		if value != "" {
			env[key] = value
		}
	}
	setIf(runv1.EnvRole, req.Role)
	setIf(runv1.EnvPermissionMode, req.PermissionMode)
	setIf(runv1.EnvAllowedTools, strings.Join(req.AllowedTools, ","))
	setIf(runv1.EnvDeniedTools, strings.Join(req.DeniedTools, ","))
	setIf(runv1.EnvRepoURL, req.RepoURL)
	setIf(runv1.EnvGitProvider, string(req.GitProvider))
	setIf(runv1.EnvBaseBranch, req.BaseBranch)
	setIf(runv1.EnvTargetBranch, req.TargetBranch)
	if req.MaxTurns > 0 {
		env[runv1.EnvMaxTurns] = strconv.Itoa(int(req.MaxTurns))
	}
	if req.CloneDepth > 0 {
		env[runv1.EnvCloneDepth] = strconv.Itoa(int(req.CloneDepth))
	}
	if req.CreatePR {
		env[runv1.EnvCreatePR] = "true"
	}
	if req.LogChunkSeconds > 0 {
		env[runv1.EnvLogChunkSeconds] = strconv.Itoa(int(req.LogChunkSeconds))
	}

	// Applied last and able to remove: a variable set to the empty string is
	// deleted rather than blanked, because "missing" and "present but empty"
	// are different inputs and the image is entitled to treat them differently.
	for k, v := range req.Env {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return env
}

// Layout is where Materialize put things, in the shape a `docker run` command
// needs them.
type Layout struct {
	Root       string
	SecretsDir string
	RoleDir    string
	EnvFile    string
}

// Materialize writes the prepared run to disk: the secret files with the
// permissions the contract gives them, the role files, and an env file docker
// can read. It is the bridge between this package and the one way the image is
// actually verified, which is `docker run`.
//
// The secret files are 0400 and the directory 0500, matching the volume the
// controller mounts. That is not decoration: an image that writes into its own
// secret mount works here and fails in the cluster.
func (p *Prepared) Materialize(root string) (Layout, error) {
	layout := Layout{
		Root:       root,
		SecretsDir: filepath.Join(root, "secrets"),
		RoleDir:    filepath.Join(root, "role"),
		EnvFile:    filepath.Join(root, "run.env"),
	}
	if err := os.MkdirAll(layout.SecretsDir, 0o700); err != nil {
		return layout, fmt.Errorf("secrets dir: %w", err)
	}
	// Materializing twice into one root is the normal way to stage a second
	// attempt, and the mode set at the end of the previous call would stop it.
	if err := os.Chmod(layout.SecretsDir, 0o700); err != nil {
		return layout, fmt.Errorf("secrets dir mode: %w", err)
	}
	if err := os.MkdirAll(layout.RoleDir, 0o755); err != nil {
		return layout, fmt.Errorf("role dir: %w", err)
	}
	for name, value := range p.Secrets {
		path := filepath.Join(layout.SecretsDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return layout, fmt.Errorf("secret %s: %w", name, err)
		}
		// A 0400 file cannot be truncated by its owner on every platform this
		// harness runs on; replacing it is both portable and closer to what a
		// remounted volume does.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return layout, fmt.Errorf("secret %s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(value), 0o400); err != nil {
			return layout, fmt.Errorf("secret %s: %w", name, err)
		}
	}
	for name, value := range p.RoleConfig {
		path := filepath.Join(layout.RoleDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return layout, fmt.Errorf("role file %s: %w", name, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return layout, fmt.Errorf("role file %s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(value), 0o444); err != nil {
			return layout, fmt.Errorf("role file %s: %w", name, err)
		}
	}
	if err := os.WriteFile(layout.EnvFile, []byte(p.EnvFile()), 0o600); err != nil {
		return layout, fmt.Errorf("env file: %w", err)
	}
	// Tightened after writing, or the writes above would need the directory to
	// be writable and 0500 twice.
	if err := os.Chmod(layout.SecretsDir, 0o500); err != nil {
		return layout, fmt.Errorf("secrets dir mode: %w", err)
	}
	return layout, nil
}

// EnvFile renders the environment in docker's --env-file format: KEY=value,
// one per line, no quoting and no interpolation.
//
// Sorted, because a diff between two runs of the harness should show what
// changed rather than what moved.
func (p *Prepared) EnvFile() string {
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		// A value with a newline would produce a second, malformed line.
		// docker does not escape it and neither can we, so it is refused where
		// the cause is visible rather than where the symptom is.
		if strings.ContainsAny(p.Env[k], "\n\r") {
			panic("controlplane: " + k + " contains a newline and cannot go in an env file")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(p.Env[k])
		b.WriteByte('\n')
	}
	return b.String()
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Remove deletes a materialized layout.
//
// os.RemoveAll on the root is not enough and neither is t.TempDir's cleanup:
// the secrets directory is 0500, and unlinking a file inside it needs write
// permission on the directory. root ignores that, so a harness that only ever
// runs in a container passes and the same harness on a laptop fails in
// cleanup, after every assertion has already succeeded. The escape hatch ships
// with the fake so that each track does not have to rediscover it.
func (l Layout) Remove() error {
	if l.Root == "" {
		return nil
	}
	// Every directory, not just SecretsDir: the modes Materialize sets are the
	// mount's, and a mount that grows a second tightened directory should not
	// break the caller's cleanup a release later.
	err := filepath.WalkDir(l.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return os.Chmod(path, 0o700)
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("relaxing %s for removal: %w", l.Root, err)
	}
	return os.RemoveAll(l.Root)
}
