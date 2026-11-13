package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Notifier receives the phase changes the reconciler observes. The
// implementation queues: a control plane that is down slows nothing here.
type Notifier interface {
	Observed(obs clusterv1.RunObservation)
	// Forget drops anything queued about a run taken away from this host.
	Forget(runID runv1.ULID)
}

// ArtifactForgetter drops a run's spool, on abandon and when a row is reaped.
type ArtifactForgetter interface {
	ForgetArtifacts(runID runv1.ULID)
}

// BundleRefresher mints a fresh presigned bundle, in object-store mode.
type BundleRefresher interface {
	Artifacts(ctx context.Context, runID runv1.ULID, req clusterv1.ArtifactBundleRequest) (*clusterv1.ArtifactBundle, error)
}

// Defaults carried over from controller/agentrun, so the two runtimes retry,
// wait and collect on the same schedule.
const (
	DefaultMaxInfraRetries int32 = 3
	DefaultTTL                   = 24 * time.Hour
	DefaultHardTTL               = 7 * 24 * time.Hour
	DefaultAckWaitBudget         = 5 * time.Minute
	retryBase                    = 30 * time.Second
	retryCeiling                 = 5 * time.Minute
	// killSlack is how long past the grace period a stopped container may
	// keep running before the controller sends SIGKILL itself.
	killSlack = 5 * time.Second
)

