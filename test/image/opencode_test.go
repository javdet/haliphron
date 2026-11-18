package image_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// opencode, the third runtime. The rows here are the contract's table for it
// (sections 7a and 8): how the CLI is started, where its rules come from, how a
// claude-code tool policy becomes opencode's ordered rules, and how a
// marketplace plugin reaches a CLI that has no marketplaces.

// openCodeEvents is what `opencode run --format json` prints for a turn of two
// steps: a tool call, then the answer.
func openCodeEvents(answer string) string {
	lines := []map[string]any{
		{"type": "step_start", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_1"}},
		{"type": "text", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_1", "type": "text", "text": "Let me look."}},
		{"type": "tool_use", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_1", "tool": "read"}},
		{"type": "step_finish", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_1", "reason": "tool-calls",
			"cost": 0.25, "tokens": map[string]any{"input": 1000, "output": 200, "reasoning": 50,
				"cache": map[string]any{"read": 4000, "write": 300}}}},
		{"type": "step_start", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_2"}},
		{"type": "text", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_2", "type": "text", "text": answer[:len(answer)/2]}},
		{"type": "text", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_2", "type": "text", "text": answer[len(answer)/2:]}},
		{"type": "step_finish", "sessionID": "ses_01OC", "part": map[string]any{"messageID": "msg_2", "reason": "stop",
			"cost": 0.125, "tokens": map[string]any{"input": 2000, "output": 300, "reasoning": 0,
				"cache": map[string]any{"read": 5000, "write": 0}}}},
	}
	var b strings.Builder
	for _, line := range lines {
		body, _ := json.Marshal(line)
		b.Write(body)
		b.WriteByte('\n')
	}
	return b.String()
}

// openCodeScript behaves like a successful opencode: plugin installs succeed,
// `mcp list` shows every named server connected, and the run prints its events.
func openCodeScript(h *harness, connected ...string) func(entrypoint.Command) (entrypoint.CommandResult, error) {
	return gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path != "opencode" || len(c.Args) == 0 {
			return entrypoint.CommandResult{}, nil
		}
		switch c.Args[0] {
		case "mcp":
			for _, name := range connected {
				fmt.Fprintf(c.Stdout, "✓ %-10s  connected\n", name)
			}
		case "run":
			if c.Stdin != nil {
				_, _ = io.ReadAll(c.Stdin)
			}
			fmt.Fprint(c.Stdout, openCodeEvents("The answer is forty-two."))
		}
		return entrypoint.CommandResult{}, nil
	})
}

func openCodeHarness(t *testing.T, req controlplane.RunRequest) *harness {
	t.Helper()
	req.Agent = runv1.AgentOpenCode
	if req.Prompt == "" {
		req.Prompt = "do the thing"
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if _, set := req.Env[runv1.EnvRole]; !set {
		req.Env[runv1.EnvRole] = "coder"
	}
	h := newHarness(t, req)
	h.commander.handle = openCodeScript(h)
	return h
}

// openCodeLaunch is the one `opencode run` the run made.
func openCodeLaunch(t *testing.T, h *harness) entrypoint.Command {
	t.Helper()
	var found []entrypoint.Command
	for _, c := range h.commander.invocations("opencode") {
		if len(c.Args) > 0 && c.Args[0] == "run" {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("opencode run was invoked %d times, want once", len(found))
	}
	return found[0]
}

// launchConfig is the OPENCODE_CONFIG_CONTENT the run was started with.
type launchConfig struct {
	Update       string   `json:"update"`
	DefaultAgent string   `json:"default_agent"`
	Instructions []string `json:"instructions"`
	Agents       map[string]struct {
		Steps       int      `json:"steps"`
		Permissions []ocRule `json:"permissions"`
	} `json:"agents"`
	Providers map[string]struct {
		Env      []string          `json:"env"`
		Package  string            `json:"package"`
		Settings map[string]string `json:"settings"`
		Models   map[string]any    `json:"models"`
	} `json:"providers"`
}

type ocRule struct {
	Action, Resource, Effect string
}

func openCodeLaunchConfig(t *testing.T, c entrypoint.Command) launchConfig {
	t.Helper()
	raw := lastEnv(c.Env, "OPENCODE_CONFIG_CONTENT")
	if raw == "" {
		t.Fatalf("opencode was started without OPENCODE_CONFIG_CONTENT: %v", c.Env)
	}
	var cfg launchConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT does not parse: %v\n%s", err, raw)
	}
	return cfg
}

// globalConfig is the opencode.json the plugins and MCP phases wrote.
func globalConfig(t *testing.T, h *harness) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(h.layout.Home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("reading the global opencode.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the global opencode.json does not parse: %v\n%s", err, body)
	}
	return doc
}

// The rules opencode 2.x gives its agents before any configuration: build and
// general start from "everything", explore from a read-only list that does
// include the shell and the web. A run's rules are appended to these.
var (
	openCodeBaseRules    = []ocRule{{"*", "*", "allow"}}
	openCodeExploreRules = []ocRule{
		{"*", "*", "deny"}, {"shell", "*", "allow"}, {"grep", "*", "allow"}, {"glob", "*", "allow"},
		{"webfetch", "*", "allow"}, {"websearch", "*", "allow"}, {"read", "*", "allow"},
		{"subagent", "*", "deny"}, {"external_directory", "*", "allow"},
	}
)

// evaluate is opencode's own rule: the last rule whose action and resource both
// match decides, and no match asks — which `opencode run` refuses.
func evaluate(rules []ocRule, action, resource string) string {
	for i := len(rules) - 1; i >= 0; i-- {
		if wildcard(action, rules[i].Action) && wildcard(resource, rules[i].Resource) {
			return rules[i].Effect
		}
	}
	return "ask"
}

func wildcard(input, pattern string) bool {
	escaped := regexp.QuoteMeta(pattern)
	escaped = strings.ReplaceAll(escaped, `\*`, ".*")
	escaped = strings.ReplaceAll(escaped, `\?`, ".")
	if strings.HasSuffix(escaped, " .*") {
		escaped = strings.TrimSuffix(escaped, " .*") + "( .*)?"
	}
	return regexp.MustCompile("^(?s:" + escaped + ")$").MatchString(input)
}

// ---------------------------------------------------------------------------

func TestOpenCodeIsStartedHeadlessWithThePromptOnStdinAndItsInstructionAppended(t *testing.T) {
	t.Parallel()
	const prompt = "--- chat message --- поехали"
	h := openCodeHarness(t, controlplane.RunRequest{
		Prompt:     prompt,
		MaxTurns:   12,
		Secrets:    map[string]string{runv1.SecretKeyLLMAPIKey: "sk-ant-api03-a-real-looking-key"},
		RoleConfig: map[string]string{runv1.RoleConfigKeySystemPrompt: "Answer in the voice of a lighthouse keeper."},
	})
	var mu sync.Mutex
	var stdin string
	script := openCodeScript(h)
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "opencode" && len(c.Args) > 0 && c.Args[0] == "run" && c.Stdin != nil {
			read, _ := io.ReadAll(c.Stdin)
			mu.Lock()
			stdin = string(read)
			mu.Unlock()
			c.Stdin = nil
		}
		return script(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	launch := openCodeLaunch(t, h)
	for _, pair := range [][2]string{{"--format", "json"}, {"--agent", "build"}, {"--model", "anthropic/claude-opus-5"}} {
		if !containsPair(launch.Args, pair[0], pair[1]) {
			t.Errorf("opencode run was not given %s %s: %q", pair[0], pair[1], launch.Args)
		}
	}
	if !slices.Contains(launch.Args, "--standalone") {
		t.Errorf("opencode run may attach to a shared background service: %q", launch.Args)
	}
	for _, arg := range launch.Args {
		if strings.Contains(arg, "поехали") {
			t.Errorf("the prompt reached the command line: %q", launch.Args)
		}
	}
	mu.Lock()
	got := stdin
	mu.Unlock()
	if got != prompt {
		t.Errorf("stdin was %q, want the prompt alone (the instruction goes in a file)", got)
	}

	cfg := openCodeLaunchConfig(t, launch)
	if cfg.DefaultAgent != "build" || cfg.Update != "disable" {
		t.Errorf("default_agent %q, update %q; want build and disable", cfg.DefaultAgent, cfg.Update)
	}
	if cfg.Agents["build"].Steps != 12 {
		t.Errorf("maxTurns 12 became steps %d on the build agent", cfg.Agents["build"].Steps)
	}
	if len(cfg.Instructions) != 1 {
		t.Fatalf("instructions %v, want the one file the entrypoint wrote", cfg.Instructions)
	}
	body, err := os.ReadFile(cfg.Instructions[0])
	if err != nil {
		t.Fatalf("the instructions file: %v", err)
	}
	text := string(body)
	builtin := strings.Index(text, "You are running as a haliphron agent run")
	role := strings.Index(text, "lighthouse keeper")
	if builtin < 0 || role < 0 || role < builtin {
		t.Errorf("the instruction must hold the entrypoint's text and then the role's:\n%s", text)
	}
	if strings.HasPrefix(cfg.Instructions[0], h.layout.Workspace) {
		t.Errorf("the instructions file is in the work tree, where git would commit it: %s", cfg.Instructions[0])
	}

	// One variable carries the key, and the provider is told its name.
	if lastEnv(launch.Env, "HALIPHRON_LLM_API_KEY") != "sk-ant-api03-a-real-looking-key" {
		t.Error("the model key is not in HALIPHRON_LLM_API_KEY")
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY"} {
		if lastEnv(launch.Env, name) != "" {
			t.Errorf("%s is set; the key belongs in one variable", name)
		}
	}
	if env := cfg.Providers["anthropic"].Env; !slices.Equal(env, []string{"HALIPHRON_LLM_API_KEY"}) {
		t.Errorf("providers.anthropic.env = %v, want [HALIPHRON_LLM_API_KEY]", env)
	}
	if strings.Contains(lastEnv(launch.Env, "OPENCODE_CONFIG_CONTENT"), "sk-ant-api03") {
		t.Error("the key is inlined in OPENCODE_CONFIG_CONTENT")
	}
	if lastEnv(launch.Env, "OPENCODE_DISABLE_AUTOUPDATE") != "1" {
		t.Error("opencode may update itself mid-run")
	}
}

func TestOpenCodesEventsAreNormalisedIntoTheResultAndUsage(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{})
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	report := run.Report()
	if report == nil || report.Usage == nil {
		t.Fatal("no usage was reported")
	}
	u := report.Usage
	if u.SessionID != "ses_01OC" {
		t.Errorf("session %q, want ses_01OC", u.SessionID)
	}
	// Each step reports its own tokens; the run's are the sum.
	if u.NumTurns != 2 || u.InputTokens != 3000 || u.OutputTokens != 550 ||
		u.CacheReadTokens != 9000 || u.CacheWriteTokens != 300 {
		t.Errorf("usage %+v; want 2 turns, 3000 in, 550 out (reasoning included), 9000 cache read, 300 written", u)
	}
	if u.TotalCostUSD != "0.375000" {
		t.Errorf("cost %q, want the sum of the steps, 0.375000", u.TotalCostUSD)
	}
	result := string(h.mustObject(runv1.StorageKeyResult))
	if !strings.Contains(result, "The answer is forty-two.") {
		t.Errorf("the result does not carry the final message:\n%s", result)
	}
	if strings.Contains(result, "Let me look.") {
		t.Errorf("the result carries an earlier step's text rather than the answer alone:\n%s", result)
	}
}

func TestAnOpenCodeErrorEventIsTheReasonTheRunFailed(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{})
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "opencode" && len(c.Args) > 0 && c.Args[0] == "run" {
			fmt.Fprintln(c.Stdout, `{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"invalid x-api-key"}}`)
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		return entrypoint.CommandResult{}, nil
	})
	run, code := h.executeRun()
	if code != runv1.ExitAgentError {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitAgentError, run.Failure())
	}
	if !strings.Contains(run.Failure().Message(), "invalid x-api-key") {
		t.Errorf("the failure %q does not say what opencode said", run.Failure().Message())
	}
}

