package entrypoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The normative translation table from the contract, as code. Every row of it
// is a place where the same haliphron role must grant the same powers on both
// runtimes; where the two cannot be made to agree, the safer reading wins,
// because a role that is stricter than intended produces a complaint and a role
// that is looser produces an incident.

// baseEnv is what both runtimes need: a writable HOME and every cache
// redirected into it. This is what makes readOnlyRootFilesystem achievable
// rather than aspirational — a CLI that writes to /root or /usr/local/share on
// first run turns the security posture into an aspiration, and it does so
// silently until the day the pod has a read-only root.
func baseEnv(r *Run) []string {
	home := r.layout.Home
	return []string{
		"HOME=" + home,
		"PATH=" + orDefault(os.Getenv("PATH"), "/usr/local/bin:/usr/bin:/bin"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"npm_config_cache=" + filepath.Join(home, ".npm"),
		"TMPDIR=/tmp",
		// A run installs the plugin versions the plugins phase resolved and
		// then keeps them. Without this both CLIs refresh catalogues and
		// upgrade plugins in the background, so the same role could run
		// different code on two attempts of one run — and would reach the
		// network from inside the model's turn, which is the one place this
		// image has no way to report a failure from.
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		// The agent is told which run it is, so its own output can reference it.
		// Nothing here is a credential and nothing here is a capability.
		runv1.EnvRunID + "=" + string(r.cfg.RunID),
		runv1.EnvAttempt + "=" + strconv.Itoa(int(r.cfg.Attempt)),
		"HALIPHRON_OUTPUT_FILE=" + r.layout.Output,
		"HALIPHRON_ARTIFACTS_DIR=" + r.layout.Artifacts,
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// modelID strips the provider qualifier. The backend speaks
// anthropic/claude-opus-5 because it places runs across providers; the CLIs
// each want their own bare identifier.
func modelID(qualified string) string {
	if _, bare, found := strings.Cut(qualified, "/"); found {
		return bare
	}
	return qualified
}

// systemPrompt is the instruction both runtimes get appended. It is the only
// text this image puts in front of a model, and it says the two things the
// model cannot infer: where structured output goes, and that the branch is not
// its to name.
func systemPrompt(r *Run) string {
	var b strings.Builder
	b.WriteString("You are running as a haliphron agent run.\n")
	fmt.Fprintf(&b, "Write any structured output as a single JSON object to %s. ", r.layout.Output)
	b.WriteString("Write only the payload: identifiers, timestamps and status are added around it.\n")
	fmt.Fprintf(&b, "Files you want kept beyond this run go in %s.\n", r.layout.Artifacts)
	if r.cfg.HasRepo() {
		fmt.Fprintf(&b, "Work in %s. The branch %s already exists and is checked out; "+
			"do not create or rename branches, and do not push — that is done for you.\n",
			r.layout.Workspace, r.cfg.TargetBranch)
	}
	if len(r.nodeSchema) > 0 {
		fmt.Fprintf(&b, "The output object must satisfy this JSON Schema:\n%s\n", r.nodeSchema)
	}
	return b.String()
}

// ---------------------------------------------------------------------------

// claudeCode is the Claude Code CLI.
type claudeCode struct{}

func (claudeCode) Name() runv1.AgentType { return runv1.AgentClaudeCode }

func (claudeCode) Env(r *Run) []string {
	env := append(baseEnv(r),
		// ANTHROPIC_AUTH_TOKEN rather than ANTHROPIC_API_KEY: the former is
		// what a gateway in front of the model expects, and the CLI accepts it
		// for a direct key too. One name covers both deployments.
		"ANTHROPIC_AUTH_TOKEN="+r.secrets.LLMAPIKey,
		"ANTHROPIC_API_KEY="+r.secrets.LLMAPIKey,
		"ANTHROPIC_MODEL="+modelID(r.cfg.Model),
		// Into $HOME, or the CLI writes to a read-only root and the pod dies
		// with a permission error nobody expected.
		"CLAUDE_CONFIG_DIR="+filepath.Join(r.layout.Home, ".claude"),
	)
	return env
}

func (claudeCode) PrepareMCP(r *Run) ([]string, error) {
	// claude-code substitutes ${VAR} out of its own environment, so the
	// configuration can name the variables and keep the values out of the file.
	config, env, err := referenceSecrets(r.secrets.MCPConfig, func(name string) string {
		return "${" + name + "}"
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(r.mcpConfigPath(), config, 0o600); err != nil {
		return nil, failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", r.mcpConfigPath())
	}
	return env, nil
}

func (claudeCode) VerifyMCP(r *Run) (Command, bool) {
	return Command{
		Path: "claude",
		Args: []string{"mcp", "list", "--mcp-config", r.mcpConfigPath()},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}, true
}

// AddMarketplace registers a cloned catalogue.
//
// --scope user rather than project: project scope writes into the cloned
// repository's own .claude/settings.json, which would put the marketplace in
// the diff and then in the pull request.
func (claudeCode) AddMarketplace(r *Run, source string) Command {
	return Command{
		Path: "claude",
		Args: []string{"plugin", "marketplace", "add", source, "--scope", "user"},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}
}

func (claudeCode) InstallPlugin(r *Run, id string) Command {
	return Command{
		Path: "claude",
		Args: []string{"plugin", "install", id, "--scope", "user", "--json"},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}
}

func (claudeCode) Launch(r *Run) Command {
	args := []string{
		"-p", string(r.prompt),
		"--output-format", "json",
		"--append-system-prompt", systemPrompt(r),
	}
	if model := modelID(r.cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if r.cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(r.cfg.MaxTurns))
	}
	if r.cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", r.cfg.PermissionMode)
	}
	if len(r.cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(r.cfg.AllowedTools, ","))
	}
	if len(r.cfg.DeniedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(r.cfg.DeniedTools, ","))
	}
	if r.hasMCPServers() {
		args = append(args, "--mcp-config", r.mcpConfigPath())
	}
	return Command{Path: "claude", Args: args, Dir: r.layout.Workspace, Env: r.agentEnv}
}

// claudeResult is the shape of `claude -p --output-format json`.
type claudeResult struct {
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	NumTurns   int32   `json:"num_turns"`
	TotalCost  float64 `json:"total_cost_usd"`
	DurationMs int64   `json:"duration_ms"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (claudeCode) Parse(stdout []byte) AgentResult {
	var parsed claudeResult
	if err := json.Unmarshal(lastJSONObject(stdout), &parsed); err != nil {
		// The CLI printed something this build does not recognise. Falling back
		// to the raw text loses the usage record and keeps the work, which is
		// the right way round: a run reported with no cost is a billing
		// question, and a run reported with no result is a lost run.
		return AgentResult{Text: string(stdout)}
	}
	return AgentResult{
		Text:      parsed.Result,
		SessionID: parsed.SessionID,
		Usage: &runv1.Usage{
			NumTurns: parsed.NumTurns,
			// Money as a decimal string, never a float on the wire: the CLI
			// gives a float and this is the last point at which it can be
			// pinned to six places before it becomes a bill.
			TotalCostUSD:     runv1.MoneyUSD(strconv.FormatFloat(parsed.TotalCost, 'f', 6, 64)),
			InputTokens:      parsed.Usage.InputTokens,
			OutputTokens:     parsed.Usage.OutputTokens,
			CacheReadTokens:  parsed.Usage.CacheReadInputTokens,
			CacheWriteTokens: parsed.Usage.CacheCreationInputTokens,
			SessionID:        parsed.SessionID,
		},
	}
}

// ---------------------------------------------------------------------------

// codex is the codex CLI.
type codex struct{}

func (codex) Name() runv1.AgentType { return runv1.AgentCodex }

func (codex) Env(r *Run) []string {
	return append(baseEnv(r),
		"OPENAI_API_KEY="+r.secrets.LLMAPIKey,
		"OPENAI_MODEL="+modelID(r.cfg.Model),
		"CODEX_HOME="+filepath.Join(r.layout.Home, ".codex"),
	)
}

func (codex) PrepareMCP(r *Run) ([]string, error) {
	// codex reads TOML and resolves header values through env_http_headers,
	// which names the variable rather than carrying the value.
	servers, env, err := codexMCPConfig(r.secrets.MCPConfig)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(r.layout.Home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "creating %s", filepath.Dir(path))
	}
	// Appended, not written. The plugins phase runs before this one and records
	// what it installed in this same file — codex keeps its marketplaces and
	// its enabled plugins in config.toml — so a truncating write here would
	// silently undo the whole of it on every codex run.
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, failWrap(runv1.ExitConfig, "LayoutUnreadable", err, "reading %s", path)
	}
	body := servers
	if len(bytes.TrimSpace(existing)) > 0 {
		body = string(existing) + "\n" + servers
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", path)
	}
	return env, nil
}

func (codex) AddMarketplace(r *Run, source string) Command {
	return Command{
		Path: "codex",
		Args: []string{"plugin", "marketplace", "add", source, "--json"},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}
}

func (codex) InstallPlugin(r *Run, id string) Command {
	return Command{
		Path: "codex",
		Args: []string{"plugin", "add", id, "--json"},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}
}

func (codex) VerifyMCP(r *Run) (Command, bool) {
	return Command{
		Path: "codex",
		Args: []string{"mcp", "list"},
		Dir:  r.layout.Workspace,
		Env:  r.agentEnv,
	}, true
}

// sandboxFor translates a permission mode into codex's sandbox. codex has no
// per-tool switch for its built-ins, so the whole of a role's file and shell
// access is decided here — which is why an unknown mode maps to the most
// restrictive setting rather than to the default.
func sandboxFor(mode string) []string {
	switch mode {
	case "plan":
		return []string{"-s", "read-only"}
	case "acceptEdits":
		return []string{"-s", "workspace-write"}
	case "bypassPermissions":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default:
		return []string{"-s", "read-only"}
	}
}

func (codex) Launch(r *Run) Command {
	args := []string{"exec", "--json"}
	args = append(args, sandboxFor(r.cfg.PermissionMode)...)
	if model := modelID(r.cfg.Model); model != "" {
		args = append(args, "-m", model)
	}
	// codex has no --append-system-prompt: the instruction becomes a prefix of
	// the prompt itself. Same content, different carrier — and the difference is
	// invisible to the role that asked for it, which is the point.
	args = append(args, systemPrompt(r)+"\n\n"+string(r.prompt))
	return Command{Path: "codex", Args: args, Dir: r.layout.Workspace, Env: r.agentEnv}
}

// codexEvent is one line of `codex exec --json`.
type codexEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
	Text      string `json:"text"`
	Usage     struct {
		InputTokens       int64 `json:"input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
	} `json:"usage"`
}

