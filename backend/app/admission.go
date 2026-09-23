package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Admission: the one path into the system.
//
// REST, MCP and Slack all arrive here, and that is the point — three callers
// with three shapes of request and one set of rules about what a run is
// allowed to be.
//
// It used to begin by writing the prompt into object storage and then insert a
// row pointing at it: two stores, two failure modes, and an object store on the
// critical path of *starting* a run rather than finishing one. The prompt is
// now a column, written in the same INSERT as everything else, and the whole
// step is gone. What remains:
//
//  1. the request is validated, and a prompt over the ceiling is a 413 naming
//     the limit — refused here, where the caller can summarise and try again,
//     rather than later as a Secret the controller cannot create on a run that
//     was already accepted;
//  2. the spec is rendered once and carries the prompt's digest, so "the run
//     executed what was admitted" is provable after the fact;
//  3. placement happens at admission when a cluster is eligible, and is left
//     to the placement loop when none is — a request that fails because a
//     controller is mid-rollout is a request that should have waited.

// SubmitOptions carries the idempotency claim, which belongs to the transport
// rather than to the run.
type SubmitOptions struct {
	// Key is the caller's Idempotency-Key. A repeat with the same key returns
	// the first run rather than creating a second one: Slack and n8n retry on
	// a timeout, and a timeout is exactly when a run has already started.
	Key string
	// Body is what the digest is taken over. The same key with a different
	// body is a client defect and must not be answered with the first run.
	Body []byte
	// Scope separates the key spaces of the callers, so an MCP client and a
	// REST client cannot collide on "1".
	Scope string
}

// Submitted is the outcome of admission.
type Submitted struct {
	Run store.Run
	// Replayed means this was a repeat under a known idempotency key and no
	// new run was created.
	Replayed bool
	// Response is what the first request answered, when Replayed is set.
	Response json.RawMessage
}

// Submit admits a run.
func (s *Service) Submit(ctx context.Context, req run.SubmitRequest, opts SubmitOptions) (Submitted, error) {
	scope := opts.Scope
	if scope == "" {
		scope = "runs.create"
	}

	if opts.Key != "" {
		replay, err := s.store.ClaimIdempotencyKey(ctx, scope, opts.Key, opts.Body, s.limits.IdempotencyTTL)
		if err != nil {
			return Submitted{}, err
		}
		if replay != nil {
			if replay.RunID == "" {
				// The first request claimed the key and has not finished. A
				// second answer would be a second run; the caller retries.
				return Submitted{}, ErrSubmissionInFlight
			}
			r, err := s.store.RunByID(ctx, replay.RunID)
			if err != nil {
				return Submitted{}, err
			}
			return Submitted{Run: r, Replayed: true, Response: replay.Response}, nil
		}
	}

	submitted, err := s.submit(ctx, req)
	if err != nil {
		// The claim is released so the caller's retry is a retry rather than a
		// permanent replay of a failure.
		if opts.Key != "" {
			if releaseErr := s.store.ReleaseIdempotencyKey(ctx, scope, opts.Key); releaseErr != nil {
				s.log.Error("could not release an idempotency claim",
					"scope", scope, "error", releaseErr)
			}
		}
		return Submitted{}, err
	}
	return submitted, nil
}

// ErrSubmissionInFlight is a repeat that arrived while the first request is
// still being admitted.
var ErrSubmissionInFlight = errors.New("app: a request with this idempotency key is in flight")

