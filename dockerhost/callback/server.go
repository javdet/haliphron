// Package callback is the endpoint agent containers post to: /completion,
// /phase and /artifacts, authenticated with the per-run callback token. The
// handlers are controller/callback's, with the row in place of the AgentRun:
// the status codes, the size limits, the key allow-list and the order of
// obligations are the same, because the image on the other end is the same.
//
// It listens on the agents bridge's gateway address, the one host address the
// firewall lets an agent reach.
package callback

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/spool"

	"github.com/automagicops/haliphron/dockerhost/state"
)

// Sink is where accepted material goes. The handlers never wait on the control
// plane: the agent is blocked on the response.
type Sink interface {
	Completion(runID runv1.ULID, epoch int64, attempt int32, report runv1.CompletionReport)
	Artifact(entry spool.Entry, body io.Reader, budget int64) (spool.Entry, error)
	Forwarded()
}

// Server receives the agents' reports.
type Server struct {
	Store          *state.Store
	Sink           Sink
	Addr           string
	Base           string
	MaxBytesPerRun int64
	Spent          func(runv1.ULID) int64
	Log            *slog.Logger
}

// BaseOf is the path prefix of a callback URL, so the routes cannot drift from
// the URL the containers were told.
func BaseOf(callbackURL string) string {
	base := ""
	if idx := strings.Index(callbackURL, "://"); idx >= 0 {
		rest := callbackURL[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			base = strings.TrimSuffix(rest[slash:], "/")
		}
	}
	return strings.TrimSuffix(base, runv1.CallbackPathCompletion)
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler is the routing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathCompletion), s.handleCompletion)
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathPhase), s.handlePhase)
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathArtifacts), s.handleArtifact)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (s *Server) path(suffix string) string { return strings.TrimSuffix(s.Base, "/") + suffix }

// Start serves until the context ends. No read or write timeout, for the
// reason controller/callback gives: /artifacts carries objects of hundreds of
// megabytes, bounded by size, not by time.
func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("callback: listen on %s: %w", s.Addr, err)
	}
	s.log().Info("the callback endpoints are listening", "addr", listener.Addr().String(), "base", s.Base)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("callback: serve: %w", err)
	}
}

func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var report runv1.CompletionReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, clusterv1.MaxRequestBytes)).Decode(&report); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "the report exceeds 1 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	if report.RunID == "" || report.Attempt < 1 {
		http.Error(w, "the report does not name its run and attempt", http.StatusBadRequest)
		return
	}
	row, ok := s.authenticatedRun(ctx, w, r, report.RunID)
	if !ok {
		return
	}
	if _, err := s.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch {
			return false
		}
		recordReport(cur, report)
		return true
	}); err != nil {
		// Bookkeeping: the report is still forwarded, and it is already in
		// storage as completion.json.
		s.log().Error("recording a completion", "runID", report.RunID, "error", err)
	}
	s.Sink.Completion(row.RunID, row.Epoch, report.Attempt, report)
	w.WriteHeader(http.StatusAccepted)
}

// recordReport puts the pointers on the row. Pointers only.
func recordReport(r *state.Run, report runv1.CompletionReport) {
	r.Status.Result = &state.ResultRefs{
		Result: report.ResultRef,
		Output: report.OutputRef,
		Log:    report.LogRef,
		Completion: &runv1.ObjectRef{
			Key: fmt.Sprintf(runv1.StoragePrefixRun, report.RunID) + runv1.StorageKeyCompletion,
		},
		Relayed: r.ArtifactMode() != runv1.ArtifactModeObjectStore,
	}
	r.Status.CompletedPhases = UnionPhases(r.Status.CompletedPhases, report.CompletedPhases)
	if report.Repo != nil && report.Repo.PRURL != "" {
		r.Status.PRURL = report.Repo.PRURL
	}
	if report.Usage != nil {
		r.Status.Usage = report.Usage
	}
	r.Status.ResultReported = true
}

