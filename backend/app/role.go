package app

import (
	"encoding/json"
	"fmt"

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

	// ConfigFiles are the fallback role files: .claude/settings.json and
	// whatever else the entrypoint's resolution chain looks for when the
	// repository does not carry its own. They become a ConfigMap in the
	// cluster, which is why they are here and not in the spec.
	ConfigFiles map[string]string `json:"configFiles,omitempty"`

	ClusterSelector map[string]string `json:"clusterSelector,omitempty"`
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
	}, nil
}
