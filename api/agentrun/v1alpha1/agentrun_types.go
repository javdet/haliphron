package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// AgentRunSpec is the lease, materialised. It is RenderedRunSpec verbatim plus
// exactly four things the backend cannot know: the identity of the run, the
// fencing epoch it was handed out under, the names of the objects the
// controller created for it, and a callback URL that only makes sense inside
// this cluster.
//
// The spec is immutable in practice: the controller writes it once at creation
// and never again, and a new epoch produces a new AgentRun rather than an edit.
// The transition rules below pin the fields whose change would make the status
// describe a different run than the one that started.
type AgentRunSpec struct {
	// RunID matches runs.id in the backend. It is the correlation key for every
	// report, log line and metric about this run.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runID is immutable"
	RunID runv1.ULID `json:"runID"`

	// LeaseEpoch is the fencing token this work was handed out under. The
	// backend rejects any report carrying a stale epoch, which is what stops a
	// controller that lost connectivity and came back from writing into a run
	// that has since been reassigned.
	//
	// It is immutable here because a new epoch means new ownership: the
	// controller deletes this AgentRun and creates a fresh one, so that a
	// second epoch can never inherit the first one's status.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="leaseEpoch is immutable; a new epoch gets a new AgentRun"
	LeaseEpoch int64 `json:"leaseEpoch"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="the rendered spec is immutable"
	runv1.RenderedRunSpec `json:",inline"`

	// Materials names the Secret and ConfigMap the controller built from the
	// lease. Names only: the values stay in the objects, where RBAC applies to
	// them. A reader with `get agentruns` learns that a git token exists, not
	// what it is.
	Materials MaterialsRef `json:"materials"`

	// CallbackURL is where the pod posts its completion report. The value is
	// cluster-local — a Service in the controller's namespace — so the backend
	// cannot supply it and the controller fills it in.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	CallbackURL string `json:"callbackURL"`
}

// MaterialsRef names the per-run objects created from the lease's materials.
// Both carry an ownerReference to the AgentRun, so deleting the run collects
// them; no long-lived secret is left in the agents namespace.
type MaterialsRef struct {
	// SecretName holds git-token, llm-api-key, mcp.json, presigned.json and the
	// controller-minted callback-token.
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`

	// ConfigMapName holds the role's fallback config files, mounted at
	// /haliphron/role/. Absent when the role contributed no files.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ConfigMapName string `json:"configMapName,omitempty"`
}

