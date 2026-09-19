// Package mcp is the MCP listener on :8081 — the same use cases as the REST
// API, spoken as tools.
//
// It is the same binary and the same commands, on a separate listener. A
// separate process would mean either duplicating database access and policies
// or an extra network hop, and the chart can still deploy the listeners apart
// (--mode=mcp, --mode=api) when MCP has to be exposed outward while REST stays
// inside the perimeter. That is a configuration decision, not an architectural
// one.
//
// The transport is JSON-RPC 2.0 over a single HTTP endpoint: one POST per
// request, the response in the body. No SSE and no long-lived session, because
// nothing here streams — the tools return an identifier or a result, and a run
// that takes ten minutes is waited for by the caller, not pushed to it.
package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// Server is the MCP listener.
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

// Path is where the endpoint lives. It is what goes into the pod's mcp.json,
// so it is a constant rather than a configuration value with two spellings.
const Path = "/mcp"

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2025-06-18"

const maxRequestBytes = 8 << 20

// Handler is the endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, s.serve)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		mux.ServeHTTP(w, r)
	})
}

// JSON-RPC 2.0, the subset this protocol uses.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC error codes, plus the one this server adds.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeUnauthorized is outside the reserved range, as the specification
	// allows. It is distinct because a client that is missing a token has to
	// do something different from a client that sent a bad argument.
	codeUnauthorized = -32001
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.reply(w, response{JSONRPC: "2.0", Error: &rpcError{
			Code: codeParseError, Message: "the request body could not be read"}})
		return
	}

	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		s.reply(w, response{JSONRPC: "2.0", Error: &rpcError{
			Code: codeParseError, Message: "the request body is not valid JSON"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code: codeInvalidRequest, Message: "not a JSON-RPC 2.0 request"}})
		return
	}

	// A notification has no id and takes no answer. initialized is the one
	// this server receives, and answering it would be a protocol error rather
	// than a harmless extra.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "haliphron", "version": "1"},
		}})
		return
	case "ping":
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
		return
	}

	// Everything past this point acts on the platform, so it needs a caller.
	c, problem := s.authenticate(r)
	if problem != nil {
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Error: problem})
		return
	}

	switch req.Method {
	case "tools/list":
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefinitions(c)}})
	case "tools/call":
		result, rpcErr := s.call(r, c, req.Params)
		if rpcErr != nil {
			s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
			return
		}
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Result: result})
	default:
		s.reply(w, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code: codeMethodNotFound, Message: "no method " + req.Method}})
	}
}

// Caller is an authenticated MCP client.
type Caller struct {
	Token store.Token
	// Parent is set when the credential is a per-run token — an agent calling
	// from inside a pod. It is the detail the original specification missed:
	// without knowing which run is calling, a child run cannot be bound to its
	// parent, checked against the depth limit, or charged to the right budget,
	// and a global token makes all three impossible at once.
	Parent *store.Run
}

func (s *Server) authenticate(r *http.Request) (Caller, *rpcError) {
	presented, ok := bearer(r)
	if !ok {
		return Caller{}, &rpcError{Code: codeUnauthorized, Message: "a bearer token is required"}
	}
	token, err := s.app.Store().AuthenticateToken(r.Context(), presented)
	if err != nil {
		if errors.Is(err, store.ErrTokenInvalid) {
			return Caller{}, &rpcError{Code: codeUnauthorized, Message: "the token is not usable"}
		}
		s.log.Error("MCP authentication failed", "error", err)
		return Caller{}, &rpcError{Code: codeInternalError, Message: "authentication failed"}
	}

	c := Caller{Token: token}
	if token.Kind == store.TokenKindRunMCP {
		parent, err := s.app.ResolveParent(r.Context(), token)
		if err != nil {
			s.log.Error("could not resolve the parent of a per-run token",
				"token", token.ID, "run", token.RunID, "error", err)
			return Caller{}, &rpcError{Code: codeInternalError, Message: "the calling run could not be resolved"}
		}
		c.Parent = &parent
	}
	return c, nil
}

// Name is who a run created by this caller is recorded as. A per-run token
// names its run, so a chain of agent-started runs is readable from the audit
// log alone.
func (c Caller) Name() string {
	if c.Parent != nil {
		return "run:" + string(c.Parent.ID)
	}
	if c.Token.Subject != "" {
		return c.Token.Subject
	}
	return "token:" + c.Token.Name
}

// Via is how a run this caller starts arrived.
func (c Caller) Via() string {
	if c.Parent != nil {
		return "agent"
	}
	return "mcp"
}

func (s *Server) reply(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	// The body can carry a run's result, and a per-run token in the request.
	// Neither belongs in an intermediary's cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}
