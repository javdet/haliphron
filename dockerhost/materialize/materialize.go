// Package materialize turns a lease into a row: preflight, one transaction,
// and nothing else. The acknowledgement is the lease loop's; the container is
// the reconciler's, and only after the acknowledgement.
//
// The rule from controller/materialize carries over: whatever the controller
// materialises does not go into the spec. Secrets, the prompt, the role's
// files and the presigned bundle go into the row's materials column, which
// never leaves this host and is never logged.
package materialize

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	k8slauncher "github.com/automagicops/haliphron/controller/launcher"

	"github.com/automagicops/haliphron/dockerhost/capacity"
	"github.com/automagicops/haliphron/dockerhost/rawspec"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Gate is a condition that must hold for the host to take work: the network
// check, above all.
type Gate interface {
	Healthy() error
}

// Materializer writes the row one lease becomes.
type Materializer struct {
	Store    *state.Store
	Capacity *capacity.Checker
	Network  Gate
	RawSpecs *rawspec.Tap
	// Runtimes is what this host declared; empty means any.
	Runtimes    []runv1.AgentType
	CallbackURL string
	Clock       func() time.Time
	Log         *slog.Logger
}

// Result is the outcome of materialising one lease. Exactly one holds.
type Result struct {
	// Run is set when the work is durable and the lease may be acknowledged.
	Run *state.Run
	// Rejection is set when nothing was created and nothing will start.
	Rejection *clusterv1.AckRejection
	// Superseded means a previous epoch of this run is still being torn
	// down. The lease is neither acknowledged nor refused; if the teardown
	// outlasts the ack deadline the backend reissues at a higher epoch.
	Superseded bool
}

func (m *Materializer) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

func (m *Materializer) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Apply makes the lease durable. It is idempotent on (runID, epoch): a
// controller that crashed between the row and the ack gets the same row back
// on the next poll, and the ack it repeats is idempotent on the backend.
func (m *Materializer) Apply(ctx context.Context, lease clusterv1.Lease) (Result, error) {
	raw, haveRaw := m.RawSpecs.Take(lease.RunID, lease.Epoch)

	existing, err := m.Store.Get(ctx, lease.RunID)
	switch {
	case err == nil:
		return m.existing(ctx, &existing, lease)
	case !errors.Is(err, state.ErrNotFound):
		return Result{}, fmt.Errorf("materialize: read %s: %w", lease.RunID, err)
	}

	if rejection := m.preflight(ctx, lease, raw, haveRaw); rejection != nil {
		return Result{Rejection: rejection}, nil
	}

	token, err := mintCallbackToken()
	if err != nil {
		return Result{}, err
	}
	bundle := lease.Artifacts
	if bundle.Mode == "" {
		bundle.Mode = runv1.ArtifactModeRelay
	}
	row := state.Run{
		RunID:         lease.RunID,
		Epoch:         lease.Epoch,
		LeaseAttempt:  max(lease.Attempt, 1),
		Spec:          lease.Spec,
		SpecRawSHA256: rawspec.Digest(raw),
		LeasedAt:      m.now().UTC(),
		CallbackToken: token,
		Materials: state.Materials{
			Prompt:     lease.Prompt,
			Secrets:    lease.Secrets,
			RoleConfig: lease.RoleConfig,
			Artifacts:  bundle,
		},
		Status: state.Status{
			// The backend's copy of the checkpoint: non-empty when it is
			// reissuing work a previous controller made progress on and then
			// lost. It is what stops the replacement paying for the model again.
			CompletedPhases: lease.CompletedPhases,
		},
	}
	written, err := m.Store.Insert(ctx, row)
	if errors.Is(err, state.ErrExists) {
		// A concurrent writer for the same run: the next poll takes the
		// idempotent path.
		return Result{Superseded: true}, nil
	}
	if err != nil {
		// No ack at all. The ack deadline expires and the backend reissues,
		// which is the right outcome for a host that cannot write its own
		// store.
		return Result{}, err
	}
	m.log().Info("lease materialised", "runID", lease.RunID, "epoch", lease.Epoch, "attempt", lease.Attempt)
	return Result{Run: &written}, nil
}

// existing handles the three ways a row for this run can already be there.
func (m *Materializer) existing(ctx context.Context, row *state.Run, lease clusterv1.Lease) (Result, error) {
	switch {
	case row.Epoch == lease.Epoch && !row.IsAbandoned():
		m.log().Info("lease already materialised; acknowledging again", "runID", lease.RunID, "epoch", lease.Epoch)
		return Result{Run: row}, nil
	case row.Epoch > lease.Epoch:
		return Result{Rejection: &clusterv1.AckRejection{
			Code: clusterv1.RejectMaterializationFailed,
			Message: fmt.Sprintf("this host already holds run %s at epoch %d, newer than the offered %d",
				lease.RunID, row.Epoch, lease.Epoch),
		}}, nil
	default:
		// A new epoch is new ownership. The old row is marked abandoned so the
		// reconciler removes its containers and then the row; until it is gone
		// the new lease waits.
		if !row.IsAbandoned() {
			m.log().Info("replacing a superseded run", "runID", lease.RunID, "was", row.Epoch, "now", lease.Epoch)
			now := m.now()
			if _, err := m.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
				if cur.IsAbandoned() || cur.Epoch >= lease.Epoch {
					return false
				}
				cur.AbandonedAt = &now
				return true
			}); err != nil && !errors.Is(err, state.ErrNotFound) {
				return Result{}, err
			}
		}
		return Result{Superseded: true}, nil
	}
}