func (codex) Parse(stdout []byte) AgentResult {
	result := AgentResult{Usage: &runv1.Usage{}}
	var text []string

	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
			result.Usage.SessionID = event.SessionID
		}
		switch event.Type {
		case "item.completed", "agent_message", "message":
			if msg := orDefault(event.Message, event.Text); msg != "" {
				text = append(text, msg)
			}
		}
		if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
			// Last one wins: codex reports cumulative usage per turn, so the
			// final event carries the total. Summing would double-count.
			result.Usage.InputTokens = event.Usage.InputTokens
			result.Usage.OutputTokens = event.Usage.OutputTokens
			result.Usage.CacheReadTokens = event.Usage.CachedInputTokens
			result.Usage.NumTurns++
		}
	}
	if len(text) == 0 {
		// Nothing recognisable. Keeping the raw stream is worth more than an
		// empty result: somebody will read it.
		result.Text = string(stdout)
		return result
	}
	result.Text = strings.Join(text, "\n\n")
	return result
}

// ---------------------------------------------------------------------------

// referenceSecrets rewrites an MCP configuration so that header values are
// replaced by references, and returns the variables the child process needs for
// those references to resolve.
//
// The configuration file outlives the pod in log chunks and in dumps. The
// child's environment does not, which is the whole reason for the indirection.
func referenceSecrets(config []byte, reference func(name string) string) ([]byte, []string, error) {
	if len(config) == 0 {
		return config, nil, nil
	}
	var doc struct {
		Servers map[string]map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, nil, failWrap(runv1.ExitConfig, "MalformedSecret", err,
			"%s does not parse", runv1.SecretKeyMCPConfig)
	}

	var env []string
	names := make([]string, 0, len(doc.Servers))
	for name := range doc.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		server := doc.Servers[name]
		raw, ok := server["headers"]
		if !ok {
			continue
		}
		var headers map[string]string
		if err := json.Unmarshal(raw, &headers); err != nil {
			continue
		}
		headerNames := make([]string, 0, len(headers))
		for h := range headers {
			headerNames = append(headerNames, h)
		}
		sort.Strings(headerNames)

		for _, header := range headerNames {
			variable := envNameFor(name, header)
			env = append(env, variable+"="+headers[header])
			headers[header] = reference(variable)
		}
		replaced, err := json.Marshal(headers)
		if err != nil {
			return nil, nil, failWrap(runv1.ExitConfig, "MalformedSecret", err, "rewriting headers of %s", name)
		}
		server["headers"] = replaced
		doc.Servers[name] = server
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, failWrap(runv1.ExitConfig, "MalformedSecret", err, "rendering the MCP configuration")
	}
	return out, env, nil
}

