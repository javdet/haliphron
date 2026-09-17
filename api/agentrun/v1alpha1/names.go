package v1alpha1

import (
	"fmt"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Object names are derived, not invented, and derived from the full run ID.
//
// The obvious shortening — take the first eight characters of the ULID — does
// not work: those characters encode the timestamp to roughly one second, so two
// runs submitted in the same second collide, and the collision surfaces as one
// run silently adopting another's Secret. A full lowercased ULID costs 26
// characters out of the 253 a name may have, and the alphabet is
// case-insensitive, so lowercasing loses nothing.
//
// These helpers live in the contract package because the backend's tests assert
// on the objects the controller builds, and a second copy of the rule is a
// second chance to get it wrong.
const (
	namePrefix      = "ar-"
	suffixJob       = "-j"
	suffixSecret    = "-s"
	suffixConfigMap = "-c"
)

// ObjectName returns the AgentRun name for a run. Stable across attempts and
// across epochs: one run has at most one AgentRun in a cluster at a time, which
// is what makes "is this work already here" a get rather than a list.
func ObjectName(runID runv1.ULID) string {
	return namePrefix + strings.ToLower(string(runID))
}

// JobName returns the Job name for one attempt. The attempt is part of the name
// because each attempt gets its own Job: the Job's own backoffLimit is set to
// zero, so that every restart is a decision the controller makes, counts and
// reports, rather than one Kubernetes makes silently.
func JobName(runID runv1.ULID, attempt int32) string {
	return fmt.Sprintf("%s%s%d", ObjectName(runID), suffixJob, attempt)
}

// SecretName returns the per-run Secret name.
func SecretName(runID runv1.ULID) string { return ObjectName(runID) + suffixSecret }

// ConfigMapName returns the per-run role ConfigMap name.
func ConfigMapName(runID runv1.ULID) string { return ObjectName(runID) + suffixConfigMap }
