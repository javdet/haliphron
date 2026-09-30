package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// MCP tasks: run_agent started as a task, for a client that negotiated
// 2025-11-25.
//
// A task is a run and its ID is the run ID; its status is derived from the run
// on every read. The property worth most is the one the user of an existing
// client never sees: a client that did not ask for tasks is answered exactly as
// it was before they existed, down to the fields in tools/list.

const tasksVersion = "2025-11-25"

type taskObject struct {
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
	PollInterval  int64  `json:"pollInterval"`
}

type taskToolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
	Meta              map[string]any `json:"_meta"`
}

// startTask calls run_agent as a task and returns the task it was answered with.
func (h *harness) startTask(t *testing.T, token string, args map[string]any) (taskObject, map[string]any) {
	t.Helper()
	resp := h.rpcAt(t, tasksVersion, token, "tools/call", map[string]any{
		"name": "run_agent", "arguments": args, "task": map[string]any{"ttl": 60000},
	})
	if resp.Error != nil {
		t.Fatalf("run_agent as a task: rpc error %d %s", resp.Error.Code, resp.Error.Message)
	}
	var created struct {
		Task json.RawMessage `json:"task"`
		Meta map[string]any  `json:"_meta"`
	}
	if err := json.Unmarshal(resp.Result, &created); err != nil || len(created.Task) == 0 {
		t.Fatalf("run_agent as a task did not answer with a task: %v (%s)", err, resp.Result)
	}
	var fields map[string]any
	_ = json.Unmarshal(created.Task, &fields)
	var out taskObject
	if err := json.Unmarshal(created.Task, &out); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return out, fields
}

func (h *harness) getTask(t *testing.T, token, id string) taskObject {
	t.Helper()
	resp := h.rpcAt(t, tasksVersion, token, "tasks/get", map[string]any{"taskId": id})
	if resp.Error != nil {
		t.Fatalf("tasks/get: rpc error %d %s", resp.Error.Code, resp.Error.Message)
	}
	var out taskObject
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode task: %v (%s)", err, resp.Result)
	}
	return out
}

func (h *harness) taskResult(t *testing.T, token, id string) taskToolResult {
	t.Helper()
	resp := h.rpcAt(t, tasksVersion, token, "tasks/result", map[string]any{"taskId": id})
	if resp.Error != nil {
		t.Fatalf("tasks/result: rpc error %d %s", resp.Error.Code, resp.Error.Message)
	}
	var out taskToolResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode task result: %v (%s)", err, resp.Result)
	}
	return out
}

func relatedTask(meta map[string]any) string {
	related, _ := meta["io.modelcontextprotocol/related-task"].(map[string]any)
	id, _ := related["taskId"].(string)
	return id
}

// A client that has not negotiated tasks sees the server it saw before: the
// version it was answered with, no tasks capability, no execution field in the
// tool list, and a task augmentation ignored rather than acted on.
func TestAClientThatDidNotAskForTasksIsAnsweredAsBefore(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	for _, requested := range []string{"2025-06-18", "2025-03-26", ""} {
		resp := h.rpc(t, "", "initialize", map[string]any{"protocolVersion": requested})
		if resp.Error != nil {
			t.Fatalf("initialize %q: %d %s", requested, resp.Error.Code, resp.Error.Message)
		}
		var hello struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
		}
		if err := json.Unmarshal(resp.Result, &hello); err != nil {
			t.Fatalf("decode initialize: %v", err)
		}
		if hello.ProtocolVersion != "2025-06-18" {
			t.Errorf("initialize %q answered %q, want 2025-06-18", requested, hello.ProtocolVersion)
		}
		if _, ok := hello.Capabilities["tasks"]; ok {
			t.Errorf("initialize %q declared tasks: %s", requested, resp.Result)
		}
	}

	for _, version := range []string{"", "2025-06-18"} {
		listed := h.rpcAt(t, version, token, "tools/list", nil)
		if listed.Error != nil {
			t.Fatalf("tools/list: %d %s", listed.Error.Code, listed.Error.Message)
		}
		if strings.Contains(string(listed.Result), `"execution"`) {
			t.Errorf("tools/list at %q carries an execution field: %s", version, listed.Result)
		}

		// The same call a task-aware client would make, from one that is not:
		// it is served as the plain call, and answers with the run.
		resp := h.rpcAt(t, version, token, "tools/call", map[string]any{
			"name": "run_agent", "arguments": map[string]any{"prompt": "add a health endpoint"},
			"task": map[string]any{"ttl": 60000},
		})
		if resp.Error != nil {
			t.Fatalf("run_agent: rpc error %d %s", resp.Error.Code, resp.Error.Message)
		}
		var plain taskToolResult
		if err := json.Unmarshal(resp.Result, &plain); err != nil {
			t.Fatalf("decode run_agent: %v", err)
		}
		if plain.StructuredContent["run_id"] == nil || strings.Contains(string(resp.Result), `"taskId"`) {
			t.Errorf("run_agent at %q with a task field answered %s, want the plain run", version, resp.Result)
		}
	}
}

