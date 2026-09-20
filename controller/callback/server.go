// Package callback is the twelve inches between the pod and the controller: the
// in-cluster HTTP endpoint an agent posts to.
//
// It exists because the pod has no credential for the control plane and must
// not have one — it is the least trusted component in the system, executing
// text from outside with tools that text chose. So it reports to the
// controller, which forwards what it was given unchanged.
//
// Three things arrive here, on three paths, all authenticated with the same
// per-run callback token:
//
//	/completion  the report, posted last
//	/phase       one entrypoint phase, as it completes
//	/artifacts   one object, bytes in the body — relay mode only
//
// The three are not equally important and the handlers say so. Losing a
// completion costs nothing structural: the same document is already durable as
// completion.json, written before the call was made, which is what makes the
// webhook an optimisation rather than a correctness condition. Losing a phase
// report costs a retry paying for the model again. Losing an artifact costs the
// result, which is why /artifacts is the one endpoint here that does not answer
// until the bytes are on a disk that is not the pod's — that acknowledgement is
// the whole of principle P5 in relay mode.
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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/spool"
)

// Sink is where accepted material goes. It is an interface because these
// handlers must never wait on the control plane: the pod is blocked on the
// response, and an agent container that cannot exit because a backend is down
// is a worse failure than a report delivered a minute late.
//
// Artifacts are the exception to that rule and only halfway: the handler waits
// for the spool, which is local, and never for the backend, which is not.
type Sink interface {
	Completion(runID runv1.ULID, epoch int64, attempt int32, report runv1.CompletionReport)
	// Artifact spools one object and returns what landed. It is the only
	// method here that can fail in a way the pod must hear about: an
	// acknowledgement the pod acts on has to mean the bytes are durable.
	Artifact(entry spool.Entry, body io.Reader, budget int64) (spool.Entry, error)
	// Forwarded nudges the delivery loop. Separate from Artifact so that the
	// handler's obligation ends at the spool.
	Forwarded()
}

// Server receives completion reports from agent pods.
type Server struct {
	K8s       client.Client
	Namespace string
	Sink      Sink
	Addr      string
	// Base is the URL prefix the pods were told to post under, taken from the
	// same callback URL that goes into the CR so that the two cannot drift
	// apart. Normally empty.
	Base string

	// MaxBytesPerRun is the artifact budget the backend stated in the lease.
	// Enforced here rather than by the backend so the transfer is not paid for
	// twice: this side is one hop from the pod.
	MaxBytesPerRun int64
	// Spent reports what a run has already spooled, so an acknowledgement can
	// tell the pod what is left. An entrypoint about to upload a two-gigabyte
	// log can then drop it and say so, instead of discovering the cap as a
	// refusal after the transfer.
	Spent func(runv1.ULID) int64

	Clock func() time.Time
	Log   *slog.Logger
}

// Config is what a Server needs.
type Config struct {
	K8s       client.Client
	Namespace string
	Sink      Sink
	Addr      string
	// CallbackURL is the URL handed to pods; its path is what this server
	// listens on, so that the two cannot drift apart.
	CallbackURL string
	// MaxBytesPerRun and Spent are the artifact budget and its ledger. Zero
	// means the contract's default.
	MaxBytesPerRun int64
	Spent          func(runv1.ULID) int64
	Clock          func() time.Time
	Log            *slog.Logger
}

// New builds the server, taking the path from the callback URL so that a chart
// which changes one changes both.
func New(cfg Config) (*Server, error) {
	if cfg.K8s == nil || cfg.Sink == nil {
		return nil, errors.New("callback: K8s and Sink are required")
	}
	// The base path the pods were told to post under, taken from the same
	// callback URL that goes into the CR so that the two cannot drift apart.
	// Normally empty — the URL is a bare Service — and non-empty when an
	// installation puts the controller behind a path prefix.
	base := ""
	if cfg.CallbackURL != "" {
		if idx := strings.Index(cfg.CallbackURL, "://"); idx >= 0 {
			rest := cfg.CallbackURL[idx+3:]
			if slash := strings.Index(rest, "/"); slash >= 0 {
				base = strings.TrimSuffix(rest[slash:], "/")
			}
		}
		// A URL that still names the completion endpoint is a chart written
		// against the older contract, where the CR carried one path rather
		// than a base. Accepting it keeps such a chart working; the pods it
		// starts read the base out of the CR either way.
		base = strings.TrimSuffix(base, runv1.CallbackPathCompletion)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8083"
	}
	return &Server{
		K8s: cfg.K8s, Namespace: cfg.Namespace, Sink: cfg.Sink,
		Addr: cfg.Addr, Base: base, Clock: cfg.Clock, Log: cfg.Log,
		MaxBytesPerRun: cfg.MaxBytesPerRun, Spent: cfg.Spent,
	}, nil
}

