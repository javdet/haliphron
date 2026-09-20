package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Runs: admission, placement, the operator's two verbs, and the reads the API
// and the UI are built on.
//
// Admission writes the three groups of columns exactly once between them:
// spec and the promoted fields are frozen by the guard trigger from the moment
// of the INSERT, ownership is written by the lease path, and observation by
// the report path. The separation is the whole design of the table, and it is
// why nothing here touches status beyond the initial Queued.

// NewRun is an admitted run, ready to be written.
type NewRun struct {
	ID          runv1.ULID
	ParentRunID runv1.ULID
	Depth       int16

	CreatedBy  string
	CreatedVia string
	Priority   int32

	Spec json.RawMessage
	// Prompt is the task, and the reason the artifact store is not on the path
	// to starting a run any more. Bounded by run.MaxPromptBytes at admission,
	// which is what keeps it insertable and what keeps the per-run Secret the
	// controller builds from it under the 1 MiB Kubernetes cap.
	Prompt       string
	PromptSHA256 []byte

	Agent          runv1.AgentType
	Model          string
	Role           string
	RepoURL        string
	RepoProvider   runv1.GitProvider
	BaseBranch     string
	TargetBranch   string
	TimeoutSeconds int32
	MaxCostUSD     runv1.MoneyUSD

	// ClusterID is placement's answer, if there was one at admission. A run
	// admitted while no cluster is eligible waits unassigned rather than being
	// refused: clusters come and go, and a request that fails because a
	// controller is mid-rollout is a request that should have waited.
	ClusterID runv1.ULID
}

