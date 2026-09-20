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

	// Prompt is the task, in the clear, exactly as runs.prompt holds it.
	//
	// It travels here rather than through the artifact store because the store
	// is optional and the prompt is not: putting it there would make an object
	// store a prerequisite for *starting* a run, which is the one thing
	// section 9.2 set out to remove. The backend caps it at
	// Defaults.MaxPromptBytes at admission, so a lease can never carry a value
	// the controller then fails to fit into a Secret.
	//
	// The controller writes it into the per-run Secret under
	// runv1.SecretKeyPrompt and nowhere else. Like Secrets below, it is
	// no-store, never logged, never a span attribute — not because it is a
	// credential but because it is the customer's text.
	Prompt string `json:"prompt"`

	// PromptSHA256 duplicates Spec.PromptSHA256, and the duplication is the
	// point: the digest in the spec is what the CR carries and the pod
	// verifies, and this one is what the controller checks the value above
	// against before it writes a Secret. A lease whose two halves disagree is
	// caught at materialisation rather than by a pod that refuses to start.
	PromptSHA256 string `json:"promptSHA256"`

	// Secrets become a single per-run Secret with an ownerReference to the CR,
	// so that deleting the CR collects them and no long-lived secret is left in
	// the agent namespace. Known keys are the SecretKey* constants in run/v1;
	// unknown ones are passed through untouched.
	//
	// The invariant, checked by admission on the controller's side: no value
	// from here ever reaches the CR's spec. `get agentruns` must not be a way
	// to read tokens.
	//
	// The prompt is not in here. It has a field of its own because it is not a
	// credential and the two are governed by different rules: every key in this
	// map becomes a file under MountSecrets and none of them may become an
	// environment variable, while the prompt becomes exactly one.
	Secrets map[string]string `json:"secrets"`

	// CompletedPhases is the attempt checkpoint of section 9.3, as the backend
	// holds it in run_attempts.completed_phases for this run's current epoch.
	//
	// It is normally empty: a lease is normally the first anyone has executed
	// this run. It is not empty when the backend is re-issuing work whose
	// previous controller reported progress and then lost its CRs, and in that
	// case it is what stops the replacement attempt paying for the model again.
	// The CR's own .status.completedPhases is the copy the controller reads
	// while the backend is unreachable; this is where it comes back from after
	// the CR is gone.
	// +optional
	CompletedPhases []runv1.RuntimePhase `json:"completedPhases,omitempty"`

	// RoleConfig is the role's fallback files, keyed by the filename mounted
	// into /haliphron/role/. It sits beside the spec rather than inside it
	// because it is material: the controller turns it into a ConfigMap and only
	// the ConfigMap's name rides in the CR. The rule is mechanical — whatever
	// the controller materialises never goes into the spec.
	// +optional
	RoleConfig map[string]string `json:"roleConfig,omitempty"`

	Artifacts ArtifactBundle `json:"artifacts"`
}

// ArtifactBundleRequest asks for a fresh bundle for an active lease. Meaningful
// only in object-store mode; in relay mode there is no signature to expire and
// the call is never made.
type ArtifactBundleRequest struct {
	Epoch   int64 `json:"epoch"`
	Attempt int32 `json:"attempt"`
}

// ArtifactBundle says how this run's results reach durable storage.
//
// In the default relay mode it holds Mode and nothing else: the pod posts its
// artifacts to the controller Service it already posts the completion to, and
// there is no bucket, no endpoint and no signature anywhere in the lease. Every
// other field below is object-store mode.
//
// In object-store mode it is the pod's whole access to the store and it holds
// no credential (ADR 14): capabilities to write into its own prefix, and no
// reads at all — the two objects the pod used to read, prompt.txt and
// state.json, are both gone from this store. That removes exit code 21's most
// common cause and one whole class of "the run failed and the reason was a URL
// expiry". Being bearer capabilities, the bundle is secret material and belongs
// in the per-run Secret.
type ArtifactBundle struct {
	// Mode is which half of the port is in force. An empty value reads as
	// relay: a controller newer than its backend must default to the mode that
	// needs no configuration, not to the one that needs a bucket.
	// +optional
	Mode runv1.ArtifactMode `json:"mode,omitempty"`

	// MaxBytesPerRun caps what one run may store, and is enforced by the
	// controller in relay mode so the transfer is not paid for twice — once
	// into the spool and once into a refusal. Zero means the installation's
	// default.
	// +optional
	MaxBytesPerRun int64 `json:"maxBytesPerRun,omitempty"`

	// +optional
	Bucket string `json:"bucket,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// Endpoint is set for MinIO and other S3-compatible stores.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	KeyPrefix string `json:"keyPrefix,omitempty"`

	// Put is keyed by the storage keys known in advance: output.json,
	// result.md, completion.json, logs/agent.log.
	//
	// completion.json is the copy that makes the report survivable. The webhook
	// and its forwarding live in the controller's memory; a crash between them
	// loses the cost and the PR link, and neither result.md nor output.json
	// carries either. One extra PUT makes the loss recoverable.
	// +optional
	Put map[string]PresignedURL `json:"put,omitempty"`

	// Post covers prefixes whose object names are not known in advance: log
	// chunks and free-form artifacts.
	// +optional
	Post []PresignedPostPolicy `json:"post,omitempty"`

	// ExpiresAt is the minimum over every link's expiry. Before creating the
	// Job for attempt > 1 the controller compares it with the expected duration
	// and mints a new bundle if it falls short — an expired signature surfaces
	// as a lost result on work that actually succeeded. Zero in relay mode,
	// where nothing expires.
	// +optional
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// Relay reports whether this run's artifacts travel through the controller. An
// unset mode reads as relay, so a lease from a backend that predates the field
// takes the path that needs no configuration.
func (b ArtifactBundle) Relay() bool { return b.Mode != runv1.ArtifactModeObjectStore }

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
