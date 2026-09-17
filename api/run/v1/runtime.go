package v1

// The runtime contract: what the controller puts into the pod, what the pod
// writes, and where. The constants live in the shared package for the reason
// the Secret keys do — the controller sets these variables, the image reads
// them, and the backend's tests assert on the container the controller builds.
// A typo discovered at runtime looks like "the agent silently had no prompt".

// ContractMajor is the runtime contract this build speaks. The controller sets
// it in EnvContract and the entrypoint compares: an image that predates a
// breaking change exits ExitConfig instead of starting an agent under
// assumptions nobody holds any more.
//
// Major only. Minor versions are additive by definition — a new optional
// variable, a new field in the report — and an image that ignores them behaves
// exactly as it did before.
const (
	ContractMajor   = 1
	ContractVersion = "1.0"
)

// Environment variables the controller sets on the agent container.
//
// Everything here is non-secret and therefore safe in `kubectl describe`. The
// secret material arrives as files under MountSecrets instead: env would put it
// in /proc/self/environ, which every child process inherits — including the
// agent, which is the one process in this system explicitly assumed to be
// capable of exfiltrating what it can read.
const (
	EnvContract  = "HALIPHRON_CONTRACT"
	EnvRunID     = "HALIPHRON_RUN_ID"
	EnvAttempt   = "HALIPHRON_ATTEMPT"
	EnvClusterID = "HALIPHRON_CLUSTER_ID"

	// EnvCallbackURL is cluster-local, which is why it comes from the
	// controller rather than the backend.
	EnvCallbackURL = "HALIPHRON_CALLBACK_URL"

	// EnvGraceSeconds is the pod's terminationGracePeriodSeconds, passed in so
	// the entrypoint can budget its own shutdown instead of guessing how long
	// it has between SIGTERM and SIGKILL.
	EnvGraceSeconds = "HALIPHRON_GRACE_SECONDS"

	EnvAgent          = "HALIPHRON_AGENT"
	EnvModel          = "HALIPHRON_MODEL"
	EnvRole           = "HALIPHRON_ROLE"
	EnvTimeoutSeconds = "HALIPHRON_TIMEOUT_SECONDS"
	EnvMaxTurns       = "HALIPHRON_MAX_TURNS"
	EnvPermissionMode = "HALIPHRON_PERMISSION_MODE"
	EnvAllowedTools   = "HALIPHRON_ALLOWED_TOOLS"
	EnvDeniedTools    = "HALIPHRON_DENIED_TOOLS"

	EnvRepoURL      = "HALIPHRON_REPO_URL"
	EnvGitProvider  = "HALIPHRON_GIT_PROVIDER"
	EnvBaseBranch   = "HALIPHRON_BASE_BRANCH"
	EnvTargetBranch = "HALIPHRON_TARGET_BRANCH"
	EnvCloneDepth   = "HALIPHRON_CLONE_DEPTH"
	EnvSubmodules   = "HALIPHRON_SUBMODULES"
	EnvLFS          = "HALIPHRON_LFS"
	EnvCreatePR     = "HALIPHRON_CREATE_PR"

	// EnvPromptSHA256 is the digest of prompt.txt as the backend posted it. The
	// pod fetches the prompt through a presigned GET and refuses to run on a
	// mismatch: a run that executes something other than what was admitted is
	// worse than a run that does not start.
	EnvPromptSHA256 = "HALIPHRON_PROMPT_SHA256"

	EnvLogChunkSeconds = "HALIPHRON_LOG_CHUNK_SECONDS"
	EnvOTLPEndpoint    = "HALIPHRON_OTLP_ENDPOINT"
	EnvTraceparent     = "HALIPHRON_TRACEPARENT"

	// EnvStorageBucket and EnvStoragePrefix are informational: every access to
	// storage goes through a presigned URL from the bundle, and the pod holds
	// no storage credential. They exist so that a log line can say where a
	// result went without the operator decoding a signed URL.
	EnvStorageBucket = "HALIPHRON_STORAGE_BUCKET"
	EnvStoragePrefix = "HALIPHRON_STORAGE_PREFIX"

	// EnvImageVersion is the image's own tag or digest, baked in at build time
	// rather than set by the controller, and echoed back in the report.
	EnvImageVersion = "HALIPHRON_IMAGE_VERSION"
)