// InsertRun writes an admitted run.
func (s *Store) InsertRun(ctx context.Context, r NewRun) error {
	provider := r.RepoProvider
	if provider == "" {
		provider = runv1.GitProviderNone
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, parent_run_id, depth, created_by, created_via, priority,
		                  spec, prompt, prompt_sha256, agent, model, role_name,
		                  repo_url, repo_provider, base_branch, target_branch,
		                  timeout_seconds, max_cost_usd, cluster_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12,
		        $13, $14, $15, $16, $17, $18::numeric, $19, 'Queued')`,
		r.ID, nullString(string(r.ParentRunID)), r.Depth, r.CreatedBy, r.CreatedVia, r.Priority,
		[]byte(r.Spec), r.Prompt, r.PromptSHA256, string(r.Agent), r.Model, nullString(r.Role),
		nullString(r.RepoURL), string(provider), nullString(r.BaseBranch), nullString(r.TargetBranch),
		r.TimeoutSeconds, nullString(string(r.MaxCostUSD)), nullString(string(r.ClusterID)))
	if err != nil {
		return fmt.Errorf("store: insert run %s: %w", r.ID, err)
	}
	return nil
}

// Run is a run as everything outside the lease path reads it.
type Run struct {
	ID          runv1.ULID
	ParentRunID runv1.ULID
	Depth       int16

	CreatedBy  string
	CreatedVia string
	Priority   int32

	Spec         json.RawMessage
	PromptSHA256 []byte

	Agent          runv1.AgentType
	Model          string
	Role           string
	RepoURL        string
	RepoProvider   runv1.GitProvider
	BaseBranch     string
	TargetBranch   string
	TimeoutSeconds int32
	MaxCostUSD     runv1.MoneyUSD

	ClusterID     runv1.ULID
	Epoch         int64
	Attempt       int32
	AckDeadline   *time.Time
	LeaseDeadline *time.Time

	Status        string
	StatusReason  string
	StatusMessage string
	FailureClass  runv1.FailureClass
	ExitCode      *int32
	ObservedPhase runv1.Phase

	CancelRequestedAt *time.Time
	CancelReason      string

	ResultSummary string
	// ResultRef is a URI carrying its scheme — file://runs/… or s3://bucket/runs/…
	// — so a row read years later says which store wrote it rather than leaving
	// the reader to assume today's mode.
	ResultRef string
	PRURL     string
	PRNumber  *int32
	PRAction  runv1.PRAction
	CommitSHA string
	Pushed    bool

	CostUSD          runv1.MoneyUSD
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	NumTurns         int32

	CompletionReceivedAt *time.Time
	CreatedAt            time.Time
	QueuedAt             time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	UpdatedAt            time.Time
}

// ReportedStatus is the status as a caller outside the database sees it: the
// stored value, except that a terminal run whose report has not arrived is
// CompletedWithoutResult. The run_status domain deliberately has no value for
// that — it is the absence of a completion, not a state of the run — and this
// is the one place the distinction is made visible.
func (r Run) ReportedStatus() string {
	if r.ObservedPhase.IsTerminal() && r.CompletionReceivedAt == nil {
		return clusterv1.StatusCompletedWithoutResult
	}
	return r.Status
}

const runColumns = `
	SELECT id, parent_run_id, depth, created_by, created_via, priority,
	       spec, prompt_sha256, agent, model, role_name,
	       repo_url, repo_provider, base_branch, target_branch,
	       timeout_seconds, max_cost_usd,
	       cluster_id, lease_epoch, attempt, ack_deadline, lease_deadline,
	       status, status_reason, status_message, failure_class, exit_code, observed_phase,
	       cancel_requested_at, cancel_reason,
	       result_summary, result_ref, pr_url, pr_number, pr_action, commit_sha, pushed,
	       cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, num_turns,
	       completion_received_at, created_at, queued_at, started_at, finished_at, updated_at`

// RunByID reads one run.
func (s *Store) RunByID(ctx context.Context, id runv1.ULID) (Run, error) {
	row := s.db.QueryRowContext(ctx, runColumns+` FROM runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// RunFilter is the run list as the API and the UI ask for it.
type RunFilter struct {
	Status  []string
	Role    string
	Agent   runv1.AgentType
	Cluster runv1.ULID
	Parent  runv1.ULID
	Query   string
	Limit   int
	// Before pages backwards through identifiers. ULIDs sort by mint time, so
	// a cursor is the last id of the previous page and needs no sort key of
	// its own.
	Before runv1.ULID
}

// ListRuns returns a page, newest first.
//
// Neither prompt nor result_summary is selected. Both are up to 64 KiB or more
// and TOAST moves them out of line, so wide columns cost the list nothing —
// provided the list does not ask for them, which is a rule about the query
// rather than about the schema. The prompt in particular is read in exactly two
// places: the lease that hands it to a cluster, and the one detail endpoint
// that shows a user what they asked for.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if len(f.Status) > 0 {
		add("status = ANY($%d::text[]::run_status[])", textArray(f.Status))
	}
	if f.Role != "" {
		add("role_name = $%d", f.Role)
	}
	if f.Agent != "" {
		add("agent = $%d::agent_type", string(f.Agent))
	}
	if f.Cluster != "" {
		add("cluster_id = $%d", string(f.Cluster))
	}
	if f.Parent != "" {
		add("parent_run_id = $%d", string(f.Parent))
	}
	if f.Before != "" {
		add("id < $%d", string(f.Before))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// A substring match over the two fields an operator actually searches
		// by. It is a scan over the filtered set and is honest about being
		// one: full-text search over runs is a phase 2 feature that comes with
		// an index, and an index added on a hunch is an index nobody measured.
		args = append(args, q)
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(repo_url ILIKE '%%' || $%d || '%%' OR model ILIKE '%%' || $%d || '%%')", n, n))
	}

	query := runColumns + ` FROM runs`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestCancel records the intent. Cancellation is state rather than a
