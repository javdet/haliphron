package mcp

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/restapi"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The tools.
//
// They are the REST commands under different names, which is the point: a run
// started by an agent and a run started by a person are the same run, admitted
// by the same rules, visible in the same list. Where they differ is who the
// caller is, and that difference is carried by the token rather than by the
// arguments — an agent cannot claim to be a different parent than the one its
// token names.

// tool is one definition as tools/list returns it.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func toolDefinitions(c Caller) []tool {
	tools := []tool{
		{
			Name:        "run_agent",
			Title:       "Run an agent",
			Description: "Start one agent run. Returns its run_id immediately, or waits for the result when async is false.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":          map[string]any{"type": "string", "description": "What the agent should do."},
					"agent":           map[string]any{"type": "string", "enum": []string{"claude-code", "codex"}},
					"model":           map[string]any{"type": "string"},
					"role":            map[string]any{"type": "string", "description": "A configured role: its tools, model and files."},
					"repo":            map[string]any{"type": "string", "description": "Clone URL. Omit for a run without a repository."},
					"base_branch":     map[string]any{"type": "string"},
					"timeout_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400},
					"max_cost_usd":    map[string]any{"type": "string", "description": "Ceiling for this run, as a decimal."},
					"async":           map[string]any{"type": "boolean", "description": "Default true. False waits for the run to finish."},
					"wait_seconds":    map[string]any{"type": "integer", "description": "How long to wait when async is false."},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name:        "get_run_result",
			Title:       "Read a run",
			Description: "The state of a run, and its result when it has one.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string"},
					"wait_seconds": map[string]any{
						"type":        "integer",
						"description": "Wait this long for the run to finish before answering.",
					},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "cancel_run",
			Title:       "Cancel a run",
			Description: "Ask a run to stop. Delivery is asynchronous: the instruction reaches the cluster on its next heartbeat.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string"},
					"reason": map[string]any{"type": "string"},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "list_runs",
			Title:       "List runs",
			Description: "Recent runs, most recent first.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"role":   map[string]any{"type": "string"},
					"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				},
			},
		},
		{
			Name:        "list_roles",
			Title:       "List roles",
			Description: "The roles a run may be started under.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}

	if c.Parent != nil {
		// An agent sees what it may do to its own children, and nothing about
		// the platform's configuration. list_clusters exists for an operator
		// without a UI, not for a pod.
		return tools
	}
	return append(tools, tool{
		Name:        "list_clusters",
		Title:       "List clusters",
		Description: "The clusters registered with this control plane and their capacity.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	})
}

// callParams is the tools/call envelope.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolResult is what a tool returns. The structured content is the answer;
// the text block beside it is the same thing rendered, because a model reads
// the text and a program reads the structure, and giving only one of them
// forces the other to guess.
type toolResult struct {
	Content           []contentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) call(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "params must hold name and arguments"}
	}
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	switch params.Name {
	case "run_agent":
		return s.runAgent(r, c, args)
	case "get_run_result":
		return s.getRunResult(r, c, args)
	case "cancel_run":
		return s.cancelRun(r, c, args)
	case "list_runs":
		return s.listRuns(r, c, args)
	case "list_roles":
		return s.listRoles(r)
	case "list_clusters":
		if c.Parent != nil {
			return failed("an agent's token does not carry the platform's configuration"), nil
		}
		return s.listClusters(r)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "no tool " + params.Name}
	}
}

