package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The operator's verbs, and the two reads that go to storage rather than to
// the database.

// Cancel asks for a run to stop.
//
// It is asynchronous by construction and the contract is explicit about why:
// the backend has no path into a cluster, so the instruction is recorded here
// and delivered on the next heartbeat. A run that has not been leased yet is
// cancelled outright — there is no Job to kill, and leaving it Queued would
// hand it out afterwards.
func (s *Service) Cancel(ctx context.Context, id runv1.ULID, by, reason string) (store.Run, error) {
	r, err := s.store.RequestCancel(ctx, id, by, reason, 30)
	if err != nil {
		return store.Run{}, err
	}
	if err := s.store.RevokeRunTokens(ctx, id); err != nil {
		return store.Run{}, err
	}
	s.log.Info("cancellation requested", "run", id, "by", by, "status", r.Status)
	return r, nil
}

// Retry starts a new ownership of a run that ended or got stuck.
//
// The epoch rises and the attempt resets, which is what makes the previous
// holder's late report compare as stale rather than as equal. The prompt, the
// spec and the storage prefix are the same: a retry is a replay of what was
// admitted, not a new admission.
func (s *Service) Retry(ctx context.Context, id runv1.ULID, by string) (store.Run, error) {
	r, err := s.store.Retry(ctx, id, by)
	if err != nil {
		return store.Run{}, err
	}
	s.work.broadcast()
	s.log.Info("run retried", "run", id, "by", by, "epoch", r.Epoch, "cluster", r.ClusterID)
	return r, nil
}

// Run reads one run.
func (s *Service) Run(ctx context.Context, id runv1.ULID) (store.Run, error) {
	return s.store.RunByID(ctx, id)
}

// Runs lists them.
func (s *Service) Runs(ctx context.Context, f store.RunFilter) ([]store.Run, error) {
	return s.store.ListRuns(ctx, f)
}

// ResultLink hands out a presigned GET for a run's result.
//
// A link rather than the bytes: proxying a result through the control plane
// would put every byte of every run through one process, and the object store
// is already reachable from wherever the caller is — it is where the pod wrote
// it from inside a cluster.
func (s *Service) ResultLink(ctx context.Context, id runv1.ULID, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if key == "" {
		key = runv1.StorageKeyResult
	}
	if !allowedResultKey(key) {
		return "", &run.InvalidRequestError{
			Field: "key", Detail: fmt.Sprintf("%q is not a result of a run", key)}
	}

	if _, err := s.store.RunByID(ctx, id); err != nil {
		return "", err
	}
	signed, err := s.artifacts.PresignGet(artifacts.Key(id, key), ttl)
	if err != nil {
		return "", err
	}
	return signed.URL, nil
}

// allowedResultKey bounds what a caller may ask to be signed. Without it the
// endpoint is a way to mint a capability for any key in the bucket, including
// another run's prefix, by sending "../".
func allowedResultKey(key string) bool {
	switch key {
	case runv1.StorageKeyResult, runv1.StorageKeyOutput,
		runv1.StorageKeyState, runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog:
		return true
	}
	return false
}

// LogChunk is one uploaded piece of an agent's log.
type LogChunk struct {
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	At        time.Time `json:"at"`
	URL       string    `json:"url"`
}

// Logs pages over a run's log chunks.
//
// The listing is a LIST by prefix rather than a table, which is the reason
// there is no artifacts table in the schema: the keys are derived from the run
// identifier, and an index that has to be kept in agreement with a bucket is a
// source of divergence rather than a source of answers.
func (s *Service) Logs(ctx context.Context, id runv1.ULID, after string, limit int, ttl time.Duration) ([]LogChunk, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if _, err := s.store.RunByID(ctx, id); err != nil {
		return nil, err
	}

	prefix := artifacts.RunPrefix(id) + runv1.StoragePrefixChunks
	objects, err := s.artifacts.List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	out := make([]LogChunk, 0, limit)
	for _, obj := range objects {
		// The cursor is the last key of the previous page. Chunk names sort in
		// the order they were written, so a key is a position and no separate
		// sort key is needed.
		if after != "" && obj.Key <= after {
			continue
		}
		signed, err := s.artifacts.PresignGet(obj.Key, ttl)
		if err != nil {
			return nil, err
		}
		out = append(out, LogChunk{
			Key: strings.TrimPrefix(obj.Key, prefix), SizeBytes: obj.SizeBytes,
			At: obj.LastModified, URL: signed.URL,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Attempts returns a run's ledger: one row per attempt, with what it cost.
func (s *Service) Attempts(ctx context.Context, id runv1.ULID) ([]store.Attempt, error) {
	return s.store.Attempts(ctx, id)
}

// ChildRuns returns the runs a run started through its per-run MCP token.
func (s *Service) ChildRuns(ctx context.Context, id runv1.ULID) ([]store.Run, error) {
	return s.store.ListRuns(ctx, store.RunFilter{Parent: id, Limit: 200})
}

// ResolveParent turns a per-run token into the parent a child run inherits
// from: its identifier, its depth, and the fields a child is meant to keep.
//
// This is what makes a run_agent call from inside an agent accountable. Without
// it the platform cannot tell which run is calling, and the depth limit, the
// parent's budget and the child-run rollup all lose their subject at once.
func (s *Service) ResolveParent(ctx context.Context, token store.Token) (store.Run, error) {
	if token.Kind != store.TokenKindRunMCP || token.RunID == "" {
		return store.Run{}, errors.New("app: not a per-run token")
	}
	parent, err := s.store.RunByID(ctx, token.RunID)
	if err != nil {
		return store.Run{}, fmt.Errorf("resolve parent run: %w", err)
	}
	return parent, nil
}

// WaitForResult blocks until a run ends or the deadline passes.
//
// It exists for the synchronous callers — a REST client with async false, an
// MCP tool call inside another agent — and it polls rather than subscribing.
// Polling once a second is the right trade here: the alternative is a
// notification fabric between the ingest path and every waiting request
// handler, which is a large mechanism for a caller that is already prepared to
// wait minutes for an agent.
//
// The run is returned whatever state it is in when the wait ends, and the
// caller distinguishes them: a run that is still going is not a failure, it is
// a run the caller stopped waiting for.
func (s *Service) WaitForResult(ctx context.Context, id runv1.ULID, timeout time.Duration) (store.Run, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		r, err := s.store.RunByID(ctx, id)
		if err != nil {
			return store.Run{}, err
		}
		// Terminal with its report collected. A terminal phase whose
		// completion has not arrived is deliberately not the end of the wait:
		// the contents are what the caller asked for, and they are on their way
		// from storage.
		if r.ObservedPhase.IsTerminal() && r.CompletionReceivedAt != nil {
			return r, nil
		}
		if isFinishedWithoutResult(r) {
			return r, nil
		}

		select {
		case <-ticker.C:
		case <-deadline.C:
			return r, nil
		case <-ctx.Done():
			return r, ctx.Err()
		}
	}
}

// isFinishedWithoutResult covers the endings that never produce a report: a run
// cancelled before it was dispatched, and one no cluster would take.
func isFinishedWithoutResult(r store.Run) bool {
	switch r.Status {
	case "Cancelled", "Failed":
		return r.ObservedPhase == "" && r.FinishedAt != nil
	}
	return false
}
