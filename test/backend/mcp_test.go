package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The MCP listener: the same use cases as REST, spoken as tools.
//
// The property worth most here is the per-run token. When an agent inside a pod
// calls run_agent, the platform has to know which run is calling — to bind the
// child to its parent, check the depth and charge the right budget — and a
// global token makes all three impossible at once. That is the detail the
// original specification missed, so it is the one these tests spend most of
// their assertions on.

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc sends one JSON-RPC request to the MCP endpoint.
func (h *harness) rpc(t *testing.T, token, method string, params any) rpcResponse {
	t.Helper()

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.MCP.URL+"/mcp", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.MCP.Client().Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()

	var decoded rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return decoded
}

// callTool calls a tool and returns its structured content.
func (h *harness) callTool(t *testing.T, token, name string, args any) (map[string]any, bool) {
	t.Helper()

	resp := h.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("tools/call %s: rpc error %d %s", name, resp.Error.Code, resp.Error.Message)
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, resp.Result)
	}
	if len(result.Content) == 0 {
		t.Errorf("%s answered with no text block; a model has nothing to read", name)
	}
	return result.StructuredContent, result.IsError
}

// The handshake, and the fact that it does not require a credential: a client
// that cannot initialize cannot be told why it needs one.
func TestTheMCPHandshakeAndToolListing(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	resp := h.rpc(t, "", "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if resp.Error != nil {
		t.Fatalf("initialize: %d %s", resp.Error.Code, resp.Error.Message)
	}

	listed := h.rpc(t, token, "tools/list", nil)
	if listed.Error != nil {
		t.Fatalf("tools/list: %d %s", listed.Error.Code, listed.Error.Message)
	}
	var tools struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed.Result, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}

	byName := map[string]bool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = true
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("tool %s is not usable without a description and a schema", tool.Name)
		}
	}
	for _, want := range []string{"run_agent", "get_run_result", "cancel_run", "list_runs", "list_roles"} {
		if !byName[want] {
			t.Errorf("tools/list does not offer %s", want)
		}
	}

	// Without a credential, everything past the handshake is refused.
	unauthenticated := h.rpc(t, "", "tools/list", nil)
	if unauthenticated.Error == nil {
		t.Fatal("tools/list was served without a token")
	}
}

// run_agent is the REST command under another name, and it produces the same
// run: one admission path, one set of rules, one list to look at afterwards.
func TestRunAgentAdmitsTheSameRunAsTheAPI(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	result, isError := h.callTool(t, token, "run_agent", map[string]any{
		"prompt": "add a health endpoint",
		"repo":   "https://github.com/example/repo",
	})
	if isError {
		t.Fatalf("run_agent failed: %+v", result)
	}

	id, _ := result["run_id"].(string)
	if id == "" {
		t.Fatalf("run_agent returned no run_id: %+v", result)
	}
	stored := h.Run(runv1.ULID(id))
	if stored.CreatedVia != "mcp" {
		t.Errorf("created_via = %s, want mcp", stored.CreatedVia)
	}
	if stored.Status != "Queued" {
		t.Errorf("status = %s, want Queued", stored.Status)
	}

	// And the run is readable through the tool that exists for it.
	read, isError := h.callTool(t, token, "get_run_result", map[string]any{"run_id": id})
	if isError {
		t.Fatalf("get_run_result failed: %+v", read)
	}
	if read["run_id"] != id {
		t.Errorf("get_run_result returned %v, want %s", read["run_id"], id)
	}

	// A bad argument is a tool error the model can act on, not a JSON-RPC
	// error: the call happened, and the answer is something to respond to.
	failure, isError := h.callTool(t, token, "run_agent", map[string]any{"prompt": "   "})
	if !isError {
		t.Errorf("a run with no prompt was accepted: %+v", failure)
	}
}