// Filesystem layout inside the pod.
//
// The clone occupies MountWorkspace directly, not a subdirectory: agents write
// paths relative to their working directory, and every extra level makes "the
// file at src/main.go" ambiguous between the repository and the pod.
//
// The cost of that choice is that DirRunIO lives inside the work tree, where
// git would offer it up for commit. The entrypoint writes DirRunIO into
// .git/info/exclude before the agent starts; forgetting that turns the forced
// commit of phase PhaseCommit into a commit of the run's own output.
const (
	MountWorkspace = "/workspace"

	// DirRunIO is the exchange with the agent: what it reads, what it writes.
	DirRunIO = "/workspace/.haliphron"
	// FileOutput is the agent's structured output — the payload only. The
	// envelope around it is assembled by the entrypoint; see OutputEnvelope.
	FileOutput = "/workspace/.haliphron/output.json"
	// DirInputs holds artifacts of previous workflow steps. Empty in v1; the
	// init container that fills it arrives with the workflow engine.
	DirInputs = "/workspace/.haliphron/inputs"
	// DirArtifacts is where the agent puts files it wants kept. The entrypoint
	// uploads the tree under runs/{id}/artifacts/ through the POST policy.
	DirArtifacts = "/workspace/.haliphron/artifacts"

	// MountRoleConfig is the per-run ConfigMap, read-only.
	MountRoleConfig = "/haliphron/role"
	// MountSecrets is the per-run Secret, read-only, mode 0400. A volume and
	// not envFrom: two of its keys are file-shaped (mcp.json, presigned.json)
	// and would not survive the conversion to a variable name at all, and the
	// rest must not be inherited by the agent process.
	MountSecrets = "/haliphron/secrets"

	// DirRunPrivate is the entrypoint's own scratch: the fetched prompt, the
	// rolling log, the checkpoint. Outside the work tree and outside anything
	// the agent is pointed at, so that a prompt injection that gets the agent
	// to rewrite "its instructions" rewrites nothing that is read again.
	DirRunPrivate = "/haliphron/run"

	// HomeDir is writable (emptyDir). Every cache the CLIs would put under a
	// read-only root is redirected here, which is what makes
	// readOnlyRootFilesystem achievable rather than aspirational.
	HomeDir = "/home/agent"
)

// RoleConfigKeyOutputSchema is a reserved key in the per-run ConfigMap: the
// JSON Schema the node declares for its structured output. Reserved rather than
// given a lease field of its own because it is rendered by the backend, mounted
// read-only and consumed once — exactly like the role files it travels with,
// and unlike them only in where the value came from.
//
// A role that ships a file under this name loses it. That is the price of the
// reservation and the reason it is named here rather than agreed informally.
const RoleConfigKeyOutputSchema = "output.schema.json"

// RuntimePhase names one step of the entrypoint. The names are contract, not
// logging: they key the checkpoint, they are the phase field of PhaseTiming,
// and the UI groups a run's timeline by them.
//
// +kubebuilder:validation:Enum=init;validate;fetch;checkpoint;auth;clone;role;mcp-prepare;mcp-verify;run;parse;output;persist;commit;push;pr;finalize;notify
type RuntimePhase string

const (
	// RuntimePhaseInit establishes identity and the writable layout.
	RuntimePhaseInit RuntimePhase = "init"
	// RuntimePhaseValidate checks the configuration before anything costs
	// money. Exits ExitConfig on anything missing.
	RuntimePhaseValidate RuntimePhase = "validate"
	// RuntimePhaseFetch downloads prompt.txt and verifies EnvPromptSHA256.
	RuntimePhaseFetch RuntimePhase = "fetch"
	// RuntimePhaseCheckpoint reads state.json. A 404 here is the normal answer
	// on the first attempt, not a failure.
	RuntimePhaseCheckpoint RuntimePhase = "checkpoint"
	// RuntimePhaseAuth wires the model credential and the git credential
	// helper. The token is never written into .git/config.
	RuntimePhaseAuth  RuntimePhase = "auth"
	RuntimePhaseClone RuntimePhase = "clone"
	// RuntimePhaseRole resolves the role config chain and intersects it with
	// the policy ceiling.
	RuntimePhaseRole RuntimePhase = "role"
	// RuntimePhaseMCPPrepare renders the MCP configuration for the runtime,
	// referencing secrets rather than inlining them.
	RuntimePhaseMCPPrepare RuntimePhase = "mcp-prepare"
	// RuntimePhaseMCPVerify proves the servers came up. An agent missing the
	// tools it was promised does not fail — it cheerfully does the wrong
	// thing, which costs more than an explicit refusal.
	RuntimePhaseMCPVerify RuntimePhase = "mcp-verify"
	// RuntimePhaseRun is the agent CLI itself, under the timeout.
	RuntimePhaseRun RuntimePhase = "run"
	// RuntimePhaseParse normalises the runtime's own output format into
	// result.md and Usage.
	RuntimePhaseParse RuntimePhase = "parse"
	// RuntimePhaseOutput wraps the agent's payload in the envelope and, when
	// the node declared a schema, validates it.
	RuntimePhaseOutput RuntimePhase = "output"
	// RuntimePhasePersist uploads result.md, output.json, the log so far and
	// the checkpoint — before the git phases, not after. Everything from here
	// on can fail and be retried; what the model produced is already durable
	// and is never paid for twice.
	RuntimePhasePersist RuntimePhase = "persist"
	// RuntimePhaseCommit commits what the agent left uncommitted. The agent's
	// own commits are kept as they are.
	RuntimePhaseCommit RuntimePhase = "commit"
	// RuntimePhasePush pushes with --force-with-lease to the deterministic
	// branch the backend named.
	RuntimePhasePush RuntimePhase = "push"
	// RuntimePhasePR creates or updates. On a retry the PR already exists.
	RuntimePhasePR RuntimePhase = "pr"
	// RuntimePhaseFinalize uploads the final log, the completion report and
	// the last checkpoint.
	RuntimePhaseFinalize RuntimePhase = "finalize"
	// RuntimePhaseNotify posts the report to the controller. Last, and
	// deliberately unable to change the exit code: by the time it runs, the
	// result is already in storage.
	RuntimePhaseNotify RuntimePhase = "notify"
)