// Reconciler drives one run at a time. It is safe for concurrent use on
// different runs; the driver guarantees one worker per run.
type Reconciler struct {
	Store     *state.Store
	Engine    docker.Engine
	Host      launcher.Host
	Notifier  Notifier
	Artifacts ArtifactForgetter
	Bundles   BundleRefresher
	Auths     *docker.Auths

	StartupDeadline time.Duration
	BundleSlack     time.Duration
	HardTTL         time.Duration
	AckWaitBudget   time.Duration
	// ChownFiles is false only in tests that do not run as root.
	ChownFiles bool
	// RetryBase is the first pause between attempts; zero is the default 30 s.
	// Tests shorten it.
	RetryBase time.Duration

	Clock func() time.Time
	Log   *slog.Logger
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Reconcile observes one run and does the one thing the observation calls
// for. It returns when to look again; zero leaves it to events and the resync.
//
// The order is the Kubernetes controller's. Abandon first, because an
// abandoned run must not be reported on even once more. Then the container is
// observed, and the observation is folded into the row before anything acts on
// it, so that whatever happens next the backend has been told what was seen.
func (r *Reconciler) Reconcile(ctx context.Context, runID runv1.ULID) (time.Duration, error) {
	row, err := r.Store.Get(ctx, runID)
	if errors.Is(err, state.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if row.IsAbandoned() {
		return 0, r.abandon(ctx, &row)
	}

	attempt := row.CurrentAttempt()
	ctr, err := r.container(ctx, &row, attempt)
	if err != nil {
		return 0, err
	}

	obs := Derive(&row, ctr, r.now(), r.StartupDeadline)
	requeue := time.Duration(0)

	if row.IsCancelRequested() {
		var wait time.Duration
		if obs, wait, err = r.applyCancel(ctx, &row, ctr, obs); err != nil {
			return 0, err
		}
		requeue = minPositive(requeue, wait)
	} else if wait, err := r.enforceDeadline(ctx, &row, ctr, obs); err != nil {
		return 0, err
	} else {
		requeue = minPositive(requeue, wait)
	}
	obs = Monotonic(&row, attempt, obs)

	changed, err := r.applyObservation(ctx, &row, attempt, obs)
	if errors.Is(err, state.ErrConflict) {
		return time.Millisecond, nil
	}
	if err != nil {
		return 0, err
	}
	if changed {
		r.notify(&row)
	}

	if row.Status.Phase.IsTerminal() {
		wait, err := r.handleTerminal(ctx, &row, ctr)
		return minPositive(requeue, wait), err
	}
	if ctr == nil {
		if !row.IsAcknowledged() {
			return r.awaitAck(ctx, &row)
		}
		wait, err := r.ensureContainer(ctx, &row, attempt)
		return minPositive(requeue, wait), err
	}
	return minPositive(requeue, r.startupCheck(&row, ctr)), nil
}

// container fetches the current attempt's container by its deterministic name.
func (r *Reconciler) container(ctx context.Context, row *state.Run, attempt int32) (*docker.Container, error) {
	c, err := r.Engine.ContainerInspect(ctx, launcher.ContainerName(row.RunID, row.Epoch, attempt))
	if errors.Is(err, docker.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect the container of %s attempt %d: %w", row.RunID, attempt, err)
	}
	// A container of that name with somebody else's labels is not ours, and
	// reading it as ours would report a stranger's exit code.
	if c.Config.Labels[launcher.LabelManagedBy] != launcher.ManagedBy {
		return nil, fmt.Errorf("container %s exists and is not managed by this controller", c.Name)
	}
	return &c, nil
}

// startupCheck is when to look again at a run that has not started: the one
// deadline no event will ever announce.
func (r *Reconciler) startupCheck(row *state.Run, ctr *docker.Container) time.Duration {
	if row.Status.Phase == runv1.PhaseRunning {
		return r.deadlineCheck(row)
	}
	anchor := ctr.Created
	if row.Status.StartupBeganAt != nil {
		anchor = *row.Status.StartupBeganAt
	}
	return atLeastSecond(anchor.Add(r.startupDeadline()).Sub(r.now()))
}

// deadlineCheck is when the run's own backstop fires.
func (r *Reconciler) deadlineCheck(row *state.Run) time.Duration {
	if row.Status.StartedAt == nil {
		return 0
	}
	return atLeastSecond(row.Status.StartedAt.Add(r.runDeadline(row)).Sub(r.now()))
}

func (r *Reconciler) runDeadline(row *state.Run) time.Duration {
	slack := r.Host.DeadlineSlackSeconds
	if slack <= 0 {
		slack = 600
	}
	return time.Duration(int64(row.Spec.Runtime.TimeoutSeconds)+slack) * time.Second
}

func (r *Reconciler) startupDeadline() time.Duration {
	if r.StartupDeadline > 0 {
		return r.StartupDeadline
	}
	return DefaultStartupDeadline
}

func (r *Reconciler) grace(row *state.Run) time.Duration {
	if row.CancelGraceSeconds > 0 && row.IsCancelRequested() {
		return time.Duration(row.CancelGraceSeconds) * time.Second
	}
	if r.Host.GraceSeconds > 0 {
		return time.Duration(r.Host.GraceSeconds) * time.Second
	}
	return 60 * time.Second
}

// enforceDeadline is activeDeadlineSeconds, enforced by the controller: a run
// still running past timeoutSeconds plus the slack, measured from the start
// time the daemon reported, is stopped.
func (r *Reconciler) enforceDeadline(ctx context.Context, row *state.Run, ctr *docker.Container, obs Observation) (time.Duration, error) {
	if obs.Phase != runv1.PhaseRunning || obs.StartedAt == nil || ctr == nil {
		return 0, nil
	}
	if r.now().Before(obs.StartedAt.Add(r.runDeadline(row))) {
		return 0, nil
	}
	return r.stop(ctx, row, ctr, state.StopDeadline, r.grace(row))
}

// applyCancel carries out a cancellation: SIGTERM with the command's grace,
// so the entrypoint can persist and report, then SIGKILL. The phase becomes
// Cancelled only once the process is gone, and only if it did not reach an
// outcome of its own first.
func (r *Reconciler) applyCancel(ctx context.Context, row *state.Run, ctr *docker.Container, obs Observation) (Observation, time.Duration, error) {
	if outranksCancellation(obs) {
		return obs, 0, nil
	}
	if ctr != nil && !ctr.State.Finished() {
		if ctr.State.Status == docker.StateCreated {
			// Never started: there is no process to give a grace period to.
			if err := r.Engine.ContainerRemove(ctx, ctr.ID, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
				return obs, 0, fmt.Errorf("remove the unstarted container of %s: %w", row.RunID, err)
			}
		} else {
			wait, err := r.stop(ctx, row, ctr, state.StopCancel, r.grace(row))
			return obs, wait, err
		}
	}
	obs.Phase = runv1.PhaseCancelled
	obs.Reason = ReasonCancelled
	obs.Message = row.CancelReason
	obs.FailureClass = runv1.FailureNone
	if obs.FinishedAt == nil {
		obs.FinishedAt = ptr(r.now())
	}
	return obs, 0, nil
}

// stop is the controller's stop sequence. Every signal is recorded in the row
// before it is sent: that record is how a container this controller killed is
// told apart from one a daemon restart killed.
func (r *Reconciler) stop(ctx context.Context, row *state.Run, ctr *docker.Container, reason string, grace time.Duration) (time.Duration, error) {
	now := r.now()
	stop := row.Status.Stop
	switch {
	case stop == nil:
		rec := &state.StopRecord{Reason: reason, At: now, GraceSeconds: int64(grace.Seconds())}
		written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
			if cur.Epoch != row.Epoch || cur.Status.Stop != nil {
				return false
			}
			cur.Status.Stop = rec
			return true
		})
		if err != nil {
			return 0, err
		}
		*row = written
		if row.Status.Stop == nil || row.Status.Stop.At != now {
			return time.Millisecond, nil
		}
		r.log().Info("stopping a container", "runID", row.RunID, "reason", reason, "grace", grace)
		if err := r.Engine.ContainerKill(ctx, ctr.ID, "SIGTERM"); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return 0, fmt.Errorf("signal the container of %s: %w", row.RunID, err)
		}
		return grace + killSlack, nil
	case stop.KilledAt == nil:
		killAt := stop.At.Add(time.Duration(stop.GraceSeconds)*time.Second + killSlack)
		if now.Before(killAt) {
			return killAt.Sub(now), nil
		}
		written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
			if cur.Status.Stop == nil || cur.Status.Stop.KilledAt != nil {
				return false
			}
			cur.Status.Stop.KilledAt = ptr(now)
			return true
		})
		if err != nil {
			return 0, err
		}
		*row = written
		r.log().Warn("the container outlived its grace period; killing it", "runID", row.RunID)
		if err := r.Engine.ContainerKill(ctx, ctr.ID, "SIGKILL"); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return 0, fmt.Errorf("kill the container of %s: %w", row.RunID, err)
		}
		return 10 * time.Second, nil
	default:
		return 10 * time.Second, nil
	}
}