// The per-run token is what makes a child run accountable. It is minted by the
// backend when the lease is assembled, travels in the per-run Secret, and names
// its run — so an agent cannot claim a different parent than the one it has.
func TestAChildRunStartedByAnAgentIsBoundToItsParent(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	parent := h.Submit(func(r *run.SubmitRequest) { r.Role = "" })

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}

	// The token the pod would read out of mcp.json in its Secret.
	var mcpConfig struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(lease.Secrets[runv1.SecretKeyMCPConfig]), &mcpConfig); err != nil {
		t.Fatalf("decode the rendered MCP configuration: %v", err)
	}
	server, ok := mcpConfig.MCPServers["haliphron"]
	if !ok {
		t.Fatal("the rendered configuration does not point the agent at this control plane")
	}
	runToken := server.Headers["Authorization"]
	if len(runToken) < 8 {
		t.Fatalf("no per-run credential in the configuration: %q", runToken)
	}
	runToken = runToken[len("Bearer "):]

	child, isError := h.callTool(t, runToken, "run_agent", map[string]any{
		"prompt": "write the tests for what I just built",
	})
	if isError {
		t.Fatalf("a child run was refused: %+v", child)
	}

	childID, _ := child["run_id"].(string)
	stored := h.Run(runv1.ULID(childID))
	if stored.ParentRunID != parent.ID {
		t.Errorf("parent = %s, want %s", stored.ParentRunID, parent.ID)
	}
	if stored.Depth != parent.Depth+1 {
		t.Errorf("depth = %d, want %d", stored.Depth, parent.Depth+1)
	}
	if stored.CreatedVia != "agent" {
		t.Errorf("created_via = %s, want agent", stored.CreatedVia)
	}
	if stored.CreatedBy != "run:"+string(parent.ID) {
		t.Errorf("created_by = %s, want the parent run", stored.CreatedBy)
	}
	// The repository is inherited: an agent asked to write tests for what it
	// just built means the repository it is working in.
	if stored.RepoURL != parent.RepoURL {
		t.Errorf("repo = %q, want the parent's %q", stored.RepoURL, parent.RepoURL)
	}

	// An agent lists what it started, not what the installation is doing.
	listed, isError := h.callTool(t, runToken, "list_runs", map[string]any{})
	if isError {
		t.Fatalf("list_runs failed: %+v", listed)
	}
	runs, _ := listed["runs"].([]any)
	if len(runs) != 1 {
		t.Errorf("an agent saw %d runs, want only the one it started", len(runs))
	}

	// And it cannot read a run that is neither its own nor its child's.
	stranger := h.Submit()
	denied, isError := h.callTool(t, runToken, "get_run_result",
		map[string]any{"run_id": string(stranger.ID)})
	if !isError {
		t.Errorf("a per-run token read an unrelated run: %+v", denied)
	}

	// The token dies with its run: once the parent ends, nothing more can be
	// started on its budget.
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("finish the parent: %+v", problem)
	}
	resp := h.rpc(t, runToken, "tools/list", nil)
	if resp.Error == nil {
		t.Error("the per-run token still worked after its run ended")
	}
}

// Depth is bounded because the failure mode of an unbounded chain is spend
// rather than incorrectness: nothing breaks, it just keeps paying.
func TestTheDepthLimitStopsARunawayChain(t *testing.T) {
	h := newHarness(t)

	// A parent already at the limit, reached by the path a chain takes.
	parent := h.Submit()
	if _, err := h.Store.DB().ExecContext(context.Background(),
		`UPDATE runs SET depth = 8 WHERE id = $1`, parent.ID); err != nil {
		t.Fatalf("stage a deep run: %v", err)
	}

	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, 0)
	if err == nil {
		t.Fatal("a per-run token without an expiry was accepted")
	}
	token, err = h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatalf("mint a per-run token: %v", err)
	}

	result, isError := h.callTool(t, token.Secret, "run_agent",
		map[string]any{"prompt": "and another one"})
	if !isError {
		t.Fatalf("a run nine levels deep was admitted: %+v", result)
	}
}

