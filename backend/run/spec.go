package run

import (
	"fmt"
	"regexp"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Admission: turning a request into a RenderedRunSpec.
//
// The spec is rendered once and handed out byte-for-byte on every lease
// afterwards, including a lease to a different cluster after a reassignment.
// Re-rendering between attempts would let an edited role change what attempt 2
// executes, and attempt 2 would stop being a replay of attempt 1 — the property
// the CRD spends a CEL immutability rule on and the image spends a digest pin
// on. The store enforces it with a trigger; this is the code that must
// therefore get it right the first time.

// SubmitRequest is one admitted run, in the terms a caller uses. The REST body
// and the MCP tool arguments both decode into this, which is what keeps them
// from being two policies.
type SubmitRequest struct {
	Prompt string

	Agent runv1.AgentType
	Model string
	Role  string

	RepoURL      string
	BaseBranch   string
	TargetBranch string
	CreatePR     *bool

	TimeoutSeconds int32
	MaxCostUSD     runv1.MoneyUSD
	MaxTurns       int32
	Priority       int32

	// ParentRunID and Depth are set when the caller is an agent holding a
	// per-run MCP token. They are what make a child run accountable: bound to
	// its parent, checked against the depth limit, charged to the right budget.
	ParentRunID runv1.ULID
	Depth       int16

	CreatedBy  string
	CreatedVia string
}

// Defaults are the installation-wide values admission fills in. They live in
// configuration rather than in the request because a caller that has to name
// an image digest to start a run is a caller that pins an old image forever.
type Defaults struct {
	Image           string
	ImagePullPolicy string
	Model           string
	Agent           runv1.AgentType
	TimeoutSeconds  int32
	TTLSeconds      int32
	MaxInfraRetries int32

	// ToolPolicyCeiling is intersected with the role's policy. A deny here
	// cannot be lifted by a role, which is what makes it a ceiling rather than
	// a default.
	ToolPolicyCeiling *runv1.ToolPolicy

	OTLPEndpoint            string
	LogChunkIntervalSeconds int32

	MaxPromptBytes int
	MaxDepth       int16
}

// Role is a role as admission uses it: the runtime shape it implies, the
// policy it asks for, and the files that become a ConfigMap in the cluster.
//
// It is the parsed form of roles.spec, which is stored whole as jsonb because
// it is a product object whose shape changes with the product. Admission reads
// the parts it understands and ignores the rest, by the same compatibility rule
// the wire contracts follow.
type Role struct {
	Name string

	Agent runv1.AgentType
	Model string
	Image string

	PermissionMode runv1.PermissionMode
	MaxTurns       int32
	Env            []runv1.EnvVar
	Resources      runv1.Resources
	MCPServers     []runv1.MCPServer
	NodeSelector   map[string]runv1.LabelValue
	Tolerations    []runv1.Toleration

	ToolPolicy *runv1.ToolPolicy

	// ConfigFiles become the per-run ConfigMap, keyed by the filename mounted
	// into /haliphron/role/. They travel beside the spec in the lease and never
	// inside it: whatever the controller materialises is not a spec field.
	ConfigFiles map[string]string

	// ClusterSelector restricts placement to clusters carrying these labels.
	ClusterSelector map[string]string
}

var (
	moneyPattern = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)
	// Branch names are generated, never taken from the request when absent,
	// but a caller may name one and this is the shape git will accept without
	// argument.
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
)

// Limits the contract and the schema already state, restated where admission
// can refuse a request instead of the database refusing a write.
const (
	MinTimeoutSeconds = 60
	MaxTimeoutSeconds = 86400
	MaxPromptBytes    = 4 << 20
	MaxDepth          = 8
)

// TargetBranch is the branch name for a run, derived from its identifier.
//
// Generated rather than invented by the agent, and that is a correctness
// decision rather than a naming one: a name the agent chooses is a different
// name on the second attempt, so the retry pushes a second branch and opens a
// second pull request instead of updating the first. Determinism here is half
// of what makes a retry idempotent; create-or-update on the PR is the other.
func TargetBranch(id runv1.ULID) string {
	return "haliphron/run-" + strings.ToLower(string(id))
}