func (s *Service) submit(ctx context.Context, req run.SubmitRequest) (Submitted, error) {
	var role *run.Role
	if req.Role != "" {
		stored, err := s.store.RoleByName(ctx, req.Role)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return Submitted{}, &run.InvalidRequestError{
					Field: "role", Detail: fmt.Sprintf("no role named %q", req.Role)}
			}
			return Submitted{}, err
		}
		if role, err = parseRole(stored); err != nil {
			return Submitted{}, err
		}
	}

	id, err := run.NewULID(s.now())
	if err != nil {
		return Submitted{}, err
	}

	spec, err := run.Render(id, req, role, s.defaults)
	if err != nil {
		return Submitted{}, err
	}

	// The digest is over the bytes as admitted, and it is the only thing about
	// the prompt that travels in the spec. The pod checks the value it is given
	// against it and refuses to run on a mismatch, which is the one property
	// the old presigned GET bought and the one worth keeping.
	digest := sha256.Sum256([]byte(req.Prompt))
	spec.PromptSHA256 = hex.EncodeToString(digest[:])

	encoded, err := json.Marshal(spec)
	if err != nil {
		return Submitted{}, fmt.Errorf("encode spec for %s: %w", id, err)
	}

	cluster, err := s.store.SelectCluster(ctx, id, spec.Agent)
	if err != nil {
		return Submitted{}, err
	}

	if err := s.store.InsertRun(ctx, store.NewRun{
		ID:             id,
		ParentRunID:    req.ParentRunID,
		Depth:          req.Depth,
		CreatedBy:      req.CreatedBy,
		CreatedVia:     req.CreatedVia,
		Priority:       req.Priority,
		Spec:           encoded,
		Prompt:         req.Prompt,
		PromptSHA256:   digest[:],
		Agent:          spec.Agent,
		Model:          spec.Model,
		Role:           req.Role,
		RepoURL:        spec.Repo.URL,
		RepoProvider:   spec.Repo.Provider,
		BaseBranch:     spec.Repo.BaseBranch,
		TargetBranch:   spec.Repo.TargetBranch,
		TimeoutSeconds: spec.Runtime.TimeoutSeconds,
		MaxCostUSD:     req.MaxCostUSD,
		ClusterID:      cluster,
		MaxChildren:    s.defaults.ChildLimit(),
	}); err != nil {
		if errors.Is(err, store.ErrTooManyChildren) {
			// Worth a line of its own: a run that hits this is either a
			// workflow that wants a bigger ceiling or an agent that has been
			// talked into a fan-out bomb by the repository it was pointed at,
			// and the two are told apart by how often it happens.
			s.log.Warn("a run was refused because its parent has started its full complement of children",
				"parent", req.ParentRunID, "limit", s.defaults.ChildLimit())
			return Submitted{}, run.TooManyChildren(req.ParentRunID, s.defaults.ChildLimit())
		}
		return Submitted{}, err
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: req.CreatedBy, ActorKind: actorKindFor(req.CreatedVia),
		Action: store.AuditRunSubmitted, SubjectKind: "run", SubjectID: string(id),
		RunID: id, ClusterID: cluster,
		Payload: map[string]any{
			"agent": spec.Agent, "model": spec.Model, "role": req.Role,
			"repo": spec.Repo.URL, "via": req.CreatedVia, "parent": string(req.ParentRunID),
		},
	}); err != nil {
		return Submitted{}, err
	}

	// A cluster that is sitting in a long poll gets the work now rather than
	// when its poll expires. Without this the first run of a quiet
	// installation waits out a full waitSeconds for no reason.
	s.work.broadcast()

	stored, err := s.store.RunByID(ctx, id)
	if err != nil {
		return Submitted{}, err
	}
	return Submitted{Run: stored}, nil
}

// CompleteSubmission records what the caller answered, so a retry under the
// same key is given the same thing. It is separate from Submit because only
// the transport knows what it rendered.
func (s *Service) CompleteSubmission(ctx context.Context, opts SubmitOptions, id runv1.ULID,
	status int, response json.RawMessage) error {

	scope := opts.Scope
	if scope == "" {
		scope = "runs.create"
	}
	return s.store.CompleteIdempotencyKey(ctx, scope, opts.Key, id, status, response)
}

// actorKindFor maps how a run arrived to who is recorded as having started it.
// The distinction that matters is 'agent': a run started from inside another
// run is the one case where the actor is not a person or a service.
func actorKindFor(via string) string {
	switch via {
	case "agent":
		return "agent"
	case "ui", "slack":
		return "user"
	default:
		return "token"
	}
}
