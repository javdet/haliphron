package restapi

import (
	"encoding/json"
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/store"
)

// The configuration endpoints: roles, clusters, secrets and tokens.
//
// They are what makes the platform usable without the UI — the scenario the MCP
// configuration tools exist for too — and they are all admin-scoped except the
// reads, because a role is what decides which tools an agent may run.

// ---------------------------------------------------------------------------
// roles
// ---------------------------------------------------------------------------

type roleResponse struct {
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	CreatedBy string          `json:"created_by"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func roleView(r store.Role) roleResponse {
	return roleResponse{Name: r.Name, Spec: r.Spec, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt}
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request, _ caller) {
	roles, err := s.app.Store().ListRoles(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]roleResponse, 0, len(roles))
	for _, role := range roles {
		items = append(items, roleView(role))
	}
	s.write(w, http.StatusOK, map[string]any{"roles": items})
}

func (s *Server) getRole(w http.ResponseWriter, r *http.Request, _ caller) {
	role, err := s.app.Store().RoleByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, roleView(role))
}

// putRole creates or replaces a role.
//
// PUT rather than PATCH, and the whole spec every time: a role is edited in a
// UI that holds the whole object, and merge semantics on a document whose
// shape changes with the product is where a half-applied edit becomes a run
// with the wrong tool policy.
func (s *Server) putRole(w http.ResponseWriter, r *http.Request, c caller) {
	var spec json.RawMessage
	if _, ok := s.decode(w, r, &spec); !ok {
		return
	}
	if len(spec) == 0 || spec[0] != '{' {
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request", "a role spec is an object", "spec")
		return
	}

	role, err := s.app.Store().UpsertRole(r.Context(), r.PathValue("name"), spec, c.Name())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, roleView(role))
}

// deleteRole is a soft delete. Runs admitted under the role keep naming it:
// their specs are frozen, and the role is part of what they froze.
func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request, _ caller) {
	if err := s.app.Store().DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// clusters
// ---------------------------------------------------------------------------

type clusterResponse struct {
	ClusterID         string            `json:"cluster_id"`
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels,omitempty"`
	Status            string            `json:"status"`
	AgentNamespace    string            `json:"agent_namespace"`
	ControllerVersion string            `json:"controller_version"`
	K8sVersion        string            `json:"k8s_version,omitempty"`
	Runtimes          []string          `json:"runtimes,omitempty"`
	CapacitySlots     int32             `json:"capacity_slots"`
	FreeSlots         int32             `json:"free_slots"`
	QuotaExhausted    bool              `json:"quota_exhausted"`
	RegisteredAt      time.Time         `json:"registered_at"`
	LastHeartbeatAt   *time.Time        `json:"last_heartbeat_at,omitempty"`
	RevokedReason     string            `json:"revoked_reason,omitempty"`
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request, _ caller) {
	clusters, err := s.app.Store().ListClusters(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	items := make([]clusterResponse, 0, len(clusters))
	for _, c := range clusters {
		runtimes := make([]string, 0, len(c.Runtimes))
		for _, rt := range c.Runtimes {
			runtimes = append(runtimes, string(rt))
		}
		items = append(items, clusterResponse{
			ClusterID: string(c.ID), Name: c.Name, Labels: c.Labels, Status: c.Status,
			AgentNamespace: c.AgentNamespace, ControllerVersion: c.ControllerVersion,
			K8sVersion: c.K8sVersion, Runtimes: runtimes,
			CapacitySlots: c.CapacitySlots, FreeSlots: c.FreeSlots,
			QuotaExhausted: c.QuotaExhausted, RegisteredAt: c.RegisteredAt,
			LastHeartbeatAt: c.LastHeartbeatAt, RevokedReason: c.RevokedReason,
		})
	}
	// No public key here, and no credential of any kind: there is none to
	// show. What the control plane holds is the public half of a pair the
	// controller generated, and printing it in an API response would only
	// invite somebody to think it is a secret.
	s.write(w, http.StatusOK, map[string]any{"clusters": items})
}

type bootstrapTokenRequest struct {
	Name       string `json:"name"`
	TTLSeconds int32  `json:"ttl_seconds,omitempty"`
	MaxUses    int    `json:"max_uses,omitempty"`
}

// createBootstrapToken mints the credential a controller registers with. It is
// returned once and stored only as a digest: a dump of that table containing
// the token itself would be a way to register a rogue controller.
func (s *Server) createBootstrapToken(w http.ResponseWriter, r *http.Request, c caller) {
	var req bootstrapTokenRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	if req.Name == "" {
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request", "a name is required", "name")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	token, err := s.app.Store().CreateBootstrapToken(r.Context(), req.Name, c.Name(), ttl, req.MaxUses)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, http.StatusCreated, map[string]any{
		"token_id":   string(token.ID),
		"name":       token.Name,
		"token":      token.Token,
		"expires_at": token.ExpiresAt,
		"max_uses":   token.MaxUses,
	})
}