// message: the command is regenerated from this column on every heartbeat
// until the phase is terminal, which is why the contract gives commands no
// acknowledgement and why there is no command queue to keep in agreement.
func (s *Store) RequestCancel(ctx context.Context, id runv1.ULID, by, reason string, grace int32) (Run, error) {
	var out Run
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := lockRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if locked.Phase.IsTerminal() || isTerminalStatus(locked.Status) {
			// Already over. Saying so is more useful than accepting an
			// instruction that will never be delivered.
			return ErrRunTerminal
		}

		// A run that has not been leased yet is cancelled here and now: there
		// is no Job to kill and nothing to send a command to, and leaving it
		// Queued would hand it out afterwards.
		if locked.Status == clusterv1.StatusQueued {
			if _, err := tx.ExecContext(ctx, `
				UPDATE runs SET
					status              = 'Cancelled',
					observed_phase      = NULL,
					cancel_requested_at = now(),
					cancel_requested_by = $2,
					cancel_reason       = $3,
					cancel_grace_seconds= $4,
					finished_at         = now(),
					status_reason       = 'CancelledBeforeDispatch'
				WHERE id = $1`, id, by, nullString(reason), grace); err != nil {
				return fmt.Errorf("store: cancel queued run %s: %w", id, err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET
				cancel_requested_at  = COALESCE(cancel_requested_at, now()),
				cancel_requested_by  = $2,
				cancel_reason        = $3,
				cancel_grace_seconds = $4
			WHERE id = $1`, id, by, nullString(reason), grace); err != nil {
			return fmt.Errorf("store: request cancel of %s: %w", id, err)
		}

		if err := appendAudit(ctx, tx, AuditEntry{
			Actor: by, ActorKind: "user", Action: AuditRunCancelled,
			SubjectKind: "run", SubjectID: string(id), RunID: id,
			Payload: map[string]any{"reason": reason, "status": locked.Status},
		}); err != nil {
			return err
		}

		out, err = readRun(ctx, tx, id)
		return err
	})
	return out, err
}

// ErrRunTerminal is an operation on a run that has already ended.
var ErrRunTerminal = errors.New("store: run has already ended")

// Retry starts a new ownership of a finished or stuck run.
//
// The epoch rises because ownership is being revoked, and the attempt resets
// with it: the pair is what makes the previous holder's late report compare as
// stale rather than as equal. The observation is cleared as well — without
// that, the new attempt's Pending would lose to the old attempt's terminal
// rank and the run would never move.
func (s *Store) Retry(ctx context.Context, id runv1.ULID, by string) (Run, error) {
	var out Run
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := lockRun(ctx, tx, id)
		if err != nil {
			return err
		}

		agent, err := runAgent(ctx, tx, id)
		if err != nil {
			return err
		}
		cluster, err := pickCluster(ctx, tx, id, agent)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET
				status              = 'Queued',
				lease_epoch         = lease_epoch + 1,
				attempt             = 1,
				cluster_id          = $2,
				observed_phase      = NULL,
				status_reason       = 'OperatorRetry',
				status_message      = NULL,
				failure_class       = 'none',
				exit_code           = NULL,
				cancel_requested_at = NULL,
				cancel_requested_by = NULL,
				cancel_reason       = NULL,
				ack_deadline        = NULL,
				lease_deadline      = NULL,
				queued_at           = now(),
				started_at          = NULL,
				finished_at         = NULL,
				completion_received_at = NULL
			WHERE id = $1`, id, nullString(string(cluster))); err != nil {
			return fmt.Errorf("store: retry %s: %w", id, err)
		}

		if err := appendAudit(ctx, tx, AuditEntry{
			Actor: by, ActorKind: "user", Action: AuditRunRetried,
			SubjectKind: "run", SubjectID: string(id), RunID: id,
			Payload: map[string]any{"previousEpoch": locked.Epoch, "previousStatus": locked.Status},
		}); err != nil {
			return err
		}

		out, err = readRun(ctx, tx, id)
		return err
	})
	return out, err
}