// notify hands the current state to the reporter. Nothing is said about an
// abandoned run, ever.
func (r *Reconciler) notify(row *state.Run) {
	if row.IsAbandoned() || r.Notifier == nil {
		return
	}
	r.Notifier.Observed(ObservationFor(row, r.now()))
}

// ObservationFor is what the backend is told about a row: pointers and facts,
// never content.
func ObservationFor(row *state.Run, now time.Time) clusterv1.RunObservation {
	st := row.Status
	obs := clusterv1.RunObservation{
		RunID:           row.RunID,
		Epoch:           row.Epoch,
		Attempt:         max(st.Attempt, 1),
		Phase:           st.Phase,
		Reason:          st.Reason,
		Message:         truncate(st.Message, 1024),
		JobName:         st.ContainerName,
		PodName:         st.ContainerName,
		ExitCode:        st.ExitCode,
		FailureClass:    st.FailureClass,
		CompletedPhases: st.CompletedPhases,
		ObservedAt:      ptr(now.UTC()),
	}
	if st.StartedAt != nil {
		obs.StartedAt = ptr(st.StartedAt.UTC())
	}
	if st.FinishedAt != nil {
		obs.FinishedAt = ptr(st.FinishedAt.UTC())
	}
	return obs
}

// applyObservation folds an observation into the row and writes it if
// anything changed. A status nobody changed is not news.
func (r *Reconciler) applyObservation(ctx context.Context, row *state.Run, attempt int32, obs Observation) (bool, error) {
	next := *row
	st := &next.Status
	st.Attempt = attempt
	st.Phase = obs.Phase
	st.Reason = obs.Reason
	st.Message = truncate(obs.Message, 1024)
	if obs.StartedAt != nil {
		st.StartedAt = obs.StartedAt
	}
	if obs.FinishedAt != nil {
		st.FinishedAt = obs.FinishedAt
	}
	if obs.ExitCode != nil {
		st.ExitCode = obs.ExitCode
	}
	if obs.FailureClass != "" {
		st.FailureClass = obs.FailureClass
	}
	if sameObservation(row.Status, *st) {
		return false, nil
	}
	written, err := r.Store.Update(ctx, next)
	if err != nil {
		return false, err
	}
	*row = written
	return true, nil
}

