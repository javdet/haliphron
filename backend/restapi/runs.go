package restapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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
	runFields

	// Async false waits for the run to finish. The default is true, because a
	// synchronous agent run holds a connection open for minutes and most
	// callers — Slack, n8n, the UI — would rather have an identifier.
	Async *bool `json:"async,omitempty"`
	// WaitSeconds bounds a synchronous call.
	WaitSeconds int32 `json:"wait_seconds,omitempty"`
}

// runFields are what a run is, as opposed to how the caller wants to be
// answered about it. A schedule's template is the same object.
type runFields struct {
	Prompt string `json:"prompt"`

	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Role  string `json:"role,omitempty"`

	Repo         string `json:"repo,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	CreatePR     *bool  `json:"create_pr,omitempty"`
	// Repos names several repositories: repos[0] is the working one, the
	// one repo names, and the rest are cloned beside it. target_branch and
	// create_pr apply to all of them. Exclusive with repo and base_branch.
	Repos []repoField `json:"repos,omitempty"`

	TimeoutSeconds int32  `json:"timeout_seconds,omitempty"`
	MaxCostUSD     string `json:"max_cost_usd,omitempty"`
	MaxTurns       int32  `json:"max_turns,omitempty"`
	Priority       int32  `json:"priority,omitempty"`
}

// repoField is one entry of repos.
type repoField struct {
	URL        string `json:"url"`
	BaseBranch string `json:"base_branch,omitempty"`
	// Name is the directory under /repos; the URL's basename when absent.
	// Not allowed on repos[0], which is always /workspace.
	Name string `json:"name,omitempty"`
	// Depth is the clone depth, 0 for a full clone; 1 when absent. Not
	// allowed on repos[0].
	Depth *int32 `json:"depth,omitempty"`
}

func (f runFields) submitRequest() (run.SubmitRequest, error) {
	req := run.SubmitRequest{
		Prompt:         f.Prompt,
		Agent:          runv1.AgentType(f.Agent),
		Model:          f.Model,
		Role:           f.Role,
		RepoURL:        f.Repo,
		BaseBranch:     f.BaseBranch,
		TargetBranch:   f.TargetBranch,
		CreatePR:       f.CreatePR,
		TimeoutSeconds: f.TimeoutSeconds,
		MaxCostUSD:     runv1.MoneyUSD(f.MaxCostUSD),
		MaxTurns:       f.MaxTurns,
		Priority:       f.Priority,
	}
	if len(f.Repos) > 0 {
		repos := make([]run.RepoRequest, len(f.Repos))
		for i, r := range f.Repos {
			repos[i] = run.RepoRequest{URL: r.URL, BaseBranch: r.BaseBranch, Name: r.Name, Depth: r.Depth}
		}
		if err := req.SetRepos(repos); err != nil {
			return run.SubmitRequest{}, err
		}
	}
	return req, nil
}

func runFieldsOf(req run.SubmitRequest) runFields {
	f := runFields{
		Prompt:         req.Prompt,
		Agent:          string(req.Agent),
		Model:          req.Model,
		Role:           req.Role,
		Repo:           req.RepoURL,
		BaseBranch:     req.BaseBranch,
		TargetBranch:   req.TargetBranch,
		CreatePR:       req.CreatePR,
		TimeoutSeconds: req.TimeoutSeconds,
		MaxCostUSD:     string(req.MaxCostUSD),
		MaxTurns:       req.MaxTurns,
		Priority:       req.Priority,
	}
	if repos := req.Repos(); repos != nil {
		f.Repo, f.BaseBranch = "", ""
		f.Repos = make([]repoField, len(repos))
		for i, r := range repos {
			f.Repos[i] = repoField{URL: r.URL, BaseBranch: r.BaseBranch, Name: r.Name, Depth: r.Depth}
		}
	}
	return f
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

	// Repos is every repository of a run that has more than one, the working
	// one first: what it was asked to work in, and what became of each. Absent
	// for a run with one repository or none, which the fields above describe
	// completely.
	Repos []repoView `json:"repos,omitempty"`

	CostUSD      string `json:"cost_usd"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	NumTurns     int32  `json:"num_turns"`

	ParentRunID string `json:"parent_run_id,omitempty"`
	Depth       int16  `json:"depth"`
	ScheduleID  string `json:"schedule_id,omitempty"`
	// AlertReceiverID is the alert receiver whose notification started the run.
	AlertReceiverID string `json:"alert_receiver_id,omitempty"`

	CreatedBy  string     `json:"created_by"`
	CreatedVia string     `json:"created_via"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// repoView is one repository of a run with several.
type repoView struct {
	URL string `json:"url"`
	// Path is where the agent found it: /workspace for the working
	// repository, /repos/<name> for the others.
	Path         string `json:"path"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	Pushed       bool   `json:"pushed"`
	CommitSHA    string `json:"commit_sha,omitempty"`
	PRURL        string `json:"pr_url,omitempty"`
	PRNumber     *int32 `json:"pr_number,omitempty"`
	PRAction     string `json:"pr_action,omitempty"`
}