// MarkUnknown is the backend losing sight of a run: the cluster it belongs to
// did not mention it in a complete report.
//
// observed_phase is deliberately untouched, exactly as in expire_lease. The
// rank stays where it was, so the report that arrives if the cluster comes back
// compares equal and is applied as an idempotent repeat — which clears Unknown.
// Giving Unknown a rank of its own would make that recovery a phase regression
// and the run would be stuck in it forever.
func (s *Store) MarkUnknown(ctx context.Context, ids []runv1.ULID, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE runs SET status = 'Unknown', status_reason = $2
				WHERE id = $1 AND status IN ('Dispatched', 'Starting', 'Running')`,
				id, reason); err != nil {
				return fmt.Errorf("store: mark %s unknown: %w", id, err)
			}
			if err := appendAudit(ctx, tx, AuditEntry{
				Actor: "expiry", ActorKind: "system", Action: AuditRunUnknown,
				SubjectKind: "run", SubjectID: string(id), RunID: id,
				Payload: map[string]any{"reason": reason},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ActiveRun is a run the backend believes a cluster is working on.
type ActiveRun struct {
	RunID             runv1.ULID
	Epoch             int64
	Attempt           int32
	Status            string
	Phase             runv1.Phase
	CancelRequestedAt *time.Time
	CancelReason      string
	CancelGrace       int32
}

// Acked reports whether the cluster has confirmed it holds this work. Only an
// acknowledged run may be concluded missing from a complete heartbeat: a run
// leased a moment ago is one the controller has not had a chance to mention.
func (a ActiveRun) Acked() bool { return a.Status != clusterv1.StatusLeased }

// ActiveRuns is what the backend considers this cluster to be running. It is
// the left-hand side of the heartbeat reconciliation in section 8.
func (s *Store) ActiveRuns(ctx context.Context, clusterID runv1.ULID) ([]ActiveRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, lease_epoch, attempt, status, observed_phase,
		       cancel_requested_at, cancel_reason, cancel_grace_seconds
		FROM runs
		WHERE cluster_id = $1
		  AND status IN ('Leased', 'Dispatched', 'Starting', 'Running', 'Unknown')`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("store: active runs for %s: %w", clusterID, err)
	}
	defer rows.Close()

	var out []ActiveRun
	for rows.Next() {
		var (
			a         ActiveRun
			phase     sql.NullString
			cancelled sql.NullTime
			reason    sql.NullString
			grace     sql.NullInt32
		)
		if err := rows.Scan(&a.RunID, &a.Epoch, &a.Attempt, &a.Status, &phase,
			&cancelled, &reason, &grace); err != nil {
			return nil, fmt.Errorf("store: scan active run: %w", err)
		}
		a.Phase = runv1.Phase(phase.String)
		a.CancelRequestedAt = timePtr(cancelled)
		a.CancelReason = reason.String
		a.CancelGrace = grace.Int32
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingResult is a run that ended without its report. The contents are in
// storage — the pod writes them before it calls back — so this is a read the
// backend owes itself rather than a failure.
type PendingResult struct {
	RunID   runv1.ULID
	Epoch   int64
	Attempt int32
	Phase   runv1.Phase
}

// PendingResults lists them, oldest first.
func (s *Store) PendingResults(ctx context.Context, limit int) ([]PendingResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, lease_epoch, attempt, observed_phase
		FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled')
		  AND completion_received_at IS NULL
		  AND observed_phase IS NOT NULL
		ORDER BY finished_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending results: %w", err)
	}
	defer rows.Close()

	var out []PendingResult
	for rows.Next() {
		var (
			p     PendingResult
			phase sql.NullString
		)
		if err := rows.Scan(&p.RunID, &p.Epoch, &p.Attempt, &phase); err != nil {
			return nil, fmt.Errorf("store: scan pending result: %w", err)
		}
		p.Phase = runv1.Phase(phase.String)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlaceQueued assigns a cluster to runs that are waiting for one, and returns
// how many found a home.
//
// Placement is separate from leasing because the lease statement selects by
// cluster_id: a run with no cluster is invisible to every poll, which is
// exactly the behaviour wanted while no cluster can run it. The partial index
// on (status='Queued' AND cluster_id IS NULL) is this query's index.
func (s *Store) PlaceQueued(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	placed := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, agent::text FROM runs
			WHERE status = 'Queued' AND cluster_id IS NULL
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1`, limit)
		if err != nil {
			return fmt.Errorf("store: select runs needing placement: %w", err)
		}
		type pending struct {
			id    runv1.ULID
			agent string
		}
		var waiting []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.agent); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan run needing placement: %w", err)
			}
			waiting = append(waiting, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: rows needing placement: %w", err)
		}

		for _, p := range waiting {
			cluster, err := pickCluster(ctx, tx, p.id, p.agent)
			if err != nil {
				return err
			}
			if cluster == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE runs SET cluster_id = $2 WHERE id = $1`, p.id, cluster); err != nil {
				return fmt.Errorf("store: place %s: %w", p.id, err)
			}
			placed++
		}
		return nil
	})
	return placed, err
}