func sameObservation(a, b state.Status) bool {
	return a.Attempt == b.Attempt && a.Phase == b.Phase && a.Reason == b.Reason &&
		a.Message == b.Message && a.FailureClass == b.FailureClass &&
		equalTime(a.StartedAt, b.StartedAt) && equalTime(a.FinishedAt, b.FinishedAt) &&
		equalInt(a.ExitCode, b.ExitCode)
}

// handleTerminal decides what follows an attempt's end: the attempt's
// container and volumes go, then another attempt or the wait for delivery and
// the TTL.
func (r *Reconciler) handleTerminal(ctx context.Context, row *state.Run, ctr *docker.Container) (time.Duration, error) {
	if ctr != nil && !ctr.State.Finished() && ctr.State.Status != docker.StateCreated {
		// Terminal in the row with a process still running: a validation
		// failure recorded over a container, or a phase that won the
		// monotonic rule. The process has no future either way.
		if err := r.Engine.ContainerKill(ctx, ctr.ID, "SIGKILL"); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return 0, err
		}
	}
	// Whatever ended the attempt — an exit, a removed container, a startup
	// that never happened — its secret files must not outlive it.
	if attempt := row.CurrentAttempt(); ctr != nil || row.Status.CleanedAttempt != attempt {
		if err := r.cleanupAttempt(ctx, row, attempt); err != nil {
			r.log().Warn("removing a finished attempt", "runID", row.RunID, "error", err)
			return 10 * time.Second, nil
		}
		written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
			if cur.Epoch != row.Epoch || cur.Status.CleanedAttempt == attempt {
				return false
			}
			cur.Status.CleanedAttempt = attempt
			return true
		})
		if err != nil {
			return 0, err
		}
		*row = written
	}
	if r.shouldRetry(row) {
		return r.scheduleRetry(ctx, row)
	}
	if at := row.Status.Retry.NextAttemptAt; at != nil {
		if wait := at.Sub(r.now()); wait > 0 {
			return wait, nil
		}
		return r.startNextAttempt(ctx, row)
	}
	return r.reap(ctx, row)
}

// cleanupAttempt removes one attempt's container, its four volumes and its
// secret files. Called only once the attempt's terminal state is in the row:
// the exit code has been read and the observation is owed from the row, not
// from the container.
func (r *Reconciler) cleanupAttempt(ctx context.Context, row *state.Run, attempt int32) error {
	name := launcher.ContainerName(row.RunID, row.Epoch, attempt)
	if err := r.Engine.ContainerRemove(ctx, name, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
		return fmt.Errorf("remove container %s: %w", name, err)
	}
	for _, v := range launcher.VolumeNames(row.RunID, row.Epoch, attempt) {
		if err := r.Engine.VolumeRemove(ctx, v, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("remove volume %s: %w", v, err)
		}
	}
	return launcher.RemoveFiles(r.Host.AttemptDir(row.RunID, row.Epoch, attempt))
}

// shouldRetry: only infra and git failures are repeated, within the budget.
func (r *Reconciler) shouldRetry(row *state.Run) bool {
	st := row.Status
	if st.Retry.NextAttemptAt != nil || !st.Phase.IsTerminal() || st.Phase == runv1.PhaseSucceeded {
		return false
	}
	if st.Phase == runv1.PhaseCancelled || row.IsCancelRequested() {
		return false
	}
	if !st.FailureClass.Retriable() {
		return false
	}
	return st.Retry.InfraRetries < maxInfraRetries(row)
}

