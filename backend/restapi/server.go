// Package restapi is the public API on :8080 — the listener clients, the UI
// and the Slack adapter speak to.
//
// It is transport and nothing else: every decision about what a run may be
// lives in app, so that a run started from REST, from MCP and from Slack is the
// same run admitted by the same rules. What belongs here is the shape of the
// wire — snake_case, cursor pagination, Idempotency-Key, an error envelope with
// a field name in it — and the authentication model, which is a bearer token
// with scopes rather than the self-signed cluster JWTs of the other listener.
package restapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Server is the public API.
type Server struct {
	app *app.Service
	log *slog.Logger
}

// New builds it over the use cases.
func New(service *app.Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{app: service, log: logger}
}

// BasePath is the versioned prefix. The version is in the path rather than in a
// header because it is the thing a customer's script pins, and a pinned version
// that is invisible in a URL is a version nobody notices changing.
const BasePath = "/api/v1"

// maxRequestBytes bounds a public request. Larger than the Cluster API's ceiling
// because a prompt legitimately grows — a workflow step's prompt carries the
// output of the steps before it — and still finite.
const maxRequestBytes = 8 << 20

// Handler is the routed API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST "+BasePath+"/runs", s.scoped(store.ScopeRunsWrite, s.createRun))
	mux.HandleFunc("GET "+BasePath+"/runs", s.scoped(store.ScopeRunsRead, s.listRuns))
	mux.HandleFunc("GET "+BasePath+"/runs/{id}", s.scoped(store.ScopeRunsRead, s.getRun))
	mux.HandleFunc("GET "+BasePath+"/runs/{id}/result", s.scoped(store.ScopeRunsRead, s.runResult))
	mux.HandleFunc("GET "+BasePath+"/runs/{id}/logs", s.scoped(store.ScopeRunsRead, s.runLogs))
	mux.HandleFunc("GET "+BasePath+"/runs/{id}/attempts", s.scoped(store.ScopeRunsRead, s.runAttempts))
	mux.HandleFunc("POST "+BasePath+"/runs/{id}/cancel", s.scoped(store.ScopeRunsWrite, s.cancelRun))
	mux.HandleFunc("POST "+BasePath+"/runs/{id}/retry", s.scoped(store.ScopeRunsWrite, s.retryRun))

	mux.HandleFunc("GET "+BasePath+"/roles", s.scoped(store.ScopeRunsRead, s.listRoles))
	mux.HandleFunc("PUT "+BasePath+"/roles/{name}", s.scoped(store.ScopeAdmin, s.putRole))
	mux.HandleFunc("GET "+BasePath+"/roles/{name}", s.scoped(store.ScopeRunsRead, s.getRole))
	mux.HandleFunc("DELETE "+BasePath+"/roles/{name}", s.scoped(store.ScopeAdmin, s.deleteRole))

	mux.HandleFunc("GET "+BasePath+"/clusters", s.scoped(store.ScopeRunsRead, s.listClusters))
	mux.HandleFunc("POST "+BasePath+"/clusters/bootstrap-tokens", s.scoped(store.ScopeAdmin, s.createBootstrapToken))
	mux.HandleFunc("GET "+BasePath+"/clusters/bootstrap-tokens", s.scoped(store.ScopeAdmin, s.listBootstrapTokens))
	mux.HandleFunc("POST "+BasePath+"/clusters/{id}/revoke", s.scoped(store.ScopeAdmin, s.revokeCluster))

	mux.HandleFunc("GET "+BasePath+"/secrets", s.scoped(store.ScopeAdmin, s.listSecrets))
	mux.HandleFunc("PUT "+BasePath+"/secrets/{name}", s.scoped(store.ScopeAdmin, s.putSecret))

	mux.HandleFunc("POST "+BasePath+"/tokens", s.scoped(store.ScopeAdmin, s.createToken))
	mux.HandleFunc("GET "+BasePath+"/tokens", s.scoped(store.ScopeAdmin, s.listTokens))
	mux.HandleFunc("DELETE "+BasePath+"/tokens/{id}", s.scoped(store.ScopeAdmin, s.revokeToken))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		mux.ServeHTTP(w, r)
	})
}

