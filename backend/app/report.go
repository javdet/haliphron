package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The heartbeat and the two ingest paths.
//
// They share applyObservation, and that sharing is a requirement of the
// contract rather than a convenience: /ingest/status is the low-latency path
// and the heartbeat is the periodic reconciliation, and if the two applied
// reports by different rules the slow one would start rolling back state the
// fast one delivered. What they do not share is what they do afterwards — a
// heartbeat renews leases and answers with commands, an ingest answers per row.

// Heartbeat does four things in one call: proves the cluster is alive, renews
// every listed lease, reconciles state, and collects commands. They are one
// call because they are one interval.
func (s *Service) Heartbeat(ctx context.Context, cluster store.Cluster,
	req clusterv1.HeartbeatRequest) (clusterv1.HeartbeatResponse, error) {

	if len(req.Runs) > clusterv1.MaxHeartbeatRuns {
		return clusterv1.HeartbeatResponse{}, &clusterv1.Problem{
			Title: "too many runs in one heartbeat", Status: 400,
			Detail: fmt.Sprintf("runs holds %d, the maximum is %d", len(req.Runs), clusterv1.MaxHeartbeatRuns),
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		}
	}

	facts := store.ClusterFacts{
		FreeSlots:         req.FreeSlots,
		CapacitySlots:     req.CapacitySlots,
		ControllerVersion: versionOf(req.Controller),
	}
	if req.Cluster != nil {
		facts.K8sVersion = req.Cluster.K8sVersion
		facts.Runtimes = req.Cluster.Runtimes
		facts.CRDVersions = req.Cluster.CRDVersions
		facts.QuotaExhausted = req.Cluster.QuotaExhausted
		if req.Cluster.NodeCount > 0 {
			count := req.Cluster.NodeCount
			facts.NodeCount = &count
		}
	}
	if err := s.store.RecordHeartbeat(ctx, cluster.ID, facts); err != nil {
		return clusterv1.HeartbeatResponse{}, err
	}

	resp := clusterv1.HeartbeatResponse{
		ServerTime: s.now(),
		Leases:     []clusterv1.LeaseRenewal{},
		Commands:   []clusterv1.Command{},
	}

	mentioned := make(map[runv1.ULID]bool, len(req.Runs))
	for _, obs := range req.Runs {
		mentioned[obs.RunID] = true

		applied, err := s.applyObservation(ctx, cluster, obs)
		if err != nil {
			return clusterv1.HeartbeatResponse{}, err
		}
		if !applied.result.Accepted {
			// Only rejected rows are answered. A response listing every
			// accepted row would be the heartbeat's largest field and say
			// nothing.
			resp.Observations = append(resp.Observations, applied.result)
			continue
		}
		if !applied.deadline.IsZero() {
			resp.Leases = append(resp.Leases, clusterv1.LeaseRenewal{
				RunID: obs.RunID, Epoch: applied.epoch, LeaseDeadline: applied.deadline,
			})
		}
	}

	active, err := s.store.ActiveRuns(ctx, cluster.ID)
	if err != nil {
		return clusterv1.HeartbeatResponse{}, err
	}

	var lost []runv1.ULID
	for _, a := range active {
		if a.CancelRequestedAt != nil && !a.Phase.IsTerminal() {
			issued := s.now()
			grace := a.CancelGrace
			if grace == 0 {
				grace = 30
			}
			resp.Commands = append(resp.Commands, clusterv1.Command{
				Type: clusterv1.CommandCancel, RunID: a.RunID, Epoch: a.Epoch,
				Reason: a.CancelReason, IssuedAt: &issued, GracePeriodSeconds: grace,
			})
		}

		// Silence means something only under reportComplete. Without the flag,
		// "I do not have this run" and "I have not told you about it yet" are
		// the same message, and reacting to the second is how a controller
		// whose informer cache is still warming gets its work taken away.
		if req.ReportComplete && !mentioned[a.RunID] && a.Acked() && a.Status != clusterv1.StatusUnknown {
			lost = append(lost, a.RunID)
		}
	}

	if len(lost) > 0 {
		// This is what turns "the agent namespace was recreated" from a batch
		// of manual triage into one heartbeat. Unknown rather than requeued:
		// the work may be running this second, and nobody can say whether it
		// is. observed_phase is left where it was, so the run recovers by
		// itself if the controller comes back with the same epoch.
		if err := s.store.MarkUnknown(ctx, lost, "NotReportedByCluster"); err != nil {
			return clusterv1.HeartbeatResponse{}, err
		}
		s.log.Warn("cluster did not mention runs it is believed to hold",
			"cluster", cluster.ID, "count", len(lost))
		resp.UnknownRuns = lost
	}

	return resp, nil
}

