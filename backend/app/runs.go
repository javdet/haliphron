package app

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// ResultAccess is how a caller gets at one stored object. Exactly one of the
// two halves is set.
//
// Two halves rather than one, because the two modes have genuinely different
// right answers and pretending otherwise would make one of them bad. With an
// object store the caller should fetch the bytes from it directly — proxying a
// gigabyte of log through the control plane to save a redirect is not a
// simplification. Without one there is nothing to redirect to, and the backend
// that holds the volume is the only thing that can serve it.
type ResultAccess struct {
	// RedirectURL is set in object-store mode: a presigned GET the caller
	// follows.
	RedirectURL string
	// Body is set in relay mode. The caller closes it.
	Body io.ReadCloser

	Key         string
	ContentType string
	SizeBytes   int64
}

// Result opens one of a run's stored objects.
//
// The mode is resolved here rather than in the transport, so that the REST
// handler and the UI behind it are written once against "here is the object"
// instead of once per storage configuration.
func (s *Service) Result(ctx context.Context, id runv1.ULID, key string, ttl time.Duration) (ResultAccess, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if key == "" {
		key = runv1.StorageKeyResult
	}
	if !allowedResultKey(key) {
		return ResultAccess{}, &run.InvalidRequestError{
			Field: "key", Detail: fmt.Sprintf("%q is not a result of a run", key)}
	}
	if _, err := s.store.RunByID(ctx, id); err != nil {
		return ResultAccess{}, err
	}

	full := artifacts.Key(id, key)
	access := ResultAccess{Key: full, ContentType: contentTypeFor(key)}

	if s.artifacts.Mode() == runv1.ArtifactModeObjectStore {
		signed, err := s.artifacts.PresignGet(full, ttl)
		if err != nil {
			return ResultAccess{}, err
		}
		access.RedirectURL = signed.URL
		return access, nil
	}

	body, err := s.artifacts.Open(ctx, full)
	if err != nil {
		return ResultAccess{}, err
	}
	access.Body = body
	return access, nil
}

// contentTypeFor is the type of a key whose name is fixed by the layout. Only
// the four objects allowedResultKey admits reach it, so a lookup beats sniffing
// the bytes — and sniffing a result.md that begins with a code fence gets it
// wrong.
func contentTypeFor(key string) string {
	switch key {
	case runv1.StorageKeyResult:
		return "text/markdown; charset=utf-8"
	case runv1.StorageKeyOutput, runv1.StorageKeyCompletion:
		return "application/json"
	default:
		return "text/plain; charset=utf-8"
	}
}

// allowedResultKey bounds what a caller may ask for. Without it the endpoint is
// a way to read any key under the store — including another run's prefix, by
// sending "../" — and in object-store mode a way to mint a capability for one.
//
// state.json is not in the list any more because it does not exist: the
// checkpoint is a column in run_attempts, and the ledger endpoint is where a
// caller reads it.
func allowedResultKey(key string) bool {
	switch key {
	case runv1.StorageKeyResult, runv1.StorageKeyOutput,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog:
		return true
	}
	return false
}

// LogChunk is one uploaded piece of an agent's log.
type LogChunk struct {
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	At        time.Time `json:"at"`
	// URL is where to fetch it: a presigned GET in object-store mode, and this
	// backend's own chunk endpoint in relay mode. The caller follows it either
	// way and does not learn which store answered.
	URL string `json:"url"`
}

// Logs pages over a run's log chunks.
//
// The listing is a LIST by prefix rather than a table, which is the reason
// there is no artifacts table in the schema: the keys are derived from the run
// identifier, and an index that has to be kept in agreement with the store is a
// source of divergence rather than a source of answers. It reads the same way
// over a bucket and over a directory, which is most of why the mode can stay
// invisible above the port.
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
	objectStore := s.artifacts.Mode() == runv1.ArtifactModeObjectStore

	out := make([]LogChunk, 0, limit)
	for _, obj := range objects {
		// The cursor is the last key of the previous page. Chunk names sort in
		// the order they were written, so a key is a position and no separate
		// sort key is needed.
		if after != "" && obj.Key <= after {
			continue
		}
		chunk := LogChunk{
			Key: strings.TrimPrefix(obj.Key, prefix), SizeBytes: obj.SizeBytes,
			At: obj.LastModified,
		}
		if objectStore {
			signed, err := s.artifacts.PresignGet(obj.Key, ttl)
			if err != nil {
				return nil, err
			}
			chunk.URL = signed.URL
		} else {
			chunk.URL = fmt.Sprintf("/api/v1/runs/%s/logs/%s", id, chunk.Key)
		}
		out = append(out, chunk)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LogChunkBody opens one chunk. Relay mode only: with an object store the
// listing hands out presigned links and the caller never comes back here.
//
// The chunk name is checked rather than trusted. It arrives from a URL path,
// and the one thing a name from there must not be able to do is leave the
// run's own log prefix.
func (s *Service) LogChunkBody(ctx context.Context, id runv1.ULID, name string) (io.ReadCloser, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return nil, &run.InvalidRequestError{
			Field: "chunk", Detail: fmt.Sprintf("%q is not a log chunk of this run", name)}
	}
	if _, err := s.store.RunByID(ctx, id); err != nil {
		return nil, err
	}
	return s.artifacts.Open(ctx, artifacts.RunPrefix(id)+runv1.StoragePrefixChunks+name)
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