// AgentRunStatus is what the controller observes, and nothing else. It holds no
// business state: no result text, no logs, no workflow context. Pointers and
// counters only, so that the object stays small enough to update often and
// never becomes a second place where the truth about a run lives.
type AgentRunStatus struct {
	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is derived from the Job and pod, never set directly by anything
	// outside the controller. It is monotonic within an attempt: a late
	// observation cannot move it backwards, and the first terminal phase wins.
	// +optional
	Phase runv1.Phase `json:"phase,omitempty"`

	// Reason is a short CamelCase cause in the Kubernetes idiom —
	// ImagePullBackOff, OOMKilled, DeadlineExceeded, Unschedulable.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Reason string `json:"reason,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`

	// Conditions carry the reconcile steps: Validated, JobCreated, Completed,
	// ResultReported. Phase answers "where is it"; conditions answer "what has
	// the controller managed to do about it".
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Attempt is the current attempt within this lease. It starts at the value
	// the lease carried and is incremented only by the controller, only when it
	// starts another Job after a retriable failure. The backend accepts it as
	// monotonically increasing and resets the phase rank when it grows.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=253
	JobName string `json:"jobName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PodName string `json:"podName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// +optional
	FailureClass runv1.FailureClass `json:"failureClass,omitempty"`

	// Retry records the controller's local retry budget. It is here rather than
	// in memory so that a controller restart does not hand a failing run a
	// fresh set of attempts.
	// +optional
	Retry RetryStatus `json:"retry,omitempty"`

	// Result points at what the pod uploaded. Pointers only — the result text
	// belongs in object storage, and a 64 KiB summary in etcd would be read on
	// every reconcile of every run.
	// +optional
	Result *ResultRefs `json:"result,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=512
	PRURL string `json:"prURL,omitempty"`

	// Usage is what the pod declared it spent. Self-reported and therefore not
	// authoritative; it is here for `kubectl` and for the backend's
	// cross-check against the observed Job duration.
	// +optional
	Usage *runv1.Usage `json:"usage,omitempty"`

	// Reported is the delivery bookkeeping the controller needs to survive a
	// restart: what it has already got the backend to accept. Without it, a
	// controller that restarts after a terminal phase either re-sends
	// everything forever or drops the TTL guard that keeps the CR alive until
	// the backend has heard the outcome.
	// +optional
	Reported ReportStatus `json:"reported,omitempty"`
}

// RetryStatus is the local, infra-only retry budget.
type RetryStatus struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	InfraRetries int32 `json:"infraRetries,omitempty"`
	// NextAttemptAt is when the next Job may be created. Backoff is exponential
	// with jitter; an ImagePullBackOff on a bad node otherwise turns into a
	// tight create/fail loop against the API server.
	// +optional
	NextAttemptAt *metav1.Time `json:"nextAttemptAt,omitempty"`
}

// ResultRefs are the objects the pod uploaded before it reported anything.
// Storage first, callback second: that ordering is what makes the callback an
// optimisation rather than a correctness requirement.
type ResultRefs struct {
	// +optional
	Result *runv1.ObjectRef `json:"result,omitempty"`
	// +optional
	Output *runv1.ObjectRef `json:"output,omitempty"`
	// +optional
	Log *runv1.ObjectRef `json:"log,omitempty"`
	// +optional
	State *runv1.ObjectRef `json:"state,omitempty"`
	// Completion is the full report, also written to storage by the pod. It is
	// the reason a controller crash between the callback and the ingest costs
	// nothing: the backend reads the same report from the bucket.
	// +optional
	Completion *runv1.ObjectRef `json:"completion,omitempty"`
}

// ReportStatus records what the backend has acknowledged.
type ReportStatus struct {
	// Phase is the last phase the backend accepted for Attempt.
	// +optional
	Phase runv1.Phase `json:"phase,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt,omitempty"`
	// CompletionDelivered means /ingest/completion returned accepted or
	// duplicate. Only then may the TTL reaper remove this AgentRun.
	// +optional
	CompletionDelivered bool `json:"completionDelivered,omitempty"`
	// +optional
	LastDeliveredAt *metav1.Time `json:"lastDeliveredAt,omitempty"`
}

// Condition types set by the controller.
const (
	// ConditionValidated is True once the spec passed the checks the schema
	// cannot express: parseable quantities, a resolvable image reference,
	// tolerations this cluster accepts, and — the one that matters — that the
	// API server stored the spec the controller was handed, without pruning
	// fields an older CRD does not know about.
	ConditionValidated = "Validated"
	// ConditionJobCreated is True while a Job exists for the current attempt.
	ConditionJobCreated = "JobCreated"
	// ConditionCompleted is True once the phase is terminal. Reason carries the
	// terminal phase, so a client can distinguish Succeeded from Cancelled
	// without parsing the phase field.
	ConditionCompleted = "Completed"
	// ConditionResultReported is True once the pod's completion callback has
	// been received. Its absence on a completed run is what the backend sees as
	// CompletedWithoutResult, and its cue to read the bucket itself.
	ConditionResultReported = "ResultReported"
)

// Condition reasons. Stable strings: they end up in operator runbooks.
const (
	ReasonPending             = "Pending"
	ReasonMaterialsReady      = "MaterialsReady"
	ReasonSpecPruned          = "SpecFieldsPruned"
	ReasonInvalidSpec         = "InvalidSpec"
	ReasonJobCreated          = "JobCreated"
	ReasonJobCreateFailed     = "JobCreateFailed"
	ReasonBackoff             = "BackingOff"
	ReasonCancelRequested     = "CancelRequested"
	ReasonAbandonRequested    = "AbandonRequested"
	ReasonCallbackReceived    = "CallbackReceived"
	ReasonCallbackNotReceived = "CallbackNotReceived"
)

// Labels and annotations. Selectable facts go in labels; everything else in
// annotations, because every label value is an index entry in etcd.
const (
	// LabelRunID is the lowercased ULID. It is what an operator selects on, and
	// what correlates the AgentRun, its Job, its pod and its Secret.
	LabelRunID = GroupName + "/run-id"
	// LabelEpoch and LabelAttempt make "show me the current generation of this
	// work" a selector rather than a script.
	LabelEpoch   = GroupName + "/epoch"
	LabelAttempt = GroupName + "/attempt"
	// LabelClusterID is the cluster identity the backend registered. Useful
	// when one namespace is inspected next to a backend UI.
	LabelClusterID = GroupName + "/cluster-id"
	// LabelAgent is claude-code or codex.
	LabelAgent = GroupName + "/agent"
	// LabelComponent marks the objects the controller owns.
	LabelComponent = GroupName + "/component"

	// AnnotationSpecHash is the digest of the RenderedRunSpec exactly as it
	// arrived in the lease. The controller re-reads the object after creating
	// it and compares: annotations are never pruned, spec fields an older CRD
	// does not know are silently dropped, and this is the only way to notice.
	AnnotationSpecHash = GroupName + "/spec-hash"
	// AnnotationLeasedAt is when the lease was received, for latency triage.
	AnnotationLeasedAt = GroupName + "/leased-at"
	// AnnotationRunURL links back to the run in the backend UI.
	AnnotationRunURL = GroupName + "/run-url"
)

// FinalizerTerminateJob keeps the AgentRun alive until the controller has
// stopped the Job and its pod. It is released unconditionally after a bounded
// wait: a finalizer that can wedge is worse than the leak it prevents.
const FinalizerTerminateJob = GroupName + "/terminate-job"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=ar,categories=haliphron
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.status.attempt`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.status.failureClass`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.spec.leaseEpoch`,priority=1
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`,priority=1
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.prURL`,priority=1

// AgentRun is one leased execution of one agent, as seen from inside the
// cluster that runs it.
type AgentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentRunSpec `json:"spec"`
	// +optional
	Status AgentRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRunList is a list of AgentRun. Names are lowercased ULIDs, so the
// default alphabetical listing is also chronological.
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}