type runAgentArgs struct {
	Prompt string `json:"prompt"`

	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Role  string `json:"role,omitempty"`

	Repo         string `json:"repo,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`

	TimeoutSeconds int32  `json:"timeout_seconds,omitempty"`
	MaxCostUSD     string `json:"max_cost_usd,omitempty"`
	Priority       int32  `json:"priority,omitempty"`

	Async       *bool `json:"async,omitempty"`
	WaitSeconds int32 `json:"wait_seconds,omitempty"`

	// IdempotencyKey lets a caller that retries on a timeout get the first run
	// back instead of a second one. Named in the arguments rather than taken
	// from a header because an MCP client has no headers to speak of.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func (s *Server) runAgent(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	var args runAgentArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	if !c.Token.Allows(store.ScopeRunsWrite) {
		return failed("this token cannot start runs"), nil
	}

	submit := run.SubmitRequest{
		Prompt:         args.Prompt,
		Agent:          runv1.AgentType(args.Agent),
		Model:          args.Model,
		Role:           args.Role,
		RepoURL:        args.Repo,
		BaseBranch:     args.BaseBranch,
		TargetBranch:   args.TargetBranch,
		TimeoutSeconds: args.TimeoutSeconds,
		MaxCostUSD:     runv1.MoneyUSD(args.MaxCostUSD),
		Priority:       args.Priority,
		CreatedBy:      c.Name(),
		CreatedVia:     c.Via(),
	}
	if c.Parent != nil {
		submit.ParentRunID = c.Parent.ID
		submit.Depth = c.Parent.Depth + 1
		if submit.Role == "" {
			submit.Role = c.Parent.Role
		}
		if submit.RepoURL == "" {
			submit.RepoURL = c.Parent.RepoURL
		}
		if submit.MaxCostUSD == "" {
			// A child inherits its parent's ceiling rather than the
			// installation's: an agent that can start children with a fresh
			// budget each is an agent with no budget at all.
			submit.MaxCostUSD = c.Parent.MaxCostUSD
		}
	}

	opts := app.SubmitOptions{Scope: "mcp.run_agent"}
	if args.IdempotencyKey != "" {
		opts.Key = args.IdempotencyKey
		opts.Body = raw
	}

	submitted, err := s.app.Submit(r.Context(), submit, opts)
	if err != nil {
		return toolError(err), nil
	}

	result := submitted.Run
	if args.Async != nil && !*args.Async {
		waited, err := s.app.WaitForResult(r.Context(), result.ID,
			time.Duration(args.WaitSeconds)*time.Second)
		if err != nil {
			return toolError(err), nil
		}
		result = waited
	}

	view := restapi.RunView(result)
	if !submitted.Replayed {
		body, err := json.Marshal(view)
		if err == nil {
			if err := s.app.CompleteSubmission(r.Context(), opts, result.ID, 200, body); err != nil {
				s.log.Error("could not record an idempotent answer", "run", result.ID, "error", err)
			}
		}
	}
	return ok(fmt.Sprintf("run %s is %s", result.ID, result.ReportedStatus()), view), nil
}

type runIDArgs struct {
	RunID       string `json:"run_id"`
	WaitSeconds int32  `json:"wait_seconds,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (s *Server) getRunResult(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	var args runIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	id := runv1.ULID(args.RunID)
	if !run.ValidULID(id) {
		return failed("run_id is not an identifier this system issues"), nil
	}

	item, err := s.app.Run(r.Context(), id)
	if err != nil {
		return toolError(err), nil
	}
	if !s.mayRead(c, item) {
		// A per-run token reaches its own run and the runs it started, and
		// nothing else. Otherwise one compromised agent reads every result in
		// the installation.
		return failed("this token does not reach that run"), nil
	}

	if args.WaitSeconds > 0 {
		waited, err := s.app.WaitForResult(r.Context(), id, time.Duration(args.WaitSeconds)*time.Second)
		if err != nil {
			return toolError(err), nil
		}
		item = waited
	}

	view := restapi.RunView(item)
	text := fmt.Sprintf("run %s is %s", item.ID, item.ReportedStatus())
	if item.ResultSummary != "" {
		text += "\n\n" + item.ResultSummary
	}
	if item.PRURL != "" {
		text += "\n\npull request: " + item.PRURL
	}
	return ok(text, view), nil
}

func (s *Server) cancelRun(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	var args runIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	if !c.Token.Allows(store.ScopeRunsWrite) {
		return failed("this token cannot cancel runs"), nil
	}
	id := runv1.ULID(args.RunID)
	if !run.ValidULID(id) {
		return failed("run_id is not an identifier this system issues"), nil
	}

	item, err := s.app.Run(r.Context(), id)
	if err != nil {
		return toolError(err), nil
	}
	if !s.mayRead(c, item) {
		return failed("this token does not reach that run"), nil
	}

	cancelled, err := s.app.Cancel(r.Context(), id, c.Name(), args.Reason)
	if err != nil {
		return toolError(err), nil
	}
	return ok(fmt.Sprintf("cancellation requested for %s", id), restapi.RunView(cancelled)), nil
}

type listRunsArgs struct {
	Status []string `json:"status,omitempty"`
	Role   string   `json:"role,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

func (s *Server) listRuns(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	var args listRunsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}

	filter := store.RunFilter{Status: args.Status, Role: args.Role, Limit: args.Limit}
	if c.Parent != nil {
		// An agent lists what it started, not what the installation is doing.
		filter.Parent = c.Parent.ID
	}

	runs, err := s.app.Runs(r.Context(), filter)
	if err != nil {
		return toolError(err), nil
	}

	views := make([]any, 0, len(runs))
	lines := make([]string, 0, len(runs))
	for _, item := range runs {
		views = append(views, restapi.RunView(item))
		lines = append(lines, fmt.Sprintf("%s  %-12s %s", item.ID, item.ReportedStatus(), item.Model))
	}
	return ok(joinLines(lines), map[string]any{"runs": views}), nil
}