func TestOpenCodeReachesAGatewayAsAProviderOfItsOwn(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		Model: "anthropic/claude-opus-5",
		Secrets: map[string]string{
			runv1.SecretKeyLLMAPIKey:  "sk-or-v1-an-openrouter-key",
			runv1.SecretKeyLLMBaseURL: "https://openrouter.ai/api",
		},
	})
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	launch := openCodeLaunch(t, h)
	// The gateway routes by the qualified name, so the whole of it is the model.
	if !containsPair(launch.Args, "--model", "haliphron-gateway/anthropic/claude-opus-5") {
		t.Errorf("opencode was not pointed at the gateway's provider: %q", launch.Args)
	}
	gw := openCodeLaunchConfig(t, launch).Providers["haliphron-gateway"]
	if gw.Settings["baseURL"] != "https://openrouter.ai/api/v1" {
		t.Errorf("baseURL %q, want the root plus /v1", gw.Settings["baseURL"])
	}
	if gw.Package != "@opencode/ai/providers/openai-compatible" || !slices.Equal(gw.Env, []string{"HALIPHRON_LLM_API_KEY"}) {
		t.Errorf("the gateway provider is %+v", gw)
	}
	if _, ok := gw.Models["anthropic/claude-opus-5"]; !ok {
		t.Errorf("the model is not declared on the gateway, which has no catalogue: %v", gw.Models)
	}
}