// reposView lists a run's repositories when it has more than one: the working
// one from its columns, the others from the frozen spec joined with what the
// reports said about them.
func reposView(r store.Run) []repoView {
	if len(r.Spec) == 0 {
		return nil
	}
	var spec struct {
		ExtraRepos []runv1.ExtraRepoSpec `json:"extraRepos"`
	}
	if err := json.Unmarshal(r.Spec, &spec); err != nil || len(spec.ExtraRepos) == 0 {
		return nil
	}
	out := make([]repoView, 0, len(spec.ExtraRepos)+1)
	out = append(out, repoView{
		URL: r.RepoURL, Path: runv1.MountWorkspace, BaseBranch: r.BaseBranch, TargetBranch: r.TargetBranch,
		Pushed: r.Pushed, CommitSHA: r.CommitSHA, PRURL: r.PRURL, PRNumber: r.PRNumber,
		PRAction: string(r.PRAction),
	})
	results := make(map[string]runv1.ExtraRepoResult, len(r.ExtraRepoResults))
	for _, res := range r.ExtraRepoResults {
		results[res.Path] = res
	}
	for _, extra := range spec.ExtraRepos {
		view := repoView{
			URL: extra.URL, Path: runv1.MountRepos + "/" + extra.Path,
			BaseBranch: extra.BaseBranch, TargetBranch: r.TargetBranch,
		}
		if res, ok := results[extra.Path]; ok {
			view.Pushed, view.CommitSHA = res.Pushed, res.CommitSHA
			view.PRURL, view.PRAction = res.PRURL, string(res.PRAction)
			if res.PRNumber != 0 {
				number := res.PRNumber
				view.PRNumber = &number
			}
		}
		out = append(out, view)
	}
	return out
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
		Status:          r.ReportedStatus(),
		Agent:           string(r.Agent),
		Model:           r.Model,
		Role:            r.Role,
		Repo:            r.RepoURL,
		BaseBranch:      r.BaseBranch,
		TargetBranch:    r.TargetBranch,
		ClusterID:       string(r.ClusterID),
		Epoch:           r.Epoch,
		Attempt:         r.Attempt,
		Phase:           string(r.ObservedPhase),
		FailureClass:    string(r.FailureClass),
		StatusReason:    r.StatusReason,
		StatusMessage:   r.StatusMessage,
		ExitCode:        r.ExitCode,
		ResultSummary:   r.ResultSummary,
		PRURL:           r.PRURL,
		PRNumber:        r.PRNumber,
		CommitSHA:       r.CommitSHA,
		Repos:           reposView(r),
		CostUSD:         string(r.CostUSD),
		InputTokens:     r.InputTokens,
		OutputTokens:    r.OutputTokens,
		NumTurns:        r.NumTurns,
		ParentRunID:     string(r.ParentRunID),
		Depth:           r.Depth,
		ScheduleID:      string(r.ScheduleID),
		AlertReceiverID: string(r.AlertReceiverID),
		CreatedBy:       r.CreatedBy,
		CreatedVia:      r.CreatedVia,
		CreatedAt:       r.CreatedAt,
		StartedAt:       r.StartedAt,
		FinishedAt:      r.FinishedAt,
	}
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, c caller) {
	var req createRunRequest
	raw, ok := s.decode(w, r, &req)
	if !ok {
		return
	}

	submit, err := req.submitRequest()
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	submit.CreatedBy = c.Name()
	submit.CreatedVia = c.Via()
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
		Role:          query.Get("role"),
		Agent:         runv1.AgentType(query.Get("agent")),
		Cluster:       runv1.ULID(query.Get("cluster_id")),
		Parent:        runv1.ULID(query.Get("parent_run_id")),
		Schedule:      runv1.ULID(query.Get("schedule_id")),
		AlertReceiver: runv1.ULID(query.Get("alert_receiver_id")),
		Query:         query.Get("q"),
		Limit:         intParam(r, "limit", 50),
		Before:        runv1.ULID(query.Get("before")),
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

// runResult serves a stored object, by streaming it or by redirecting to it.
//
// Which of the two depends on the artifact mode, and the difference is not
// cosmetic. With an object store the bytes are already reachable from wherever
// the caller is — it is where the pod wrote them from inside a cluster — and
// putting every byte of every result through one process is how a control plane
// becomes a bandwidth bottleneck for work it did not do. Without one the
// backend holds the volume and is the only thing that can serve it, so it does.
//
// The choice is the service's; this handler only knows which half of
// ResultAccess came back.
func (s *Server) runResult(w http.ResponseWriter, r *http.Request, _ caller) {
	key := r.URL.Query().Get("key")
	access, err := s.app.Result(r.Context(), runv1.ULID(r.PathValue("id")), key, 15*time.Minute)
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	if access.RedirectURL != "" {
		// The link is a bearer capability with a short life, so the redirect
		// must not be cached anywhere between here and the caller.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", access.RedirectURL)
		w.WriteHeader(http.StatusFound)
		return
	}

	defer func() { _ = access.Body.Close() }()
	w.Header().Set("Content-Type", access.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	// Nothing sniffs the body: a result.md is whatever an agent wrote, and a
	// browser that decides for itself that it is HTML will render it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, access.Body); err != nil {
		// The status is long gone; there is nothing to tell the caller that
		// their connection has not already told them.
		s.log.Warn("streaming a result was cut short",
			"run", r.PathValue("id"), "key", access.Key, "error", err)
	}
}

// runLogChunk streams one log chunk. Relay mode only: with an object store the
// listing hands out presigned links and no caller arrives here.
func (s *Server) runLogChunk(w http.ResponseWriter, r *http.Request, _ caller) {
	body, err := s.app.LogChunkBody(r.Context(),
		runv1.ULID(r.PathValue("id")), r.PathValue("chunk"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, body); err != nil {
		s.log.Warn("streaming a log chunk was cut short",
			"run", r.PathValue("id"), "chunk", r.PathValue("chunk"), "error", err)
	}
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

// inputResponse is one file a person sent with the request.
type inputResponse struct {
	// File is the name under /workspace/.haliphron/inputs/ and the last
	// segment of the download URL.
	File      string `json:"file"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// runInputs lists the files sent with a run.
func (s *Server) runInputs(w http.ResponseWriter, r *http.Request, _ caller) {
	inputs, err := s.app.RunInputs(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]inputResponse, 0, len(inputs))
	for _, in := range inputs {
		ref := in.Ref()
		items = append(items, inputResponse{
			File: ref.File(), Name: in.Name, MediaType: in.MediaType,
			SizeBytes: in.SizeBytes, SHA256: ref.SHA256,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"inputs": items})
}

// runInput streams one file sent with a run.
//
// Served as a download, never rendered in the UI's origin as what it sniffed
// as: the file is whatever a person attached, and an HTML or SVG one opened
// inline would be a script running with the UI's cookies.
func (s *Server) runInput(w http.ResponseWriter, r *http.Request, _ caller) {
	in, body, err := s.app.OpenRunInput(r.Context(), runv1.ULID(r.PathValue("id")), r.PathValue("file"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", app.InputServedType(in.MediaType))
	w.Header().Set("Content-Length", strconv.FormatInt(in.SizeBytes, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+in.Ref().File()+`"`)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, body); err != nil {
		s.log.Warn("streaming a run input was cut short", "run", r.PathValue("id"), "error", err)
	}
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

// deleteRun removes a run that has ended, with its attempts and its stored
// objects. The audit log keeps a record of it.
func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.app.Delete(r.Context(), runv1.ULID(r.PathValue("id")), c.Name()); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retryRun(w http.ResponseWriter, r *http.Request, c caller) {
	item, err := s.app.Retry(r.Context(), runv1.ULID(r.PathValue("id")), c.Name())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusAccepted, RunView(item))
}