// Render validates a request and produces the spec that will be stored, leased
// and materialised.
func Render(id runv1.ULID, req SubmitRequest, role *Role, def Defaults) (runv1.RenderedRunSpec, error) {
	if err := validate(req, def); err != nil {
		return runv1.RenderedRunSpec{}, err
	}

	spec := runv1.RenderedRunSpec{
		Agent:           pickAgent(req, role, def),
		Model:           pickModel(req, role, def),
		Role:            req.Role,
		Image:           pickImage(role, def),
		ImagePullPolicy: def.ImagePullPolicy,
		Prompt: runv1.ObjectRef{
			// Bucket is filled in by the caller, which is the only component
			// that knows which store it wrote the prompt to.
			Key:         promptKey(id),
			ContentType: "text/plain; charset=utf-8",
		},
		Repo:    renderRepo(id, req),
		Runtime: renderRuntime(req, role, def),
	}

	if policy := intersect(def.ToolPolicyCeiling, roleToolPolicy(role)); policy != nil {
		spec.ToolPolicy = policy
	}
	if req.MaxCostUSD != "" {
		spec.Budget = &runv1.BudgetSpec{MaxCostUSD: req.MaxCostUSD}
	}
	if def.MaxInfraRetries > 0 {
		retries := def.MaxInfraRetries
		spec.Retry = &runv1.RetrySpec{MaxInfraRetries: &retries}
	}
	if def.OTLPEndpoint != "" || def.LogChunkIntervalSeconds > 0 {
		obs := &runv1.ObservabilitySpec{OTLPEndpoint: def.OTLPEndpoint}
		if def.LogChunkIntervalSeconds > 0 {
			interval := def.LogChunkIntervalSeconds
			obs.LogChunkIntervalSeconds = &interval
		}
		spec.Observability = obs
	}
	if def.TTLSeconds > 0 {
		ttl := def.TTLSeconds
		spec.TTLSecondsAfterFinished = &ttl
	}
	return spec, nil
}

func promptKey(id runv1.ULID) string {
	return fmt.Sprintf(runv1.StoragePrefixRun, string(id)) + runv1.StorageKeyPrompt
}

// InvalidRequestError is a request admission refused. It is a distinct type so
// that the REST layer answers 422 and the MCP layer answers a tool error
// without either of them inspecting strings.
type InvalidRequestError struct {
	Field  string
	Detail string
}

func (e *InvalidRequestError) Error() string { return e.Field + ": " + e.Detail }

func invalid(field, format string, args ...any) error {
	return &InvalidRequestError{Field: field, Detail: fmt.Sprintf(format, args...)}
}

func validate(req SubmitRequest, def Defaults) error {
	maxPrompt := def.MaxPromptBytes
	if maxPrompt <= 0 {
		maxPrompt = MaxPromptBytes
	}
	maxDepth := def.MaxDepth
	if maxDepth <= 0 {
		maxDepth = MaxDepth
	}

	switch {
	case strings.TrimSpace(req.Prompt) == "":
		return invalid("prompt", "a run without a prompt has no task")
	case len(req.Prompt) > maxPrompt:
		return invalid("prompt", "%d bytes exceeds the limit of %d", len(req.Prompt), maxPrompt)
	}

	if req.Agent != "" && req.Agent != runv1.AgentClaudeCode && req.Agent != runv1.AgentCodex {
		return invalid("agent", "%q is not a known runtime", req.Agent)
	}
	if req.TimeoutSeconds != 0 && (req.TimeoutSeconds < MinTimeoutSeconds || req.TimeoutSeconds > MaxTimeoutSeconds) {
		return invalid("timeout_seconds", "must be between %d and %d", MinTimeoutSeconds, MaxTimeoutSeconds)
	}
	if req.MaxCostUSD != "" && !moneyPattern.MatchString(string(req.MaxCostUSD)) {
		// The store refuses a negative cost outright; refusing it here is the
		// difference between a 422 naming the field and a 500 naming a domain.
		return invalid("max_cost_usd", "must be a non-negative decimal with at most 6 fractional digits")
	}
	for field, branch := range map[string]string{
		"base_branch": req.BaseBranch, "target_branch": req.TargetBranch,
	} {
		if branch != "" && !branchPattern.MatchString(branch) {
			return invalid(field, "%q is not a usable git branch name", branch)
		}
	}
	if req.RepoURL != "" && len(req.RepoURL) > 2048 {
		return invalid("repo", "url exceeds 2048 characters")
	}
	if req.RepoURL == "" && req.TargetBranch != "" {
		return invalid("target_branch", "a run without a repository has no branch to create")
	}
	if req.Depth > maxDepth {
		// The failure mode of an unbounded chain is spend, not incorrectness,
		// which is exactly why it is refused rather than watched.
		return invalid("depth", "run nesting depth %d exceeds the limit of %d", req.Depth, maxDepth)
	}
	if req.Priority < -1000 || req.Priority > 1000 {
		return invalid("priority", "must be between -1000 and 1000")
	}
	return nil
}

