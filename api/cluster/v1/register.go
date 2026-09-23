package v1

import (
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Protocol constants. Both sides hard-code these; a divergence in any of them
// is an authentication failure diagnosed as a network problem.
const (
	// BasePath is the listener's prefix. A separate port (:8082) from the
	// public API: different authentication model, different consumers,
	// different reasons to be reachable at all.
	BasePath = "/cluster-api/v1"

	// TokenAudience is the required aud claim. A token minted for anything
	// else — the public API, another installation — is rejected here.
	TokenAudience = "haliphron-cluster-api"

	// TokenMaxTTLSeconds bounds exp-iat. Long-lived cluster tokens are the
	// thing the self-signed scheme exists to avoid.
	TokenMaxTTLSeconds = 300

	// ClockSkewToleranceSeconds is how far the two clocks may disagree before
	// a token is refused. Registration returns serverTime so a controller can
	// notice the drift at startup instead of through random 401s under load.
	ClockSkewToleranceSeconds = 60

	// BootstrapTokenPrefix is how the handler tells a one-time token from a
	// JWT without parsing it.
	BootstrapTokenPrefix = "hlb_"

	// HeaderControllerVersion is mandatory on every request: in a multi-cluster
	// installation there is otherwise no telling which version sent what.
	HeaderControllerVersion = "X-Haliphron-Controller-Version"

	// SigningAlgorithm is the only accepted JWT alg. Ed25519 and nothing else:
	// an alg the backend is willing to negotiate is an alg an attacker can
	// negotiate down.
	SigningAlgorithm = "EdDSA"
	// KeyAlgorithm is the value of PublicKey.Alg.
	KeyAlgorithm = "Ed25519"
)

// RegisterRequest exchanges a one-time bootstrap token for a cluster identity.
//
// The controller generates the Ed25519 pair inside the cluster and sends only
// the public half. The alternative — the backend issuing credentials — would
// let a compromised control plane mint a token for any cluster, which
// contradicts the one assumption the whole threat model rests on (ADR 1).
type RegisterRequest struct {
	// Name is unique, human-readable, and used in role cluster selectors.
	Name string `json:"name"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	PublicKey PublicKey `json:"publicKey"`

	ControllerVersion string `json:"controllerVersion"`
	// AgentNamespace is where the controller will create agent Jobs.
	AgentNamespace string `json:"agentNamespace"`

	// +optional
	K8sVersion string `json:"k8sVersion,omitempty"`
	// Runtimes restricts what may be scheduled here; it feeds the placement
	// filter.
	// +optional
	Runtimes []runv1.AgentType `json:"runtimes,omitempty"`
	// CapacitySlots is declared by the controller, not computed by the backend.
	// The real ceiling is the agent namespace's ResourceQuota, which the
	// backend cannot see.
	// +optional
	CapacitySlots int32 `json:"capacitySlots,omitempty"`
	// CRDVersions tells the backend whether this cluster can materialise a new
	// spec shape at all.
	// +optional
	CRDVersions []string `json:"crdVersions,omitempty"`
}

// PublicKey is the half of the pair that leaves the cluster.
type PublicKey struct {
	// KID goes into the JWT header and selects the key on verification.
	KID string `json:"kid"`
	Alg string `json:"alg"`
	// Key is the raw 32-byte Ed25519 public key, base64url without padding.
	Key string `json:"key"`
}

// RegisterResponse confirms the registration and bootstraps the operational
// parameters in one call. The intervals come from the control plane rather than
// the cluster's values.yaml: otherwise staleAfter drifts between installations
// and no two clusters agree on what "stale" means.
type RegisterResponse struct {
	ClusterID runv1.ULID `json:"clusterID"`
	Name      string     `json:"name"`
	// KeyID echoes the accepted publicKey.kid, so a controller that sent two
	// keys during a restart knows which one is live.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	TokenAudience string `json:"tokenAudience"`
	// +optional
	TokenMaxTTLSeconds int32 `json:"tokenMaxTTLSeconds,omitempty"`

	Timings                     Timings      `json:"timings"`
	SupportedControllerVersions VersionRange `json:"supportedControllerVersions"`

	// ServerTime is for estimating clock skew. A controller whose divergence
	// exceeds the tolerance is obliged to log it at startup.
	ServerTime time.Time `json:"serverTime"`
}

// VersionRange is the controller SemVer window this control plane speaks. The
// chart and the control plane are upgraded independently, so a divergence is a
// normal state and at least N-1 minor versions are supported.
type VersionRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

// Timings are the operational parameters, issued at registration and refreshable
// through the heartbeat.
type Timings struct {
	HeartbeatIntervalSeconds int32 `json:"heartbeatIntervalSeconds"`
	// StaleAfterSeconds without a heartbeat marks the cluster Unreachable.
	StaleAfterSeconds int32 `json:"staleAfterSeconds"`
	// LeaseTTLSeconds is by how much each heartbeat extends a run's lease
	// deadline.
	LeaseTTLSeconds int32 `json:"leaseTTLSeconds"`
	// AckTimeoutSeconds is the window for materialising and acknowledging.
	// Deliberately much shorter than the lease TTL: before the ack the work is
	// guaranteed not to have started, so it can be reassigned immediately and
	// safely, and a controller that died before creating the Job must not keep
	// the work idle for minutes.
	AckTimeoutSeconds int32 `json:"ackTimeoutSeconds"`
	// MaxWaitSeconds caps the long poll. It must stay below the ingress's
	// proxy_read_timeout, or the controller sees disconnects where it should
	// see empty responses and reports it as network instability.
	MaxWaitSeconds   int32 `json:"maxWaitSeconds"`
	MaxLeasesPerPoll int32 `json:"maxLeasesPerPoll"`
	// +optional
	ArtifactTTLMultiplier float32 `json:"artifactTTLMultiplier,omitempty"`
}

// DefaultTimings are the contract's stated defaults. They live in the shared
// package because the fake and the real backend must hand out the same numbers:
// a controller tested against one set and deployed against another has its
// expiry arithmetic silently rescaled.
func DefaultTimings() Timings {
	return Timings{
		HeartbeatIntervalSeconds: 10,
		StaleAfterSeconds:        90,
		LeaseTTLSeconds:          120,
		AckTimeoutSeconds:        60,
		MaxWaitSeconds:           30,
		MaxLeasesPerPoll:         10,
		ArtifactTTLMultiplier:    2,
	}
}

// DefaultMaxAckExpiries is how many leases of one run may expire
// unacknowledged before the backend stops handing it out and fails it with
// AckTimeoutExhausted.
//
// An ack timeout is the one revocation with no brake of its own: a negative ack
// excludes the cluster, so refusals run out once every cluster has refused, but
// a lease that simply is not acknowledged goes back to the same cluster, and a
// controller that materialises fine and cannot reach the ack endpoint would
// cycle every ackTimeout forever, minting a per-run token each time. The
// ordinary cause — a controller restarting between lease and ack — costs one
// expiry, a rolling update or a short crash loop two or three; five is past
// anything transient, and at the default 60 s timeout it gives up after about
// five minutes.
//
// Not transmitted: it is the backend's policy, not a timing a controller
// schedules against. It lives here for the reason DefaultTimings does — the
// fake and the real backend must give up at the same point.
const DefaultMaxAckExpiries = 5
