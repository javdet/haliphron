package entrypoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// openCode is the OpenCode CLI, 2.x.
//
// Three things set it apart from the other two runtimes, and each of them is a
// place where the same role could quietly grant different powers.
//
// It has no permission flags. What a run may do is an ordered list of rules in
// its configuration, the last match wins, and every configuration layer adds to
// the list — the repository's opencode.json included, and it loads after the
// global one. So the rules this file computes travel in OPENCODE_CONFIG_CONTENT,
// the one layer that loads after every project file, and they are attached to
// the agents by name, because an agent's own rules are applied after the
// global list.
//
// It has no way to append to its system prompt. An agent's `system` replaces
// the provider's base prompt, tool instructions included; `instructions` is
// added after it. The entrypoint's instruction and the role's prompt go in an
// instructions file, which is the append claude-code gets with
// --append-system-prompt.
//
// It has no marketplaces. See opencode_plugins.go.

type openCode struct{}

func (openCode) Name() runv1.AgentType { return runv1.AgentOpenCode }

const (
	// openCodeKeyEnv carries the model credential. One name, whatever the
	// provider: the provider's entry in the launch configuration names it in
	// `env`, so the key is in exactly one variable and the provider-native
	// names (ANTHROPIC_API_KEY, OPENAI_API_KEY…) stay unset.
	openCodeKeyEnv = "HALIPHRON_LLM_API_KEY"
	// openCodeGatewayProvider is the provider a gateway is declared as.
	openCodeGatewayProvider = "haliphron-gateway"
	// openCodeGatewayPackage speaks the chat completions API. It is the one
	// every gateway this system has been pointed at offers — OpenRouter,
	// LiteLLM, a corporate proxy — under the root plus /v1.
	openCodeGatewayPackage = "@opencode/ai/providers/openai-compatible"
	// openCodeAgent is the agent a run starts. Named on the command line and
	// as default_agent, so a repository's default_agent cannot swap in an agent
	// of its own with rules of its own.
	openCodeAgent = "build"
)

// openCodeSubagents are the built-in agents the run's agent may hand work to.
// Each gets the run's rules attached by name; an agent outside this list and
// outside what the plugins phase installed is refused as a subagent, because
// nothing attached the rules to it.
var openCodeSubagents = []string{"general", "explore"}

func (r *Run) openCodeConfigDir() string {
	return filepath.Join(r.layout.Home, ".config", "opencode")
}

func (r *Run) openCodeConfigPath() string {
	return filepath.Join(r.openCodeConfigDir(), "opencode.json")
}

func (r *Run) openCodeInstructionsPath() string {
	return filepath.Join(r.layout.RunPrivate, "opencode-instructions.md")
}

// openCodePluginRoot is where imported plugins are copied. Under $HOME, outside
// the work tree, for the reason marketplaces are: a plugin in the work tree is
// a plugin in the pull request.
func (r *Run) openCodePluginRoot() string {
	return filepath.Join(r.layout.Home, ".local", "share", "haliphron", "plugins")
}