func TestAClaudeOAuthTokenIsRefusedForOpenCodeBeforeTheModel(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		Secrets: map[string]string{runv1.SecretKeyLLMAPIKey: "sk-ant-oat01-a-subscription-token"},
	})
	run, code := h.executeRun()
	if code != runv1.ExitConfig || run.Failure().Reason != "CredentialMismatch" {
		t.Fatalf("exit %d (%v), want %d CredentialMismatch", code, run.Failure(), runv1.ExitConfig)
	}
	for _, c := range h.commander.invocations("opencode") {
		if len(c.Args) > 0 && c.Args[0] == "run" {
			t.Fatal("opencode was launched with a credential that cannot work")
		}
	}
}

// The equivalence the contract demands for every runtime: one role, the same
// powers. claude-code under -p refuses whatever would need approval, so these
// are the decisions it makes for each role, and opencode's rules must make the
// same ones.
func TestOpenCodesRulesGrantWhatClaudeCodeWouldAndNothingMore(t *testing.T) {
	t.Parallel()
	platform := `{"mcpServers":{"` + runv1.PlatformMCPServer + `":{"type":"http","url":"https://mcp.invalid/mcp"},` +
		`"jira":{"command":"jira-mcp"}}}`
	type decision struct {
		action, resource, want string
	}
	cases := []struct {
		name      string
		mode      string
		allowed   []string
		denied    []string
		decisions []decision
		explore   []decision
	}{
		{
			name: "default mode, no lists",
			decisions: []decision{
				{"read", "/workspace/src/main.go", "allow"}, // opencode's own default, untouched
				{"shell", "ls", "deny"},
				{"edit", "/workspace/src/main.go", "deny"},
				{"webfetch", "https://example.com/", "deny"},
				{"jira_search", "*", "deny"},
				{"haliphron_run_agent", "*", "allow"},
				{"external_directory", "/etc/*", "deny"},
				{"subagent", "general", "allow"},
				{"subagent", "a-repository-agent", "deny"},
			},
			// explore allows the shell and the web on its own; the run's
			// rules take them back, and reading stays.
			explore: []decision{
				{"shell", "ls", "deny"},
				{"webfetch", "https://example.com/", "deny"},
				{"read", "/workspace/src/main.go", "allow"},
			},
		},
		{
			name:    "default mode with an allow list and a deny list",
			allowed: []string{"Bash(git:*)", "Edit", "mcp__jira__search", "WebFetch(domain:docs.example.com)"},
			denied:  []string{"Bash(git push:*)", "mcp__haliphron__run_agent"},
			decisions: []decision{
				{"shell", "git status", "allow"},
				{"shell", "git", "allow"},
				{"shell", "git push origin main", "deny"},
				{"shell", "rm -rf /", "deny"},
				{"edit", "/workspace/src/main.go", "allow"},
				{"jira_search", "*", "allow"},
				{"jira_delete", "*", "deny"},
				{"webfetch", "https://docs.example.com/page", "allow"},
				{"webfetch", "https://evil.example.org/", "deny"},
				{"haliphron_run_agent", "*", "deny"},
				{"haliphron_list_roles", "*", "allow"},
			},
			// explore gets the denials and none of the grants.
			explore: []decision{
				{"shell", "git push origin main", "deny"},
				{"shell", "git status", "deny"},
				{"edit", "/workspace/src/main.go", "deny"},
			},
		},
		{
			name: "acceptEdits",
			mode: "acceptEdits",
			decisions: []decision{
				{"edit", "/workspace/src/main.go", "allow"},
				{"shell", "make", "deny"},
			},
			explore: []decision{{"edit", "/workspace/src/main.go", "deny"}},
		},
		{
			name:    "plan mode cannot be widened by the allow list",
			mode:    "plan",
			allowed: []string{"Edit", "Bash", "Read"},
			decisions: []decision{
				{"edit", "/workspace/src/main.go", "deny"},
				{"shell", "ls", "deny"},
				{"read", "/workspace/README.md", "allow"},
			},
		},
		{
			name:   "bypassPermissions, and a deny still wins",
			mode:   "bypassPermissions",
			denied: []string{"Bash(rm:*)", "WebFetch"},
			decisions: []decision{
				{"shell", "make test", "allow"},
				{"edit", "/etc/passwd", "allow"},
				{"jira_search", "*", "allow"},
				{"shell", "rm -rf /workspace", "deny"},
				{"webfetch", "https://example.com/", "deny"},
			},
			explore: []decision{{"shell", "rm -rf /workspace", "deny"}},
		},
		{
			name: "an unknown mode is read as the most restrictive",
			mode: "whatever-comes-next",
			decisions: []decision{
				{"edit", "/workspace/src/main.go", "deny"},
				{"shell", "ls", "deny"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := openCodeHarness(t, controlplane.RunRequest{
				PermissionMode: tc.mode,
				AllowedTools:   tc.allowed,
				DeniedTools:    tc.denied,
				Secrets:        map[string]string{runv1.SecretKeyMCPConfig: platform},
			})
			h.commander.handle = openCodeScript(h, runv1.PlatformMCPServer, "jira")
			run, code := h.executeRun()
			if code != runv1.ExitSuccess {
				t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
			}
			cfg := openCodeLaunchConfig(t, openCodeLaunch(t, h))
			// The rules are rewritten so that /workspace means the run's.
			resource := func(r string) string {
				if strings.HasPrefix(r, "/workspace") {
					return filepath.ToSlash(h.layout.Workspace) + strings.TrimPrefix(r, "/workspace")
				}
				return r
			}
			for _, agent := range []string{"build", "general"} {
				for _, d := range tc.decisions {
					rules := append(slices.Clone(openCodeBaseRules), cfg.Agents[agent].Permissions...)
					if got := evaluate(rules, d.action, resource(d.resource)); got != d.want {
						t.Errorf("%s: %s %s is %s, want %s", agent, d.action, d.resource, got, d.want)
					}
				}
			}
			for _, d := range tc.explore {
				rules := append(slices.Clone(openCodeExploreRules), cfg.Agents["explore"].Permissions...)
				if got := evaluate(rules, d.action, resource(d.resource)); got != d.want {
					t.Errorf("explore: %s %s is %s, want %s", d.action, d.resource, got, d.want)
				}
			}
		})
	}
}

func TestOpenCodesMCPServersKeepTheirSecretsOutOfTheFileAndAreChecked(t *testing.T) {
	t.Parallel()
	const secret = "mcp-header-secret-value"
	h := openCodeHarness(t, controlplane.RunRequest{
		Secrets: map[string]string{runv1.SecretKeyMCPConfig: `{"mcpServers":{"kiss":{"type":"http",` +
			`"url":"https://mcp.invalid/mcp","headers":{"Authorization":"Bearer ` + secret + `"}},` +
			`"jira":{"command":"jira-mcp","args":["--stdio"]}}}`},
	})
	h.commander.handle = openCodeScript(h, "jira", "kiss")
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}

	body, _ := os.ReadFile(filepath.Join(h.layout.Home, ".config", "opencode", "opencode.json"))
	if strings.Contains(string(body), secret) {
		t.Fatalf("the header value is in opencode.json:\n%s", body)
	}
	servers := globalConfig(t, h)["mcp"].(map[string]any)["servers"].(map[string]any)
	kiss := servers["kiss"].(map[string]any)
	if kiss["type"] != "remote" || kiss["oauth"] != false || kiss["codemode"] != false {
		t.Errorf("kiss is %v; want a remote server with oauth and Code Mode off", kiss)
	}
	ref := kiss["headers"].(map[string]any)["Authorization"]
	if ref != "{env:HALIPHRON_MCP_KISS_AUTHORIZATION}" {
		t.Errorf("the header is %v, want a reference to its variable", ref)
	}
	jira := servers["jira"].(map[string]any)
	if fmt.Sprint(jira["command"]) != "[jira-mcp --stdio]" || jira["type"] != "local" {
		t.Errorf("jira is %v; want a local server with its command and arguments", jira)
	}

	launch := openCodeLaunch(t, h)
	if lastEnv(launch.Env, "HALIPHRON_MCP_KISS_AUTHORIZATION") != "Bearer "+secret {
		t.Error("the variable the reference names is not in the agent's environment")
	}
	var listed bool
	for _, c := range h.commander.invocations("opencode") {
		if slices.Equal(c.Args, []string{"mcp", "list", "--standalone"}) {
			listed = true
		}
	}
	if !listed {
		t.Error("the servers were never checked with opencode mcp list --standalone")
	}
}