func (s *Server) listRoles(r *http.Request) (any, *rpcError) {
	roles, err := s.app.Store().ListRoles(r.Context())
	if err != nil {
		return toolError(err), nil
	}
	names := make([]string, 0, len(roles))
	items := make([]any, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
		items = append(items, map[string]any{"name": role.Name, "spec": role.Spec})
	}
	return ok(joinLines(names), map[string]any{"roles": items}), nil
}

func (s *Server) listClusters(r *http.Request) (any, *rpcError) {
	clusters, err := s.app.Store().ListClusters(r.Context())
	if err != nil {
		return toolError(err), nil
	}
	lines := make([]string, 0, len(clusters))
	items := make([]any, 0, len(clusters))
	for _, c := range clusters {
		lines = append(lines, fmt.Sprintf("%s  %-12s %d/%d slots", c.Name, c.Status, c.FreeSlots, c.CapacitySlots))
		items = append(items, map[string]any{
			"cluster_id": string(c.ID), "name": c.Name, "status": c.Status,
			"free_slots": c.FreeSlots, "capacity_slots": c.CapacitySlots,
			"quota_exhausted": c.QuotaExhausted, "last_heartbeat_at": c.LastHeartbeatAt,
		})
	}
	return ok(joinLines(lines), map[string]any{"clusters": items}), nil
}

// mayRead bounds what a per-run token reaches: its own run, its parent's other
// children, and the runs it started itself. A token that reached every run
// would turn one compromised agent into a reader of every result in the
// installation.
func (s *Server) mayRead(c Caller, item store.Run) bool {
	if c.Parent == nil {
		return c.Token.Allows(store.ScopeRunsRead)
	}
	return item.ID == c.Parent.ID || item.ParentRunID == c.Parent.ID
}

// ok is a successful tool result: text for the model, structure for a program.
func ok(text string, structured any) toolResult {
	return toolResult{
		Content:           []contentBlock{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}

// failed is a tool-level failure. It is a result rather than a JSON-RPC error
// on purpose: the protocol reserves errors for the call not happening, while a
// refusal the model can act on — a bad argument, a run it cannot reach — is
// something it should see and respond to.
func failed(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}, IsError: true}
}

func toolError(err error) toolResult {
	var invalid *run.InvalidRequestError
	if errorsAs(err, &invalid) {
		return failed(invalid.Field + ": " + invalid.Detail)
	}
	if errorsIs(err, store.ErrNotFound) {
		return failed("no such run")
	}
	if errorsIs(err, store.ErrRunTerminal) {
		return failed("that run has already ended")
	}
	return failed("the request could not be completed")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return "(none)"
	}
	out := lines[0]
	for _, line := range lines[1:] {
		out += "\n" + line
	}
	return out
}

// errorsIs and errorsAs are the standard helpers, named locally so that the
// file reads without a package qualifier on every line of toolError.
func errorsIs(err, target error) bool { return stderrors.Is(err, target) }

func errorsAs(err error, target any) bool { return stderrors.As(err, target) }