// RuntimePhases is the execution order. The order is contract because the
// checkpoint's resume rule is "every phase before the first unfinished one is
// done", which is only meaningful against a fixed sequence.
var RuntimePhases = []RuntimePhase{
	RuntimePhaseInit, RuntimePhaseValidate, RuntimePhaseFetch, RuntimePhaseCheckpoint,
	RuntimePhaseAuth, RuntimePhaseClone, RuntimePhaseRole,
	RuntimePhaseMCPPrepare, RuntimePhaseMCPVerify,
	RuntimePhaseRun, RuntimePhaseParse, RuntimePhaseOutput, RuntimePhasePersist,
	RuntimePhaseCommit, RuntimePhasePush, RuntimePhasePR,
	RuntimePhaseFinalize, RuntimePhaseNotify,
}

// PhaseOutcome is how a phase ended. "skipped" is a first-class answer and not
// a synonym for failure: a run without a repository skips four phases, and a
// resumed attempt skips everything up to and including RuntimePhaseRun.
//
// +kubebuilder:validation:Enum=ok;skipped;failed
type PhaseOutcome string

const (
	PhaseOutcomeOK      PhaseOutcome = "ok"
	PhaseOutcomeSkipped PhaseOutcome = "skipped"
	PhaseOutcomeFailed  PhaseOutcome = "failed"
)

// Resumable reports whether a phase recorded as ok in a previous attempt may be
// skipped on this one.
//
// Only RuntimePhaseRun is worth skipping and it is the only one whose replay
// costs money. Everything else is cheap to redo and unsafe to trust: a clone
// from a previous attempt is gone with the pod that made it, and a push
// recorded as done may since have been force-pushed over by a human.
func (p RuntimePhase) Resumable() bool { return p == RuntimePhaseRun }

// ContractEnv enumerates every variable this contract defines. Go cannot
// reflect over constants, so without this list the documented table and the
// constants can drift apart silently — and the table is what the person writing
// the image reads. The contract test compares the two.
//
// Runtime-native variables (ANTHROPIC_*, OPENAI_*, CODEX_HOME) are deliberately
// absent: the entrypoint derives them, and the controller has no business
// knowing that codex calls its key OPENAI_API_KEY.
var ContractEnv = []string{
	EnvContract, EnvRunID, EnvAttempt, EnvClusterID, EnvCallbackURL, EnvGraceSeconds,
	EnvAgent, EnvModel, EnvRole, EnvTimeoutSeconds, EnvMaxTurns, EnvPermissionMode,
	EnvAllowedTools, EnvDeniedTools,
	EnvRepoURL, EnvGitProvider, EnvBaseBranch, EnvTargetBranch,
	EnvCloneDepth, EnvSubmodules, EnvLFS, EnvCreatePR,
	EnvPromptSHA256,
	EnvLogChunkSeconds, EnvOTLPEndpoint, EnvTraceparent,
	EnvStorageBucket, EnvStoragePrefix, EnvImageVersion,
}
