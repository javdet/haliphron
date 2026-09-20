package v1

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// What the pod says to the controller before it is finished: the phases it has
// got through, and — in relay mode — the artifact bytes themselves.
//
// Both travel to the same in-cluster Service the completion already goes to,
// on paths of their own, authenticated with the same SecretKeyCallbackToken.
// The pod still has no credential for the control plane and still needs no
// egress to it; in relay mode it needs none to an object store either, which
// is what reduces the agent namespace's whole egress allowance to DNS, the
// controller, the model, the git host and the permitted MCPs.

// Callback paths, relative to the callback URL the controller put in the CR.
// They are constants because two independently written sides address them: the
// entrypoint posts and the controller routes, and a divergence looks like "the
// agent finished and no result arrived".
const (
	// CallbackPathCompletion is the report, posted last.
	CallbackPathCompletion = "/completion"
	// CallbackPathPhase is one phase transition, posted as it happens.
	CallbackPathPhase = "/phase"
	// CallbackPathArtifacts is one object, posted with its bytes as the body.
	// Relay mode only; in object-store mode the pod PUTs to the store instead
	// and this endpoint is never called.
	CallbackPathArtifacts = "/artifacts"
)

// Query and header names on CallbackPathArtifacts. Metadata rides outside the
// body because the body is the object: an artifact has no ceiling worth
// wrapping in JSON, and base64 in a field would cost a third of the bytes on
// the one path in the system that carries gigabytes.
const (
	// QueryKey is the object's key relative to the run's prefix — "result.md",
	// "logs/chunks/7.log". Relative, and stamped with the run prefix by the
	// controller from the CR it authenticated against: a pod that could name an
	// absolute key could name another run's.
	QueryKey = "key"
	// QueryAttempt is which attempt produced it.
	QueryAttempt = "attempt"
	// HeaderSHA256 is the digest of the body as the pod computed it. The
	// controller verifies it before acknowledging, so a truncated upload is
	// refused rather than spooled.
	HeaderSHA256 = "X-Haliphron-SHA256"
)

// PhaseReport is one entrypoint phase, reported as it completes.
//
// It exists so that the checkpoint survives the pod. The previous design wrote
// the same fact into an object the next attempt read back; this one hands it to
// something that outlives the pod at the moment it becomes true, which means a
// pod killed by an OOM between two phases has still recorded the one it
// finished.
type PhaseReport struct {
	RunID   ULID  `json:"runID"`
	Attempt int32 `json:"attempt"`

	Phase   RuntimePhase `json:"phase"`
	Outcome PhaseOutcome `json:"outcome"`
	// +optional
	DurationMs int64 `json:"durationMs,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ArtifactAck is what the controller answers an upload with.
//
// The pod waits for it, and what it means is the whole of principle P5 in relay
// mode: "written to a disk that is not yours". From that moment the pod may
// exit — the controller owns delivery onward, and it survives both the pod's
// deletion and a backend outage.
type ArtifactAck struct {
	// Ref is the object as the controller stored it, with the run prefix the
	// controller stamped and Uploaded set. The pod puts it in its report
	// verbatim rather than constructing one of its own.
	Ref ObjectRef `json:"ref"`
	// BytesRemaining is what is left of the run's artifact budget. Reported so
	// that an entrypoint about to upload a two-gigabyte log can drop it and say
	// so, instead of discovering the cap as a refusal after the transfer.
	// +optional
	BytesRemaining int64 `json:"bytesRemaining,omitempty"`
}

// ArtifactKey checks a key a pod chose against the layout it may write into,
// and returns it normalised.
//
// It lives in the contract because three components apply it: the controller
// before it spools, the backend before it stores, and the disk store before it
// writes. Three copies of a rule is three chances to disagree, and the
// disagreement that matters is the cheap one — a key the controller accepts and
// the backend refuses is an object spooled, forwarded, rejected and dropped,
// with the pod long gone and no way to tell it.
//
// An allow-list of names and prefixes rather than a sanitiser. The four fixed
// keys are the ones the contract names; the two open prefixes are the ones
// whose object names genuinely cannot be known in advance, log chunks and the
// agent's own artifacts. Anything else is refused by name, so a new key in the
// layout is a deliberate edit here rather than something a pod can invent.
//
// It refuses rather than repairs. Cleaning "../x" into "x" would accept a key
// that is a defect upstream and land the object somewhere nobody looks for it.
func ArtifactKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return "", errors.New("the empty key names no object")
	case strings.HasPrefix(key, "/"):
		return "", fmt.Errorf("%q is absolute; keys are relative to the run", key)
	case key != path.Clean(key):
		return "", fmt.Errorf("%q is not normalised", key)
	case strings.Contains(key, ".."):
		return "", fmt.Errorf("%q escapes the run's prefix", key)
	}

	switch key {
	case StorageKeyResult, StorageKeyOutput, StorageKeyCompletion, StorageKeyAgentLog:
		return key, nil
	}
	for _, prefix := range []string{StoragePrefixChunks, StoragePrefixArtifacts} {
		if strings.HasPrefix(key, prefix) && len(key) > len(prefix) {
			return key, nil
		}
	}
	return "", fmt.Errorf("%q is not a key a run may write; "+
		"the layout admits %s, %s, %s, %s and anything under %s or %s",
		key, StorageKeyResult, StorageKeyOutput, StorageKeyCompletion,
		StorageKeyAgentLog, StoragePrefixChunks, StoragePrefixArtifacts)
}
