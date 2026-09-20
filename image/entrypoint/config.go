package entrypoint

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Layout is where everything lives. The constants in run/v1 are the contract
// and the defaults here are those constants; the struct exists so that a test
// can put the whole tree under a temporary directory without the entrypoint
// needing a mode that only tests use.
//
// An entrypoint with a test-only switch in it is an entrypoint whose tested
// path and shipped path are not the same path.
type Layout struct {
	Workspace  string
	RunIO      string
	Output     string
	Inputs     string
	Artifacts  string
	RoleConfig string
	Secrets    string
	RunPrivate string
	Home       string
}

// DefaultLayout is the layout inside the image.
func DefaultLayout() Layout {
	return Layout{
		Workspace:  runv1.MountWorkspace,
		RunIO:      runv1.DirRunIO,
		Output:     runv1.FileOutput,
		Inputs:     runv1.DirInputs,
		Artifacts:  runv1.DirArtifacts,
		RoleConfig: runv1.MountRoleConfig,
		Secrets:    runv1.MountSecrets,
		RunPrivate: runv1.DirRunPrivate,
		Home:       runv1.HomeDir,
	}
}

// LayoutUnder reproduces the image's tree beneath a root, preserving the shape
// that matters: DirRunIO inside the work tree, everything the entrypoint owns
// outside it.
func LayoutUnder(root string) Layout {
	under := func(p string) string { return filepath.Join(root, p) }
	return Layout{
		Workspace:  under(runv1.MountWorkspace),
		RunIO:      under(runv1.DirRunIO),
		Output:     under(runv1.FileOutput),
		Inputs:     under(runv1.DirInputs),
		Artifacts:  under(runv1.DirArtifacts),
		RoleConfig: under(runv1.MountRoleConfig),
		Secrets:    under(runv1.MountSecrets),
		RunPrivate: under(runv1.DirRunPrivate),
		Home:       under(runv1.HomeDir),
	}
}

// Config is the non-secret half of what the pod receives: the environment,
// parsed once and validated before anything costs money.
//
// Everything here is visible in `kubectl describe pod` and inherited by every
// child process. That is a property of the contract rather than an accident,
// and it is why the secret half is a separate type read from files.
type Config struct {
	ContractMajor int
	RunID         runv1.ULID
	Attempt       int32
	ClusterID     string
	CallbackURL   string
	Grace         time.Duration

	Agent          runv1.AgentType
	Model          string
	Role           string
	Timeout        time.Duration
	MaxTurns       int
	PermissionMode string
	AllowedTools   []string
	DeniedTools    []string

	RepoURL      string
	GitProvider  runv1.GitProvider
	BaseBranch   string
	TargetBranch string
	CloneDepth   int
	Submodules   bool
	LFS          bool
	CreatePR     bool

	// Prompt is the task, from HALIPHRON_PROMPT, which the controller sourced
	// from one key of the per-run Secret. It is the one value in this struct
	// that is not safe in `kubectl describe pod` — and it is not there, because
	// a secretKeyRef puts nothing in the pod's spec.
	//
	// It is in the environment at all because it is the one value the agent is
	// meant to read. ADR 33 keeps credentials out of the environment because
	// the agent inherits it; nothing is protected by withholding from that
	// process the text describing what it is for.
	Prompt       string
	PromptSHA256 string

	// CompletedPhases is what an earlier attempt of this run got through, from
	// HALIPHRON_COMPLETED_PHASES. Absent on a first attempt, which is a normal
	// answer and not a failure.
	CompletedPhases []runv1.RuntimePhase

	LogChunkInterval time.Duration
	OTLPEndpoint     string
	Traceparent      string

	// ArtifactMode is where results go: through the controller, or straight to
	// an object store. Told rather than inferred, so that a phase which fails
	// can say which path it was taking.
	ArtifactMode  runv1.ArtifactMode
	StoragePrefix string
	ImageVersion  string
}

// HasRepo reports whether this run has a repository at all. A run without one
// is a legal case — four phases are skipped and the run succeeds — and the
// alternative reading, that an empty URL is a misconfiguration, would refuse
// every analysis and review task the system exists to run.
func (c *Config) HasRepo() bool { return c.RepoURL != "" }

