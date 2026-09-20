package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/store"
)

// Registration, leasing and acknowledgement.
//
// The lease is the only message in the system that carries secret material in
// the clear: the backend cannot create a Secret inside a cluster — that is what
// the pull model costs — so the controller creates it from what arrives here.
// The handling rules that follow from that are not expressible in a type, and
// they are kept in the handler: no-store on the response, never logged at any
// level, never in a span attribute. What is logged here is a count.

// knownAgents is the full set of runtimes, used when neither the poll nor the
// cluster restricts issuance. The lease statement filters with agent = ANY($2),
// so "no restriction" has to be spelled out as every value rather than as an
// empty array.
var knownAgents = []runv1.AgentType{runv1.AgentClaudeCode, runv1.AgentCodex}

// Register exchanges a bootstrap token for a cluster identity.
func (s *Service) Register(ctx context.Context, token string, req clusterv1.RegisterRequest) (clusterv1.RegisterResponse, error) {
	key, err := base64.RawURLEncoding.DecodeString(req.PublicKey.Key)
	if err != nil || len(key) != ed25519.PublicKeySize || req.PublicKey.Alg != clusterv1.KeyAlgorithm {
		return clusterv1.RegisterResponse{}, &clusterv1.Problem{
			Title: "invalid public key", Status: 400,
			Detail: "publicKey.key must be a raw Ed25519 key, base64url without padding",
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		}
	}
	if req.Name == "" || req.AgentNamespace == "" || req.PublicKey.KID == "" {
		return clusterv1.RegisterResponse{}, &clusterv1.Problem{
			Title: "invalid registration", Status: 400,
			Detail: "name, agentNamespace and publicKey.kid are required",
			Code:   clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
		}
	}

	cluster, err := s.store.Register(ctx, store.Registration{
		BootstrapToken:    token,
		Name:              req.Name,
		Labels:            req.Labels,
		KeyID:             req.PublicKey.KID,
		PublicKey:         key,
		ControllerVersion: req.ControllerVersion,
		AgentNamespace:    req.AgentNamespace,
		K8sVersion:        req.K8sVersion,
		Runtimes:          req.Runtimes,
		CRDVersions:       req.CRDVersions,
		CapacitySlots:     req.CapacitySlots,
	})
	switch {
	case errors.Is(err, store.ErrBootstrapTokenInvalid):
		return clusterv1.RegisterResponse{}, &clusterv1.Problem{
			Title: "unknown or expired bootstrap token", Status: 401,
			Code: clusterv1.CodeBootstrapTokenInvalid, Action: clusterv1.ActionFatal,
		}
	case errors.Is(err, store.ErrBootstrapTokenConsumed):
		return clusterv1.RegisterResponse{}, &clusterv1.Problem{
			Title: "bootstrap token already spent by another key", Status: 401,
			Code: clusterv1.CodeBootstrapTokenConsumed, Action: clusterv1.ActionFatal,
		}
	case errors.Is(err, store.ErrClusterNameTaken):
		return clusterv1.RegisterResponse{}, &clusterv1.Problem{
			Title: "cluster name is taken", Status: 409,
			Code: clusterv1.CodeClusterNameTaken, Action: clusterv1.ActionFatal,
		}
	case err != nil:
		return clusterv1.RegisterResponse{}, err
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: string(cluster.ID), ActorKind: "cluster", Action: store.AuditClusterRegistered,
		SubjectKind: "cluster", SubjectID: string(cluster.ID), ClusterID: cluster.ID,
		Payload: map[string]any{
			"name": cluster.Name, "controllerVersion": req.ControllerVersion,
			"namespace": req.AgentNamespace,
		},
	}); err != nil {
		return clusterv1.RegisterResponse{}, err
	}

	return clusterv1.RegisterResponse{
		ClusterID:                   cluster.ID,
		Name:                        cluster.Name,
		KeyID:                       cluster.KeyID,
		TokenAudience:               clusterv1.TokenAudience,
		TokenMaxTTLSeconds:          clusterv1.TokenMaxTTLSeconds,
		Timings:                     s.timings,
		SupportedControllerVersions: s.versions,
		// serverTime is what lets a controller notice clock skew at startup
		// rather than through random 401s under load.
		ServerTime: s.now(),
	}, nil
}

