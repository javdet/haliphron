package v1

import "encoding/json"

// The structured output of a run, as it lands in storage under
// runs/{runID}/output.json.
//
// The split this file encodes is the whole of R5: **the agent writes the
// payload, the entrypoint fills in the envelope.** The agent's file at
// FileOutput is a single JSON object and nothing more; everything mechanical
// around it — identity, timestamps, the status derived from the exit code,
// references to uploaded artifacts — is assembled by the machine.
//
// The reason is not tidiness. Every formal requirement placed on a model is
// another chance to burn a forty-minute paid run on a forgotten field, and the
// model is the one participant here that forgets. The machine does not.
//
// The normative shape is api/runtime/v1/output.schema.json; this is what the
// three Go components compile against. The entrypoint writes it, the backend
// reads it, and the workflow engine will validate Data against the schema the
// node declared. A test holds the two together.

// OutputSchemaVersion is the envelope's major. A consumer that meets a higher
// one must refuse rather than pick through the keys it recognises: the fields
// it knows may have changed meaning, and a partial read of a contract is worse
// than no read at all.
const OutputSchemaVersion = 1

// OutputStatus is the envelope's verdict on the run, derived from the exit code
// rather than declared by the agent.
//
// The agent's own assessment of whether it succeeded would be worth exactly
// what its assessment of its own cost is worth, which is why neither is taken.
//
// +kubebuilder:validation:Enum=ok;partial;failed
type OutputStatus string

const (
	// OutputStatusOK is exit 0.
	OutputStatusOK OutputStatus = "ok"
	// OutputStatusPartial is exit 11: the run hit its timeout, and there is
	// work all the same. Forty minutes that did not fit into an hour cost the
	// same as forty minutes that did.
	OutputStatusPartial OutputStatus = "partial"
	// OutputStatusFailed is everything else. The output was kept for the
	// post-mortem, not because anyone trusts it.
	OutputStatusFailed OutputStatus = "failed"
)

// OutputStatusForExitCode derives the envelope's status. It is deliberately
// total: an exit code this build has never seen is a failure, because the one
// thing worse than losing a result is reporting an unexamined one as good.
func OutputStatusForExitCode(code int32) OutputStatus {
	switch code {
	case ExitSuccess:
		return OutputStatusOK
	case ExitAgentTimeout:
		return OutputStatusPartial
	default:
		return OutputStatusFailed
	}
}

// OutputEnvelope is runs/{runID}/output.json.
type OutputEnvelope struct {
	SchemaVersion int   `json:"schemaVersion"`
	RunID         ULID  `json:"runID"`
	Attempt       int32 `json:"attempt"`

	Agent AgentType `json:"agent"`
	// +optional
	Model string `json:"model,omitempty"`

	Status OutputStatus `json:"status"`
	// ProducedAt is RFC 3339, set by the entrypoint when it assembles the
	// envelope — not when the agent finished, and not when the object was
	// uploaded. The three differ by the length of the git phases.
	ProducedAt string `json:"producedAt"`

	// Summary is the first 64 KiB of result.md, duplicated here so that a
	// consumer of the structured output does not fetch a second object for one
	// line of UI.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Data is the payload exactly as the agent wrote it, byte for byte. It is
	// json.RawMessage and not map[string]any for two reasons: the entrypoint
	// has no business reordering keys inside something it did not author, and
	// the node's schema is validated against the bytes rather than against a
	// Go round-trip of them.
	//
	// An empty object is a legal value and the normal outcome of a run with no
	// declared node schema. It is never null: a consumer that must branch on
	// null before it can index is a consumer that will forget to.
	Data json.RawMessage `json:"data"`

	// Artifacts are references to the files uploaded from DirArtifacts, never
	// their contents: nothing bounds the size of what an agent decides to keep.
	// +optional
	Artifacts []OutputArtifact `json:"artifacts,omitempty"`
}

// OutputArtifact is one file the agent left in DirArtifacts, as uploaded.
type OutputArtifact struct {
	// Path is relative to DirArtifacts, which is what the agent knows. Key is
	// where it ended up, which is what the backend needs. Both, because
	// neither side should have to reconstruct the other's addressing.
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path"`
	// +kubebuilder:validation:MaxLength=1024
	Key string `json:"key"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ContentType string `json:"contentType,omitempty"`
}