// IngestStatus applies a batch of observations, row by row.
//
// Partial success is the normal outcome: failing the whole request over one
// stale row would lose every other report in the batch, and a reconcile burst
// is exactly when a batch is large.
func (s *Service) IngestStatus(ctx context.Context, cluster store.Cluster,
	req clusterv1.StatusIngestRequest) (clusterv1.StatusIngestResponse, error) {

	if len(req.Reports) == 0 || len(req.Reports) > clusterv1.MaxStatusReports {
		return clusterv1.StatusIngestResponse{}, &clusterv1.Problem{
			Title: "invalid batch", Status: 400,
			Detail: fmt.Sprintf("reports must hold between 1 and %d items", clusterv1.MaxStatusReports),
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		}
	}

	resp := clusterv1.StatusIngestResponse{
		Results: make([]clusterv1.StatusIngestResult, 0, len(req.Reports)),
	}
	for _, obs := range req.Reports {
		applied, err := s.applyObservation(ctx, cluster, obs)
		if err != nil {
			return clusterv1.StatusIngestResponse{}, err
		}
		resp.Results = append(resp.Results, applied.result)
	}
	return resp, nil
}

// appliedObservation is what the two callers need out of one application.
type appliedObservation struct {
	result   clusterv1.StatusIngestResult
	epoch    int64
	deadline time.Time
}

func (s *Service) applyObservation(ctx context.Context, cluster store.Cluster,
	obs clusterv1.RunObservation) (appliedObservation, error) {

	applied, err := s.store.ApplyObservation(ctx, cluster.ID, obs, s.leaseTTL())
	if errors.Is(err, store.ErrNotFound) {
		// A run this control plane has no record of. Abandon: the controller
		// is holding work that cannot be reconciled with anything here, and
		// reporting on it forever helps nobody.
		return appliedObservation{result: clusterv1.StatusIngestResult{
			RunID: obs.RunID, Accepted: false,
			Code: clusterv1.CodeRunNotFound, Action: clusterv1.ActionAbandon,
		}}, nil
	}
	if err != nil {
		return appliedObservation{}, err
	}

	result := clusterv1.StatusIngestResult{
		RunID:         obs.RunID,
		Accepted:      applied.Decision.Accepted(),
		AppliedStatus: applied.Status,
		Code:          applied.Decision.Code,
		Action:        applied.Decision.Action,
		CurrentEpoch:  applied.Epoch,
	}

	if !result.Accepted {
		if applied.Decision.Code == clusterv1.CodeEpochMismatch {
			s.log.Warn("report under a stale epoch",
				"run", obs.RunID, "cluster", cluster.ID,
				"epoch", obs.Epoch, "current", applied.Epoch)
		}
		return appliedObservation{result: result, epoch: applied.Epoch}, nil
	}

	if obs.Phase.IsTerminal() {
		// The result was in storage before the callback was made, so a
		// terminal status without a report is a read the backend owes itself
		// rather than a loss. Trying immediately keeps the common case — the
		// controller died between the webhook and the ingest — from waiting
		// for the next sweep.
		if applied.Status == clusterv1.StatusCompletedWithoutResult {
			recovered, err := s.recoverResult(ctx, store.PendingResult{
				RunID: obs.RunID, Epoch: applied.Epoch, Attempt: obs.Attempt, Phase: obs.Phase,
			})
			if err != nil {
				s.log.Error("could not recover a result from storage",
					"run", obs.RunID, "error", err)
			} else if recovered {
				result.AppliedStatus = run.StoredStatus(obs.Phase)
			}
		}
		if err := s.store.RevokeRunTokens(ctx, obs.RunID); err != nil {
			return appliedObservation{}, err
		}
	}

	return appliedObservation{
		result: result, epoch: applied.Epoch, deadline: applied.LeaseDeadline,
	}, nil
}