func (s *Server) listBootstrapTokens(w http.ResponseWriter, r *http.Request, _ caller) {
	tokens, err := s.app.Store().ListBootstrapTokens(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, map[string]any{
			"token_id": string(t.ID), "name": t.Name, "expires_at": t.ExpiresAt,
			"max_uses": t.MaxUses, "uses": t.Uses,
			"created_by": t.CreatedBy, "created_at": t.CreatedAt, "revoked_at": t.RevokedAt,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"bootstrap_tokens": items})
}

type revokeRequest struct {
	Reason string `json:"reason,omitempty"`
}

// revokeCluster ends a cluster's ability to act, within the life of a token
// already issued. Its runs are left alone: they are the record of work that
// happened, and what to do with the ones still in flight is an operator's
// decision rather than a side effect.
func (s *Server) revokeCluster(w http.ResponseWriter, r *http.Request, c caller) {
	var req revokeRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	id := runv1.ULID(r.PathValue("id"))
	if err := s.app.Store().RevokeCluster(r.Context(), id, req.Reason); err != nil {
		s.failFor(w, r, err)
		return
	}
	if err := s.app.Store().Audit(r.Context(), store.AuditEntry{
		Actor: c.Name(), ActorKind: "user", Action: store.AuditClusterRevoked,
		SubjectKind: "cluster", SubjectID: string(id), ClusterID: id,
		Payload: map[string]any{"reason": req.Reason},
	}); err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// secrets
// ---------------------------------------------------------------------------

type secretRequest struct {
	// Value is a managed secret: encrypted here under a key of its own, whose
	// wrapping key never enters the database.
	Value string `json:"value,omitempty"`
	// Ref is a referenced secret: a pointer into the customer's Vault or
	// External Secrets, which the backend never resolves itself.
	Ref string `json:"ref,omitempty"`
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, c caller) {
	var req secretRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	name := r.PathValue("name")

	switch {
	case req.Value != "" && req.Ref != "":
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request",
			"a secret is either managed or referenced, not both", "value")
	case req.Value != "":
		secret, err := s.app.Store().PutManagedSecret(r.Context(), name, []byte(req.Value), c.Name())
		if err != nil {
			s.failFor(w, r, err)
			return
		}
		s.write(w, http.StatusOK, map[string]any{"name": secret.Name, "kind": secret.Kind})
	case req.Ref != "":
		secret, err := s.app.Store().PutReferencedSecret(r.Context(), name, req.Ref, c.Name())
		if err != nil {
			s.failFor(w, r, err)
			return
		}
		s.write(w, http.StatusOK, map[string]any{
			"name": secret.Name, "kind": secret.Kind, "ref": secret.RefURI})
	default:
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request",
			"either value or ref is required", "value")
	}
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request, _ caller) {
	secrets, err := s.app.Store().ListSecrets(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(secrets))
	for _, secret := range secrets {
		// Names, kinds and references. Never a value, and never a ciphertext:
		// an endpoint that returns one turns a read scope into a credential.
		items = append(items, map[string]any{
			"name": secret.Name, "kind": secret.Kind, "ref": secret.RefURI,
			"updated_at": secret.UpdatedAt, "rotated_at": secret.RotatedAt,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"secrets": items})
}

// ---------------------------------------------------------------------------
// tokens
// ---------------------------------------------------------------------------

type tokenRequest struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Subject    string   `json:"subject,omitempty"`
	TTLSeconds int32    `json:"ttl_seconds,omitempty"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, c caller) {
	var req tokenRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	if req.Name == "" {
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request", "a name is required", "name")
		return
	}
	if len(req.Scopes) == 0 {
		s.fail(w, http.StatusUnprocessableEntity, "invalid_request",
			"a token with no scopes can do nothing", "scopes")
		return
	}
	for _, scope := range req.Scopes {
		switch scope {
		case store.ScopeRunsRead, store.ScopeRunsWrite, store.ScopeAdmin:
		default:
			s.fail(w, http.StatusUnprocessableEntity, "invalid_request",
				"unknown scope "+scope, "scopes")
			return
		}
	}

	token, err := s.app.Store().CreateToken(r.Context(), store.Token{
		Name: req.Name, Kind: store.TokenKindService, Scopes: req.Scopes,
		Subject: req.Subject, CreatedBy: c.Name(),
	}, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	// Returned once. Only the digest is stored, so there is no second chance
	// to read it and no way for this API to show it again.
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, http.StatusCreated, map[string]any{
		"token_id": string(token.ID), "name": token.Name, "token": token.Secret,
		"scopes": token.Scopes, "expires_at": token.ExpiresAt,
	})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, _ caller) {
	tokens, err := s.app.Store().ListTokens(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, map[string]any{
			"token_id": string(t.ID), "name": t.Name, "kind": t.Kind, "scopes": t.Scopes,
			"subject": t.Subject, "run_id": string(t.RunID),
			"expires_at": t.ExpiresAt, "created_at": t.CreatedAt, "revoked_at": t.RevokedAt,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"tokens": items})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, _ caller) {
	if err := s.app.Store().RevokeToken(r.Context(), runv1.ULID(r.PathValue("id"))); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