// imageRef is a permissive shape check of a Docker image reference: something
// the daemon will at least parse. The registry is the judge of whether it
// exists.
var imageRef = regexp.MustCompile(`^[a-z0-9]+([._/:-][a-zA-Z0-9]+)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

// preflight refuses, before anything durable exists, what this host would
// refuse anyway. Every refusal names the spec path an operator sees in the UI.
func (m *Materializer) preflight(ctx context.Context, lease clusterv1.Lease, raw []byte, haveRaw bool) *clusterv1.AckRejection {
	if m.CallbackURL == "" {
		return &clusterv1.AckRejection{
			Code:    clusterv1.RejectMaterializationFailed,
			Message: "this controller has no callback URL configured",
		}
	}
	if m.Network != nil {
		if err := m.Network.Healthy(); err != nil {
			return &clusterv1.AckRejection{
				Code:    clusterv1.RejectMaterializationFailed,
				Message: "this host's agent network check is failing: " + err.Error(),
			}
		}
	}

	if haveRaw {
		if lost := rawspec.Dropped(raw, lease.Spec); len(lost) > 0 {
			m.log().Error("the lease carries spec fields this controller does not know",
				"runID", lease.RunID, "fields", lost)
			return &clusterv1.AckRejection{
				Code: clusterv1.RejectSpecFieldsPruned,
				Message: fmt.Sprintf("this controller does not know %d spec field(s) and would ignore them: %s",
					len(lost), strings.Join(lost, ", ")),
				Fields: lost,
			}
		}
	}

	if _, err := k8slauncher.Resources(lease.Spec.Runtime.Resources); err != nil {
		var invalid *k8slauncher.InvalidFieldError
		if errors.As(err, &invalid) {
			return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: invalid.Error(),
				Fields: []string{invalid.Path}}
		}
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: err.Error()}
	}
	if lease.Spec.Image == "" {
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: "spec.image is empty",
			Fields: []string{"image"}}
	}
	if !imageRef.MatchString(lease.Spec.Image) {
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf("spec.image %q is not an image reference", lease.Spec.Image),
			Fields:  []string{"image"}}
	}
	switch lease.Spec.ImagePullPolicy {
	case "", "Always", "IfNotPresent", "Never":
	default:
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf("imagePullPolicy %q is not one of Always, IfNotPresent, Never", lease.Spec.ImagePullPolicy),
			Fields:  []string{"imagePullPolicy"}}
	}
	if len(m.Runtimes) > 0 && !contains(m.Runtimes, lease.Spec.Agent) {
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf("this host does not run %q agents", lease.Spec.Agent),
			Fields:  []string{"agent"}}
	}
	if lease.Prompt == "" {
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: "the lease carries no prompt: this run has no task", Fields: []string{"prompt"}}
	}
	sum := sha256.Sum256([]byte(lease.Prompt))
	got := hex.EncodeToString(sum[:])
	switch {
	case lease.PromptSHA256 != "" && lease.PromptSHA256 != got:
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf("the lease's prompt hashes to %s and the lease says %s", got, lease.PromptSHA256),
			Fields:  []string{"promptSHA256"}}
	case lease.Spec.PromptSHA256 != "" && lease.Spec.PromptSHA256 != got:
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf("the lease's prompt hashes to %s and the spec says %s", got, lease.Spec.PromptSHA256),
			Fields:  []string{"promptSHA256"}}
	}
	for key := range lease.Secrets {
		if !validKey(key) {
			return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
				Message: fmt.Sprintf("secret key %q cannot be a file name", key), Fields: []string{"secrets"}}
		}
	}
	for key := range lease.RoleConfig {
		if !validKey(key) {
			return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec,
				Message: fmt.Sprintf("role file %q cannot be a file name", key), Fields: []string{"roleConfig"}}
		}
	}

	if m.Capacity != nil {
		snap, err := m.Capacity.Take(ctx)
		if err != nil {
			return &clusterv1.AckRejection{Code: clusterv1.RejectMaterializationFailed,
				Message: "could not measure this host's capacity: " + err.Error()}
		}
		if ok, why := m.Capacity.Fits(snap, lease.Spec.Runtime.Resources); !ok {
			return &clusterv1.AckRejection{Code: clusterv1.RejectQuotaExhausted, Message: why,
				Fields: []string{"runtime.resources"}}
		}
	}
	return nil
}

var keyShape = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func validKey(k string) bool { return keyShape.MatchString(k) && k != "." && k != ".." }

func contains(list []runv1.AgentType, v runv1.AgentType) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// mintCallbackToken is minted here, not by the backend: the callback is host
// local, so the backend could supply neither half of it.
func mintCallbackToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("materialize: mint callback token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