func maxInfraRetries(row *state.Run) int32 {
	if row.Spec.Retry != nil && row.Spec.Retry.MaxInfraRetries != nil {
		return *row.Spec.Retry.MaxInfraRetries
	}
	return DefaultMaxInfraRetries
}

// scheduleRetry books the next attempt without starting it: the terminal phase
// of the attempt that ended stays in the row until then, which is what
// guarantees the backend hears about it.
func (r *Reconciler) scheduleRetry(ctx context.Context, row *state.Run) (time.Duration, error) {
	retries := row.Status.Retry.InfraRetries + 1
	delay := retryDelay(retries, r.RetryBase)
	at := r.now().Add(delay)
	written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch || cur.Status.Retry.NextAttemptAt != nil {
			return false
		}
		cur.Status.Retry.InfraRetries = retries
		cur.Status.Retry.NextAttemptAt = &at
		return true
	})
	if err != nil {
		return 0, err
	}
	*row = written
	r.log().Info("an attempt failed and will be retried",
		"runID", row.RunID, "attempt", row.Status.Attempt, "class", row.Status.FailureClass,
		"retry", retries, "of", maxInfraRetries(row), "in", delay.Round(time.Second))
	return delay, nil
}

// startNextAttempt raises the attempt and clears the observations of the one
// that failed. The new attempt is what resets the phase rank on the backend.
func (r *Reconciler) startNextAttempt(ctx context.Context, row *state.Run) (time.Duration, error) {
	next := row.Status.Attempt + 1
	written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch || cur.Status.Attempt != row.Status.Attempt {
			return false
		}
		st := &cur.Status
		st.Attempt = next
		st.Phase = runv1.PhasePending
		st.Reason = ReasonAwaitingContainer
		st.Message = ""
		st.ExitCode = nil
		st.FailureClass = ""
		st.StartedAt = nil
		st.FinishedAt = nil
		st.ContainerName = ""
		st.ContainerID = ""
		st.StartupBeganAt = nil
		st.PullError = ""
		st.Stop = nil
		st.Retry.NextAttemptAt = nil
		return true
	})
	if err != nil {
		return 0, err
	}
	*row = written
	r.notify(row)
	return time.Millisecond, nil
}

// retryDelay doubles from the base, capped, with up to a tenth of jitter added
// on top and never taken away: the schedule is a floor.
func retryDelay(retry int32, base time.Duration) time.Duration {
	if base <= 0 {
		base = retryBase
	}
	d := base << min(retry-1, 5)
	if d > retryCeiling {
		d = retryCeiling
	}
	return d + time.Duration(rand.Int64N(int64(d/10)+1))
}