// A client that asks for 2025-11-25 is given it, with tasks declared for
// tools/call and cancel, and run_agent offered as an optional task — the plain
// call still works, and no other tool can be a task.
func TestTheTaskHandshakeOffersRunAgentAsAnOptionalTask(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	resp := h.rpc(t, "", "initialize", map[string]any{"protocolVersion": tasksVersion})
	if resp.Error != nil {
		t.Fatalf("initialize: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var hello struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tasks *struct {
				List     json.RawMessage `json:"list"`
				Cancel   json.RawMessage `json:"cancel"`
				Requests struct {
					Tools struct {
						Call json.RawMessage `json:"call"`
					} `json:"tools"`
				} `json:"requests"`
			} `json:"tasks"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if hello.ProtocolVersion != tasksVersion {
		t.Errorf("protocolVersion = %q, want %q", hello.ProtocolVersion, tasksVersion)
	}
	tasks := hello.Capabilities.Tasks
	if tasks == nil || tasks.Requests.Tools.Call == nil || tasks.Cancel == nil {
		t.Fatalf("tasks for tools/call and cancel are not declared: %s", resp.Result)
	}
	// tasks/list is not offered: listing runs is list_runs.
	if tasks.List != nil {
		t.Errorf("tasks.list is declared: %s", resp.Result)
	}

	listed := h.rpcAt(t, tasksVersion, token, "tools/list", nil)
	var tools struct {
		Tools []struct {
			Name      string `json:"name"`
			Execution *struct {
				TaskSupport string `json:"taskSupport"`
			} `json:"execution"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed.Result, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	for _, tool := range tools.Tools {
		switch {
		case tool.Name == "run_agent":
			if tool.Execution == nil || tool.Execution.TaskSupport != "optional" {
				t.Errorf("run_agent execution = %+v, want taskSupport optional", tool.Execution)
			}
		case tool.Execution != nil:
			t.Errorf("%s offers execution %+v; only run_agent is a task", tool.Name, tool.Execution)
		}
	}

	// A task augmentation on a tool that is not one is refused, as the
	// specification has it, with method not found.
	refused := h.rpcAt(t, tasksVersion, token, "tools/call", map[string]any{
		"name": "list_runs", "arguments": map[string]any{}, "task": map[string]any{},
	})
	if refused.Error == nil || refused.Error.Code != -32601 {
		t.Errorf("list_runs as a task = %+v %s, want error -32601", refused.Error, refused.Result)
	}

	// And the plain call is still the plain call.
	plain, isError := h.callTool(t, token, "run_agent", map[string]any{"prompt": "still works"})
	if isError || plain["run_id"] == nil {
		t.Errorf("the plain run_agent stopped working: %+v", plain)
	}

	// tasks/list is not a method here.
	if unlisted := h.rpcAt(t, tasksVersion, token, "tasks/list", map[string]any{}); unlisted.Error == nil ||
		unlisted.Error.Code != -32601 {
		t.Errorf("tasks/list = %+v, want -32601", unlisted.Error)
	}
}

// The whole life of a task: started, polled while working, and collected once
// the run has ended with its report — the same run run_agent admits, with the
// same answer get_run_result gives. tasks/result blocks until then.
func TestRunAgentAsATaskIsTheSameRunCollectedAtTheEnd(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	p := newProbe(t, h, "east")

	created, fields := h.startTask(t, token, map[string]any{
		"prompt": "add a health endpoint", "repo": "https://github.com/example/repo",
	})
	if created.Status != "working" {
		t.Errorf("a new task is %q, want working", created.Status)
	}
	if created.CreatedAt == "" || created.LastUpdatedAt == "" || created.PollInterval <= 0 {
		t.Errorf("task is missing a timestamp or a poll interval: %+v", created)
	}
	// Runs are kept for ever by default, and ttl says so with null rather than
	// by being absent.
	if ttl, present := fields["ttl"]; !present || ttl != nil {
		t.Errorf("ttl = %v (present %t), want null", ttl, present)
	}

	stored := h.Run(runv1.ULID(created.TaskID))
	if stored.CreatedVia != "mcp" || stored.Status != "Queued" {
		t.Errorf("the task's run is via %s, %s; want an mcp run, Queued", stored.CreatedVia, stored.Status)
	}
	if got := h.getTask(t, token, created.TaskID); got.Status != "working" {
		t.Errorf("tasks/get = %q, want working", got.Status)
	}

	// tasks/result, asked before the end, waits for it.
	collected := make(chan rpcResponse, 1)
	go func() {
		resp, err := h.sendRPC(tasksVersion, token, "tasks/result", map[string]any{"taskId": created.TaskID})
		if err != nil {
			t.Error(err)
		}
		collected <- resp
	}()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	select {
	case early := <-collected:
		t.Fatalf("tasks/result answered before the report arrived: %s", early.Result)
	case <-time.After(1500 * time.Millisecond):
	}
	// Ended with the report still owed: the task is not over yet.
	if got := h.getTask(t, token, created.TaskID); got.Status != "working" {
		t.Errorf("tasks/get between the terminal status and the report = %q, want working", got.Status)
	}

	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1,
		completionFor(lease.RunID, 1, "1.500000", "https://github.com/example/repo/pull/7")); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}

	var answered rpcResponse
	select {
	case answered = <-collected:
	case <-time.After(10 * time.Second):
		t.Fatal("tasks/result did not answer once the task completed")
	}
	if answered.Error != nil {
		t.Fatalf("tasks/result: rpc error %d %s", answered.Error.Code, answered.Error.Message)
	}
	var result taskToolResult
	if err := json.Unmarshal(answered.Result, &result); err != nil {
		t.Fatalf("decode task result: %v (%s)", err, answered.Result)
	}
	if result.IsError {
		t.Errorf("a succeeded run's result is an error: %+v", result)
	}
	if result.StructuredContent["run_id"] != created.TaskID || result.StructuredContent["status"] != "Succeeded" {
		t.Errorf("tasks/result = %+v, want the succeeded run", result.StructuredContent)
	}
	if relatedTask(result.Meta) != created.TaskID {
		t.Errorf("tasks/result does not name its task in _meta: %+v", result.Meta)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "pull request") {
		t.Errorf("the text block does not carry the result: %+v", result.Content)
	}

	if got := h.getTask(t, token, created.TaskID); got.Status != "completed" {
		t.Errorf("tasks/get after the report = %q, want completed", got.Status)
	}
	// Collected again, the answer is the same and immediate.
	if again := h.taskResult(t, token, created.TaskID); again.StructuredContent["run_id"] != created.TaskID {
		t.Errorf("a second tasks/result = %+v", again)
	}
	// And a completed task cannot be cancelled.
	refused := h.rpcAt(t, tasksVersion, token, "tasks/cancel", map[string]any{"taskId": created.TaskID})
	if refused.Error == nil || refused.Error.Code != -32602 {
		t.Errorf("tasks/cancel of a completed task = %+v, want -32602", refused.Error)
	}
}

