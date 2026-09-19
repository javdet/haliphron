package clusterapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// Server is the Cluster API listener.
type Server struct {
	app *app.Service
	log *slog.Logger
}

// New builds the listener over the use cases.
func New(service *app.Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{app: service, log: logger}
}

// Handler is the routed, pre-flighted API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	p := clusterv1.BasePath
	mux.HandleFunc("POST "+p+"/register", s.register)
	mux.HandleFunc("POST "+p+"/clusters/{clusterID}/leases", s.leases)
	mux.HandleFunc("POST "+p+"/clusters/{clusterID}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST "+p+"/leases/{runID}/ack", s.ack)
	mux.HandleFunc("POST "+p+"/leases/{runID}/artifacts", s.artifacts)
	mux.HandleFunc("POST "+p+"/ingest/status", s.ingestStatus)
	mux.HandleFunc("POST "+p+"/ingest/completion", s.ingestCompletion)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.preflight(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// preflight applies what every endpoint owes the caller before it reads a
// body: the size ceiling and the version window. Both answer with an action,
// so the controller never has to infer behaviour from the number.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > clusterv1.MaxRequestBytes {
		s.problem(w, clusterv1.Problem{
			Title: "body exceeds 1 MiB", Status: http.StatusRequestEntityTooLarge,
			Code: clusterv1.CodePayloadTooLarge, Action: clusterv1.ActionFatal,
		})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, clusterv1.MaxRequestBytes)

	// Mandatory on every request, including /register: in a multi-cluster
	// installation there is otherwise no telling which version sent what, and
	// that question is always asked after the fact.
	version := r.Header.Get(clusterv1.HeaderControllerVersion)
	if version == "" {
		s.problem(w, clusterv1.Problem{
			Title:  "missing " + clusterv1.HeaderControllerVersion,
			Status: http.StatusBadRequest,
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		})
		return false
	}
	versions := s.app.Versions()
	if !versionInRange(version, versions) {
		// A version this control plane does not speak. Fatal rather than
		// retry: the controller has to be upgraded or downgraded, and a retry
		// loop against an incompatibility is a busy loop with a log line.
		s.problem(w, clusterv1.Problem{
			Title: "controller " + version + " is outside the supported range " +
				versions.Min + ".." + versions.Max,
			Status: http.StatusUnprocessableEntity,
			Code:   clusterv1.CodeUnsupportedControllerVersion, Action: clusterv1.ActionFatal,
		})
		return false
	}
	return true
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r)
	if !ok || !strings.HasPrefix(token, clusterv1.BootstrapTokenPrefix) {
		s.problem(w, clusterv1.Problem{
			Title: "missing bootstrap token", Status: http.StatusUnauthorized,
			Code: clusterv1.CodeBootstrapTokenInvalid, Action: clusterv1.ActionFatal,
		})
		return
	}

	var req clusterv1.RegisterRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.ControllerVersion == "" {
		req.ControllerVersion = r.Header.Get(clusterv1.HeaderControllerVersion)
	}

	resp, err := s.app.Register(r.Context(), token, req)
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	s.log.Info("cluster registered", "cluster", resp.ClusterID, "name", resp.Name)
	// no-store: the response carries an identity, and a cache between the
	// controller and the backend holding one is a cluster's credentials on
	// somebody's disk.
	s.writeSecret(w, http.StatusOK, resp)
}

