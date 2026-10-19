package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// A role's stored spec, decoded.
//
// The wire shape is deliberately close to RenderedRunSpec's own fields: a role
// is a set of defaults for a run, and inventing a second vocabulary for the
// same settings is how "the role says memory 4Gi and the run got 2Gi" becomes
// a support conversation. The parts this build does not understand are ignored
// rather than refused, by the same rule the wire contracts follow, so a role
// written for a newer control plane still runs here.
type roleSpec struct {
	// Description says what the role is for, in words written for whoever is
	// choosing between roles — an agent reading list_roles as much as a person
	// reading the UI. It configures nothing: parseRole does not carry it into
	// the run, and no run behaves differently because of it.
	Description string `json:"description,omitempty"`

	Agent runv1.AgentType `json:"agent,omitempty"`
	Model string          `json:"model,omitempty"`
	Image string          `json:"image,omitempty"`

	PermissionMode runv1.PermissionMode `json:"permissionMode,omitempty"`
	MaxTurns       int32                `json:"maxTurns,omitempty"`
	Env            []runv1.EnvVar       `json:"env,omitempty"`
	Resources      runv1.Resources      `json:"resources,omitempty"`
	MCPServers     []runv1.MCPServer    `json:"mcpServers,omitempty"`

	NodeSelector map[string]runv1.LabelValue `json:"nodeSelector,omitempty"`
	Tolerations  []runv1.Toleration          `json:"tolerations,omitempty"`

	ToolPolicy *runv1.ToolPolicy `json:"toolPolicy,omitempty"`

	// ConfigFiles are the fallback role files: settings.<role>.json and
	// whatever else the entrypoint's resolution chain looks for when the
	// repository does not carry its own. They become a ConfigMap in the
	// cluster, which is why they are here and not in the spec.
	//
	// The keys are flat file names, not paths. They become ConfigMap data keys
	// verbatim and a ConfigMap key may not contain a separator, so a role
	// written with ".claude/settings.json" in it would be accepted here and
	// fail materialisation in the cluster. PutRole refuses such a key rather
	// than rewriting it, by the rule ArtifactKey follows.
	ConfigFiles map[string]string `json:"configFiles,omitempty"`

	ClusterSelector map[string]string `json:"clusterSelector,omitempty"`

	// Plugins are the marketplaces to register and the plugins to install
	// before the agent starts. Material, not spec: they are rendered into the
	// per-run ConfigMap beside ConfigFiles, under RoleConfigKeyPlugins.
	Plugins *runv1.PluginSpec `json:"plugins,omitempty"`

	// SystemPrompt is added to the agent's system prompt — after the CLI's own
	// and after the entrypoint's instruction, never instead of them. Material
	// by the same argument as Plugins, under RoleConfigKeySystemPrompt.
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// SecretEnv adds variables to the agent's own environment from managed
	// secrets — the credential a plugin's MCP server expands from ${NAME}. It
	// is the role's decision to hand the agent that credential; see
	// SecretKeyAgentEnv for why it is an exception and what it may not name.
	SecretEnv []run.SecretEnvVar `json:"secretEnv,omitempty"`
}

func parseRole(r store.Role) (*run.Role, error) {
	var spec roleSpec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("decode role %s: %w", r.Name, err)
	}
	plugins := spec.Plugins
	if !plugins.Declares() {
		// A role may state its plugins as a claude-code settings.json among
		// its files instead. Read leniently here — PutRole read it strictly
		// when it was saved, and a role saved by an older build is not a
		// reason to refuse every run under it.
		if derived, _, err := settingsPlugins(r.Name, spec.ConfigFiles, false); err == nil && derived != nil {
			plugins = derived
		}
	}
	return &run.Role{
		Name:            r.Name,
		Agent:           spec.Agent,
		Model:           spec.Model,
		Image:           spec.Image,
		PermissionMode:  spec.PermissionMode,
		MaxTurns:        spec.MaxTurns,
		Env:             spec.Env,
		Resources:       spec.Resources,
		MCPServers:      spec.MCPServers,
		NodeSelector:    spec.NodeSelector,
		Tolerations:     spec.Tolerations,
		ToolPolicy:      spec.ToolPolicy,
		ConfigFiles:     spec.ConfigFiles,
		ClusterSelector: spec.ClusterSelector,
		Plugins:         plugins,
		SystemPrompt:    spec.SystemPrompt,
		SecretEnv:       spec.SecretEnv,
	}, nil
}

// settingsPlugins reads the plugin half of a role's own settings.json, if the
// role carries one among its files, and names the file it read. It returns a
// nil spec when there is no such file.
func settingsPlugins(role string, files map[string]string, strict bool) (*runv1.PluginSpec, string, error) {
	for _, name := range runv1.RoleSettingsFiles(role) {
		body, ok := files[name]
		if !ok {
			continue
		}
		spec, _, err := runv1.PluginSpecFromSettings([]byte(body), strict)
		if err != nil {
			return nil, name, err
		}
		return spec, name, nil
	}
	return nil, "", nil
}

// MaxRoleDescriptionBytes bounds a role's description.
//
// list_roles returns every role's description at once, into the context of the
// agent deciding which one to start a child under. A description is a sentence
// or two; one that is an essay costs every caller, every time it lists.
const MaxRoleDescriptionBytes = 1 << 10

// RoleDescription reads a stored role's description. A spec that does not
// decode has none, rather than failing the listing it appears in: one
// malformed row must not hide every other role from the caller choosing
// between them.
func RoleDescription(r store.Role) string {
	var spec struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return ""
	}
	return spec.Description
}

