package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/restapi"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Tasks: run_agent started as an MCP task, for a client that negotiated
// 2025-11-25.
//
// A task is a run, and the task ID is the run ID. Nothing is stored for the
// task itself: its status is derived from the run on every read, so there is no
// second state machine to drift from the first, and a run started as a task is
// the same run in list_runs, the UI and the REST API. The mapping is the whole
// design:
//
//	working    the run has not ended, or has ended with its report on the way
//	completed  Succeeded — the agent exited 0, which is all Succeeded means
//	failed     Failed or TimedOut
//	cancelled  a cancellation was requested, by anyone, or the run was Cancelled
//
// Cancelled wins over whatever the run does next. The specification has a
// cancelled task stay cancelled even if the work completes, and a cancellation
// here is an instruction delivered on the next heartbeat, so a run can still
// succeed after it was asked to stop.

// Task statuses, as the specification spells them.
const (
	taskWorking   = "working"
	taskCompleted = "completed"
	taskFailed    = "failed"
	taskCancelled = "cancelled"
)

const (
	metaRelatedTask       = "io.modelcontextprotocol/related-task"
	metaImmediateResponse = "io.modelcontextprotocol/model-immediate-response"
)

// taskPollInterval is what a host is asked to poll at. A run takes minutes and
// every poll is a database read, so a second would buy nothing a person could
// see.
const taskPollInterval = 5 * time.Second

// task is the Task object: what tasks/get answers, and what a task-augmented
// call answers with in place of its result.
type task struct {
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
	// TTL is null for unlimited, and the specification has it present either
	// way, so it is not omitempty.
	TTL          *int64 `json:"ttl"`
	PollInterval int64  `json:"pollInterval,omitempty"`
}

func (s *Server) taskOf(item store.Run) task {
	status, message := s.taskStatus(item)
	updated := item.UpdatedAt
	if updated.IsZero() {
		updated = item.CreatedAt
	}
	t := task{
		TaskID:        string(item.ID),
		Status:        status,
		StatusMessage: message,
		CreatedAt:     item.CreatedAt.UTC().Format(time.RFC3339Nano),
		LastUpdatedAt: updated.UTC().Format(time.RFC3339Nano),
		PollInterval:  taskPollInterval.Milliseconds(),
	}
	if s.runRetention > 0 {
		// Retention counts from when a run finished and a ttl from when it was
		// created, so the retention is a lower bound on the ttl and the honest
		// number to give: the task is there for at least this long.
		ttl := s.runRetention.Milliseconds()
		t.TTL = &ttl
	}
	return t
}

// taskStatus derives a task's status from its run.
func (s *Server) taskStatus(item store.Run) (status, message string) {
	if item.CancelRequestedAt != nil || item.Status == clusterv1.StatusCancelled {
		message = "cancellation was requested"
		if item.CancelReason != "" {
			message += ": " + item.CancelReason
		}
		return taskCancelled, message
	}
	if !s.app.ResultSettled(item) {
		if item.ObservedPhase.IsTerminal() {
			return taskWorking, "the run has ended and its result is being collected"
		}
		return taskWorking, "run is " + item.ReportedStatus()
	}
	if item.Status == clusterv1.StatusSucceeded {
		// Not "the task was solved": exit code 0 is what Succeeded means.
		return taskCompleted, "run " + item.ReportedStatus() + ": the agent exited with code 0"
	}
	message = "run ended " + item.ReportedStatus()
	switch {
	case item.StatusMessage != "":
		message += ": " + item.StatusMessage
	case item.StatusReason != "":
		message += ": " + item.StatusReason
	}
	return taskFailed, message
}

func isTerminalTask(status string) bool { return status != taskWorking }

// runAgentTask is run_agent called as a task: the same admission, answered
// with the task instead of the run.
func (s *Server) runAgentTask(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	_, opts, submitted, err := s.admit(r, c, raw)
	if err != nil {
		return nil, s.admissionError(err)
	}
	s.recordSubmission(r, opts, submitted, restapi.RunView(submitted.Run))

	id := submitted.Run.ID
	return map[string]any{
		"task": s.taskOf(submitted.Run),
		"_meta": map[string]any{
			metaImmediateResponse: fmt.Sprintf("Run %s has started as a task. Its result is delivered "+
				"when it finishes; get_run_result reads it in the meantime.", id),
		},
	}, nil
}