// defaultLogChunkInterval is used when the controller did not set one. Short
// enough that a pod killed without warning loses seconds of log rather than
// minutes, long enough that a chatty agent does not turn the run into an upload
// benchmark.
const defaultLogChunkInterval = 10 * time.Second

// LoadConfig parses the environment. It answers with a Failure carrying exit
// 30, because every fault it can find is one a retry would reproduce exactly.
//
// The contract check comes first and on its own: an image a major behind, asked
// to parse variables whose meaning has changed underneath it, produces a
// confident wrong answer. With the check it produces ContractMismatch.
func LoadConfig(env func(string) string) (*Config, error) {
	c := &Config{}

	raw := env(runv1.EnvContract)
	if raw == "" {
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"%s is not set: the controller did not say which contract it expects", runv1.EnvContract)
	}
	major, err := strconv.Atoi(raw)
	if err != nil {
		return nil, failWrap(runv1.ExitConfig, "ContractMismatch", err,
			"%s=%q is not a major version", runv1.EnvContract, raw)
	}
	if major != runv1.ContractMajor {
		return nil, fail(runv1.ExitConfig, "ContractMismatch",
			"the controller expects runtime contract major %d and this image implements %d; "+
				"refusing to start an agent under assumptions nobody holds", major, runv1.ContractMajor)
	}
	c.ContractMajor = major

	var missing []string
	required := func(name string) string {
		v := env(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}

	c.RunID = runv1.ULID(required(runv1.EnvRunID))
	c.CallbackURL = required(runv1.EnvCallbackURL)
	c.Model = required(runv1.EnvModel)
	c.PromptSHA256 = required(runv1.EnvPromptSHA256)
	// Required, and its absence is the clearest possible message. A Secret
	// without the prompt key, or a controller that built the container without
	// the reference, produces an agent with no task; saying so at validate
	// costs nothing, and discovering it at the run phase costs a model call
	// against an empty string.
	c.Prompt = required(runv1.EnvPrompt)
	agent := required(runv1.EnvAgent)
	attempt := required(runv1.EnvAttempt)
	timeout := required(runv1.EnvTimeoutSeconds)
	grace := required(runv1.EnvGraceSeconds)

	if len(missing) > 0 {
		// Reported together. A user who fixes one variable, waits for a pod and
		// learns about the next has been made to pay for our convenience.
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"required variables are not set: %s", strings.Join(missing, ", "))
	}

	switch runv1.AgentType(agent) {
	case runv1.AgentClaudeCode, runv1.AgentCodex:
		c.Agent = runv1.AgentType(agent)
	default:
		return nil, fail(runv1.ExitConfig, "UnknownAgent",
			"%s=%q: this image implements %s and %s",
			runv1.EnvAgent, agent, runv1.AgentClaudeCode, runv1.AgentCodex)
	}

	n, err := strconv.Atoi(attempt)
	if err != nil || n < 1 {
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"%s=%q is not an attempt number counting from 1", runv1.EnvAttempt, attempt)
	}
	c.Attempt = int32(n)

	if c.Timeout, err = seconds(runv1.EnvTimeoutSeconds, timeout); err != nil {
		return nil, err
	}
	if c.Grace, err = seconds(runv1.EnvGraceSeconds, grace); err != nil {
		return nil, err
	}

	c.ClusterID = env(runv1.EnvClusterID)
	c.Role = env(runv1.EnvRole)
	c.PermissionMode = env(runv1.EnvPermissionMode)
	c.AllowedTools = splitList(env(runv1.EnvAllowedTools))
	c.DeniedTools = splitList(env(runv1.EnvDeniedTools))
	c.RepoURL = env(runv1.EnvRepoURL)
	c.BaseBranch = env(runv1.EnvBaseBranch)
	c.TargetBranch = env(runv1.EnvTargetBranch)
	c.Submodules = truthy(env(runv1.EnvSubmodules))
	c.LFS = truthy(env(runv1.EnvLFS))
	c.CreatePR = truthy(env(runv1.EnvCreatePR))
	c.OTLPEndpoint = env(runv1.EnvOTLPEndpoint)
	c.Traceparent = env(runv1.EnvTraceparent)
	c.StoragePrefix = env(runv1.EnvStoragePrefix)
	c.CompletedPhases = parseCompletedPhases(env(runv1.EnvCompletedPhases))

	switch mode := runv1.ArtifactMode(env(runv1.EnvArtifactMode)); mode {
	case "", runv1.ArtifactModeRelay:
		// An unset mode is relay. A controller older than this image belongs to
		// an installation that had no other mode, and defaulting to the one
		// that needs no configuration is the only safe direction.
		c.ArtifactMode = runv1.ArtifactModeRelay
	case runv1.ArtifactModeObjectStore:
		c.ArtifactMode = runv1.ArtifactModeObjectStore
	default:
		return nil, fail(runv1.ExitConfig, "UnknownArtifactMode",
			"%s=%q: this image implements %s and %s",
			runv1.EnvArtifactMode, mode, runv1.ArtifactModeRelay, runv1.ArtifactModeObjectStore)
	}
	c.ImageVersion = env(runv1.EnvImageVersion)
	c.MaxTurns = atoiOr(env(runv1.EnvMaxTurns), 0)
	c.CloneDepth = atoiOr(env(runv1.EnvCloneDepth), 0)

	c.LogChunkInterval = defaultLogChunkInterval
	if v := atoiOr(env(runv1.EnvLogChunkSeconds), 0); v > 0 {
		c.LogChunkInterval = time.Duration(v) * time.Second
	}

	provider := runv1.GitProvider(env(runv1.EnvGitProvider))
	switch provider {
	case "", runv1.GitProviderNone:
		c.GitProvider = runv1.GitProviderNone
	case runv1.GitProviderGitHub, runv1.GitProviderGitLab:
		c.GitProvider = provider
	default:
		return nil, fail(runv1.ExitConfig, "UnknownGitProvider",
			"%s=%q: known providers are %s, %s and %s",
			runv1.EnvGitProvider, provider,
			runv1.GitProviderGitHub, runv1.GitProviderGitLab, runv1.GitProviderNone)
	}
	if c.HasRepo() && c.GitProvider == runv1.GitProviderNone {
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"%s is set but %s is not: nothing can open the pull request the change is supposed to travel in",
			runv1.EnvRepoURL, runv1.EnvGitProvider)
	}
	if c.HasRepo() && c.TargetBranch == "" {
		// The backend generates the branch name deterministically. A name
		// invented in the pod produces a second branch and a second PR on the
		// second attempt, which is exactly the non-idempotency the whole retry
		// path depends on not having.
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"%s is set but %s is not: the branch name is the backend's to generate, not the pod's",
			runv1.EnvRepoURL, runv1.EnvTargetBranch)
	}

	return c, nil
}