func (s *Server) leases(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.authenticate(w, r, runv1.ULID(r.PathValue("clusterID")))
	if !ok {
		return
	}
	var req clusterv1.LeaseRequest
	if !s.decode(w, r, &req) {
		return
	}

	leases, err := s.app.Lease(r.Context(), cluster, req)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			// The controller went away mid-poll, which is ordinary: proxies
			// cut long polls and controllers restart. Nothing to answer.
			return
		}
		s.fail(w, r, "", err)
		return
	}
	if len(leases) == 0 {
		// 204 rather than an empty list, so the controller can re-poll at once
		// without parsing a body and without backoff: backoff here would turn
		// a long poll into polling.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Identifiers and a count only. This body carries a git token, a model key
	// and presigned URLs — hence no-store, and hence no body in any log line
	// at any level.
	s.log.Info("leases handed out", "cluster", cluster.ID, "count", len(leases))
	s.writeSecret(w, http.StatusOK, clusterv1.LeaseResponse{
		Leases: leases, ServerTime: time.Now(),
	})
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.PathValue("runID"))
	var req clusterv1.AckRequest
	if !s.decode(w, r, &req) {
		return
	}
	cluster, ok := s.authenticate(w, r, req.ClusterID)
	if !ok {
		return
	}

	resp, err := s.app.Ack(r.Context(), cluster, runID, req)
	if err != nil {
		s.fail(w, r, runID, err)
		return
	}
	s.write(w, http.StatusOK, resp)
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.PathValue("runID"))
	var req clusterv1.ArtifactBundleRequest
	if !s.decode(w, r, &req) {
		return
	}
	// The path names a run rather than a cluster, so ownership is checked by
	// the use case against the run's holder.
	cluster, ok := s.authenticate(w, r, "")
	if !ok {
		return
	}

	bundle, err := s.app.ArtifactBundle(r.Context(), cluster, runID, req)
	if err != nil {
		s.fail(w, r, runID, err)
		return
	}
	s.writeSecret(w, http.StatusOK, bundle)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.authenticate(w, r, runv1.ULID(r.PathValue("clusterID")))
	if !ok {
		return
	}
	var req clusterv1.HeartbeatRequest
	if !s.decode(w, r, &req) {
		return
	}

	resp, err := s.app.Heartbeat(r.Context(), cluster, req)
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	s.write(w, http.StatusOK, resp)
}

func (s *Server) ingestStatus(w http.ResponseWriter, r *http.Request) {
	var req clusterv1.StatusIngestRequest
	if !s.decode(w, r, &req) {
		return
	}
	cluster, ok := s.authenticate(w, r, req.ClusterID)
	if !ok {
		return
	}

	resp, err := s.app.IngestStatus(r.Context(), cluster, req)
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	s.write(w, http.StatusOK, resp)
}

func (s *Server) ingestCompletion(w http.ResponseWriter, r *http.Request) {
	var req clusterv1.CompletionIngestRequest
	if !s.decode(w, r, &req) {
		return
	}
	cluster, ok := s.authenticate(w, r, req.ClusterID)
	if !ok {
		return
	}

	resp, err := s.app.IngestCompletion(r.Context(), cluster, req)
	if err != nil {
		s.fail(w, r, req.RunID, err)
		return
	}
	s.write(w, http.StatusOK, resp)
}

// authenticate verifies the JWT and returns the calling cluster.
//
// wantCluster is the identity the path or the body claims; an empty value means
// the endpoint is run-scoped and ownership is checked further in.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, wantCluster runv1.ULID) (store.Cluster, bool) {
	token, ok := bearer(r)
	if !ok {
		s.unauthenticated(w, "missing bearer token", clusterv1.ActionRetry)
		return store.Cluster{}, false
	}
	header, err := parseHeader(token)
	if err != nil {
		s.unauthenticated(w, "malformed token", clusterv1.ActionRetry)
		return store.Cluster{}, false
	}

	cluster, err := s.app.Store().ClusterByKeyID(r.Context(), header.KID)
	if errors.Is(err, store.ErrNotFound) {
		// The key is not one this control plane holds. Retrying with the same
		// key cannot help; the cluster has to register again.
		s.unauthenticated(w, "unknown key "+header.KID, clusterv1.ActionReregister)
		return store.Cluster{}, false
	}
	if err != nil {
		s.fail(w, r, "", err)
		return store.Cluster{}, false
	}

	claims, err := verify(token, cluster.PublicKey)
	if err != nil {
		s.unauthenticated(w, "signature does not verify", clusterv1.ActionRetry)
		return store.Cluster{}, false
	}
	switch {
	case !claims.hasAudience(clusterv1.TokenAudience):
		s.unauthenticated(w, "wrong audience", clusterv1.ActionRetry)
		return store.Cluster{}, false
	case !claims.lifetimeOK(time.Now(),
		clusterv1.TokenMaxTTLSeconds*time.Second, clusterv1.ClockSkewToleranceSeconds*time.Second):
		s.unauthenticated(w, "token lifetime rejected", clusterv1.ActionRetry)
		return store.Cluster{}, false
	case claims.Jti == "":
		// The replay cache is not built in phase 1 — a five-minute window
		// under TLS does not justify one — but a token without a jti could
		// never be checked later, so the field is mandatory now and the check
		// can be turned on without changing the contract.
		s.unauthenticated(w, "missing jti", clusterv1.ActionRetry)
		return store.Cluster{}, false
	case claims.Sub != string(cluster.ID):
		s.problem(w, clusterv1.Problem{
			Title: "subject does not match the key's cluster", Status: http.StatusForbidden,
			Code: clusterv1.CodeClusterMismatch, Action: clusterv1.ActionAbandon, ClusterID: cluster.ID,
		})
		return store.Cluster{}, false
	case cluster.Revoked():
		// Revocation is a status change and takes effect within the life of a
		// token already issued, which is why there is no revocation list.
		// Repeats are pointless: fatal.
		s.problem(w, clusterv1.Problem{
			Title: "cluster revoked", Status: http.StatusUnauthorized,
			Code: clusterv1.CodeClusterRevoked, Action: clusterv1.ActionFatal, ClusterID: cluster.ID,
		})
		return store.Cluster{}, false
	case wantCluster != "" && wantCluster != cluster.ID:
		s.problem(w, clusterv1.Problem{
			Title: "token is for a different cluster", Status: http.StatusForbidden,
			Code: clusterv1.CodeClusterMismatch, Action: clusterv1.ActionAbandon, ClusterID: cluster.ID,
		})
		return store.Cluster{}, false
	}
	return cluster, true
}