func TestAnOpenCodeServerThatIsNotConnectedStopsTheRunBeforeTheModel(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		Secrets: map[string]string{runv1.SecretKeyMCPConfig: `{"mcpServers":{"jira":{"command":"jira-mcp"}}}`},
	})
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "opencode" && len(c.Args) > 0 && c.Args[0] == "mcp" {
			// Exit 0, as opencode does, with the server failed.
			fmt.Fprintln(c.Stdout, "✗ jira  failed: spawn jira-mcp ENOENT")
			return entrypoint.CommandResult{}, nil
		}
		if c.Path == "opencode" && len(c.Args) > 0 && c.Args[0] == "run" {
			t.Error("the model was started without the tools it was promised")
		}
		return entrypoint.CommandResult{}, nil
	}
	run, code := h.executeRun()
	if code != runv1.ExitConfig || run.Failure().Reason != "MCPServersUnavailable" {
		t.Fatalf("exit %d (%v), want %d MCPServersUnavailable", code, run.Failure(), runv1.ExitConfig)
	}
}

func TestARepositoryIsKeptOutOfOpenCodesConfigurationOnlyWhenTheRoleSaysSo(t *testing.T) {
	t.Parallel()
	no := false
	for _, tc := range []struct {
		name string
		spec *runv1.PluginSpec
		want string
	}{
		{"silent role", nil, ""},
		{"distrusting role", &runv1.PluginSpec{TrustRepositorySources: &no}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := controlplane.RunRequest{}
			if tc.spec != nil {
				req.RoleConfig = pluginConfig(t, *tc.spec)
			}
			h := openCodeHarness(t, req)
			run, code := h.executeRun()
			if code != runv1.ExitSuccess {
				t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
			}
			if got := lastEnv(openCodeLaunch(t, h).Env, "OPENCODE_DISABLE_PROJECT_CONFIG"); got != tc.want {
				t.Errorf("OPENCODE_DISABLE_PROJECT_CONFIG=%q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Plugins

func TestOpenCodeInstallsItsOwnPackagesAndAFailedOneIsReportedToTheAgent(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			OpenCode: []string{"opencode-wakatime@1.2.0", "@acme/opencode-broken"},
		}),
	})
	script := openCodeScript(h)
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "opencode" && slices.Equal(c.Args, []string{"plugin", "add", "@acme/opencode-broken"}) {
			fmt.Fprintln(c.Stderr, "npm error 404 Not Found")
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		return script(c)
	}
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0: a package that will not install costs that package (%v)", code, run.Failure())
	}
	var added []entrypoint.Command
	for _, c := range h.commander.invocations("opencode") {
		if len(c.Args) > 1 && c.Args[0] == "plugin" && c.Args[1] == "add" {
			added = append(added, c)
		}
	}
	if len(added) != 2 || added[0].Args[2] != "opencode-wakatime@1.2.0" {
		t.Fatalf("opencode plugin add was invoked as %v", added)
	}
	if added[0].Dir != h.layout.Home {
		t.Errorf("the install ran in %s, where a repository's .npmrc could choose the registry", added[0].Dir)
	}
	cfg := openCodeLaunchConfig(t, openCodeLaunch(t, h))
	body, _ := os.ReadFile(cfg.Instructions[0])
	if !strings.Contains(string(body), "@acme/opencode-broken") {
		t.Errorf("the agent was not told the package is missing:\n%s", body)
	}
}