func pickAgent(req SubmitRequest, role *Role, def Defaults) runv1.AgentType {
	switch {
	case req.Agent != "":
		return req.Agent
	case role != nil && role.Agent != "":
		return role.Agent
	case def.Agent != "":
		return def.Agent
	default:
		return runv1.AgentClaudeCode
	}
}

func pickModel(req SubmitRequest, role *Role, def Defaults) string {
	switch {
	case req.Model != "":
		return req.Model
	case role != nil && role.Model != "":
		return role.Model
	default:
		return def.Model
	}
}

func pickImage(role *Role, def Defaults) string {
	if role != nil && role.Image != "" {
		return role.Image
	}
	return def.Image
}

func renderRepo(id runv1.ULID, req SubmitRequest) runv1.RepoSpec {
	if req.RepoURL == "" {
		return runv1.RepoSpec{Provider: runv1.GitProviderNone}
	}
	repo := runv1.RepoSpec{
		URL:          req.RepoURL,
		Provider:     ProviderFor(req.RepoURL),
		BaseBranch:   req.BaseBranch,
		TargetBranch: req.TargetBranch,
		CreatePR:     req.CreatePR,
	}
	if repo.BaseBranch == "" {
		repo.BaseBranch = "main"
	}
	if repo.TargetBranch == "" {
		repo.TargetBranch = TargetBranch(id)
	}
	return repo
}

// ProviderFor infers the hosting API from the clone URL. It is a guess with one
// safe fallback: an unrecognised host gets "none", so the run works in the
// repository and does not try to open a pull request through an API it cannot
// speak.
func ProviderFor(url string) runv1.GitProvider {
	host := strings.ToLower(url)
	switch {
	case strings.Contains(host, "github."):
		return runv1.GitProviderGitHub
	case strings.Contains(host, "gitlab."):
		return runv1.GitProviderGitLab
	default:
		return runv1.GitProviderNone
	}
}

func renderRuntime(req SubmitRequest, role *Role, def Defaults) runv1.RuntimeSpec {
	rt := runv1.RuntimeSpec{TimeoutSeconds: req.TimeoutSeconds}
	if rt.TimeoutSeconds == 0 {
		rt.TimeoutSeconds = def.TimeoutSeconds
	}
	if rt.TimeoutSeconds == 0 {
		rt.TimeoutSeconds = 3600
	}
	if role != nil {
		rt.PermissionMode = role.PermissionMode
		rt.MaxTurns = role.MaxTurns
		rt.Env = role.Env
		rt.Resources = role.Resources
		rt.MCPServers = role.MCPServers
		rt.NodeSelector = role.NodeSelector
		rt.Tolerations = role.Tolerations
	}
	if req.MaxTurns > 0 {
		rt.MaxTurns = req.MaxTurns
	}
	return rt
}

func roleToolPolicy(role *Role) *runv1.ToolPolicy {
	if role == nil {
		return nil
	}
	return role.ToolPolicy
}

// intersect applies the policy ceiling to a role's policy.
//
// The asymmetry is the point. An allow is the intersection — a role cannot
// grant a tool the installation withholds — while a deny is the union, because
// a ceiling that a role could lift by not mentioning a tool is not a ceiling.
func intersect(ceiling, role *runv1.ToolPolicy) *runv1.ToolPolicy {
	switch {
	case ceiling == nil:
		return role
	case role == nil:
		return copyPolicy(ceiling)
	}

	out := &runv1.ToolPolicy{Deny: append(append([]string{}, ceiling.Deny...), role.Deny...)}
	switch {
	case len(ceiling.Allow) == 0:
		out.Allow = append([]string(nil), role.Allow...)
	case len(role.Allow) == 0:
		out.Allow = append([]string(nil), ceiling.Allow...)
	default:
		permitted := make(map[string]bool, len(ceiling.Allow))
		for _, tool := range ceiling.Allow {
			permitted[tool] = true
		}
		for _, tool := range role.Allow {
			if permitted[tool] {
				out.Allow = append(out.Allow, tool)
			}
		}
	}
	return out
}

func copyPolicy(p *runv1.ToolPolicy) *runv1.ToolPolicy {
	if p == nil {
		return nil
	}
	return &runv1.ToolPolicy{
		Allow: append([]string(nil), p.Allow...),
		Deny:  append([]string(nil), p.Deny...),
	}
}