// ensureContainer prepares and starts the current attempt: materials, image,
// container. Nothing here runs before the acknowledgement — the caller checked.
func (r *Reconciler) ensureContainer(ctx context.Context, row *state.Run, attempt int32) (time.Duration, error) {
	if at := row.Status.Retry.NextAttemptAt; at != nil {
		if wait := at.Sub(r.now()); wait > 0 {
			return wait, nil
		}
	}
	if row.Status.StartupBeganAt == nil {
		began := r.now()
		written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
			if cur.Epoch != row.Epoch || cur.Status.StartupBeganAt != nil {
				return false
			}
			cur.Status.StartupBeganAt = &began
			cur.Status.Attempt = attempt
			return true
		})
		if err != nil {
			return 0, err
		}
		*row = written
	}

	presigned, err := r.ensureFreshBundle(ctx, row, attempt)
	if err != nil {
		return 0, err
	}
	if row.IsAbandoned() {
		return time.Millisecond, nil
	}

	spec, err := launcher.Container(row, attempt, row.Status.CompletedPhases, r.Host)
	if err != nil {
		// The spec was admitted and cannot be built: nothing a retry fixes.
		return 0, r.failValidation(ctx, row, attempt, err)
	}

	if ready, wait, err := r.ensureImage(ctx, row); err != nil || !ready {
		return wait, err
	}

	dir := r.Host.AttemptDir(row.RunID, row.Epoch, attempt)
	if err := launcher.WriteFiles(dir, launcher.MaterialFiles(row, presigned), r.ChownFiles); err != nil {
		return 0, fmt.Errorf("write the materials of %s attempt %d: %w", row.RunID, attempt, err)
	}

	name := launcher.ContainerName(row.RunID, row.Epoch, attempt)
	id, err := r.Engine.ContainerCreate(ctx, name, spec)
	if errors.Is(err, docker.ErrConflict) {
		// The name is deterministic, so this is our own earlier create — or a
		// stranger's container squatting on the name, which container()
		// refuses on the next pass.
		existing, inspectErr := r.Engine.ContainerInspect(ctx, name)
		if inspectErr != nil {
			return 5 * time.Second, nil
		}
		labels := existing.Config.Labels
		if labels[launcher.LabelManagedBy] != launcher.ManagedBy ||
			labels[launcher.LabelEpoch] != spec.Labels[launcher.LabelEpoch] ||
			labels[launcher.LabelRunID] != spec.Labels[launcher.LabelRunID] {
			r.log().Error("a container with this attempt's name exists and is not ours", "name", name)
			return 30 * time.Second, nil
		}
		id = existing.ID
	} else if err != nil {
		return 0, fmt.Errorf("create container %s: %w", name, err)
	}

	written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch {
			return false
		}
		cur.Status.Attempt = attempt
		cur.Status.ContainerName = name
		cur.Status.ContainerID = id
		cur.Status.PullError = ""
		cur.Status.Retry.NextAttemptAt = nil
		return true
	})
	if err != nil {
		return 0, err
	}
	*row = written

	if err := r.Engine.ContainerStart(ctx, id); err != nil {
		// Not returned as an error: the container now carries the runtime's
		// refusal in its state, and the next pass classifies it.
		r.log().Error("starting a container", "runID", row.RunID, "container", name, "error", err)
		return time.Second, nil
	}
	r.log().Info("attempt started", "runID", row.RunID, "epoch", row.Epoch, "attempt", attempt, "container", name)
	return time.Second, nil
}

// ensureImage makes the spec's image present, as its pull policy says. It
// reports false while the image is not there yet; the startup deadline in the
// phase table is what turns a pull that never succeeds into a failure.
func (r *Reconciler) ensureImage(ctx context.Context, row *state.Run) (bool, time.Duration, error) {
	ref := row.Spec.Image
	_, err := r.Engine.ImageInspect(ctx, ref)
	present := err == nil
	if err != nil && !errors.Is(err, docker.ErrNotFound) {
		return false, 0, fmt.Errorf("inspect image %s: %w", ref, err)
	}

	policy := row.Spec.ImagePullPolicy
	switch {
	case policy == "Never" && !present:
		return false, 15 * time.Second, r.recordPullError(ctx, row,
			"the image is not on this host and imagePullPolicy is Never")
	case policy == "Always", !present:
		budget := r.startupDeadline()
		if row.Status.StartupBeganAt != nil {
			budget = row.Status.StartupBeganAt.Add(r.startupDeadline()).Sub(r.now())
		}
		if budget < 30*time.Second {
			budget = 30 * time.Second
		}
		pullCtx, cancel := context.WithTimeout(ctx, budget)
		err := r.Engine.ImagePull(pullCtx, ref, r.Auths.For(ref))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false, 0, ctx.Err()
			}
			if present {
				// Always, with a copy on hand and a registry that is down:
				// starting the copy beats failing the run, and the spec pins
				// images by digest so the copy is the image the spec named.
				r.log().Warn("pulling an image failed; using the copy on this host", "image", ref, "error", err)
				break
			}
			r.log().Warn("pulling an image failed", "runID", row.RunID, "image", ref, "error", err)
			return false, 15 * time.Second, r.recordPullError(ctx, row, err.Error())
		}
	}
	if err := r.Store.TouchImage(ctx, ref); err != nil {
		r.log().Warn("recording an image for the collector", "image", ref, "error", err)
	}
	return true, 0, nil
}

func (r *Reconciler) recordPullError(ctx context.Context, row *state.Run, msg string) error {
	written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch || cur.Status.PullError == msg {
			return false
		}
		cur.Status.PullError = truncate(msg, 512)
		return true
	})
	if err != nil {
		return err
	}
	*row = written
	return nil
}