// admissionError renders a refused admission for the task path. The plain call
// answers a refusal with a tool error the model can read; a task-augmented call
// can only answer with a task, and there is no task when nothing was admitted,
// so the refusal becomes the protocol error the call is left with.
func (s *Server) admissionError(err error) *rpcError {
	var rpcErr *rpcError
	if errorsAs(err, &rpcErr) {
		return rpcErr
	}
	var no refusal
	if errorsAs(err, &no) {
		return &rpcError{Code: codeInvalidParams, Message: string(no)}
	}
	var invalid *run.InvalidRequestError
	if errorsAs(err, &invalid) {
		return &rpcError{Code: codeInvalidParams, Message: invalid.Field + ": " + invalid.Detail}
	}
	if errorsIs(err, app.ErrSubmissionInFlight) {
		return &rpcError{Code: codeInvalidRequest,
			Message: "a request with this idempotency key is still being admitted; retry it"}
	}
	s.log.Error("could not admit a run started as a task", "error", err)
	return &rpcError{Code: codeInternalError, Message: "the request could not be completed"}
}

type taskIDParams struct {
	TaskID string `json:"taskId"`
}

// taskNotFound is the answer for a task that does not exist and for one this
// caller does not reach alike. The specification binds a task to the
// authorization context it was created in, and here that context is the
// token's reach — mayRead, the same bound get_run_result has — so a run the
// token cannot read is not a task it can see.
var taskNotFound = &rpcError{Code: codeInvalidParams, Message: "Failed to retrieve task: Task not found"}

func (s *Server) taskFor(r *http.Request, c Caller, raw json.RawMessage) (store.Run, *rpcError) {
	var params taskIDParams
	if err := json.Unmarshal(raw, &params); err != nil || params.TaskID == "" {
		return store.Run{}, &rpcError{Code: codeInvalidParams, Message: "params must hold taskId"}
	}
	id := runv1.ULID(params.TaskID)
	if !run.ValidULID(id) {
		return store.Run{}, taskNotFound
	}
	item, err := s.app.Run(r.Context(), id)
	if err != nil {
		if errorsIs(err, store.ErrNotFound) {
			return store.Run{}, taskNotFound
		}
		s.log.Error("could not read the run behind a task", "run", id, "error", err)
		return store.Run{}, &rpcError{Code: codeInternalError, Message: "the task could not be read"}
	}
	if !s.mayRead(c, item) {
		return store.Run{}, taskNotFound
	}
	return item, nil
}

// getTask is tasks/get.
func (s *Server) getTask(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	item, rpcErr := s.taskFor(r, c, raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.taskOf(item), nil
}

// taskResult is tasks/result: it blocks until the task is terminal and answers
// with what run_agent would have answered had it waited for the end.
//
// The one difference from that plain answer is isError, which is set when the
// task failed. The specification has a tool call whose result is an error be a
// failed task, and the reverse has to hold for the two to agree.
func (s *Server) taskResult(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	item, rpcErr := s.taskFor(r, c, raw)
	if rpcErr != nil {
		return nil, rpcErr
	}

	if status, _ := s.taskStatus(item); !isTerminalTask(status) {
		waited, err := s.app.WaitUntil(r.Context(), item.ID, func(r store.Run) bool {
			status, _ := s.taskStatus(r)
			return isTerminalTask(status)
		})
		if err != nil {
			if errorsIs(err, store.ErrNotFound) {
				return nil, taskNotFound
			}
			// Almost always the client gone, and then the answer goes nowhere.
			return nil, &rpcError{Code: codeInternalError, Message: "the wait for the task ended before the task did"}
		}
		item = waited
	}

	status, _ := s.taskStatus(item)
	result := ok(runText(item), restapi.RunView(item))
	result.IsError = status == taskFailed
	result.Meta = map[string]any{metaRelatedTask: map[string]any{"taskId": string(item.ID)}}
	return result, nil
}

// cancelTask is tasks/cancel: cancel_run, answered with the task.
func (s *Server) cancelTask(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	item, rpcErr := s.taskFor(r, c, raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if !c.Token.Allows(store.ScopeRunsWrite) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "this token cannot cancel runs"}
	}
	if status, _ := s.taskStatus(item); isTerminalTask(status) {
		return nil, &rpcError{Code: codeInvalidParams,
			Message: fmt.Sprintf("Cannot cancel task: already in terminal status '%s'", status)}
	}

	cancelled, err := s.app.Cancel(r.Context(), item.ID, c.Name(), "cancelled through tasks/cancel")
	if err != nil {
		switch {
		case errorsIs(err, store.ErrRunTerminal):
			// Ended, with its result still being collected: nothing is left to
			// stop, and the task is about to be terminal on its own.
			return nil, &rpcError{Code: codeInvalidParams, Message: "Cannot cancel task: the run has already ended"}
		case errorsIs(err, store.ErrNotFound):
			return nil, taskNotFound
		}
		s.log.Error("could not cancel the run behind a task", "run", item.ID, "error", err)
		return nil, &rpcError{Code: codeInternalError, Message: "the task could not be cancelled"}
	}
	return s.taskOf(cancelled), nil
}