// Lease answers one poll, waiting for work until the deadline.
//
// A 204 after the full wait is not a failure and the controller re-polls
// immediately; that is why this returns an empty slice rather than an error
// when nothing turned up. The wait itself holds no database connection: the
// loop sleeps on the notifier and asks again, so a hundred idle clusters cost
// a hundred goroutines rather than a hundred connections.
func (s *Service) Lease(ctx context.Context, cluster store.Cluster, req clusterv1.LeaseRequest) ([]clusterv1.Lease, error) {
	wait := req.WaitSeconds
	if wait <= 0 || wait > s.timings.MaxWaitSeconds {
		wait = s.timings.MaxWaitSeconds
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()

	limit := int(req.FreeSlots)
	if max := int(s.timings.MaxLeasesPerPoll); limit > max {
		limit = max
	}
	runtimes := effectiveRuntimes(req.Runtimes, cluster.Runtimes)

	for {
		// Taken before the query, so a broadcast that lands between the two is
		// not lost. The other order produces a poll that waits out its full
		// interval while work sits in the queue.
		wake := s.work.wait()

		leases, err := s.issue(ctx, cluster, runtimes, limit)
		if err != nil {
			return nil, err
		}
		if len(leases) > 0 {
			return leases, nil
		}

		select {
		case <-wake:
		case <-deadline.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Service) issue(ctx context.Context, cluster store.Cluster, runtimes []runv1.AgentType, limit int) ([]clusterv1.Lease, error) {
	if limit <= 0 || cluster.QuotaExhausted || cluster.Status != "Active" {
		// An exhausted ResourceQuota is declared by the controller. Not
		// issuing into it is the whole value of the flag: otherwise the fact
		// surfaces as a run of Failed runs with "exceeded quota" in the
		// message.
		return nil, nil
	}

	leased, err := s.store.Lease(ctx, cluster.ID, runtimes, limit, s.ackTimeout(), s.leaseTTL())
	if err != nil || len(leased) == 0 {
		return nil, err
	}

	out := make([]clusterv1.Lease, 0, len(leased))
	for _, l := range leased {
		var spec runv1.RenderedRunSpec
		if err := json.Unmarshal(l.Spec, &spec); err != nil {
			return nil, fmt.Errorf("decode stored spec for %s: %w", l.RunID, err)
		}

		secrets, roleConfig, err := s.materials(ctx, l.RunID, spec)
		if err != nil {
			return nil, err
		}
		bundle, err := artifacts.Bundle(s.artifacts, l.RunID,
			s.bundleTTL(l.TimeoutSeconds), s.limits.MaxArtifactBytesPerRun)
		if err != nil {
			return nil, err
		}

		out = append(out, clusterv1.Lease{
			RunID:         l.RunID,
			Epoch:         l.Epoch,
			Attempt:       l.Attempt,
			AckDeadline:   l.AckDeadline,
			LeaseDeadline: l.LeaseDeadline,
			Priority:      l.Priority,
			Spec:          spec,
			// The prompt travels here, in the clear, for the same reason the
			// secrets do: the backend cannot create a Secret inside the
			// cluster. Unlike them it is not a credential, and unlike them it
			// becomes exactly one environment variable rather than a file.
			Prompt:          l.Prompt,
			PromptSHA256:    hex.EncodeToString(l.PromptSHA256),
			CompletedPhases: l.CompletedPhases,
			Secrets:         secrets,
			RoleConfig:      roleConfig,
			Artifacts:       bundle,
		})
	}

	if err := s.store.RecordLease(ctx, cluster.ID); err != nil {
		return nil, err
	}
	// Identifiers and a count. The body holds a git token, a model key, the
	// customer's prompt and — in object-store mode — presigned URLs, and this
	// is the line where a control plane usually leaks them.
	s.log.Info("leases issued", "cluster", cluster.ID, "count", len(out))
	return out, nil
}

// effectiveRuntimes intersects what the poll asked for with what the cluster
// declared at registration. An empty list on either side means no restriction,
// which is the only reading that keeps a controller declaring nothing usable.
func effectiveRuntimes(requested, declared []runv1.AgentType) []runv1.AgentType {
	switch {
	case len(requested) == 0 && len(declared) == 0:
		return knownAgents
	case len(requested) == 0:
		return declared
	case len(declared) == 0:
		return requested
	}
	allowed := make(map[runv1.AgentType]bool, len(declared))
	for _, a := range declared {
		allowed[a] = true
	}
	var out []runv1.AgentType
	for _, a := range requested {
		if allowed[a] {
			out = append(out, a)
		}
	}
	return out
}

// Ack records that a lease became durable in the cluster, or that it could not
// be materialised at all.
func (s *Service) Ack(ctx context.Context, cluster store.Cluster, runID runv1.ULID,
	req clusterv1.AckRequest) (clusterv1.AckResponse, error) {

	if !req.IsAccepted() {
		return s.rejectAck(ctx, cluster, runID, req)
	}

	outcome, err := s.store.AckLease(ctx, runID, cluster.ID, req.Epoch, s.leaseTTL())
	if err != nil {
		return clusterv1.AckResponse{}, s.problemFor(runID, err)
	}
	if !outcome.Repeat {
		s.log.Info("lease acknowledged", "run", runID, "epoch", outcome.Epoch, "cluster", cluster.ID)
	}

	commands, err := s.commandsFor(ctx, runID)
	if err != nil {
		return clusterv1.AckResponse{}, err
	}
	return clusterv1.AckResponse{
		RunID: runID, Epoch: outcome.Epoch, Status: outcome.Status,
		LeaseDeadline: outcome.LeaseDeadline,
		// Anything that queued while the controller was materialising goes out
		// now. A cancellation that arrived in that window would otherwise wait
		// a full heartbeat, having first started a Job that must be killed.
		Commands: commands,
	}, nil
}

func (s *Service) rejectAck(ctx context.Context, cluster store.Cluster, runID runv1.ULID,
	req clusterv1.AckRequest) (clusterv1.AckResponse, error) {

	rejection := store.Rejection{Code: clusterv1.RejectMaterializationFailed}
	if req.Rejection != nil {
		rejection = store.Rejection{
			Code: req.Rejection.Code, Message: req.Rejection.Message, Fields: req.Rejection.Fields,
		}
	}

	outcome, err := s.store.RejectLease(ctx, runID, cluster.ID, req.Epoch, rejection)
	if err != nil {
		return clusterv1.AckResponse{}, s.problemFor(runID, err)
	}
	s.log.Warn("lease refused by cluster",
		"run", runID, "cluster", cluster.ID, "code", rejection.Code,
		"outcome", outcome.Status, "reassigned", outcome.Reassigned)

	if outcome.Status == clusterv1.StatusQueued {
		s.work.broadcast()
	}
	return clusterv1.AckResponse{
		RunID: runID, Epoch: outcome.Epoch, Status: outcome.Status,
	}, nil
}

// ArtifactBundle reissues the presigned capabilities for an active lease.
//
// Deliberately not idempotent: the point of the call is an expiry later than
// the one the caller already holds. The controller makes it before creating
// the Job for a further attempt, because an expired signature surfaces as a
// lost result on work that actually succeeded.
//
// In relay mode there are no signatures and nothing expires, so the answer is
// the same bundle every time and the controller never asks. The endpoint still
// answers, because a controller configured for one mode against a backend
// configured for the other should get a usable bundle rather than a 404 it
// reports as a broken control plane.
func (s *Service) ArtifactBundle(ctx context.Context, cluster store.Cluster, runID runv1.ULID,
	req clusterv1.ArtifactBundleRequest) (clusterv1.ArtifactBundle, error) {

	r, err := s.store.VerifyOwnership(ctx, runID, cluster.ID, req.Epoch)
	if err != nil {
		return clusterv1.ArtifactBundle{}, s.problemFor(runID, err)
	}

	bundle, err := artifacts.Bundle(s.artifacts, runID,
		s.bundleTTL(r.TimeoutSeconds), s.limits.MaxArtifactBytesPerRun)
	if err != nil {
		return clusterv1.ArtifactBundle{}, err
	}
	s.log.Info("artifact bundle reissued",
		"run", runID, "epoch", req.Epoch, "attempt", req.Attempt, "expires", bundle.ExpiresAt)
	return bundle, nil
}

// commandsFor is what the cluster holding this run should be told to do.
//
// Both commands are functions of the current state rather than rows in a
// queue: cancel while the request is recorded and the phase is not terminal,
// abandon when a report arrives under a stale epoch. That is also why they
// carry no acknowledgement — one would have to be stored and expired, and
// re-sending cancel to an already cancelled run costs nothing.
func (s *Service) commandsFor(ctx context.Context, runID runv1.ULID) ([]clusterv1.Command, error) {
	r, err := s.store.RunByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.CancelRequestedAt == nil || r.ObservedPhase.IsTerminal() {
		return nil, nil
	}
	issued := s.now()
	return []clusterv1.Command{{
		Type:               clusterv1.CommandCancel,
		RunID:              runID,
		Epoch:              r.Epoch,
		Reason:             r.CancelReason,
		IssuedAt:           &issued,
		GracePeriodSeconds: 30,
	}}, nil
}

// problemFor turns a store failure into the answer the contract specifies.
//
// The mapping is one place rather than per handler, because the field that
// matters is action: the controller must not infer behaviour from the status
// code, and two handlers that disagree about what a 409 means is precisely the
// divergence the field exists to prevent.
func (s *Service) problemFor(runID runv1.ULID, err error) error {
	var epochErr *store.EpochError
	if errors.As(err, &epochErr) {
		if !epochErr.Stale() {
			// The backend never issued this epoch. Not a race — a defect, and
			// no retry improves it.
			return &clusterv1.Problem{
				Title: "epoch was never issued", Status: 400,
				Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal,
				RunID: runID, CurrentEpoch: epochErr.Current, CurrentStatus: epochErr.Status,
			}
		}
		return &clusterv1.Problem{
			Title: "stale epoch", Status: 409,
			Code: clusterv1.CodeEpochMismatch, Action: clusterv1.ActionAbandon,
			RunID: runID, CurrentEpoch: epochErr.Current, CurrentStatus: epochErr.Status,
		}
	}

	var ownership *store.OwnershipError
	if errors.As(err, &ownership) {
		return &clusterv1.Problem{
			Title: "run is leased by another cluster", Status: 403,
			Code: clusterv1.CodeRunLeasedByAnotherCluster, Action: clusterv1.ActionAbandon,
			RunID: runID, CurrentEpoch: ownership.Current,
		}
	}

	if errors.Is(err, store.ErrNotFound) {
		return &clusterv1.Problem{
			Title: "unknown run", Status: 404,
			Code: clusterv1.CodeRunNotFound, Action: clusterv1.ActionAbandon, RunID: runID,
		}
	}
	return err
}
