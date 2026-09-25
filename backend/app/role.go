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
}

func parseRole(r store.Role) (*run.Role, error) {
	var spec roleSpec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("decode role %s: %w", r.Name, err)
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
		Plugins:         spec.Plugins,
		SystemPrompt:    spec.SystemPrompt,
	}, nil
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

	return s.store.UpsertRole(ctx, name, spec, by)
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
