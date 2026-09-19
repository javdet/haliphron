package restapi

import (
	"encoding/json"
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The run endpoints. snake_case on the wire, by the naming rule that separates
// the public API from the machine contracts: the convention is arbitrary but
// uniform, and mixing the two inside one contract is a guaranteed source of
// translation bugs.

// createRunRequest is run_agent as a REST body.
type createRunRequest struct {
	Prompt string `json:"prompt"`

	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Role  string `json:"role,omitempty"`

	Repo         string `json:"repo,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	CreatePR     *bool  `json:"create_pr,omitempty"`

	TimeoutSeconds int32  `json:"timeout_seconds,omitempty"`
	MaxCostUSD     string `json:"max_cost_usd,omitempty"`
	MaxTurns       int32  `json:"max_turns,omitempty"`
	Priority       int32  `json:"priority,omitempty"`

	// Async false waits for the run to finish. The default is true, because a
	// synchronous agent run holds a connection open for minutes and most
	// callers — Slack, n8n, the UI — would rather have an identifier.
	Async *bool `json:"async,omitempty"`
	// WaitSeconds bounds a synchronous call.
	WaitSeconds int32 `json:"wait_seconds,omitempty"`
}

// runResponse is a run as the API shows it.
type runResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`

	Agent string `json:"agent"`
	Model string `json:"model"`
	Role  string `json:"role,omitempty"`

	Repo         string `json:"repo,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`

	ClusterID string `json:"cluster_id,omitempty"`
	Epoch     int64  `json:"epoch"`
	Attempt   int32  `json:"attempt"`

	Phase         string `json:"observed_phase,omitempty"`
	FailureClass  string `json:"failure_class,omitempty"`
	StatusReason  string `json:"status_reason,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	ExitCode      *int32 `json:"exit_code,omitempty"`

	ResultSummary string `json:"result_summary,omitempty"`
	PRURL         string `json:"pr_url,omitempty"`
	PRNumber      *int32 `json:"pr_number,omitempty"`
	CommitSHA     string `json:"commit_sha,omitempty"`

	CostUSD      string `json:"cost_usd"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	NumTurns     int32  `json:"num_turns"`

	ParentRunID string `json:"parent_run_id,omitempty"`
	Depth       int16  `json:"depth"`

	CreatedBy  string     `json:"created_by"`
	CreatedVia string     `json:"created_via"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// RunView renders a run for the public API. It is exported because the MCP
// tools answer with the same object: two shapes for one thing is how a UI and
// an agent end up disagreeing about what a run is.
func RunView(r store.Run) runResponse {
	return runResponse{
		RunID: string(r.ID),
		// ReportedStatus, not the stored one: a terminal run whose report has
		// not been collected is CompletedWithoutResult, and hiding that behind
		// Succeeded would show a cost of zero as if it were the truth.
		Status:        r.ReportedStatus(),
		Agent:         string(r.Agent),
		Model:         r.Model,
		Role:          r.Role,
		Repo:          r.RepoURL,
		BaseBranch:    r.BaseBranch,
		TargetBranch:  r.TargetBranch,
		ClusterID:     string(r.ClusterID),
		Epoch:         r.Epoch,
		Attempt:       r.Attempt,
		Phase:         string(r.ObservedPhase),
		FailureClass:  string(r.FailureClass),
		StatusReason:  r.StatusReason,
		StatusMessage: r.StatusMessage,
		ExitCode:      r.ExitCode,
		ResultSummary: r.ResultSummary,
		PRURL:         r.PRURL,
		PRNumber:      r.PRNumber,
		CommitSHA:     r.CommitSHA,
		CostUSD:       string(r.CostUSD),
		InputTokens:   r.InputTokens,
		OutputTokens:  r.OutputTokens,
		NumTurns:      r.NumTurns,
		ParentRunID:   string(r.ParentRunID),
		Depth:         r.Depth,
		CreatedBy:     r.CreatedBy,
		CreatedVia:    r.CreatedVia,
		CreatedAt:     r.CreatedAt,
		StartedAt:     r.StartedAt,
		FinishedAt:    r.FinishedAt,
	}
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, c caller) {
	var req createRunRequest
	raw, ok := s.decode(w, r, &req)
	if !ok {
		return
	}

	submit := run.SubmitRequest{
		Prompt:         req.Prompt,
		Agent:          runv1.AgentType(req.Agent),
		Model:          req.Model,
		Role:           req.Role,
		RepoURL:        req.Repo,
		BaseBranch:     req.BaseBranch,
		TargetBranch:   req.TargetBranch,
		CreatePR:       req.CreatePR,
		TimeoutSeconds: req.TimeoutSeconds,
		MaxCostUSD:     runv1.MoneyUSD(req.MaxCostUSD),
		MaxTurns:       req.MaxTurns,
		Priority:       req.Priority,
		CreatedBy:      c.Name(),
		CreatedVia:     c.Via(),
	}
	if c.Parent != nil {
		// A run started from inside an agent inherits its parent's identity in
		// the two ways that make it accountable: it is bound to the parent,
		// and it is one level deeper, which is what the depth limit counts.
		submit.ParentRunID = c.Parent.ID
		submit.Depth = c.Parent.Depth + 1
		if submit.Role == "" {
			submit.Role = c.Parent.Role
		}
		if submit.RepoURL == "" {
			submit.RepoURL = c.Parent.RepoURL
		}
	}

	opts := app.SubmitOptions{Key: r.Header.Get("Idempotency-Key"), Body: raw, Scope: "runs.create"}
	submitted, err := s.app.Submit(r.Context(), submit, opts)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	if submitted.Replayed {
		// The first request's answer, verbatim where there is one: a retry
		// that gets a differently-shaped reply is a retry that looks like a
		// second run.
		if len(submitted.Response) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(submitted.Response)
			return
		}
		s.write(w, http.StatusOK, RunView(submitted.Run))
		return
	}

	result := submitted.Run
	status := http.StatusAccepted
	if req.Async != nil && !*req.Async {
		wait := time.Duration(req.WaitSeconds) * time.Second
		waited, err := s.app.WaitForResult(r.Context(), result.ID, wait)
		if err != nil {
			s.failFor(w, r, err)
			return
		}
		result = waited
		if waited.FinishedAt != nil {
			status = http.StatusOK
		}
	}

	view := RunView(result)
	body, err := json.Marshal(view)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if err := s.app.CompleteSubmission(r.Context(), opts, result.ID, status, body); err != nil {
		s.internal(w, r, err)
		return
	}
	s.write(w, status, view)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, _ caller) {
	query := r.URL.Query()
	filter := store.RunFilter{
		Role:    query.Get("role"),
		Agent:   runv1.AgentType(query.Get("agent")),
		Cluster: runv1.ULID(query.Get("cluster_id")),
		Parent:  runv1.ULID(query.Get("parent_run_id")),
		Query:   query.Get("q"),
		Limit:   intParam(r, "limit", 50),
		Before:  runv1.ULID(query.Get("before")),
	}
	if statuses, ok := query["status"]; ok {
		filter.Status = statuses
	}

	runs, err := s.app.Runs(r.Context(), filter)
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	items := make([]runResponse, 0, len(runs))
	for _, item := range runs {
		items = append(items, RunView(item))
	}
	body := map[string]any{"runs": items}
	if len(items) == filter.Limit && len(items) > 0 {
		// The cursor is the last identifier of the page. ULIDs sort by mint
		// time, so a position needs no sort key of its own.
		body["next_before"] = items[len(items)-1].RunID
	}
	s.write(w, http.StatusOK, body)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, _ caller) {
	item, err := s.app.Run(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, RunView(item))
}

// runResult redirects to a presigned link rather than proxying the bytes.
//
// The object store is already reachable from wherever the caller is — it is
// where the pod wrote the result from inside a cluster — and putting every byte
// of every result through one process is how a control plane becomes a
// bandwidth bottleneck for work it did not do.
func (s *Server) runResult(w http.ResponseWriter, r *http.Request, _ caller) {
	key := r.URL.Query().Get("key")
	url, err := s.app.ResultLink(r.Context(), runv1.ULID(r.PathValue("id")), key, 15*time.Minute)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	// The link is a bearer capability with a short life, so the redirect must
	// not be cached anywhere between here and the caller.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusFound)
}

func (s *Server) runLogs(w http.ResponseWriter, r *http.Request, _ caller) {
	chunks, err := s.app.Logs(r.Context(), runv1.ULID(r.PathValue("id")),
		r.URL.Query().Get("after"), intParam(r, "limit", 100), 15*time.Minute)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	body := map[string]any{"chunks": chunks}
	if len(chunks) > 0 {
		body["next_after"] = chunks[len(chunks)-1].Key
	}
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, http.StatusOK, body)
}

// attemptResponse is one row of the ledger. It is rendered rather than passed
// through, because the store's struct is shaped for the store: an API whose
// field names change when a column is renamed is an API nobody can depend on.
type attemptResponse struct {
	Attempt   int32  `json:"attempt"`
	Epoch     int64  `json:"epoch"`
	ClusterID string `json:"cluster_id"`

	Phase        string `json:"phase,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	ExitCode     *int32 `json:"exit_code,omitempty"`
	FailureClass string `json:"failure_class,omitempty"`

	JobName  string `json:"job_name,omitempty"`
	PodName  string `json:"pod_name,omitempty"`
	NodeName string `json:"node_name,omitempty"`

	CostUSD      string `json:"cost_usd"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`

	// The two durations sit side by side because the check the contract asks
	// for is a comparison, and a comparison whose halves live in different
	// places does not get made: declared is what the pod said, observed is the
	// window the controller's lease actually covered.
	DeclaredDurationMs *int64 `json:"declared_duration_ms,omitempty"`
	ObservedDurationMs *int64 `json:"observed_duration_ms,omitempty"`

	StartedAt            *time.Time `json:"started_at,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	CompletionReceivedAt *time.Time `json:"completion_received_at,omitempty"`
}

func (s *Server) runAttempts(w http.ResponseWriter, r *http.Request, _ caller) {
	attempts, err := s.app.Attempts(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	items := make([]attemptResponse, 0, len(attempts))
	for _, a := range attempts {
		items = append(items, attemptResponse{
			Attempt: a.Attempt, Epoch: a.Epoch, ClusterID: string(a.ClusterID),
			Phase: string(a.Phase), Reason: a.Reason, Message: a.Message,
			ExitCode: a.ExitCode, FailureClass: string(a.FailureClass),
			JobName: a.JobName, PodName: a.PodName, NodeName: a.NodeName,
			CostUSD: string(a.CostUSD), InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
			DeclaredDurationMs: a.DeclaredMs, ObservedDurationMs: a.ObservedMs,
			StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
			CompletionReceivedAt: a.CompletionReceivedAt,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"attempts": items})
}

type cancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, c caller) {
	var req cancelRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	item, err := s.app.Cancel(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), req.Reason)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	// Asynchronous by construction: the backend has no path into a cluster, so
	// the instruction is recorded and delivered on the next heartbeat.
	s.write(w, http.StatusAccepted, RunView(item))
}

func (s *Server) retryRun(w http.ResponseWriter, r *http.Request, c caller) {
	item, err := s.app.Retry(r.Context(), runv1.ULID(r.PathValue("id")), c.Name())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusAccepted, RunView(item))
}