// A run that failed is a failed task, and its result is an error: the
// specification ties the two together for tool calls. It is also where the
// grace shows: a report that never arrives holds the task only so long.
func TestAFailedRunIsAFailedTaskOnceItsReportIsGivenUpOn(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	p := newProbe(t, h, "east")

	created, _ := h.startTask(t, token, map[string]any{"prompt": "break something"})
	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseFailed); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	if got := h.getTask(t, token, created.TaskID); got.Status != "working" {
		t.Fatalf("a run whose report is owed is %q, want working", got.Status)
	}

	// The report is past its grace.
	if _, err := h.Store.DB().ExecContext(context.Background(),
		`UPDATE runs SET finished_at = now() - interval '10 minutes' WHERE id = $1`, created.TaskID); err != nil {
		t.Fatalf("age the run: %v", err)
	}

	got := h.getTask(t, token, created.TaskID)
	if got.Status != "failed" || got.StatusMessage == "" {
		t.Errorf("tasks/get = %+v, want failed with a message", got)
	}
	result := h.taskResult(t, token, created.TaskID)
	if !result.IsError {
		t.Errorf("a failed task's result is not an error: %+v", result)
	}
	if result.StructuredContent["run_id"] != created.TaskID {
		t.Errorf("tasks/result = %+v, want the run", result.StructuredContent)
	}
}

