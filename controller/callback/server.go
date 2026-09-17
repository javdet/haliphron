// Package callback is the twelve inches between the pod and the controller: an
// HTTP endpoint inside the cluster that the agent posts its report to.
//
// It exists because the pod has no credential for the control plane and must
// not have one — it is the least trusted component in the system, executing
// text from outside with tools that text chose. So it reports to the controller,
// which forwards the bytes unchanged.
//
// Losing a report here costs nothing structural: the same document is already
// in storage as completion.json, written before this call was made. That
// ordering is what makes the webhook an optimisation rather than a correctness
// condition, and it is why this handler is allowed to be simple.
package callback

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
)

// Sink is where an accepted report goes. It is an interface because this
// handler must never wait on the control plane: the pod is blocked on the
// response, and an agent container that cannot exit because a backend is down
// is a worse failure than a report delivered a minute late.
type Sink interface {
	Completion(runID runv1.ULID, epoch int64, attempt int32, report runv1.CompletionReport)
}

// Server receives completion reports from agent pods.
type Server struct {
	K8s       client.Client
	Namespace string
	Sink      Sink
	Addr      string
	// Path is the URL path the pods were told to post to, taken from the same
	// callback URL that goes into the CR.
	Path  string
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
	Clock       func() time.Time
	Log         *slog.Logger
}

// New builds the server, taking the path from the callback URL so that a chart
// which changes one changes both.
func New(cfg Config) (*Server, error) {
	if cfg.K8s == nil || cfg.Sink == nil {
		return nil, errors.New("callback: K8s and Sink are required")
	}
	path := "/completion"
	if cfg.CallbackURL != "" {
		if idx := strings.Index(cfg.CallbackURL, "://"); idx >= 0 {
			rest := cfg.CallbackURL[idx+3:]
			if slash := strings.Index(rest, "/"); slash >= 0 {
				path = rest[slash:]
			}
		}
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
		Addr: cfg.Addr, Path: path, Clock: cfg.Clock, Log: cfg.Log,
	}, nil
}

// NeedLeaderElection is false: the pod reaches this through a Service, which
// picks a replica without asking who leads.
func (s *Server) NeedLeaderElection() bool { return false }

// Handler is the routing, exposed so that it can be exercised without a
// listening socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+s.Path, s.handleCompletion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Start serves until the context ends.
func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Handler: s.Handler(),
		// Generous but finite: the body is at most a megabyte and the client is
		// inside the cluster.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
	}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("callback: listen on %s: %w", s.Addr, err)
	}
	s.Log.Info("the completion endpoint is listening", "addr", listener.Addr().String(), "path", s.Path)

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

	var cr agentrunv1alpha1.AgentRun
	err := s.K8s.Get(ctx, types.NamespacedName{
		Namespace: s.Namespace, Name: agentrunv1alpha1.ObjectName(report.RunID),
	}, &cr)
	if apierrors.IsNotFound(err) {
		// Whatever this pod belongs to, it is not a run this controller holds.
		// A repeat will not change that, so the answer is one the pod stops on.
		s.Log.Warn("a completion arrived for a run this cluster does not hold", "runID", report.RunID)
		http.Error(w, "no such run in this cluster", http.StatusConflict)
		return
	}
	if err != nil {
		s.Log.Error("reading a run for a completion", "runID", report.RunID, "error", err)
		http.Error(w, "could not read the run", http.StatusServiceUnavailable)
		return
	}

	if !s.authenticate(ctx, r, &cr) {
		s.Log.Warn("a completion was refused: the callback token does not match the run",
			"runID", report.RunID)
		http.Error(w, "the callback token does not match this run", http.StatusUnauthorized)
		return
	}

	if err := s.recordReport(ctx, &cr, report); err != nil {
		s.Log.Error("recording a completion on the AgentRun", "runID", report.RunID, "error", err)
		// The report itself is still worth forwarding, and it is already in
		// storage; a failed status write is not the pod's problem.
	}

	s.Sink.Completion(cr.Spec.RunID, cr.Spec.LeaseEpoch, report.Attempt, report)
	w.WriteHeader(http.StatusAccepted)
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
		State:  report.StateRef,
		Completion: &runv1.ObjectRef{
			Bucket: bucketOf(report),
			Key:    fmt.Sprintf(runv1.StoragePrefixRun, report.RunID) + runv1.StorageKeyCompletion,
		},
	}
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

// bucketOf takes the bucket from whichever reference the pod managed to write.
// The completion object's own key is fixed by the storage layout, but the
// bucket is an installation's choice and the controller never sees the bundle.
func bucketOf(report runv1.CompletionReport) string {
	for _, ref := range []*runv1.ObjectRef{report.ResultRef, report.OutputRef, report.StateRef, report.LogRef} {
		if ref != nil && ref.Bucket != "" {
			return ref.Bucket
		}
	}
	return ""
}