// NeedLeaderElection is false: the pod reaches this through a Service, which
// picks a replica without asking who leads.
func (s *Server) NeedLeaderElection() bool { return false }

// Handler is the routing, exposed so that it can be exercised without a
// listening socket.
//
// The paths come from runv1 rather than from configuration. The CR carries a
// base URL and the entrypoint appends the same constants, so a chart that
// changes the Service cannot desynchronise the two halves — and a controller
// and an image built from one contract agree on the routes by construction.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathCompletion), s.handleCompletion)
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathPhase), s.handlePhase)
	mux.HandleFunc("POST "+s.path(runv1.CallbackPathArtifacts), s.handleArtifact)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// path joins the base the CR advertises with one of the contract's paths.
func (s *Server) path(suffix string) string {
	return strings.TrimSuffix(s.Base, "/") + suffix
}

// Start serves until the context ends.
func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Handler: s.Handler(),
		// ReadHeaderTimeout is short because a client inside the cluster sends
		// its headers at once. ReadTimeout and WriteTimeout are not set at all,
		// and that is deliberate: /artifacts carries objects that can be
		// hundreds of megabytes over a link the controller does not choose, and
		// a whole-request deadline there would cut the upload of a large log
		// and report it to the pod as a storage failure. The bounds that do
		// apply are the per-object ceiling and the per-run budget, which are
		// about size rather than about time.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("callback: listen on %s: %w", s.Addr, err)
	}
	s.Log.Info("the callback endpoints are listening",
		"addr", listener.Addr().String(), "base", s.Base)

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

// handleCompletion authenticates the pod, records what it said on the AgentRun
// and hands the report on for forwarding.
//
// The status codes are contract: the pod retries a 5xx and gives up on a 400,
// 401 or 409, because those three mean the next attempt would get the same
// answer. A 413 is the one refusal it can act on — it drops the summary and
// tries once more.
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body := http.MaxBytesReader(w, r.Body, clusterv1.MaxRequestBytes)

	var report runv1.CompletionReport
	if err := json.NewDecoder(body).Decode(&report); err != nil {
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

	cr, ok := s.authenticatedRun(ctx, w, r, report.RunID)
	if !ok {
		return
	}

	if err := s.recordReport(ctx, cr, report); err != nil {
		s.Log.Error("recording a completion on the AgentRun", "runID", report.RunID, "error", err)
		// The report itself is still worth forwarding, and it is already in
		// storage; a failed status write is not the pod's problem.
	}

	s.Sink.Completion(cr.Spec.RunID, cr.Spec.LeaseEpoch, report.Attempt, report)
	w.WriteHeader(http.StatusAccepted)
}

