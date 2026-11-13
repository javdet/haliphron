// Package reconcile drives each run towards the state its row describes: the
// Docker host's counterpart of controller/agentrun.
//
// The contract is the one section 14 of the CRD document states for
// Kubernetes, and docker-host.md §7 restates for this runtime: reconciliation
// is a pure function of the row and of what the daemon reports at that moment,
// with nothing remembered between calls. Everything that must survive a
// restart is in the row; everything arriving from outside — the backend's
// commands, the agent's reports — arrives as a column write. A controller
// restart is therefore uneventful, and the phase table below is testable
// without a daemon.
package reconcile

import (
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// DefaultStartupDeadline is the budget from the first pull or create to a
// running process. Without it an image that never arrives waits forever.
const DefaultStartupDeadline = 600 * time.Second

// Reasons. Those that mean what a Kubernetes reason means keep its string, so
// the UI and the backend's explanations need no second vocabulary.
const (
	ReasonAwaitingAck       = "AwaitingAck"
	ReasonAwaitingContainer = "AwaitingContainer"
	ReasonPullingImage      = "PullingImage"
	ReasonImagePullBackOff  = "ImagePullBackOff"
	ReasonContainerCreated  = "ContainerCreated"
	ReasonContainerGone     = "ContainerDisappeared"
	ReasonImagePullFailed   = "ImagePullFailed"
	ReasonStartupDeadline   = "StartupDeadlineExceeded"
	ReasonStartFailed       = "ContainerStartFailed"
	ReasonDeadline          = "DeadlineExceeded"
	ReasonOOMKilled         = "OOMKilled"
	ReasonHostRestarted     = "HostRestarted"
	ReasonCancelled         = "Cancelled"
	ReasonCompleted         = "Completed"
	ReasonInvalidSpec       = "InvalidSpec"
)

// Observation is what the controller can see about one attempt: the entire
// input to the phase decision. The agent's own account of what happened
// arrives separately, through the callback.
type Observation struct {
	Phase        runv1.Phase
	Reason       string
	Message      string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	ExitCode     *int32
	FailureClass runv1.FailureClass
}

// Derive is the phase table of docker-host.md §7.2. ctr is the current
// attempt's container, or nil when the daemon has none by that name.
func Derive(r *state.Run, ctr *docker.Container, now time.Time, startupDeadline time.Duration) Observation {
	if startupDeadline <= 0 {
		startupDeadline = DefaultStartupDeadline
	}
	st := r.Status

	if ctr == nil {
		// A container that was recorded and is gone, on a run that had not
		// finished, is somebody running docker rm. Infra, so the controller
		// retries it — which is what an operator removing a stuck container
		// meant.
		if st.ContainerName != "" && !st.Phase.IsTerminal() {
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       ReasonContainerGone,
				Message:      fmt.Sprintf("container %s is gone while the run was %s", st.ContainerName, st.Phase),
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(now),
			}
		}
		if st.StartupBeganAt != nil {
			if expired(*st.StartupBeganAt, startupDeadline, now) {
				if st.PullError != "" {
					return Observation{
						Phase:        runv1.PhaseFailed,
						Reason:       ReasonImagePullFailed,
						Message:      "the image could not be pulled within the startup deadline: " + st.PullError,
						FailureClass: runv1.FailureInfra,
						FinishedAt:   ptr(now),
					}
				}
				return Observation{
					Phase:        runv1.PhaseFailed,
					Reason:       ReasonStartupDeadline,
					Message:      "no container was started within the startup deadline",
					FailureClass: runv1.FailureInfra,
					FinishedAt:   ptr(now),
				}
			}
			if st.PullError != "" {
				return Observation{Phase: runv1.PhaseStarting, Reason: ReasonImagePullBackOff, Message: st.PullError}
			}
			return Observation{Phase: runv1.PhaseStarting, Reason: ReasonPullingImage}
		}
		if !r.IsAcknowledged() {
			return Observation{Phase: runv1.PhasePending, Reason: ReasonAwaitingAck}
		}
		return Observation{Phase: runv1.PhasePending, Reason: ReasonAwaitingContainer}
	}

	s := ctr.State
	switch s.Status {
	case docker.StateCreated:
		if s.Error != "" {
			// The runtime refused to start the process: a mount that is not
			// there, an entrypoint that does not exist. Retried as infra, like
			// CreateContainerError past the deadline on Kubernetes.
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       ReasonStartFailed,
				Message:      s.Error,
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(now),
			}
		}
		anchor := ctr.Created
		if st.StartupBeganAt != nil {
			anchor = *st.StartupBeganAt
		}
		if expired(anchor, startupDeadline, now) {
			return Observation{
				Phase:        runv1.PhaseFailed,
				Reason:       ReasonStartupDeadline,
				Message:      "the container was created and did not start within the startup deadline",
				FailureClass: runv1.FailureInfra,
				FinishedAt:   ptr(now),
			}
		}
		return Observation{Phase: runv1.PhaseStarting, Reason: ReasonContainerCreated}

	case docker.StateRunning, docker.StatePaused, docker.StateRestarting:
		obs := Observation{Phase: runv1.PhaseRunning, Reason: string(runv1.PhaseRunning)}
		if s.Started() {
			obs.StartedAt = ptr(s.StartedAt)
		}
		return obs
	}

	// exited, dead, or on its way out with an exit code already recorded.
	code := int32(s.ExitCode)
	obs := Observation{ExitCode: ptr(code)}
	if s.Started() {
		obs.StartedAt = ptr(s.StartedAt)
	}
	if !s.FinishedAt.IsZero() && s.FinishedAt.Year() > 1 {
		obs.FinishedAt = ptr(s.FinishedAt)
	} else {
		obs.FinishedAt = ptr(now)
	}

	switch {
	case st.Stop != nil && st.Stop.Reason == state.StopDeadline:
		// The controller's own backstop fired. The agent's timeout is enforced
		// inside the container and exits 11 long before this; reaching it
		// means the entrypoint itself hung.
		obs.Phase = runv1.PhaseTimedOut
		obs.Reason = ReasonDeadline
		obs.Message = "the container exceeded timeoutSeconds plus the deadline slack"
		obs.FailureClass = runv1.FailureInfra
	case !s.Started() && s.Error != "":
		obs.Phase = runv1.PhaseFailed
		obs.Reason = ReasonStartFailed
		obs.Message = s.Error
		obs.FailureClass = runv1.FailureInfra
	case code == runv1.ExitSuccess:
		// Success is exit code zero and nothing else.
		obs.Phase = runv1.PhaseSucceeded
		obs.Reason = ReasonCompleted
		obs.FailureClass = runv1.FailureNone
	case s.OOMKilled:
		obs.Phase = runv1.PhaseForExitCode(code)
		obs.Reason = ReasonOOMKilled
		obs.Message = "the container exceeded its memory limit"
		obs.FailureClass = runv1.FailureClassForExitCode(code)
	case code > 128 && st.Stop == nil:
		// Killed by a signal nobody here sent. On one host that is the daemon
		// stopping — a restart, a reboot — and the work is lost to the
		// platform, not to the agent: infra, retried within the epoch.
		obs.Phase = runv1.PhaseFailed
		obs.Reason = ReasonHostRestarted
		obs.Message = fmt.Sprintf("the container was killed (exit %d) without the controller stopping it", code)
		obs.FailureClass = runv1.FailureInfra
	default:
		obs.Phase = runv1.PhaseForExitCode(code)
		obs.Reason = string(obs.Phase)
		obs.Message = s.Error
		obs.FailureClass = runv1.FailureClassForExitCode(code)
	}
	return obs
}