// tasks/cancel is cancel_run, and a cancelled task stays cancelled even though
// the run it asked to stop goes on to succeed — the instruction reaches the
// cluster on its next heartbeat, and the run can finish first.
func TestACancelledTaskStaysCancelledWhateverItsRunDoesNext(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	p := newProbe(t, h, "east")

	created, _ := h.startTask(t, token, map[string]any{"prompt": "a long job"})
	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("running: %+v", problem)
	}

	resp := h.rpcAt(t, tasksVersion, token, "tasks/cancel", map[string]any{"taskId": created.TaskID})
	if resp.Error != nil {
		t.Fatalf("tasks/cancel: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var cancelled taskObject
	if err := json.Unmarshal(resp.Result, &cancelled); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Errorf("tasks/cancel answered %q, want cancelled", cancelled.Status)
	}
	if h.Run(runv1.ULID(created.TaskID)).CancelRequestedAt == nil {
		t.Error("tasks/cancel did not record a cancellation on the run")
	}

	// The run finishes anyway.
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1,
		completionFor(lease.RunID, 1, "0.100000", "")); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}
	if got := h.getTask(t, token, created.TaskID); got.Status != "cancelled" {
		t.Errorf("a cancelled task became %q", got.Status)
	}
	// Terminal, so collected at once, and cancelled is not failed.
	result := h.taskResult(t, token, created.TaskID)
	if result.IsError || relatedTask(result.Meta) != created.TaskID {
		t.Errorf("tasks/result of a cancelled task = %+v", result)
	}
	again := h.rpcAt(t, tasksVersion, token, "tasks/cancel", map[string]any{"taskId": created.TaskID})
	if again.Error == nil || again.Error.Code != -32602 {
		t.Errorf("a second tasks/cancel = %+v, want -32602", again.Error)
	}
}

// A task is bound to what the token reaches, which is what get_run_result is
// bound to: a per-run token sees its own children and nothing else, and a task
// it cannot see is answered exactly as one that does not exist.
func TestATaskIsBoundToWhatTheTokenReaches(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	readOnly := h.Token(store.ScopeRunsRead)

	parent := h.Submit()
	perRun, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatalf("mint a per-run token: %v", err)
	}

	// An agent's child, started as a task, is bound to its parent like any
	// other child.
	child, _ := h.startTask(t, perRun.Secret, map[string]any{"prompt": "a subtask"})
	if stored := h.Run(runv1.ULID(child.TaskID)); stored.ParentRunID != parent.ID {
		t.Errorf("a child started as a task has parent %s, want %s", stored.ParentRunID, parent.ID)
	}
	if got := h.getTask(t, perRun.Secret, child.TaskID); got.TaskID != child.TaskID {
		t.Errorf("the agent cannot see its own child's task: %+v", got)
	}

	stranger, _ := h.startTask(t, token, map[string]any{"prompt": "someone else's"})
	missing, _ := run.NewULID(time.Now())
	for name, id := range map[string]string{
		"a stranger's task": stranger.TaskID, "an unknown task": string(missing), "a malformed id": "nope",
	} {
		for _, method := range []string{"tasks/get", "tasks/result", "tasks/cancel"} {
			resp := h.rpcAt(t, tasksVersion, perRun.Secret, method, map[string]any{"taskId": id})
			if resp.Error == nil || resp.Error.Code != -32602 {
				t.Errorf("%s of %s = %+v %s, want -32602", method, name, resp.Error, resp.Result)
			}
		}
	}

	// A token that cannot start runs cannot start one as a task either, and
	// is told so with a protocol error: there is no task to carry the refusal.
	refused := h.rpcAt(t, tasksVersion, readOnly, "tools/call", map[string]any{
		"name": "run_agent", "arguments": map[string]any{"prompt": "x"}, "task": map[string]any{},
	})
	if refused.Error == nil || refused.Error.Code != -32602 {
		t.Errorf("a read-only token started a task: %+v %s", refused.Error, refused.Result)
	}
	// Nor can it cancel one it can read.
	notCancelled := h.rpcAt(t, tasksVersion, readOnly, "tasks/cancel", map[string]any{"taskId": stranger.TaskID})
	if notCancelled.Error == nil {
		t.Errorf("a read-only token cancelled a task: %s", notCancelled.Result)
	}
	// An invalid argument is refused the same way.
	invalid := h.rpcAt(t, tasksVersion, token, "tools/call", map[string]any{
		"name": "run_agent", "arguments": map[string]any{"prompt": "   "}, "task": map[string]any{},
	})
	if invalid.Error == nil || invalid.Error.Code != -32602 {
		t.Errorf("a task with no prompt = %+v %s, want -32602", invalid.Error, invalid.Result)
	}
}
