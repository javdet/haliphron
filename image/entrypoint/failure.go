// Package entrypoint is the agent image's entrypoint: the nineteen phases
// between a pod starting and a completion report being posted.
//
// It exists as a Go package rather than a shell script for a reason the
// contract states plainly — fifteen hundred lines of shell delivered through a
// ConfigMap has no versioning, no tests and a 1 MiB ceiling. Everything here is
// exercised against FakeControlPlane with `go test`, and the image is the same
// code with a Dockerfile around it.
//
// Three rules run through the whole package:
//
//   - A failure is classified by whoever has the cause, not the effect. The
//     controller sees "exit 20"; this process saw "the forge answered 403 to a
//     push into a protected branch". Every error therefore carries an exit
//     code, a reason and a message, and the classification happens where the
//     evidence is.
//   - What has been paid for is made durable before anything allowed to fail.
//     The persist phase runs immediately after the result exists and before the
//     git phases, so a retry never pays for the model twice.
//   - Nothing secret is allowed to leave. Every byte uploaded passes the
//     redactor, and the child process's environment is built rather than
//     inherited.
package entrypoint

import (
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Failure is an error that knows what it will cost. The exit code is the one
// channel that survives everything — the controller reads it from the pod's
// status even if this process never managed to say anything — and the reason
// and message are the only instance of the evidence for what that code meant.
//
// They exist once, in this pod, at the moment of failure. Losing them turns
// "Failed, config" in the UI into a user who cannot fix their run.
type Failure struct {
	// Code is the process exit code. The table lives in api/run/v1/phase.go and
	// is normative there, not here.
	Code int32
	// Reason is a short CamelCase token in the Kubernetes idiom:
	// PromptDigestMismatch, MCPServersUnavailable, PushRejected. It reaches the
	// CR's status.reason and the UI, so it is a closed vocabulary in practice
	// even though nothing enforces that.
	Reason string
	// Phase is stamped by the runner. A phase that set it itself would be a
	// phase that can lie about where it failed.
	Phase runv1.RuntimePhase

	message string
	cause   error
}

// Reason and message ceilings, from the completion report's schema. Truncating
// here rather than at the wire means the report that goes to storage and the
// one that goes to the controller are the same bytes, which is the property
// that makes either of them usable as the surviving copy.
const (
	maxReasonLen  = 128
	maxMessageLen = 1024
)

func (f *Failure) Error() string {
	if f.cause != nil {
		return f.message + ": " + f.cause.Error()
	}
	return f.message
}

func (f *Failure) Unwrap() error { return f.cause }

// Message is the human-readable explanation, bounded to what the report can
// carry. The cause is folded in here rather than kept apart: a message that
// says "push failed" without the forge's answer has cost the reader the only
// fact that mattered.
func (f *Failure) Message() string { return truncate(f.Error(), maxMessageLen) }

// Class is what the controller uses to decide whether to try again. Derived
// from the code rather than carried alongside it, so the two cannot disagree.
func (f *Failure) Class() runv1.FailureClass {
	return runv1.FailureClassForExitCode(f.Code)
}

// fail builds a classified failure.
func fail(code int32, reason, format string, args ...any) *Failure {
	return &Failure{
		Code:    code,
		Reason:  truncate(reason, maxReasonLen),
		message: fmt.Sprintf(format, args...),
	}
}

// failWrap is fail with the underlying error kept for errors.Is and for the
// message. Use it whenever there is a cause: a classified error that swallows
// what happened underneath is the reason post-mortems take a day.
func failWrap(code int32, reason string, cause error, format string, args ...any) *Failure {
	f := fail(code, reason, format, args...)
	f.cause = cause
	return f
}

// classify turns any error into a Failure. An error that arrived unclassified
// is a bug in this package rather than a condition of the run, and it is given
// the phase's own default code rather than being guessed at: the alternative is
// an unlabelled failure that the controller silently retries forever.
func classify(err error, fallback int32) *Failure {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return failWrap(fallback, "InternalError", err, "unclassified failure")
}

// errorf is fmt.Errorf under a shorter name, for the places that want a plain
// error rather than a classified one.
func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// itoa32 renders an attempt number for a header.
func itoa32(n int32) string { return strconv.Itoa(int(n)) }

// truncate cuts a string to a byte ceiling without splitting a rune. A report
// rejected for length because a message ended mid-character is a failure to
// report a failure.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