// handlePhase records one entrypoint phase as the pod passes it.
//
// This is what replaced runs/{id}/state.json, and the difference is not only
// where the checkpoint lives. The object was written by the pod and read back
// by the next attempt, so a pod killed between two phases recorded nothing: the
// checkpoint reflected the last time the pod chose to save it. A report at the
// moment a phase completes is recorded by something that outlives the pod, so
// an OOM between two phases still leaves the one that finished on the record.
//
// It answers 202 and nothing else useful. The pod does not wait on the
// controller's status patch: a phase report that blocked would put the
// controller's API-server latency inside the agent's own budget.
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

	cr, ok := s.authenticatedRun(ctx, w, r, report.RunID)
	if !ok {
		return
	}

	// Only a phase that finished cleanly goes on the record. A failed or
	// skipped phase is not something a later attempt may assume was done, and
	// the one phase whose replay costs money is exactly the one where getting
	// this wrong would skip a model call that never happened.
	if report.Outcome != runv1.PhaseOutcomeOK {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	patch := client.MergeFrom(cr.DeepCopy())
	merged := unionPhases(cr.Status.CompletedPhases, []runv1.RuntimePhase{report.Phase})
	if len(merged) == len(cr.Status.CompletedPhases) {
		// Nothing new. Phase reports are retried by the pod on a network error,
		// and a patch per repeat is a write per retry against etcd.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	cr.Status.CompletedPhases = merged
	if err := s.K8s.Status().Patch(ctx, cr, patch); err != nil {
		// Reported to the pod as a 503 so that it retries: this record is what
		// keeps the next attempt from paying for the model again, and it is
		// worth one more round trip.
		s.Log.Error("recording a completed phase", "runID", report.RunID,
			"phase", report.Phase, "error", err)
		http.Error(w, "could not record the phase", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleArtifact spools one object and acknowledges it.
//
// It is the one endpoint in this package that must not answer until its work is
// durable. Everything else here is an optimisation over a copy that already
// exists somewhere else; this *is* the copy. The pod treats a 200 as permission
// to exit, so a 200 has to mean the bytes are on a disk that is not the pod's.
//
// The run prefix is not taken from the request. The pod names a key relative to
// its own run — "result.md", "logs/chunks/7.log" — and the controller stamps
// the run onto it from the CR it just authenticated against. A pod that could
// name an absolute key could name another run's.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	q := r.URL.Query()
	key := q.Get(runv1.QueryKey)
	runID := runv1.ULID(q.Get(clusterv1.QueryRunID))
	if runID == "" || key == "" {
		http.Error(w, "an artifact must name its run and its key", http.StatusBadRequest)
		return
	}

	// The same check the backend makes, from the same function. A key this
	// controller accepts and the backend refuses is an object spooled,
	// forwarded, rejected and dropped — with the pod long gone and no way to
	// tell it that the result it thought was safe never landed.
	key, err := runv1.ArtifactKey(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cr, ok := s.authenticatedRun(ctx, w, r, runID)
	if !ok {
		return
	}
	if cr.Spec.ArtifactMode == runv1.ArtifactModeObjectStore {
		// The pod should have PUT this to the store. A 409 rather than a 400:
		// the request is well formed and the disagreement is about
		// configuration, and it is not a request a retry improves.
		s.Log.Warn("an artifact was relayed for a run configured for object storage",
			"runID", runID, "key", key)
		http.Error(w, "this run uploads to object storage, not to the controller", http.StatusConflict)
		return
	}

	attempt := cr.Status.Attempt
	if n, err := strconv.ParseInt(q.Get(runv1.QueryAttempt), 10, 32); err == nil && n > 0 {
		attempt = int32(n)
	}

	entry, err := s.Sink.Artifact(spool.Entry{
		RunID:       cr.Spec.RunID,
		Epoch:       cr.Spec.LeaseEpoch,
		Attempt:     max(attempt, 1),
		Key:         key,
		ContentType: r.Header.Get("Content-Type"),
		SHA256:      r.Header.Get(runv1.HeaderSHA256),
	}, http.MaxBytesReader(w, r.Body, clusterv1.MaxArtifactBytes), s.budgetFor(cr))

	switch {
	case errors.Is(err, spool.ErrBudgetSpent):
		// 413, which is the one refusal the entrypoint can act on: it drops the
		// object, notes it in the log and carries on rather than failing a run
		// over an attachment.
		s.Log.Warn("a run reached its artifact budget", "runID", runID, "key", key)
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, spool.ErrDigestMismatch):
		// The transfer was corrupted or cut. A retry sends the same bytes
		// again, so this is worth one.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "the artifact exceeds 256 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		s.Log.Error("spooling an artifact", "runID", runID, "key", key, "error", err)
		http.Error(w, "could not store the artifact", http.StatusServiceUnavailable)
		return
	}

	s.recordSpooled(ctx, cr)
	// The forwarding happens after the answer. The pod has what it needs — the
	// bytes are durable — and making it wait for the control plane would undo
	// the reason the pod talks to the controller rather than to the backend.
	s.Sink.Forwarded()

	s.write(w, runv1.ArtifactAck{
		Ref: runv1.ObjectRef{
			Key:         fmt.Sprintf(runv1.StoragePrefixRun, cr.Spec.RunID) + entry.Key,
			SizeBytes:   entry.SizeBytes,
			SHA256:      entry.SHA256,
			ContentType: entry.ContentType,
			Uploaded:    true,
		},
		BytesRemaining: s.remainingFor(cr, entry.RunID),
	})
}

// budgetFor is what this run may store, as the backend stated it in the lease.
//
// Read off the CR rather than from this controller's configuration, because the
// cap is the control plane's policy: a controller that substituted its own
// would enforce a limit the backend never agreed to. It is read off the CR
// rather than held in memory for a second reason — a controller restart must
// not hand a run its whole allowance again, which is also why the spool rebuilds
// its ledger from the volume at startup.
//
// MaxBytesPerRun on the Server is the fallback for a CR written before the
// field existed.
func (s *Server) budgetFor(cr *agentrunv1alpha1.AgentRun) int64 {
	if cr.Spec.MaxArtifactBytes > 0 {
		return cr.Spec.MaxArtifactBytes
	}
	if s.MaxBytesPerRun > 0 {
		return s.MaxBytesPerRun
	}
	return clusterv1.DefaultMaxBytesPerRun
}

func (s *Server) remainingFor(cr *agentrunv1alpha1.AgentRun, id runv1.ULID) int64 {
	if s.Spent == nil {
		return 0
	}
	if remaining := s.budgetFor(cr) - s.Spent(id); remaining > 0 {
		return remaining
	}
	return 0
}

// recordSpooled marks the relay as begun but not finished.
//
// False rather than absent, and it matters: the TTL reaper leaves an AgentRun
// alone while this condition is False, so a run whose artifacts are spooled and
// not yet forwarded cannot have its CR collected out from under the spool that
// is still holding them.
func (s *Server) recordSpooled(ctx context.Context, cr *agentrunv1alpha1.AgentRun) {
	if c := meta.FindStatusCondition(cr.Status.Conditions, agentrunv1alpha1.ConditionArtifactsRelayed); c != nil {
		return
	}
	patch := client.MergeFrom(cr.DeepCopy())
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               agentrunv1alpha1.ConditionArtifactsRelayed,
		Status:             metav1.ConditionFalse,
		Reason:             agentrunv1alpha1.ReasonArtifactsSpooled,
		Message:            "artifacts are on the controller's volume and not yet forwarded",
		ObservedGeneration: cr.Generation,
	})
	if err := s.K8s.Status().Patch(ctx, cr, patch); err != nil {
		// Bookkeeping. The spool is what actually holds the bytes, and the
		// forwarder reads it rather than this condition.
		s.Log.Warn("recording that artifacts were spooled", "runID", cr.Spec.RunID, "error", err)
	}
}

// authenticatedRun is the two checks every handler here makes: the run belongs
// to this cluster, and the caller holds its callback token.
//
// One function because the three endpoints must not diverge on it. A phase
// endpoint that authenticated by namespace rather than by run would let any pod
// in the agents namespace mark another run's expensive phase as done, and the
// next attempt of that run would skip its model call.
func (s *Server) authenticatedRun(ctx context.Context, w http.ResponseWriter, r *http.Request,
	runID runv1.ULID) (*agentrunv1alpha1.AgentRun, bool) {

	var cr agentrunv1alpha1.AgentRun
	err := s.K8s.Get(ctx, types.NamespacedName{
		Namespace: s.Namespace, Name: agentrunv1alpha1.ObjectName(runID),
	}, &cr)
	if apierrors.IsNotFound(err) {
		// Whatever this pod belongs to, it is not a run this controller holds.
		// A repeat will not change that, so the answer is one the pod stops on.
		s.Log.Warn("a callback arrived for a run this cluster does not hold", "runID", runID)
		http.Error(w, "no such run in this cluster", http.StatusConflict)
		return nil, false
	}
	if err != nil {
		s.Log.Error("reading a run for a callback", "runID", runID, "error", err)
		http.Error(w, "could not read the run", http.StatusServiceUnavailable)
		return nil, false
	}
	if !s.authenticate(ctx, r, &cr) {
		s.Log.Warn("a callback was refused: the token does not match the run", "runID", runID)
		http.Error(w, "the callback token does not match this run", http.StatusUnauthorized)
		return nil, false
	}
	return &cr, true
}

// write answers with JSON.
func (s *Server) write(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.Log.Warn("writing a callback response", "error", err)
	}
}

