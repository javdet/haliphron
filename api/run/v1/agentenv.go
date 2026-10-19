package v1

import (
	"fmt"
	"regexp"
	"strings"
)

// The agent environment a role may add to, under SecretKeyAgentEnv.
//
// The entrypoint builds the agent's environment rather than inheriting the
// pod's, and the names it builds — the model key under each CLI's spelling, the
// gateway, the CLIs' own homes — are what make the run the run the control
// plane leased. A role that could set ANTHROPIC_BASE_URL could send the model
// key to a host of its choosing. So the names below are refused when the role
// is saved, and dropped by the pod if one arrives anyway: both sides, one list,
// by the rule ArtifactKey states.

var agentEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedAgentEnvPrefixes and reservedAgentEnvNames are the names a role may
// not set in the agent's environment.
var (
	reservedAgentEnvPrefixes = []string{
		"ANTHROPIC_", "CLAUDE_", "CODEX_", "OPENAI_", "HALIPHRON_", "GIT_",
	}
	reservedAgentEnvNames = []string{
		"PATH", "HOME", "USER", "SHELL", "TMPDIR", "GH_TOKEN", "GL_TOKEN", "GITHUB_TOKEN",
		"GITLAB_TOKEN", "LD_PRELOAD", "LD_LIBRARY_PATH", "NODE_OPTIONS",
	}
)

// MaxAgentEnvVars bounds how many variables a role adds.
const MaxAgentEnvVars = 64

// AgentEnvName reports why a name may not be added to the agent's
// environment, or nil when it may.
func AgentEnvName(name string) error {
	if len(name) > 128 || !agentEnvNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not an environment variable name", name)
	}
	upper := strings.ToUpper(name)
	for _, reserved := range reservedAgentEnvNames {
		if upper == reserved {
			return fmt.Errorf("%q is set by the entrypoint and cannot be replaced by a role", name)
		}
	}
	for _, prefix := range reservedAgentEnvPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return fmt.Errorf("%q is reserved: names starting %s are set by the entrypoint "+
				"and cannot be replaced by a role", name, prefix)
		}
	}
	return nil
}