// LoadConfigFromEnv reads the process environment.
func LoadConfigFromEnv() (*Config, error) { return LoadConfig(os.Getenv) }

func seconds(name, raw string) (time.Duration, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fail(runv1.ExitConfig, "MissingConfiguration",
			"%s=%q is not a positive number of seconds", name, raw)
	}
	return time.Duration(n) * time.Second, nil
}

func atoiOr(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// truthy accepts what a controller rendering a bool into a string plausibly
// produces. Anything else is false: a tool policy that turns on because someone
// wrote "yes" is a policy that can turn on by accident.
func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true
	default:
		return false
	}
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Secrets is the material that arrives as files rather than variables.
//
// As a volume and not envFrom, for two independent reasons either of which
// would suffice. Mechanically, not one of the five keys is a valid environment
// variable name, and envFrom skips such keys silently — leaving a pod with no
// git token and no links to storage, whose first intelligible complaint is
// "could not download the prompt". Substantively, variables are inherited by
// every child process, and the agent is the one process here explicitly assumed
// capable of exfiltrating whatever it can read.
type Secrets struct {
	GitToken  string
	LLMAPIKey string
	MCPConfig []byte
	// Bundle is object-store mode only, and nil in relay mode. A pointer rather
	// than a zero value so that "there is no bundle" and "there is an empty
	// bundle" are different states: the second is a defect and would otherwise
	// be indistinguishable from the default mode.
	Bundle        *clusterv1.ArtifactBundle
	CallbackToken string
}

