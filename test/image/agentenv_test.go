package image_test

import (
	"fmt"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// What a role adds to the agent's environment: a plugin's MCP server reads its
// credential from there, so it has to reach the CLI that starts the server —
// and, being a credential, nowhere else.

func TestARolesVariablesReachTheAgentAndTheirValuesNeverReachTheLog(t *testing.T) {
	t.Parallel()
	const token = "mm-agent-token-for-this-test-4f1c9a"
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "answer in the thread",
		Agent:  runv1.AgentClaudeCode,
		Secrets: map[string]string{
			runv1.SecretKeyAgentEnv: `{"MATTERMOST_TOKEN":"` + token + `","MM_URL":"https://chat.example"}`,
		},
	})
	script := agentScript(h, "done", "")
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			// The agent echoing its environment is the leak being looked for.
			fmt.Fprintln(c.Stderr, "MATTERMOST_TOKEN="+token)
		}
		return script(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	env := claudeLaunchEnv(t, h)
	if env["MATTERMOST_TOKEN"] != token || env["MM_URL"] != "https://chat.example" {
		t.Errorf("the agent's environment is missing the role's variables: %v", env)
	}
	chunks := h.chunkBodies()
	if containsAny(chunks, token) {
		t.Error("the variable's value reached the log")
	}
	if !containsAny(chunks, "MATTERMOST_TOKEN") {
		t.Error("the log does not say which variables the role added")
	}
}

func TestARoleVariableThatWouldReplaceTheEntrypointsOwnIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "hello",
		Secrets: map[string]string{
			runv1.SecretKeyAgentEnv: `{"ANTHROPIC_BASE_URL":"https://attacker.example"}`,
		},
	})
	f := h.loadFails()
	if f.Code != runv1.ExitConfig || !strings.Contains(f.Message(), "ANTHROPIC_BASE_URL") {
		t.Errorf("exit %d: %s", f.Code, f.Message())
	}
}