// authenticate compares the bearer token with the one the controller minted for
// this run. Constant-time, and bound to the run rather than to the namespace:
// without the binding, any pod in the agents namespace could post a forged
// completion for somebody else's work.
func (s *Server) authenticate(ctx context.Context, r *http.Request, cr *agentrunv1alpha1.AgentRun) bool {
	presented, ok := bearer(r)
	if !ok {
		return false
	}
	var secret corev1.Secret
	if err := s.K8s.Get(ctx, types.NamespacedName{
		Namespace: cr.Namespace, Name: cr.Spec.Materials.SecretName,
	}, &secret); err != nil {
		s.Log.Error("reading the per-run secret for authentication",
			"runID", cr.Spec.RunID, "error", err)
		return false
	}
	expected := secret.Data[runv1.SecretKeyCallbackToken]
	if len(expected) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), expected) == 1
}

func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

// recordReport puts the pointers on the status. Pointers only: the result text
// lives in storage, and a 64 KiB summary in etcd would be read on every
// reconcile of every run.
func (s *Server) recordReport(ctx context.Context, cr *agentrunv1alpha1.AgentRun, report runv1.CompletionReport) error {
	patch := client.MergeFrom(cr.DeepCopy())

	cr.Status.Result = &agentrunv1alpha1.ResultRefs{
		Result: report.ResultRef,
		Output: report.OutputRef,
		Log:    report.LogRef,
		Completion: &runv1.ObjectRef{
			Key: fmt.Sprintf(runv1.StoragePrefixRun, report.RunID) + runv1.StorageKeyCompletion,
		},
		// Relayed is what an operator reading a stuck run needs first: the
		// difference between "the backend has these" and "the backend can
		// fetch these".
		Relayed: cr.Spec.ArtifactMode != runv1.ArtifactModeObjectStore,
	}
	// The report's own summary of what it got through, unioned with what the
	// phase endpoint already recorded. Unioned because the two are the same
	// fact arriving twice, and a report from a pod that was killed and restarted
	// mid-phase can be shorter than what this controller already saw.
	cr.Status.CompletedPhases = unionPhases(cr.Status.CompletedPhases, report.CompletedPhases)
	if report.Repo != nil && report.Repo.PRURL != "" {
		cr.Status.PRURL = report.Repo.PRURL
	}
	if report.Usage != nil {
		cr.Status.Usage = report.Usage
	}
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               agentrunv1alpha1.ConditionResultReported,
		Status:             metav1.ConditionTrue,
		Reason:             agentrunv1alpha1.ReasonCallbackReceived,
		Message:            string(report.Status),
		ObservedGeneration: cr.Generation,
	})
	if err := s.K8s.Status().Patch(ctx, cr, patch); err != nil {
		return fmt.Errorf("patch the status of %s: %w", cr.Name, err)
	}
	return nil
}

// unionPhases merges two checkpoints into the contract's execution order.
//
// Unioned rather than replaced, and ordered by the contract rather than by
// arrival, for the same reason the column in run_attempts is: the resume rule
// is "every phase before the first unfinished one is done", which is only
// meaningful against a fixed sequence, and a later report that happened to be
// shorter must not shorten what is already known.
func unionPhases(existing, incoming []runv1.RuntimePhase) []runv1.RuntimePhase {
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