// LoadSecrets reads the mount. A missing file is exit 30 naming the file: the
// alternative — a permission error three phases later, or a 403 from storage —
// sends the operator looking in the wrong place.
func LoadSecrets(dir string, c *Config) (*Secrets, error) {
	s := &Secrets{}

	read := func(key string, required bool) ([]byte, error) {
		path := filepath.Join(dir, key)
		body, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !required {
				return nil, nil
			}
			return nil, fail(runv1.ExitConfig, "MissingSecret",
				"%s is not in the secret mount; the per-run Secret was not created with that key "+
					"or the volume was not mounted", path)
		case err != nil:
			return nil, failWrap(runv1.ExitConfig, "MissingSecret", err, "reading %s", path)
		}
		return body, nil
	}

	// The presigned bundle exists only in object-store mode. Required exactly
	// there, and refused nowhere: in relay mode the pod addresses no object
	// store, and demanding a bundle would make the default mode impossible.
	objectStore := c.ArtifactMode == runv1.ArtifactModeObjectStore
	bundle, err := read(runv1.SecretKeyPresigned, objectStore)
	if err != nil {
		return nil, err
	}
	if len(bundle) > 0 {
		var parsed clusterv1.ArtifactBundle
		if err := json.Unmarshal(bundle, &parsed); err != nil {
			return nil, failWrap(runv1.ExitConfig, "MalformedSecret", err,
				"%s does not parse as an artifact bundle", runv1.SecretKeyPresigned)
		}
		s.Bundle = &parsed
	}

	token, err := read(runv1.SecretKeyCallbackToken, true)
	if err != nil {
		return nil, err
	}
	// Trimmed: a token written by `echo` into a Secret carries a newline, and a
	// bearer header with a newline in it is rejected by net/http before it
	// reaches anything that could explain why.
	s.CallbackToken = strings.TrimSpace(string(token))

	key, err := read(runv1.SecretKeyLLMAPIKey, true)
	if err != nil {
		return nil, err
	}
	s.LLMAPIKey = strings.TrimSpace(string(key))

	// The git token is required exactly when there is a repository. Demanding
	// it otherwise would refuse every run that has nothing to clone.
	gitToken, err := read(runv1.SecretKeyGitToken, c.HasRepo())
	if err != nil {
		return nil, err
	}
	s.GitToken = strings.TrimSpace(string(gitToken))

	if s.MCPConfig, err = read(runv1.SecretKeyMCPConfig, false); err != nil {
		return nil, err
	}
	return s, nil
}

// Values returns every secret string worth redacting. Short values are dropped
// by the redactor itself; this is only the inventory.
//
// The prompt is deliberately not in it. It is the customer's text and not a
// credential, redacting it would empty the log of the one thing that explains
// what the agent was asked to do, and it is going into result.md anyway.
func (s *Secrets) Values() []string {
	values := []string{s.GitToken, s.LLMAPIKey, s.CallbackToken}
	if s.Bundle == nil {
		return values
	}
	for _, link := range s.Bundle.Put {
		values = append(values, signatureOf(link.URL))
	}
	for _, policy := range s.Bundle.Post {
		values = append(values, policy.Fields["signature"], policy.Fields["policy"])
	}
	return values
}

// signatureOf extracts the signature from a presigned URL. The whole URL is not
// a good redaction target — it contains the key, which is exactly what a log
// line about an upload should say — but the signature is a bearer capability
// and has no business surviving in a thirty-day log.
func signatureOf(rawURL string) string {
	_, query, found := strings.Cut(rawURL, "?")
	if !found {
		return ""
	}
	for _, pair := range strings.Split(query, "&") {
		if name, value, ok := strings.Cut(pair, "="); ok && (name == "sig" || name == "X-Amz-Signature") {
			return value
		}
	}
	return ""
}