// handlePhase records one entrypoint phase. Only a phase that finished cleanly
// goes on the record: that record is what lets the next attempt skip paid work.
func (s *Server) handlePhase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var report runv1.PhaseReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&report); err != nil {
		http.Error(w, "malformed phase report", http.StatusBadRequest)
		return
	}
	if report.RunID == "" || report.Phase == "" {
		http.Error(w, "the report does not name its run and phase", http.StatusBadRequest)
		return
	}
	row, ok := s.authenticatedRun(ctx, w, r, report.RunID)
	if !ok {
		return
	}
	if report.Outcome != runv1.PhaseOutcomeOK {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if _, err := s.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch {
			return false
		}
		merged := UnionPhases(cur.Status.CompletedPhases, []runv1.RuntimePhase{report.Phase})
		if len(merged) == len(cur.Status.CompletedPhases) {
			return false
		}
		cur.Status.CompletedPhases = merged
		return true
	}); err != nil {
		// 503 so the agent retries: this record keeps the next attempt from
		// paying for the model again.
		s.log().Error("recording a completed phase", "runID", report.RunID, "phase", report.Phase, "error", err)
		http.Error(w, "could not record the phase", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleArtifact spools one object and does not answer until it is durable on
// this host's disk: a 200 is the agent's permission to exit.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	key := q.Get(runv1.QueryKey)
	runID := runv1.ULID(q.Get(runv1.QueryRunID))
	if runID == "" || key == "" {
		http.Error(w, "an artifact must name its run and its key", http.StatusBadRequest)
		return
	}
	key, err := runv1.ArtifactKey(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	row, ok := s.authenticatedRun(ctx, w, r, runID)
	if !ok {
		return
	}
	if row.ArtifactMode() == runv1.ArtifactModeObjectStore {
		http.Error(w, "this run uploads to object storage, not to the controller", http.StatusConflict)
		return
	}
	attempt := row.CurrentAttempt()
	if n, err := strconv.ParseInt(q.Get(runv1.QueryAttempt), 10, 32); err == nil && n > 0 {
		attempt = int32(n)
	}

	// Marked as relaying before the bytes land, so the reaper can never see a
	// row with spooled artifacts and no record of them.
	s.markSpooled(ctx, row)

	budget := s.budgetFor(row)
	entry, err := s.Sink.Artifact(spool.Entry{
		RunID:       row.RunID,
		Epoch:       row.Epoch,
		Attempt:     max(attempt, 1),
		Key:         key,
		ContentType: r.Header.Get("Content-Type"),
		SHA256:      r.Header.Get(runv1.HeaderSHA256),
	}, http.MaxBytesReader(w, r.Body, clusterv1.MaxArtifactBytes), budget)
	switch {
	case errors.Is(err, spool.ErrBudgetSpent):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, spool.ErrDigestMismatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "the artifact exceeds 256 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		s.log().Error("spooling an artifact", "runID", runID, "key", key, "error", err)
		http.Error(w, "could not store the artifact", http.StatusServiceUnavailable)
		return
	}
	s.Sink.Forwarded()

	remaining := int64(0)
	if s.Spent != nil {
		if left := budget - s.Spent(row.RunID); left > 0 {
			remaining = left
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(runv1.ArtifactAck{
		Ref: runv1.ObjectRef{
			Key:         fmt.Sprintf(runv1.StoragePrefixRun, row.RunID) + entry.Key,
			SizeBytes:   entry.SizeBytes,
			SHA256:      entry.SHA256,
			ContentType: entry.ContentType,
			Uploaded:    true,
		},
		BytesRemaining: remaining,
	})
}

func (s *Server) markSpooled(ctx context.Context, row *state.Run) {
	if _, err := s.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch || (cur.Status.ArtifactsRelayed != nil && !*cur.Status.ArtifactsRelayed) {
			return false
		}
		f := false
		cur.Status.ArtifactsRelayed = &f
		return true
	}); err != nil {
		s.log().Warn("recording that artifacts were spooled", "runID", row.RunID, "error", err)
	}
}

func (s *Server) budgetFor(row *state.Run) int64 {
	if b := row.Materials.Artifacts.MaxBytesPerRun; b > 0 {
		return b
	}
	if s.MaxBytesPerRun > 0 {
		return s.MaxBytesPerRun
	}
	return clusterv1.DefaultMaxBytesPerRun
}

// authenticatedRun is the two checks every handler makes: the run is held
// here, and the caller holds its token. Constant-time, and bound to the run.
func (s *Server) authenticatedRun(ctx context.Context, w http.ResponseWriter, r *http.Request, runID runv1.ULID) (*state.Run, bool) {
	row, err := s.Store.Get(ctx, runID)
	if errors.Is(err, state.ErrNotFound) {
		s.log().Warn("a callback arrived for a run this host does not hold", "runID", runID)
		http.Error(w, "no such run on this host", http.StatusConflict)
		return nil, false
	}
	if err != nil {
		s.log().Error("reading a run for a callback", "runID", runID, "error", err)
		http.Error(w, "could not read the run", http.StatusServiceUnavailable)
		return nil, false
	}
	presented, ok := bearer(r)
	if !ok || row.CallbackToken == "" ||
		subtle.ConstantTimeCompare([]byte(presented), []byte(row.CallbackToken)) != 1 {
		s.log().Warn("a callback was refused: the token does not match the run", "runID", runID)
		http.Error(w, "the callback token does not match this run", http.StatusUnauthorized)
		return nil, false
	}
	if row.IsAbandoned() {
		http.Error(w, "this run is no longer held by this host", http.StatusConflict)
		return nil, false
	}
	return &row, true
}

func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

// UnionPhases merges two checkpoints into the contract's execution order.
func UnionPhases(existing, incoming []runv1.RuntimePhase) []runv1.RuntimePhase {
	seen := make(map[runv1.RuntimePhase]bool, len(existing)+len(incoming))
	for _, p := range existing {
		seen[p] = true
	}
	for _, p := range incoming {
		seen[p] = true
	}
	out := make([]runv1.RuntimePhase, 0, len(seen))
	for _, p := range runv1.RuntimePhases {
		if seen[p] {
			out = append(out, p)
		}
	}
	return out
}