// envNameFor builds a variable name from a server and a header. Deterministic,
// so the rendered configuration is the same on every attempt and a diff between
// two runs means something.
func envNameFor(server, header string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, c := range strings.ToUpper(s) {
			switch {
			case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
				b.WriteRune(c)
			default:
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return "HALIPHRON_MCP_" + clean(server) + "_" + clean(header)
}

// codexMCPConfig renders the servers as TOML with header values referenced
// through env_http_headers.
func codexMCPConfig(config []byte) (string, []string, error) {
	if len(config) == 0 {
		return "", nil, nil
	}
	var doc struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return "", nil, failWrap(runv1.ExitConfig, "MalformedSecret", err,
			"%s does not parse", runv1.SecretKeyMCPConfig)
	}

	names := make([]string, 0, len(doc.Servers))
	for name := range doc.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	var env []string
	for _, name := range names {
		server := doc.Servers[name]
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(name))
		if server.Command != "" {
			fmt.Fprintf(&b, "command = %s\n", tomlString(server.Command))
			if len(server.Args) > 0 {
				quoted := make([]string, len(server.Args))
				for i, a := range server.Args {
					quoted[i] = tomlString(a)
				}
				fmt.Fprintf(&b, "args = [%s]\n", strings.Join(quoted, ", "))
			}
		}
		if server.URL != "" {
			fmt.Fprintf(&b, "url = %s\n", tomlString(server.URL))
		}
		if len(server.Headers) > 0 {
			headers := make([]string, 0, len(server.Headers))
			for h := range server.Headers {
				headers = append(headers, h)
			}
			sort.Strings(headers)

			pairs := make([]string, 0, len(headers))
			for _, h := range headers {
				variable := envNameFor(name, h)
				env = append(env, variable+"="+server.Headers[h])
				pairs = append(pairs, tomlString(h)+" = "+tomlString(variable))
			}
			// The value is the variable's name, not its contents. codex reads
			// the variable at connect time and the file never holds the secret.
			fmt.Fprintf(&b, "env_http_headers = { %s }\n", strings.Join(pairs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String(), env, nil
}

// tomlKey quotes a table key when it is not a bare one.
func tomlKey(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return tomlString(s)
		}
	}
	return s
}

// tomlString quotes a basic TOML string. json.Marshal produces exactly the
// escaping TOML's basic strings use, which is why this is three lines rather
// than an escape table with a bug in it.
func tomlString(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(quoted)
}

// lastJSONObject returns the final top-level JSON object in a stream. The CLIs
// print progress before their summary, and the summary is the last object; a
// parser that took the first would report the usage of the greeting.
func lastJSONObject(stdout []byte) []byte {
	text := strings.TrimSpace(string(stdout))
	if strings.HasPrefix(text, "{") && json.Valid([]byte(text)) {
		return []byte(text)
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && json.Valid([]byte(line)) {
			return []byte(line)
		}
	}
	return nil
}