// ensureFreshBundle reissues the presigned bundle when it would expire before
// the attempt could finish, and returns the encoded bundle for the secret
// files. Object-store mode only; relay mode has nothing to sign.
func (r *Reconciler) ensureFreshBundle(ctx context.Context, row *state.Run, attempt int32) ([]byte, error) {
	if row.ArtifactMode() != runv1.ArtifactModeObjectStore {
		return nil, nil
	}
	bundle := row.Materials.Artifacts
	need := r.now().Add(time.Duration(row.Spec.Runtime.TimeoutSeconds) * time.Second).Add(r.bundleSlack())
	if r.Bundles != nil && !bundle.ExpiresAt.After(need) {
		fresh, err := r.Bundles.Artifacts(ctx, row.RunID, clusterv1.ArtifactBundleRequest{Epoch: row.Epoch, Attempt: attempt})
		switch {
		case err == nil:
			written, mErr := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
				if cur.Epoch != row.Epoch {
					return false
				}
				if fresh.Mode == "" {
					fresh.Mode = runv1.ArtifactModeObjectStore
				}
				cur.Materials.Artifacts = *fresh
				return true
			})
			if mErr != nil {
				return nil, mErr
			}
			*row = written
			bundle = row.Materials.Artifacts
		case clusterapi.ActionOf(err) == clusterv1.ActionAbandon:
			r.log().Info("the control plane says this run is no longer ours", "runID", row.RunID)
			written, mErr := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
				if cur.IsAbandoned() {
					return false
				}
				cur.AbandonedAt = ptr(r.now())
				return true
			})
			if mErr != nil {
				return nil, mErr
			}
			*row = written
			return nil, nil
		default:
			// Starting with links that expire early still beats not starting:
			// the entrypoint persists the result before the git phases.
			r.log().Warn("could not reissue the presigned bundle; starting with the one on hand",
				"runID", row.RunID, "expiresAt", bundle.ExpiresAt, "error", err)
		}
	}
	return json.Marshal(bundle)
}

func (r *Reconciler) bundleSlack() time.Duration {
	if r.BundleSlack > 0 {
		return r.BundleSlack
	}
	return 600 * time.Second
}

// failValidation records a spec that was admitted and cannot be run. Terminal,
// class config.
func (r *Reconciler) failValidation(ctx context.Context, row *state.Run, attempt int32, cause error) error {
	now := r.now()
	written, err := r.Store.Mutate(ctx, row.RunID, func(cur *state.Run) bool {
		if cur.Epoch != row.Epoch {
			return false
		}
		cur.Status.Attempt = attempt
		cur.Status.Phase = runv1.PhaseFailed
		cur.Status.Reason = ReasonInvalidSpec
		cur.Status.Message = truncate(cause.Error(), 1024)
		cur.Status.FailureClass = runv1.FailureConfig
		cur.Status.FinishedAt = &now
		return true
	})
	if err != nil {
		return err
	}
	*row = written
	r.notify(row)
	return nil
}

// awaitAck holds a materialised run until the control plane confirms the
// lease, and gives up if it never does: by then the backend has reassigned the
// work, and starting it anyway is how one agent runs twice on one branch.
func (r *Reconciler) awaitAck(ctx context.Context, row *state.Run) (time.Duration, error) {
	budget := r.AckWaitBudget
	if budget <= 0 {
		budget = DefaultAckWaitBudget
	}
	if r.now().After(row.LeasedAt.Add(budget)) {
		r.log().Warn("no acknowledgement for this lease; discarding it",
			"runID", row.RunID, "epoch", row.Epoch, "since", row.LeasedAt)
		if r.Notifier != nil {
			r.Notifier.Forget(row.RunID)
		}
		return 0, r.removeRun(ctx, row)
	}
	return 5 * time.Second, nil
}