func TestAClaudeCodeRunIgnoresTheRolesOpenCodePackages(t *testing.T) {
	t.Parallel()
	h := pluginRun(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{OpenCode: []string{"opencode-wakatime"}}),
	})
	run, code := h.executeRun()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	if cmds := pluginCommands(h); len(cmds) != 0 {
		t.Errorf("a claude-code run issued plugin commands for opencode's packages: %v", cmds)
	}
	if outcome := phaseOutcome(run, runv1.RuntimePhasePlugins); outcome != runv1.PhaseOutcomeSkipped {
		t.Errorf("the plugins phase ended %q, want skipped", outcome)
	}
}

// catalogueFiles is a claude-code marketplace with one plugin of every part.
var catalogueFiles = map[string]string{
	".claude-plugin/marketplace.json": `{"name":"playneta","plugins":[
		{"name":"infra","source":"./plugins/infra"},
		{"name":"escape","source":"../../outside"},
		{"name":"remote","source":{"source":"git-subdir","url":"https://example.com/x.git","path":"p"}}]}`,
	"plugins/infra/.claude-plugin/plugin.json": `{"name":"infra","version":"1.0.0"}`,
	"plugins/infra/skills/deploy-check/SKILL.md": "---\nname: deploy-check\ndescription: Check a deploy\n---\n" +
		"Run ${CLAUDE_PLUGIN_ROOT}/scripts/check.sh first.\n",
	"plugins/infra/scripts/check.sh": "#!/bin/sh\necho ok\n",
	"plugins/infra/agents/reviewer.md": "---\nname: reviewer\ndescription: >\n  Reviews infrastructure\n  changes.\n" +
		"tools: Read, Grep\nmodel: sonnet\n---\nYou review changes.\n",
	"plugins/infra/commands/rollout.md": "---\ndescription: \"Roll out $1\"\nallowed-tools: Bash(kubectl:*)\n---\nRoll out $ARGUMENTS.\n",
	"plugins/infra/.mcp.json":           `{"mcpServers":{"grafana":{"type":"http","url":"https://grafana.invalid/mcp","headers":{"Authorization":"Bearer ${GRAFANA_TOKEN}"}}}}`,
	"plugins/infra/hooks/hooks.json":    `{"hooks":{}}`,
}