// IngestCompletion accepts the pod's report, forwarded unedited.
//
// It is an optimisation rather than a correctness condition: the result is in
// storage before the callback is made (ADR 15), so a completion that never
// arrives costs a read of runs/{runID}/ and not the result.
func (s *Service) IngestCompletion(ctx context.Context, cluster store.Cluster,
	req clusterv1.CompletionIngestRequest) (clusterv1.CompletionIngestResponse, error) {

	if req.Completion.RunID != "" && req.Completion.RunID != req.RunID {
		// The report names a different run than the envelope. The duplication
		// exists so completion.json can be read from storage without an
		// envelope, and a mismatch means the wrong object was read.
		return clusterv1.CompletionIngestResponse{}, &clusterv1.Problem{
			Title: "completion does not match its envelope", Status: 400,
			Detail: "completion.runID names a different run",
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal, RunID: req.RunID,
		}
	}

	raw, err := json.Marshal(req.Completion)
	if err != nil {
		return clusterv1.CompletionIngestResponse{}, fmt.Errorf("re-encode completion for %s: %w", req.RunID, err)
	}

	outcome, err := s.store.ApplyCompletion(ctx, store.Completion{
		RunID: req.RunID, ClusterID: cluster.ID, Epoch: req.Epoch, Attempt: req.Attempt,
		ReceivedAt: req.ReceivedAt, Report: req.Completion, Raw: raw,
	})
	if err != nil {
		return clusterv1.CompletionIngestResponse{}, s.problemFor(req.RunID, err)
	}

	if outcome.Duplicate {
		// Charged once. This call is retried on any network error, and a sum
		// that grows per retry is a bill that grows per retry.
		return clusterv1.CompletionIngestResponse{
			RunID: req.RunID, Accepted: true, Duplicate: true, AppliedStatus: outcome.Status,
		}, nil
	}

	if err := s.auditUsage(ctx, cluster, req, outcome); err != nil {
		return clusterv1.CompletionIngestResponse{}, err
	}
	if err := s.store.RevokeRunTokens(ctx, req.RunID); err != nil {
		return clusterv1.CompletionIngestResponse{}, err
	}
	s.log.Info("completion accepted",
		"run", req.RunID, "attempt", req.Attempt, "status", req.Completion.Status)

	commands, err := s.commandsFor(ctx, req.RunID)
	if err != nil {
		return clusterv1.CompletionIngestResponse{}, err
	}
	return clusterv1.CompletionIngestResponse{
		RunID: req.RunID, Accepted: true, AppliedStatus: outcome.Status, Commands: commands,
	}, nil
}

// auditUsage records the pod's self-declared duration against the window the
// controller's lease actually covered.
//
// The phase 1 mitigation for self-reported cost is not a block: a threshold
// that refuses honest runs is worse than one that records dishonest ones, and
// the real answer — metering at an LLM proxy — is outside v1. What this buys is
// a record an operator can go and read.
func (s *Service) auditUsage(ctx context.Context, cluster store.Cluster,
	req clusterv1.CompletionIngestRequest, outcome store.CompletionOutcome) error {

	const tolerance = int64(60_000)
	if outcome.ObservedMs == 0 || outcome.DeclaredMs <= outcome.ObservedMs+tolerance {
		return nil
	}
	s.log.Warn("self-declared duration exceeds the observed window",
		"run", req.RunID, "declaredMs", outcome.DeclaredMs, "observedMs", outcome.ObservedMs)
	return s.store.Audit(ctx, store.AuditEntry{
		Actor: string(cluster.ID), ActorKind: "cluster", Action: store.AuditUsageDivergence,
		SubjectKind: "run", SubjectID: string(req.RunID),
		RunID: req.RunID, ClusterID: cluster.ID,
		Payload: map[string]any{
			"declaredMs": outcome.DeclaredMs, "observedMs": outcome.ObservedMs,
			"attempt": req.Attempt,
		},
	})
}

// recoverResult reads runs/{id}/completion.json and applies it.
//
// completion.json exists for exactly this: the webhook and its forwarding both
// live in the controller's memory, and a crash between them loses the cost and
// the PR link, which neither result.md nor output.json carries.
func (s *Service) recoverResult(ctx context.Context, p store.PendingResult) (bool, error) {
	raw, err := s.artifacts.Get(ctx, artifacts.Key(p.RunID, runv1.StorageKeyCompletion))
	if errors.Is(err, artifacts.ErrNotFound) {
		// Not there yet, or never written. Either way this is a run to look at
		// again later, not a failure to report now.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var report runv1.CompletionReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return false, fmt.Errorf("decode stored completion for %s: %w", p.RunID, err)
	}
	if report.RunID != "" && report.RunID != p.RunID {
		return false, fmt.Errorf("stored completion under %s names run %s", p.RunID, report.RunID)
	}

	attempt := report.Attempt
	if attempt == 0 {
		attempt = p.Attempt
	}
	if _, err := s.store.ApplyCompletion(ctx, store.Completion{
		RunID: p.RunID, Epoch: p.Epoch, Attempt: attempt,
		Report: report, Raw: raw,
	}); err != nil {
		return false, err
	}
	s.log.Info("result recovered from storage", "run", p.RunID, "attempt", attempt)
	return true, nil
}

func versionOf(health *clusterv1.ControllerHealth) string {
	if health == nil {
		return ""
	}
	return health.Version
}