// PutRole validates a role's spec and stores it.
//
// It is here rather than in the REST handler for the reason the package comment
// of service.go gives: a role admitted over one listener and a role admitted
// over another must be admitted by the same rules. Today only REST writes
// roles; the moment anything else does, the rules travel with it.
//
// Validation is deliberately narrow. Unknown keys are still ignored — a role
// written for a newer control plane must keep working here — so this checks
// only what this build will later act on and could not report on if it were
// wrong. A mistyped plugin identifier is the case that earns it: nothing
// notices until the lease is cut, an hour later, in a pod, with the run already
// paid for.
func (s *Service) PutRole(ctx context.Context, name string, spec json.RawMessage, by string) (store.Role, error) {
	if len(spec) == 0 || spec[0] != '{' {
		return store.Role{}, &run.InvalidRequestError{Field: "spec", Detail: "a role spec is an object"}
	}

	var parsed roleSpec
	if err := json.Unmarshal(spec, &parsed); err != nil {
		return store.Role{}, &run.InvalidRequestError{Field: "spec", Detail: err.Error()}
	}
	if field, err := runv1.ValidatePluginSpec(parsed.Plugins); err != nil {
		return store.Role{}, &run.InvalidRequestError{Field: field, Detail: err.Error()}
	}
	if n := len(parsed.Description); n > MaxRoleDescriptionBytes {
		return store.Role{}, &run.InvalidRequestError{Field: "description", Detail: fmt.Sprintf(
			"%d bytes is over the %d-byte limit: list_roles puts every description into the caller's context",
			n, MaxRoleDescriptionBytes)}
	}
	if n := len(parsed.SystemPrompt); n > runv1.MaxRoleSystemPromptBytes {
		return store.Role{}, &run.InvalidRequestError{Field: "systemPrompt", Detail: fmt.Sprintf(
			"%d bytes is over the %d-byte limit: the prompt reaches the CLI as a single argument",
			n, runv1.MaxRoleSystemPromptBytes)}
	}
	for key := range parsed.ConfigFiles {
		// A config file key becomes a ConfigMap data key verbatim, and a
		// ConfigMap key may not contain a separator. Refused here rather than
		// sanitised, by the rule ArtifactKey follows: a key silently rewritten
		// is a file the role thinks it shipped and the pod never sees.
		if err := configFileKey(key); err != nil {
			return store.Role{}, &run.InvalidRequestError{Field: "configFiles", Detail: err.Error()}
		}
	}
	if err := validateRoleSettings(name, parsed); err != nil {
		return store.Role{}, err
	}
	if err := validateSecretEnv(parsed.SecretEnv); err != nil {
		return store.Role{}, err
	}
	for i, v := range parsed.SecretEnv {
		// Checked here because here someone is at the keyboard. At lease time
		// a missing secret is a variable the agent does not get, and the run
		// finds out only when its MCP server refuses to start.
		exists, err := s.store.SecretExists(ctx, v.Secret)
		if err != nil {
			return store.Role{}, err
		}
		if !exists {
			return store.Role{}, &run.InvalidRequestError{
				Field:  fmt.Sprintf("secretEnv[%d].secret", i),
				Detail: fmt.Sprintf("no secret named %q; store it under Secrets first", v.Secret)}
		}
	}

	return s.store.UpsertRole(ctx, name, spec, by)
}

// validateRoleSettings reads a role's settings.json strictly, when the role
// states its plugins that way. A marketplace the pod cannot fetch is refused
// now, while the operator is looking, rather than dropped an hour later in a
// pod.
func validateRoleSettings(name string, parsed roleSpec) error {
	if parsed.Plugins.Declares() {
		// plugins wins; a settings.json beside it is only a file in the
		// mount, and the agent is never pointed at it.
		return nil
	}
	if _, file, err := settingsPlugins(name, parsed.ConfigFiles, true); err != nil {
		return &run.InvalidRequestError{Field: "configFiles." + file, Detail: err.Error()}
	}
	return nil
}

// validateSecretEnv checks what a role's secretEnv may name.
func validateSecretEnv(vars []run.SecretEnvVar) error {
	if len(vars) > runv1.MaxAgentEnvVars {
		return &run.InvalidRequestError{Field: "secretEnv", Detail: fmt.Sprintf(
			"%d variables is over the limit of %d", len(vars), runv1.MaxAgentEnvVars)}
	}
	seen := make(map[string]bool, len(vars))
	for i, v := range vars {
		if err := runv1.AgentEnvName(v.Name); err != nil {
			return &run.InvalidRequestError{Field: fmt.Sprintf("secretEnv[%d].name", i), Detail: err.Error()}
		}
		if seen[v.Name] {
			return &run.InvalidRequestError{Field: fmt.Sprintf("secretEnv[%d].name", i),
				Detail: fmt.Sprintf("%q is named twice", v.Name)}
		}
		seen[v.Name] = true
		if v.Secret == "" {
			return &run.InvalidRequestError{Field: fmt.Sprintf("secretEnv[%d].secret", i),
				Detail: "names no secret"}
		}
	}
	return nil
}

// configFileKey admits what a ConfigMap admits.
func configFileKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("the empty string names no file")
	case len(key) > 253:
		return fmt.Errorf("%q is longer than 253 characters", key)
	case key == "." || key == "..":
		return fmt.Errorf("%q names no file", key)
	case !configFileKeyPattern.MatchString(key):
		return fmt.Errorf("%q is not a usable file name: a role's files are mounted flat into %s, "+
			"so a key may hold letters, digits, dots, dashes and underscores but no %q",
			key, runv1.MountRoleConfig, "/")
	}
	return nil
}

var configFileKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