// decode reads a JSON body.
//
// Unknown fields are accepted on purpose. Both sides of this contract are
// obliged to ignore what they do not recognise — not "may", must — because the
// control plane and the chart are upgraded independently, and a backend that
// refused an unknown field would make every controller upgrade a coordinated
// one.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.problem(w, clusterv1.Problem{
				Title: "body exceeds 1 MiB", Status: http.StatusRequestEntityTooLarge,
				Code: clusterv1.CodePayloadTooLarge, Action: clusterv1.ActionFatal,
			})
			return false
		}
		s.problem(w, clusterv1.Problem{
			Title: "could not read the request body", Status: http.StatusBadRequest,
			Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionRetry,
		})
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		s.problem(w, clusterv1.Problem{
			Title: "malformed body", Status: http.StatusBadRequest, Detail: err.Error(),
			Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		})
		return false
	}
	return true
}

// fail answers an error from the use cases. A Problem passes through as it is;
// anything else is an internal failure, and the controller is told to retry,
// because the work it holds is not affected by the backend having a bad minute.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, runID runv1.ULID, err error) {
	var problem *clusterv1.Problem
	if errors.As(err, &problem) {
		s.problem(w, *problem)
		return
	}
	s.log.Error("cluster API request failed",
		"path", r.URL.Path, "run", runID, "error", err)
	s.problem(w, clusterv1.Problem{
		Title: "internal error", Status: http.StatusInternalServerError,
		Code: clusterv1.CodeInternal, Action: clusterv1.ActionRetry, RunID: runID,
	})
}

func (s *Server) unauthenticated(w http.ResponseWriter, detail string, action clusterv1.Action) {
	s.problem(w, clusterv1.Problem{
		Title: "unauthenticated", Detail: detail, Status: http.StatusUnauthorized,
		Code: clusterv1.CodeUnauthenticated, Action: action,
	})
}

func (s *Server) problem(w http.ResponseWriter, p clusterv1.Problem) {
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Type == "" {
		p.Type = clusterv1.ProblemTypeBase + kebab(string(p.Code))
	}
	if p.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(p.RetryAfterSeconds)))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(int(p.Status))
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Server) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSecret is write for the responses carrying secret material. The header
// is not decoration: a cache between the controller and the backend holding a
// lease body is a git token on disk in a customer's cluster.
func (s *Server) writeSecret(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, status, body)
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

func kebab(code string) string {
	var out strings.Builder
	for i, r := range code {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r + 32)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// versionInRange compares dotted SemVer cores, ignoring pre-release and build
// metadata: a controller built from a branch is still that minor version, and
// refusing it would make development against a real control plane impossible.
func versionInRange(v string, r clusterv1.VersionRange) bool {
	return compareVersions(v, r.Min) >= 0 && compareVersions(v, r.Max) <= 0
}

func compareVersions(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := range as {
		switch {
		case as[i] < bs[i]:
			return -1
		case as[i] > bs[i]:
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}
		}
		out[i] = n
	}
	return out
}
