package v1

import (
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// LeaseRequest is the long poll. Zero FreeSlots is a legitimate request: the
// controller keeps the channel open so it does not miss commands, and receives
// no work.
type LeaseRequest struct {
	FreeSlots int32 `json:"freeSlots"`
	// Runtimes restricts issuance, e.g. while an image is being rolled back.
	// +optional
	Runtimes []runv1.AgentType `json:"runtimes,omitempty"`
	// WaitSeconds is clamped to Timings.MaxWaitSeconds.
	// +optional
	WaitSeconds int32 `json:"waitSeconds,omitempty"`
	// +optional
	CapacitySlots int32 `json:"capacitySlots,omitempty"`
}

// LeaseResponse carries the issued work. An empty response is not sent: no work
// is a 204, so the controller can re-poll immediately without parsing a body.
type LeaseResponse struct {
	Leases     []Lease   `json:"leases"`
	ServerTime time.Time `json:"serverTime"`
}

// Lease is one unit of transferred work, complete: everything needed to execute
// it without calling the backend again (principle P4). A controller that has
// lost the backend still finishes what it holds.
//
// This is also the only message in the system carrying secret material in the
// clear. The backend cannot create a Secret inside the cluster — that is what
// the pull model costs — so the controller creates it from what arrives here.
// Hence the handling rules that are not expressible in a type: no-store, never
// logged at any level, never in span attributes.
type Lease struct {
	RunID   runv1.ULID `json:"runID"`
	Epoch   int64      `json:"epoch"`
	Attempt int32      `json:"attempt"`

	// AckDeadline is the window for materialising and acknowledging. Miss it
	// and the epoch is raised and the work returns to the queue — which is safe
	// precisely because nothing has started yet.
	AckDeadline time.Time `json:"ackDeadline"`
	// LeaseDeadline is until when the backend regards this work as belonging to
	// this cluster. Extended by every heartbeat.
	//
	// Its expiry does not oblige the controller to stop. It plays the work out
	// and reports later under its own epoch: killing an hour of agent work over
	// a ten-second network glitch is the worse failure, and the epoch check on
	// the late report is what makes it safe (ADR 5, 6).
	LeaseDeadline time.Time `json:"leaseDeadline"`

	// +optional
	Priority int32 `json:"priority,omitempty"`

	Spec runv1.RenderedRunSpec `json:"spec"`

	// Secrets become a single per-run Secret with an ownerReference to the CR,
	// so that deleting the CR collects them and no long-lived secret is left in
	// the agent namespace. Known keys are the SecretKey* constants in run/v1;
	// unknown ones are passed through untouched.
	//
	// The invariant, checked by admission on the controller's side: no value
	// from here ever reaches the CR's spec. `get agentruns` must not be a way
	// to read tokens.
	Secrets map[string]string `json:"secrets"`

	// RoleConfig is the role's fallback files, keyed by the filename mounted
	// into /haliphron/role/. It sits beside the spec rather than inside it
	// because it is material: the controller turns it into a ConfigMap and only
	// the ConfigMap's name rides in the CR. The rule is mechanical — whatever
	// the controller materialises never goes into the spec.
	// +optional
	RoleConfig map[string]string `json:"roleConfig,omitempty"`

	Artifacts ArtifactBundle `json:"artifacts"`
}

// ArtifactBundleRequest asks for a fresh bundle for an active lease.
type ArtifactBundleRequest struct {
	Epoch   int64 `json:"epoch"`
	Attempt int32 `json:"attempt"`
}

// ArtifactBundle is the pod's capability access to storage without storage
// credentials (ADR 14): it can write into its own prefix and read nothing else.
// The bundle is secret material and belongs in the per-run Secret.
type ArtifactBundle struct {
	Bucket string `json:"bucket"`
	// +optional
	Region string `json:"region,omitempty"`
	// Endpoint is set for MinIO and other S3-compatible stores.
	// +optional
	Endpoint  string `json:"endpoint,omitempty"`
	KeyPrefix string `json:"keyPrefix"`

	// Put is keyed by the storage keys known in advance: output.json,
	// result.md, state.json, completion.json, logs/agent.log.
	//
	// completion.json is the copy that makes the report survivable. The webhook
	// and its forwarding live in the controller's memory; a crash between them
	// loses the cost and the PR link, and neither result.md nor output.json
	// carries either. One extra PUT makes the loss recoverable.
	Put map[string]PresignedURL `json:"put"`

	// Get has two mandatory keys: prompt.txt, without which the pod has no
	// task, and state.json, without which an idempotent retry is impossible —
	// the pod cannot learn that the run phase is already done and pays for the
	// model a second time. A 404 on state.json is the normal first-attempt
	// answer, not a failure.
	// +optional
	Get map[string]PresignedURL `json:"get,omitempty"`

	// Post covers prefixes whose object names are not known in advance: log
	// chunks and free-form artifacts.
	// +optional
	Post []PresignedPostPolicy `json:"post,omitempty"`

	// ExpiresAt is the minimum over every link's expiry. Before creating the
	// Job for attempt > 1 the controller compares it with the expected duration
	// and mints a new bundle if it falls short — an expired signature surfaces
	// as a lost result on work that actually succeeded.
	ExpiresAt time.Time `json:"expiresAt"`
}

// PresignedURL is a bearer capability on someone else's bucket prefix. Treated
// as a secret everywhere: in the Secret, never in the CR, never in an
// environment variable that shows up in `kubectl describe`.
type PresignedURL struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	// Headers must be sent verbatim or the signature will not verify.
	// +optional
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

// PresignedPostPolicy is a presigned POST over a prefix.
type PresignedPostPolicy struct {
	Prefix string            `json:"prefix"`
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
	// +optional
	MaxSizeBytes int64     `json:"maxSizeBytes,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// AckRequest reports that the lease became durable in the cluster — the Secret,
// the ConfigMap and the AgentRun exist, and the work now survives a controller
// restart.
type AckRequest struct {
	ClusterID runv1.ULID `json:"clusterID"`
	Epoch     int64      `json:"epoch"`

	// Accepted false means the controller could not materialise the lease and
	// never started the work. Without a negative ack the only way to say "I
	// cannot run this" is to burn the run: create the Job, let it fail, report
	// Failed — and the causes are all detectable beforehand.
	//
	// It is a pointer because the wire default is true: an ack that omits the
	// field is a positive ack, and a plain bool would silently turn it into a
	// rejection.
	// +optional
	Accepted *bool `json:"accepted,omitempty"`
	// +optional
	Rejection *AckRejection `json:"rejection,omitempty"`

	// CRName and Namespace exist for log correlation: given a CR in a customer's
	// cluster, find the run in the control plane's logs.
	// +optional
	CRName string `json:"crName,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// IsAccepted applies the wire default: an absent Accepted is a positive ack.
func (r AckRequest) IsAccepted() bool { return r.Accepted == nil || *r.Accepted }

// AckRejection says why materialisation failed, before anything was spent.
type AckRejection struct {
	Code    RejectionCode `json:"code"`
	Message string        `json:"message"`
	// Fields are the spec paths at fault, e.g. runtime.resources.memory.
	// +optional
	Fields []string `json:"fields,omitempty"`
}

// RejectionCode is why a cluster refused work it was offered.
type RejectionCode string

const (
	// RejectInvalidSpec means the controller's own validation refused it.
	RejectInvalidSpec RejectionCode = "InvalidSpec"
	// RejectSpecFieldsPruned means this cluster's CRD is older than the spec
	// and dropped fields on write. Silently running a spec with a lost setting
	// is worse than refusing it: the run would succeed and mean something else.
	RejectSpecFieldsPruned RejectionCode = "SpecFieldsPruned"
	// RejectQuotaExhausted means the agent namespace's ResourceQuota is full.
	RejectQuotaExhausted RejectionCode = "QuotaExhausted"
	// RejectImageNotAllowed means policy forbids the image.
	RejectImageNotAllowed RejectionCode = "ImageNotAllowed"
	// RejectMaterializationFailed is everything else that went wrong before the
	// Job existed.
	RejectMaterializationFailed RejectionCode = "MaterializationFailed"
)

// AckResponse confirms the transition and flushes anything that queued up while
// the controller was materialising.
type AckResponse struct {
	RunID runv1.ULID `json:"runID"`
	Epoch int64      `json:"epoch"`
	// Status is the backend's own state name, e.g. Dispatched.
	Status        string    `json:"status"`
	LeaseDeadline time.Time `json:"leaseDeadline"`

	// Commands carries a cancellation that arrived between the lease and the
	// ack. Without this it would wait a full heartbeat interval, having first
	// started a Job that must immediately be killed.
	// +optional
	Commands []Command `json:"commands,omitempty"`
}