// Monotonic applies the ordering rule: within one attempt a phase never moves
// backwards, and the first terminal phase wins. The backend applies the same
// rule to the report, so the two cannot disagree about a cancellation that
// raced a successful exit.
func Monotonic(r *state.Run, attempt int32, obs Observation) Observation {
	st := r.Status
	if st.Attempt != attempt || st.Phase == "" {
		return obs
	}
	if st.Phase.IsTerminal() || obs.Phase.Rank() < st.Phase.Rank() {
		return Observation{
			Phase:        st.Phase,
			Reason:       st.Reason,
			Message:      st.Message,
			StartedAt:    st.StartedAt,
			FinishedAt:   st.FinishedAt,
			ExitCode:     st.ExitCode,
			FailureClass: st.FailureClass,
		}
	}
	return obs
}

// outranksCancellation reports whether an observation is an outcome the run
// reached on its own. Only a deliberate exit is: a container killed by a
// signal during a cancellation was killed by the cancellation.
func outranksCancellation(obs Observation) bool {
	return obs.Phase.IsTerminal() && obs.ExitCode != nil && *obs.ExitCode <= 128
}

func expired(start time.Time, budget time.Duration, now time.Time) bool {
	return !start.IsZero() && now.Sub(start) > budget
}

func ptr[T any](v T) *T { return &v }