// cloneCatalogue makes the clone of the marketplace leave catalogueFiles where
// the entrypoint asked it to.
func cloneCatalogue(t *testing.T, inner func(entrypoint.Command) (entrypoint.CommandResult, error)) func(entrypoint.Command) (entrypoint.CommandResult, error) {
	return func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "git" && gitSubcommand(c.Args) == "clone" {
			dest := c.Args[len(c.Args)-1]
			for rel, body := range catalogueFiles {
				path := filepath.Join(dest, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Errorf("staging the catalogue: %v", err)
				}
				mode := os.FileMode(0o644)
				if strings.HasSuffix(rel, ".sh") {
					mode = 0o755
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Errorf("staging the catalogue: %v", err)
				}
			}
			_ = os.MkdirAll(filepath.Join(dest, ".git"), 0o755)
			return entrypoint.CommandResult{}, nil
		}
		return inner(c)
	}
}

func TestAMarketplacePluginIsImportedIntoOpenCodesOwnPlaces(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"infra@playneta"},
		}),
	})
	h.commander.handle = cloneCatalogue(t, openCodeScript(h, "plugin_infra_grafana"))
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	for _, c := range h.commander.invocations("opencode") {
		if len(c.Args) > 0 && c.Args[0] == "plugin" {
			t.Errorf("a marketplace plugin was handed to opencode's package installer: %q", c.Args)
		}
	}

	root := filepath.Join(h.layout.Home, ".local", "share", "haliphron", "plugins", "playneta", "infra")
	doc := globalConfig(t, h)

	// Skills: the copied directory, with the plugin root pointing at the copy.
	if skills := fmt.Sprint(doc["skills"]); !strings.Contains(skills, filepath.Join(root, "skills")) {
		t.Errorf("skills %s do not include the plugin's skills directory", skills)
	}
	skill, err := os.ReadFile(filepath.Join(root, "skills", "deploy-check", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), filepath.ToSlash(root)+"/scripts/check.sh") ||
		strings.Contains(string(skill), "${CLAUDE_PLUGIN_ROOT}") {
		t.Errorf("the skill was not copied with its plugin root resolved (%v):\n%s", err, skill)
	}
	if info, err := os.Stat(filepath.Join(root, "scripts", "check.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Errorf("the skill's script lost its execute bit (%v)", err)
	}

	// Agents: a subagent named <plugin>/<name>, with the description kept and
	// claude-code's tool list and model alias left behind.
	agent, err := os.ReadFile(filepath.Join(h.layout.Home, ".config", "opencode", "agents", "infra", "reviewer.md"))
	if err != nil {
		t.Fatalf("the agent was not written: %v", err)
	}
	for _, want := range []string{`description: "Reviews infrastructure changes."`, "mode: subagent", "You review changes."} {
		if !strings.Contains(string(agent), want) {
			t.Errorf("the agent lacks %q:\n%s", want, agent)
		}
	}
	for _, unwanted := range []string{"tools:", "model:"} {
		if strings.Contains(string(agent), unwanted) {
			t.Errorf("the agent kept claude-code's %s:\n%s", unwanted, agent)
		}
	}

	// Commands.
	command, err := os.ReadFile(filepath.Join(h.layout.Home, ".config", "opencode", "commands", "infra", "rollout.md"))
	if err != nil || !strings.Contains(string(command), "Roll out $ARGUMENTS.") ||
		!strings.Contains(string(command), `description: "Roll out $1"`) || strings.Contains(string(command), "allowed-tools") {
		t.Errorf("the command was not translated (%v):\n%s", err, command)
	}

	// MCP: claude-code's name for a plugin's server, with ${VAR} as {env:VAR}.
	servers := doc["mcp"].(map[string]any)["servers"].(map[string]any)
	grafana, ok := servers["plugin_infra_grafana"].(map[string]any)
	if !ok {
		t.Fatalf("the plugin's server is not configured: %v", servers)
	}
	if got := grafana["headers"].(map[string]any)["Authorization"]; got != "Bearer {env:GRAFANA_TOKEN}" {
		t.Errorf("the header is %v, want the role's variable referenced as opencode spells it", got)
	}

	// The launch names the plugin's agent, so its rules are attached and it may
	// be handed work, and denies its server's tools until the policy allows them.
	cfg := openCodeLaunchConfig(t, openCodeLaunch(t, h))
	if _, ok := cfg.Agents["infra/reviewer"]; !ok {
		t.Errorf("the plugin's agent has no rules attached: %v", cfg.Agents)
	}
	if got := evaluate(cfg.Agents["build"].Permissions, "subagent", "infra/reviewer"); got != "allow" {
		t.Errorf("the plugin's agent may not be handed work: %s", got)
	}
	if got := evaluate(cfg.Agents["build"].Permissions, "plugin_infra_grafana_query", "*"); got != "deny" {
		t.Errorf("the plugin's MCP tools are %s in the default mode, want deny as claude-code", got)
	}
	if !containsAny(h.chunkBodies(), "does not run claude-code hooks") {
		t.Error("the run log does not say the plugin's hooks were left out")
	}
}

func TestAMarketplacePluginThatCannotBeImportedIsReportedAndTheRunGoesOn(t *testing.T) {
	t.Parallel()
	h := openCodeHarness(t, controlplane.RunRequest{
		RoleConfig: pluginConfig(t, runv1.PluginSpec{
			Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
			Enabled:      []string{"escape@playneta", "remote@playneta", "absent@playneta"},
		}),
	})
	h.commander.handle = cloneCatalogue(t, openCodeScript(h))
	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0 (%v)", code, run.Failure())
	}
	if _, err := os.Stat(filepath.Join(h.layout.Home, ".local", "share", "haliphron", "plugins", "playneta", "escape")); err == nil {
		t.Error("a plugin whose source leaves the catalogue was copied")
	}
	cfg := openCodeLaunchConfig(t, openCodeLaunch(t, h))
	body, _ := os.ReadFile(cfg.Instructions[0])
	for _, id := range []string{"escape@playneta", "remote@playneta", "absent@playneta"} {
		if !strings.Contains(string(body), id) {
			t.Errorf("the agent was not told %s is missing:\n%s", id, body)
		}
	}
}