// Breadth is bounded for the same reason depth is, and bounding depth alone
// does not bound the tree: ten children each starting ten is a hundred, and by
// the depth limit it is a number nobody meant to ask for.
func TestTheChildLimitStopsARunawayFanOut(t *testing.T) {
	h := newHarness(t)

	parent := h.Submit()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatalf("mint a per-run token: %v", err)
	}

	for i := 0; i < run.MaxChildren; i++ {
		result, isError := h.callTool(t, token.Secret, "run_agent",
			map[string]any{"prompt": fmt.Sprintf("subtask %d", i)})
		if isError {
			t.Fatalf("child %d of the permitted %d was refused: %+v", i+1, run.MaxChildren, result)
		}
	}

	result, isError := h.callTool(t, token.Secret, "run_agent",
		map[string]any{"prompt": "and one more"})
	if !isError {
		t.Fatalf("a child beyond the limit was admitted: %+v", result)
	}

	var children int
	if err := h.Store.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM runs WHERE parent_run_id = $1`, parent.ID).Scan(&children); err != nil {
		t.Fatalf("count the children: %v", err)
	}
	if children != run.MaxChildren {
		t.Errorf("the parent has %d children, want %d", children, run.MaxChildren)
	}
}

// The budget is over the parent's lifetime, not over what it has running. A
// ceiling on concurrent children bounds nothing: an agent starts its ten, waits
// for them, and starts ten more for as long as its token lives.
func TestTheChildLimitCountsChildrenThatHaveAlreadyFinished(t *testing.T) {
	h := newHarness(t)

	parent := h.Submit()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatalf("mint a per-run token: %v", err)
	}

	for i := 0; i < run.MaxChildren; i++ {
		result, isError := h.callTool(t, token.Secret, "run_agent",
			map[string]any{"prompt": fmt.Sprintf("subtask %d", i)})
		if isError {
			t.Fatalf("child %d was refused: %+v", i+1, result)
		}
	}
	// Every one of them is over and gone as far as scheduling is concerned.
	if _, err := h.Store.DB().ExecContext(context.Background(),
		`UPDATE runs SET status = 'Succeeded', finished_at = now() WHERE parent_run_id = $1`,
		parent.ID); err != nil {
		t.Fatalf("finish the children: %v", err)
	}

	result, isError := h.callTool(t, token.Secret, "run_agent",
		map[string]any{"prompt": "the next batch of ten"})
	if !isError {
		t.Fatalf("the budget refilled when the children finished: %+v", result)
	}
}

// Several run_agent calls at once must not each read the count the others have
// not yet written. The check and the insert are one transaction behind a lock
// on the parent, so the ceiling holds under concurrency rather than only in a
// test that goes one call at a time.
func TestConcurrentChildrenCannotExceedTheLimitTogether(t *testing.T) {
	h := newHarness(t)

	parent := h.Submit()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatalf("mint a per-run token: %v", err)
	}

	const callers = run.MaxChildren * 3
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			h.callTool(t, token.Secret, "run_agent",
				map[string]any{"prompt": fmt.Sprintf("racing subtask %d", i)})
		}(i)
	}
	wg.Wait()

	var children int
	if err := h.Store.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM runs WHERE parent_run_id = $1`, parent.ID).Scan(&children); err != nil {
		t.Fatalf("count the children: %v", err)
	}
	if children != run.MaxChildren {
		t.Errorf("%d concurrent calls produced %d children, want exactly %d",
			callers, children, run.MaxChildren)
	}
}

// cancel_run reaches the same recorded intent as the REST endpoint, and an
// agent may cancel what it started.
func TestCancelRunIsReachableAsATool(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	admitted := h.Submit()

	result, isError := h.callTool(t, token, "cancel_run", map[string]any{
		"run_id": string(admitted.ID), "reason": "wrong branch",
	})
	if isError {
		t.Fatalf("cancel_run failed: %+v", result)
	}
	if result["status"] != "Cancelled" {
		t.Errorf("status = %v, want Cancelled", result["status"])
	}

	// An identifier this system never issues is refused before anything is
	// looked up.
	failure, isError := h.callTool(t, token, "cancel_run", map[string]any{"run_id": "not-a-ulid"})
	if !isError {
		t.Errorf("a malformed identifier was accepted: %+v", failure)
	}
}