// pickCluster chooses where a run should go: active, with room, able to run
// this agent, and not one that has already refused this run.
//
// The exclusion check is the reason placement is a query rather than a column
// on the run. A negative ack that did not exclude would offer the same lease to
// the same cluster forever, and the run would cycle between two states until
// somebody looked at it.
func pickCluster(ctx context.Context, tx *sql.Tx, runID runv1.ULID, agent string) (runv1.ULID, error) {
	var id sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT c.id FROM clusters c
		WHERE c.status = 'Active'
		  AND NOT c.quota_exhausted
		  AND (cardinality(c.runtimes) = 0 OR $2 = ANY (c.runtimes::text[]))
		  AND NOT EXISTS (
		        SELECT 1 FROM run_cluster_exclusions e
		        WHERE e.run_id = $1 AND e.cluster_id = c.id)
		ORDER BY c.free_slots DESC, c.last_lease_at NULLS FIRST, c.registered_at
		LIMIT 1`, runID, agent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: pick a cluster for %s: %w", runID, err)
	}
	return runv1.ULID(id.String), nil
}

func runAgent(ctx context.Context, tx *sql.Tx, id runv1.ULID) (string, error) {
	var agent string
	if err := tx.QueryRowContext(ctx, `SELECT agent::text FROM runs WHERE id = $1`, id).Scan(&agent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: read agent of %s: %w", id, err)
	}
	return agent, nil
}

func isTerminalStatus(status string) bool {
	switch status {
	case clusterv1.StatusSucceeded, clusterv1.StatusFailed,
		clusterv1.StatusTimedOut, clusterv1.StatusCancelled:
		return true
	}
	return false
}

func readRun(ctx context.Context, q queryer, id runv1.ULID) (Run, error) {
	row := q.QueryRowContext(ctx, runColumns+` FROM runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func scanRun(row rowScanner) (Run, error) {
	var (
		r          Run
		parent     sql.NullString
		role       sql.NullString
		repoURL    sql.NullString
		baseBranch sql.NullString
		target     sql.NullString
		maxCost    sql.NullString
		cluster    sql.NullString
		ackAt      sql.NullTime
		leaseAt    sql.NullTime
		reason     sql.NullString
		message    sql.NullString
		exitCode   sql.NullInt32
		phase      sql.NullString
		cancelAt   sql.NullTime
		cancelWhy  sql.NullString
		summary    sql.NullString
		resultRef  sql.NullString
		prURL      sql.NullString
		prNumber   sql.NullInt32
		prAction   sql.NullString
		commit     sql.NullString
		pushed     sql.NullBool
		completed  sql.NullTime
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	err := row.Scan(&r.ID, &parent, &r.Depth, &r.CreatedBy, &r.CreatedVia, &r.Priority,
		&r.Spec, &r.PromptSHA256, &r.Agent, &r.Model, &role,
		&repoURL, &r.RepoProvider, &baseBranch, &target,
		&r.TimeoutSeconds, &maxCost,
		&cluster, &r.Epoch, &r.Attempt, &ackAt, &leaseAt,
		&r.Status, &reason, &message, &r.FailureClass, &exitCode, &phase,
		&cancelAt, &cancelWhy,
		&summary, &resultRef, &prURL, &prNumber, &prAction, &commit, &pushed,
		&r.CostUSD, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.NumTurns,
		&completed, &r.CreatedAt, &r.QueuedAt, &startedAt, &finishedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		return Run{}, fmt.Errorf("store: scan run: %w", err)
	}

	r.ParentRunID = runv1.ULID(parent.String)
	r.Role = role.String
	r.RepoURL = repoURL.String
	r.BaseBranch = baseBranch.String
	r.TargetBranch = target.String
	r.MaxCostUSD = runv1.MoneyUSD(maxCost.String)
	r.ClusterID = runv1.ULID(cluster.String)
	r.AckDeadline = timePtr(ackAt)
	r.LeaseDeadline = timePtr(leaseAt)
	r.StatusReason = reason.String
	r.StatusMessage = message.String
	r.ExitCode = int32Ptr(exitCode)
	r.ObservedPhase = runv1.Phase(phase.String)
	r.CancelRequestedAt = timePtr(cancelAt)
	r.CancelReason = cancelWhy.String
	r.ResultSummary = summary.String
	r.ResultRef = resultRef.String
	r.PRURL = prURL.String
	r.PRNumber = int32Ptr(prNumber)
	r.PRAction = runv1.PRAction(prAction.String)
	r.CommitSHA = commit.String
	r.Pushed = pushed.Bool
	r.CompletionReceivedAt = timePtr(completed)
	r.StartedAt = timePtr(startedAt)
	r.FinishedAt = timePtr(finishedAt)
	return r, nil
}

// Prompt reads a run's task.
//
// A call of its own rather than a column in runColumns: it is up to 512 KiB,
// every list would carry it, and exactly two callers want it — the lease, which
// gets it from the lease statement, and the detail view, which is one row.
func (s *Store) Prompt(ctx context.Context, id runv1.ULID) (string, error) {
	var prompt string
	err := s.db.QueryRowContext(ctx, `SELECT prompt FROM runs WHERE id = $1`, id).Scan(&prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read prompt for %s: %w", id, err)
	}
	return prompt, nil
}

// SetResultRef records where the full result landed, scheme and all.
//
// Written by the artifact path rather than by the completion path: the pod's
// report names a key, and only the backend knows which store that key is in.
func (s *Store) SetResultRef(ctx context.Context, id runv1.ULID, ref string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET result_ref = $2 WHERE id = $1 AND result_ref IS DISTINCT FROM $2`, id, ref)
	if err != nil {
		return fmt.Errorf("store: set result ref for %s: %w", id, err)
	}
	return nil
}

// SelectCluster picks a cluster for a run that is not yet placed. It is
// pickCluster with a transaction of its own, for the admission path, which
// places a run at the moment it is written rather than waiting for the
// placement loop to come round.
func (s *Store) SelectCluster(ctx context.Context, runID runv1.ULID, agent runv1.AgentType) (runv1.ULID, error) {
	var out runv1.ULID
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		id, err := pickCluster(ctx, tx, runID, string(agent))
		out = id
		return err
	})
	return out, err
}

// VerifyOwnership is the pair of checks a run-scoped call makes when it is not
// going to write: the epoch is current and the caller is the holder. It reads
// without a lock, because a call that changes nothing has nothing to serialise
// against.
func (s *Store) VerifyOwnership(ctx context.Context, runID, clusterID runv1.ULID, epoch int64) (Run, error) {
	r, err := s.RunByID(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if epoch != r.Epoch {
		return r, &EpochError{Got: epoch, Current: r.Epoch, Status: r.Status}
	}
	if r.ClusterID != "" && r.ClusterID != clusterID {
		return r, &OwnershipError{Holder: r.ClusterID, Caller: clusterID, Current: r.Epoch}
	}
	return r, nil
}

// Attempt is one row of the ledger: what happened on one try, under one
// ownership, and what it cost.
type Attempt struct {
	Attempt   int32
	Epoch     int64
	ClusterID runv1.ULID
	Phase     runv1.Phase
	Reason    string
	Message   string

	JobName  string
	PodName  string
	NodeName string

	ExitCode     *int32
	FailureClass runv1.FailureClass

	CostUSD      runv1.MoneyUSD
	InputTokens  int64
	OutputTokens int64

	DeclaredMs *int64
	ObservedMs *int64

	StartedAt            *time.Time
	FinishedAt           *time.Time
	CompletionReceivedAt *time.Time
}

// Attempts returns a run's ledger, in the order the attempts happened.
func (s *Store) Attempts(ctx context.Context, id runv1.ULID) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt, lease_epoch, cluster_id, phase, reason, message,
		       job_name, pod_name, node_name, exit_code, failure_class,
		       cost_usd, input_tokens, output_tokens,
		       declared_duration_ms, observed_duration_ms,
		       started_at, finished_at, completion_received_at
		FROM run_attempts WHERE run_id = $1
		ORDER BY lease_epoch, attempt`, id)
	if err != nil {
		return nil, fmt.Errorf("store: read attempts of %s: %w", id, err)
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var (
			a          Attempt
			phase      sql.NullString
			reason     sql.NullString
			message    sql.NullString
			job        sql.NullString
			pod        sql.NullString
			node       sql.NullString
			exitCode   sql.NullInt32
			declared   sql.NullInt64
			observed   sql.NullInt64
			startedAt  sql.NullTime
			finishedAt sql.NullTime
			completed  sql.NullTime
		)
		if err := rows.Scan(&a.Attempt, &a.Epoch, &a.ClusterID, &phase, &reason, &message,
			&job, &pod, &node, &exitCode, &a.FailureClass,
			&a.CostUSD, &a.InputTokens, &a.OutputTokens,
			&declared, &observed, &startedAt, &finishedAt, &completed); err != nil {
			return nil, fmt.Errorf("store: scan attempt: %w", err)
		}
		a.Phase = runv1.Phase(phase.String)
		a.Reason, a.Message = reason.String, message.String
		a.JobName, a.PodName, a.NodeName = job.String, pod.String, node.String
		a.ExitCode = int32Ptr(exitCode)
		if declared.Valid {
			a.DeclaredMs = &declared.Int64
		}
		if observed.Valid {
			a.ObservedMs = &observed.Int64
		}
		a.StartedAt, a.FinishedAt = timePtr(startedAt), timePtr(finishedAt)
		a.CompletionReceivedAt = timePtr(completed)
		out = append(out, a)
	}
	return out, rows.Err()
}