func (openCode) Env(r *Run) []string {
	env := append(baseEnv(r),
		// The global configuration directory, made explicit. opencode would
		// find the same one through XDG_CONFIG_HOME; naming it is what makes
		// the plugins phase and the CLI agree on the file without either of
		// them guessing.
		"OPENCODE_CONFIG_DIR="+r.openCodeConfigDir(),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		// A watcher reloads configuration mid-run when a file changes. The
		// configuration is settled before the model starts, and a reload
		// triggered by the agent's own edits is the agent rewriting it.
		"OPENCODE_DISABLE_FILEWATCHER=1",
		openCodeKeyEnv+"="+r.secrets.LLMAPIKey,
	)
	if !r.openCodeTrustsRepository() {
		// The repository's opencode.json and .opencode/ — its plugins, which
		// are JavaScript run inside the CLI, its agents, its MCP servers — are
		// the repository choosing what runs beside the agent, which is what
		// trustRepositorySources: false forbids. The switch also drops the
		// repository's AGENTS.md; that is its documented cost.
		env = append(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
	}
	return env
}

// openCodeTrustsRepository reads the role's switch. A document that cannot be
// read is read as distrust: the plugins phase fails on it anyway, and until it
// does the safer reading wins.
func (r *Run) openCodeTrustsRepository() bool {
	spec, err := r.rolePluginSpec()
	if err != nil {
		return false
	}
	return spec.TrustsRepository()
}

func (openCode) PrepareMCP(r *Run) ([]string, error) {
	servers, env, err := openCodeMCPServers(r.secrets.MCPConfig)
	if err != nil {
		return nil, err
	}
	// Merged into the global file rather than written over it: the plugins
	// phase ran first and recorded its own servers, skills and packages there.
	if err := r.updateOpenCodeConfig(func(doc map[string]any) {
		into := objectAt(objectAt(doc, "mcp"), "servers")
		for name, server := range servers {
			into[name] = server
		}
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// VerifyMCP asks `opencode mcp list`, which connects to every configured
// server. Like claude's, it exits 0 with every server failing, so the verdict
// is read from the rows.
func (openCode) VerifyMCP(r *Run) (*MCPCheck, error) {
	names := make([]string, 0, len(r.mcpServers()))
	for name := range r.mcpServers() {
		names = append(names, name)
	}
	sort.Strings(names)
	return &MCPCheck{
		List: Command{
			Path: "opencode",
			// --standalone: a private server for this one command, never a
			// shared background service that might already be running with
			// some other configuration.
			Args: []string{"mcp", "list", "--standalone"},
			Dir:  r.layout.Workspace,
			Env:  r.agentEnv,
		},
		Down: func(output string) []string { return openCodeServersDown(output, names) },
	}, nil
}

// openCodeServersDown names the servers `opencode mcp list` does not show as
// connected. A row is "<mark> <name><padding>  <status>", and only "✓ … connected"
// means the agent will have the tools; "pending", "failed: …", "needs
// authentication", "disabled" and a missing row all mean it will not.
func openCodeServersDown(output string, names []string) []string {
	connected := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "✓" && fields[2] == "connected" {
			connected[fields[1]] = true
		}
	}
	var down []string
	for _, name := range names {
		if !connected[name] {
			down = append(down, name)
		}
	}
	return down
}

// Launch passes the prompt on stdin, for the reasons claude-code's is: an
// argument is visible in /proc and capped in length, and a prompt opening with
// a dash would be read as an option. `opencode run` reads stdin to the end when
// it is given no message argument, and it is given none.
func (openCode) Launch(r *Run) Command {
	args := []string{"run", "--standalone", "--format", "json", "--agent", openCodeAgent}
	if model := r.openCodeModel(); model != "" {
		args = append(args, "--model", model)
	}
	// Images are attached rather than left for the model to find, as for
	// codex: only an attachment is something the model sees.
	for _, path := range r.imageInputs() {
		args = append(args, "--file", path)
	}

	prompt := string(r.prompt)
	instructions := r.openCodeInstructionsPath()
	if err := os.WriteFile(instructions, []byte(systemPrompt(r)), 0o600); err != nil {
		// Not a reason to lose the run: the same text goes in front of the
		// task instead, the way codex always receives it.
		r.errorf("writing %s: %v; the instruction is prefixed to the prompt instead", instructions, err)
		instructions = ""
		prompt = systemPrompt(r) + "\n\n" + prompt
	}

	env := append(slices.Clone(r.agentEnv), "OPENCODE_CONFIG_CONTENT="+string(r.openCodeLaunchConfig(instructions)))
	return Command{Path: "opencode", Args: args, Dir: r.layout.Workspace, Env: env,
		Stdin: strings.NewReader(prompt)}
}

// openCodeLaunchConfig is the highest configuration layer short of the
// system's own, as JSON. Nothing in it is a secret: the key is referenced by
// the name of its variable.
func (r *Run) openCodeLaunchConfig(instructions string) []byte {
	full, explore := r.openCodePermissions()
	attach := func(rules []openCodeRule) map[string]any { return map[string]any{"permissions": rules} }

	build := attach(full)
	if r.cfg.MaxTurns > 0 {
		// A step is one model call and the tools it asks for, which is what
		// claude-code counts as a turn. On the last one opencode takes the
		// tools away and asks for a summary rather than stopping mid-call.
		build["steps"] = r.cfg.MaxTurns
	}
	agents := map[string]any{openCodeAgent: build, "explore": attach(explore)}
	for _, id := range openCodeSubagents {
		if id != "explore" {
			agents[id] = attach(full)
		}
	}
	for _, id := range r.openCode.agents {
		agents[id] = attach(full)
	}

	doc := map[string]any{
		"update":        "disable",
		"share":         "disabled",
		"snapshots":     false,
		"default_agent": openCodeAgent,
		"agents":        agents,
	}
	if instructions != "" {
		doc["instructions"] = []string{instructions}
	}
	if providers := r.openCodeProviders(); providers != nil {
		doc["providers"] = providers
	}
	body, err := json.Marshal(doc)
	if err != nil {
		// Every value above is a string, a number, a bool or a slice of
		// them; a marshal failure is not an input this can be given.
		panic(fmt.Sprintf("encoding the opencode launch configuration: %v", err))
	}
	return body
}

// openCodeModelParts splits the run's model into opencode's provider and model.
//
// The backend already speaks provider/model, which is opencode's own
// spelling, so a qualified name is passed as written — the qualifier is not
// stripped as it is for the other runtimes, because here it is how the CLI
// picks the provider. Behind a gateway the whole qualified name is the model
// of the gateway's provider: opencode splits at the first slash, and the
// gateway routes by the rest.
func (r *Run) openCodeModelParts() (provider, model string) {
	if r.gateway() != "" {
		return openCodeGatewayProvider, r.cfg.Model
	}
	if p, m, found := strings.Cut(r.cfg.Model, "/"); found {
		return p, m
	}
	return inferredProvider(r.cfg.Model), r.cfg.Model
}

// inferredProvider guesses the provider of a bare model name from the families
// whose names say it. Anything else is passed bare, and opencode refuses it
// with a message naming the model — better than a guess that sends the key to
// the wrong company.
func inferredProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"), strings.Contains(m, "codex"):
		return "openai"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	}
	return ""
}

func (r *Run) openCodeModel() string {
	provider, model := r.openCodeModelParts()
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

// openCodeProviders declares where the key comes from and, behind a gateway,
// the gateway itself.
func (r *Run) openCodeProviders() map[string]any {
	provider, model := r.openCodeModelParts()
	if provider == "" {
		return nil
	}
	entry := map[string]any{"env": []string{openCodeKeyEnv}}
	if provider == openCodeGatewayProvider {
		entry["name"] = "haliphron gateway"
		entry["package"] = openCodeGatewayPackage
		entry["settings"] = map[string]any{"baseURL": r.gateway() + "/v1"}
		// A custom provider has no catalogue: the model has to be declared to
		// be selectable at all.
		entry["models"] = map[string]any{model: map[string]any{}}
	}
	return map[string]any{provider: entry}
}

// ---------------------------------------------------------------------------
// Permissions

// openCodeRule is one entry of opencode's ordered permission list.
type openCodeRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

func allow(action, resource string) openCodeRule { return openCodeRule{action, resource, "allow"} }
func deny(action, resource string) openCodeRule  { return openCodeRule{action, resource, "deny"} }

// openCodePermissions translates the permission mode and the tool policy into
// rules for the run's agent and its general-purpose subagents (full), and for
// the read-only explore subagent (explore).
//
// The model is claude-code under -p, because that is what the contract's table
// is written against: there, a call that would need approval is refused. So
// outside bypassPermissions the shell, the web, edits (unless acceptEdits),
// every MCP server's tools and anything outside the work tree are denied, and
// the role's allow list grants back what it names. The deny list comes last,
// and wins, as --disallowedTools does.
//
// explore gets the denials and none of the grants: it is opencode's read-only
// agent, and the role's allow list must not turn it into a second build agent
// the role never asked for — while the deny list must reach it, or a role that
// denies the shell could have the shell through explore, which allows it.
func (r *Run) openCodePermissions() (full, explore []openCodeRule) {
	mode := r.cfg.PermissionMode
	var grants, denies []openCodeRule
	for _, entry := range r.cfg.AllowedTools {
		rules, ok := claudeToolRules(entry, "allow", r.layout)
		if !ok {
			r.logf("opencode: the allowed tool %q has no opencode equivalent and grants nothing", entry)
			continue
		}
		for _, rule := range rules {
			// In plan mode nothing writes, whatever the list says: claude-code
			// does not let --allowedTools lift plan mode either.
			if mode == "plan" && (rule.Action == "edit" || rule.Action == "shell" || rule.Action == "*") {
				continue
			}
			grants = append(grants, rule)
		}
	}
	for _, entry := range r.cfg.DeniedTools {
		rules, ok := claudeToolRules(entry, "deny", r.layout)
		if !ok {
			r.errorf("opencode: the denied tool %q has no opencode equivalent and cannot be enforced", entry)
			continue
		}
		denies = append(denies, rules...)
	}
	// The platform's own server, always, for the reason claudeAllowedTools
	// gives. Before the deny list, which still wins.
	if _, ok := r.mcpServers()[runv1.PlatformMCPServer]; ok {
		grants = append(grants, allow(openCodeToolName(runv1.PlatformMCPServer)+"_*", "*"))
	}

	if mode == "bypassPermissions" {
		full = append([]openCodeRule{allow("*", "*")}, grants...)
		return append(full, denies...), slices.Clone(denies)
	}

	var restrict []openCodeRule
	restrict = append(restrict,
		deny("shell", "*"),
		deny("webfetch", "*"),
		deny("websearch", "*"),
		// Code Mode runs a program against MCP tools under one action of its
		// own, which no rule naming a server's tools would catch.
		deny("execute", "*"),
		deny("external_directory", "*"),
		// The plugins the phase copied in are outside the work tree, and a
		// skill reads its own reference files from there.
		allow("external_directory", filepath.ToSlash(r.openCodePluginRoot())+"/*"),
	)
	// The extra repositories are outside the work tree as well, and are the
	// run's to change: the same grant the other runtimes get with --add-dir.
	for _, dir := range r.extraRepoDirs() {
		dir = filepath.ToSlash(dir)
		restrict = append(restrict, allow("external_directory", dir), allow("external_directory", dir+"/*"))
	}
	for _, server := range r.openCodeServerNames() {
		restrict = append(restrict, deny(openCodeToolName(server)+"_*", "*"))
	}
	restrict = append(restrict, deny("subagent", "*"))
	for _, id := range append(slices.Clone(openCodeSubagents), r.openCode.agents...) {
		restrict = append(restrict, allow("subagent", id))
	}

	edit := deny("edit", "*")
	if mode == "acceptEdits" {
		edit = allow("edit", "*")
	}
	full = append(append(append(slices.Clone(restrict), edit), grants...), denies...)
	explore = append(append(slices.Clone(restrict), deny("edit", "*")), denies...)
	return full, explore
}

// openCodeServerNames is every MCP server the run knows it configured: the
// role's and the ones the plugins phase imported.
func (r *Run) openCodeServerNames() []string {
	names := make([]string, 0, len(r.mcpServers())+len(r.openCode.servers))
	for name := range r.mcpServers() {
		names = append(names, name)
	}
	names = append(names, r.openCode.servers...)
	sort.Strings(names)
	return slices.Compact(names)
}

// openCodeToolName is a name as opencode spells it in a tool's action: every
// character outside [A-Za-z0-9_-] becomes an underscore.
func openCodeToolName(name string) string {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// claudeToolRules translates one entry of a tool policy — written in
// claude-code's names, which is what the role and the ceiling are written in —
// into opencode rules. ok is false for a tool opencode has no equivalent of.
func claudeToolRules(entry, effect string, layout Layout) (rules []openCodeRule, ok bool) {
	entry = strings.TrimSpace(entry)
	name, spec, hasSpec := strings.Cut(entry, "(")
	if hasSpec {
		if !strings.HasSuffix(spec, ")") {
			return nil, false
		}
		spec = strings.TrimSuffix(spec, ")")
	}
	rule := func(action, resource string) []openCodeRule {
		return []openCodeRule{{Action: action, Resource: resource, Effect: effect}}
	}
	path := func() string {
		if !hasSpec || spec == "" {
			return "*"
		}
		return toolPath(spec, layout)
	}

	switch name {
	case "*":
		return rule("*", "*"), true
	case "Bash":
		if !hasSpec || spec == "" || spec == "*" {
			return rule("shell", "*"), true
		}
		// Bash(git push:*) is a prefix; opencode's "git push *" matches the
		// bare command and the command with arguments, which is the same set.
		if prefix, found := strings.CutSuffix(spec, ":*"); found {
			return rule("shell", prefix+" *"), true
		}
		return rule("shell", spec), true
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return rule("edit", path()), true
	case "Read", "LS", "NotebookRead":
		return rule("read", path()), true
	case "Glob":
		return rule("glob", "*"), true
	case "Grep":
		return rule("grep", "*"), true
	case "WebSearch":
		return rule("websearch", "*"), true
	case "WebFetch":
		domain, found := strings.CutPrefix(spec, "domain:")
		if !hasSpec || !found || domain == "" {
			return rule("webfetch", "*"), true
		}
		if effect == "deny" {
			// Wider than the domain, which is the right way round for a deny.
			return rule("webfetch", "*"+domain+"*"), true
		}
		return append(rule("webfetch", "*://"+domain), rule("webfetch", "*://"+domain+"/*")...), true
	case "Task", "Agent":
		if !hasSpec || spec == "" {
			return rule("subagent", "*"), true
		}
		return rule("subagent", spec), true
	case "Skill":
		if !hasSpec || spec == "" {
			return rule("skill", "*"), true
		}
		return rule("skill", spec), true
	}

	// mcp__server, mcp__server__*, mcp__server__tool.
	if rest, found := strings.CutPrefix(name, "mcp__"); found && !hasSpec {
		server, tool, hasTool := strings.Cut(rest, "__")
		if server == "" {
			return nil, false
		}
		if !hasTool || tool == "" || tool == "*" {
			return rule(openCodeToolName(server)+"_*", "*"), true
		}
		return rule(openCodeToolName(server)+"_"+openCodeToolName(tool), "*"), true
	}
	return nil, false
}

// toolPath makes a claude-code path pattern absolute, which is how opencode
// names the file a tool touches. claude-code reads a relative pattern against
// the working directory, and so does this.
func toolPath(spec string, layout Layout) string {
	switch {
	case strings.HasPrefix(spec, "//"):
		// claude-code's spelling of an absolute path.
		return strings.TrimPrefix(spec, "/")
	case strings.HasPrefix(spec, "/"):
		return spec
	case strings.HasPrefix(spec, "~/"):
		return filepath.ToSlash(filepath.Join(layout.Home, strings.TrimPrefix(spec, "~/")))
	}
	return filepath.ToSlash(filepath.Join(layout.Workspace, strings.TrimPrefix(spec, "./")))
}

// ---------------------------------------------------------------------------
// MCP

// openCodeMCPServers renders the role's servers in opencode's shape, with each
// header value referenced through {env:NAME} and exported under that name.
//
// Tools are listed natively (codemode: false) rather than behind Code Mode's
// single execute tool: the tool policy names tools, and a policy that cannot
// see them cannot allow or deny them. A remote server is never offered OAuth;
// there is nobody in a pod to complete the flow.
func openCodeMCPServers(config []byte) (map[string]any, []string, error) {
	if len(config) == 0 {
		return nil, nil, nil
	}
	var doc struct {
		Servers map[string]claudeMCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, nil, failWrap(runv1.ExitConfig, "MalformedSecret", err,
			"%s does not parse", runv1.SecretKeyMCPConfig)
	}
	servers := map[string]any{}
	var env []string
	for name, server := range doc.Servers {
		rendered, headerEnv := server.openCode(name, func(s string) string { return s }, true)
		servers[name] = rendered
		env = append(env, headerEnv...)
	}
	sort.Strings(env)
	return servers, env, nil
}

// claudeMCPServer is one server in claude-code's .mcp.json shape, which is the
// shape the role's mcp.json and a plugin's .mcp.json are both written in.
type claudeMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// openCode renders the server. rewrite is applied to every string taken from
// the source — a plugin's ${CLAUDE_PLUGIN_ROOT} and ${VAR} references become
// opencode's.
//
// With moveHeaders, header values are moved out to the returned variables and
// the file names them: that is the role's mcp.json, whose headers are the
// secrets themselves. A plugin's .mcp.json is not moved — its headers are
// references to the role's variables already, now spelled {env:NAME}, and a
// reference moved into a variable is a literal opencode never substitutes.
func (s claudeMCPServer) openCode(name string, rewrite func(string) string, moveHeaders bool) (map[string]any, []string) {
	if s.URL != "" || s.Type == "http" || s.Type == "sse" {
		out := map[string]any{"type": "remote", "url": rewrite(s.URL), "oauth": false, "codemode": false}
		var env []string
		if len(s.Headers) > 0 {
			headers := map[string]string{}
			for header, value := range s.Headers {
				if !moveHeaders {
					headers[header] = rewrite(value)
					continue
				}
				variable := envNameFor(name, header)
				env = append(env, variable+"="+rewrite(value))
				// The value is the variable's name: opencode substitutes it
				// when it reads the file, and the file never holds the secret.
				headers[header] = "{env:" + variable + "}"
			}
			out["headers"] = headers
		}
		return out, env
	}
	command := []string{rewrite(s.Command)}
	for _, arg := range s.Args {
		command = append(command, rewrite(arg))
	}
	out := map[string]any{"type": "local", "command": command, "codemode": false}
	if len(s.Env) > 0 {
		environment := map[string]string{}
		for key, value := range s.Env {
			environment[key] = rewrite(value)
		}
		out["environment"] = environment
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Configuration file

// updateOpenCodeConfig reads the global opencode.json, lets update change it,
// and writes it back. Read-modify-write rather than a rendered file, because
// three writers share it — `opencode plugin add`, the plugins phase's imports
// and the MCP phase — and each must keep what the others put there.
func (r *Run) updateOpenCodeConfig(update func(doc map[string]any)) error {
	path := r.openCodeConfigPath()
	doc := map[string]any{}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return failWrap(runv1.ExitConfig, "LayoutUnreadable", err, "reading %s", path)
	case len(bytes.TrimSpace(body)) > 0:
		if err := json.Unmarshal(body, &doc); err != nil {
			return failWrap(runv1.ExitConfig, "LayoutUnreadable", err, "parsing %s", path)
		}
	}
	update(doc)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "encoding %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "creating %s", filepath.Dir(path))
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", path)
	}
	return nil
}

// objectAt returns doc[key] as an object, replacing whatever else is there.
func objectAt(doc map[string]any, key string) map[string]any {
	if existing, ok := doc[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	doc[key] = created
	return created
}

// ---------------------------------------------------------------------------
// Output

// openCodeEvent is one line of `opencode run --format json`.
type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		MessageID string  `json:"messageID"`
		Text      string  `json:"text"`
		Cost      float64 `json:"cost"`
		Tokens    struct {
			Input     float64 `json:"input"`
			Output    float64 `json:"output"`
			Reasoning float64 `json:"reasoning"`
			Cache     struct {
				Read  float64 `json:"read"`
				Write float64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		// 1.x nested the message; read as well, at no cost.
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// Parse keeps the text of the last assistant message that said anything — each
// step of a turn is a message of its own, and the answer is the last one, as
// claude-code's result is — and sums usage over every step, because opencode
// reports each step's own tokens and cost rather than a running total.
func (openCode) Parse(stdout []byte) AgentResult {
	result := AgentResult{Usage: &runv1.Usage{}}
	var order []string
	texts := map[string]*strings.Builder{}
	var cost float64
	recognised := false

	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event openCodeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Type == "" {
			continue
		}
		recognised = true
		if event.SessionID != "" {
			result.SessionID = event.SessionID
			result.Usage.SessionID = event.SessionID
		}
		switch event.Type {
		case "text":
			id := event.Part.MessageID
			b, seen := texts[id]
			if !seen {
				b = &strings.Builder{}
				texts[id] = b
				order = append(order, id)
			}
			// A text event can be the rest of a part already printed, so the
			// pieces are joined as they come.
			b.WriteString(event.Part.Text)
		case "step_finish":
			t := event.Part.Tokens
			result.Usage.NumTurns++
			result.Usage.InputTokens += int64(t.Input)
			result.Usage.OutputTokens += int64(t.Output + t.Reasoning)
			result.Usage.CacheReadTokens += int64(t.Cache.Read)
			result.Usage.CacheWriteTokens += int64(t.Cache.Write)
			cost += event.Part.Cost
		case "error":
			// The last one wins, as for codex: it is the one the CLI gave up on.
			if msg := orDefault(event.Error.Message, event.Error.Data.Message); msg != "" {
				result.Error = msg
			} else if event.Error.Type != "" {
				result.Error = event.Error.Type
			}
		}
	}
	if result.Usage.NumTurns > 0 {
		result.Usage.TotalCostUSD = runv1.MoneyUSD(strconv.FormatFloat(cost, 'f', 6, 64))
	}
	for i := len(order) - 1; i >= 0; i-- {
		if text := strings.TrimSpace(texts[order[i]].String()); text != "" {
			result.Text = text
			return result
		}
	}
	if !recognised || result.Error == "" {
		// Nothing recognisable, or a run that ended saying nothing at all:
		// the raw stream is worth more than an empty result.
		result.Text = string(stdout)
	}
	return result
}