// abandon carries out the one command that ends in silence: kill, remove
// everything the run had on this host, forget it, report nothing.
func (r *Reconciler) abandon(ctx context.Context, row *state.Run) error {
	if r.Notifier != nil {
		r.Notifier.Forget(row.RunID)
	}
	if r.Artifacts != nil {
		r.Artifacts.ForgetArtifacts(row.RunID)
	}
	r.log().Info("abandoning a run reassigned by the control plane", "runID", row.RunID, "epoch", row.Epoch)
	return r.removeRun(ctx, row)
}

// removeRun deletes every container and volume the run has on this host,
// whatever attempt or epoch made them, then its files, then its row. The row
// goes last: a controller that crashes half way comes back to a row that says
// what is left to remove.
func (r *Reconciler) removeRun(ctx context.Context, row *state.Run) error {
	sel := map[string]string{
		launcher.LabelManagedBy: launcher.ManagedBy,
		launcher.LabelRunID:     lower(string(row.RunID)),
	}
	ctrs, err := r.Engine.ContainerList(ctx, sel)
	if err != nil {
		return fmt.Errorf("list the containers of %s: %w", row.RunID, err)
	}
	for _, c := range ctrs {
		if err := r.Engine.ContainerRemove(ctx, c.ID, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("remove container %s: %w", c.ID, err)
		}
	}
	vols, err := r.Engine.VolumeList(ctx, sel)
	if err != nil {
		return fmt.Errorf("list the volumes of %s: %w", row.RunID, err)
	}
	for _, v := range vols {
		if err := r.Engine.VolumeRemove(ctx, v.Name, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("remove volume %s: %w", v.Name, err)
		}
	}
	if r.Host.AttemptDir != nil {
		if err := launcher.RemoveFiles(runDirOf(r.Host, row.RunID)); err != nil {
			return err
		}
	}
	return r.Store.Delete(ctx, row.RunID)
}

// runDirOf is the directory holding every attempt of a run: two levels above
// an attempt directory (…/runs/<run>/<epoch>/a<n>).
func runDirOf(h launcher.Host, runID runv1.ULID) string {
	attemptDir := h.AttemptDir(runID, 0, 1)
	return parent(parent(attemptDir))
}

// reap deletes a finished run's row once the outcome is delivered and the TTL
// has passed, or once the hard TTL has passed whatever was delivered.
func (r *Reconciler) reap(ctx context.Context, row *state.Run) (time.Duration, error) {
	finished := r.now()
	if row.Status.FinishedAt != nil {
		finished = *row.Status.FinishedAt
	}
	hardTTL := r.HardTTL
	if hardTTL <= 0 {
		hardTTL = DefaultHardTTL
	}
	if r.now().After(finished.Add(hardTTL)) {
		if !ReportSettled(row) {
			r.log().Error("deleting a run whose outcome was never accepted",
				"runID", row.RunID, "phase", row.Status.Phase, "attempt", row.Status.Attempt)
		}
		if r.Artifacts != nil {
			r.Artifacts.ForgetArtifacts(row.RunID)
		}
		return 0, r.removeRun(ctx, row)
	}
	if !ReportSettled(row) {
		return time.Minute, nil
	}
	ttl := DefaultTTL
	if row.Spec.TTLSecondsAfterFinished != nil {
		ttl = time.Duration(*row.Spec.TTLSecondsAfterFinished) * time.Second
	}
	if wait := finished.Add(ttl).Sub(r.now()); wait > 0 {
		return wait, nil
	}
	if r.Artifacts != nil {
		r.Artifacts.ForgetArtifacts(row.RunID)
	}
	return 0, r.removeRun(ctx, row)
}

// ReportSettled reports whether the backend has heard everything this run owes
// it. It is the gate on the TTL.
func ReportSettled(row *state.Run) bool {
	st := row.Status
	if !st.Phase.IsTerminal() {
		return false
	}
	if st.Reported.Attempt != max(st.Attempt, 1) || st.Reported.Phase != st.Phase {
		return false
	}
	// Artifacts before the report: in relay mode this host holds the only copy
	// until the backend takes it.
	if st.ArtifactsRelayed != nil && !*st.ArtifactsRelayed {
		return false
	}
	if st.ResultReported {
		return st.Reported.CompletionDelivered
	}
	return true
}