// caller is an authenticated client.
type caller struct {
	token store.Token
	// Parent is set when the credential is a per-run token: an agent calling
	// from inside a pod. It is what makes a child run attributable.
	Parent *store.Run
}

// Name is who a run created by this caller is recorded as having been created
// by. A per-run token names its run, so a chain of agent-started runs is
// readable from the audit log alone.
func (c caller) Name() string {
	if c.Parent != nil {
		return "run:" + string(c.Parent.ID)
	}
	if c.token.Subject != "" {
		return c.token.Subject
	}
	return "token:" + c.token.Name
}

// Via is how the run arrived, which is the value the depth and parent
// accounting hang off.
func (c caller) Via() string {
	if c.Parent != nil {
		return "agent"
	}
	return "api"
}

// scoped authenticates and checks a scope before handing over.
func (s *Server) scoped(scope string, next func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearer(r)
		if !ok {
			s.fail(w, http.StatusUnauthorized, "unauthenticated", "a bearer token is required", "")
			return
		}
		token, err := s.app.Store().AuthenticateToken(r.Context(), presented)
		if err != nil {
			if errors.Is(err, store.ErrTokenInvalid) {
				// Unknown, revoked and expired are one answer: telling a
				// caller which of them it was is telling an attacker which
				// guess was closer.
				s.fail(w, http.StatusUnauthorized, "unauthenticated", "the token is not usable", "")
				return
			}
			s.internal(w, r, err)
			return
		}
		if !token.Allows(scope) {
			s.fail(w, http.StatusForbidden, "forbidden", "this token does not carry "+scope, "")
			return
		}

		c := caller{token: token}
		if token.Kind == store.TokenKindRunMCP {
			parent, err := s.app.ResolveParent(r.Context(), token)
			if err != nil {
				s.internal(w, r, err)
				return
			}
			c.Parent = &parent
		}
		next(w, r, c)
	}
}

// errorBody is the public error shape. snake_case, like the rest of this API,
// and with the field name in it: a caller that is told which field was refused
// fixes the request, and one that is told "invalid request" opens a ticket.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (s *Server) fail(w http.ResponseWriter, status int, code, message, field string) {
	s.write(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Field: field}})
}

// failFor maps the errors the layers below produce onto the public API's
// answers, in one place rather than per handler.
func (s *Server) failFor(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *run.InvalidRequestError
	switch {
	case errors.As(err, &invalid):
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request", invalid.Detail, invalid.Field)
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, http.StatusNotFound, "not_found", "no such object", "")
	case errors.Is(err, store.ErrRunTerminal):
		s.fail(w, http.StatusConflict, "run_terminal", "this run has already ended", "")
	case errors.Is(err, store.ErrIdempotencyConflict):
		// The same key with a different body. Answering with the first run
		// would hand back the result of work nobody ordered.
		s.fail(w, http.StatusUnprocessableEntity, "idempotency_conflict",
			"this Idempotency-Key was used with a different request body", "Idempotency-Key")
	case errors.Is(err, app.ErrSubmissionInFlight):
		w.Header().Set("Retry-After", "1")
		s.fail(w, http.StatusConflict, "in_flight",
			"a request with this Idempotency-Key is still being processed", "Idempotency-Key")
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "method", r.Method, "error", err)
	s.fail(w, http.StatusInternalServerError, "internal", "the request could not be completed", "")
}

func (s *Server) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) ([]byte, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.fail(w, http.StatusRequestEntityTooLarge, "too_large",
				"the request body exceeds "+strconv.Itoa(maxRequestBytes)+" bytes", "")
			return nil, false
		}
		s.fail(w, http.StatusBadRequest, "invalid_request", "the request body could not be read", "")
		return nil, false
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON", "")
		return nil, false
	}
	return raw, true
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
