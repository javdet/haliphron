package entrypoint

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Main is the process. It returns the exit code rather than calling os.Exit, so
// that the whole of it can be run in a test.
//
// Two things happen before the pipeline, and both of them are exit 30 with no
// report. That is not an oversight: a pod whose contract major is wrong, or
// whose secret volume is not mounted, has neither a callback token to
// authenticate with nor a presigned link to write to. It exits before any
// network call, which is exactly what the contract asks of it, and the evidence
// is on stderr where a pod that died in its first second leaves its only trace.
func Main() int {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return reportToStderr(err, runv1.ExitConfig)
	}
	secrets, err := LoadSecrets(runv1.MountSecrets, cfg)
	if err != nil {
		return reportToStderr(err, runv1.ExitConfig)
	}

	run, err := New(cfg, secrets)
	if err != nil {
		return reportToStderr(err, runv1.ExitConfig)
	}
	return int(RunWithSignals(context.Background(), run))
}

// RunWithSignals executes the pipeline under a signal handler.
//
// A SIGTERM from outside — cancellation, eviction, drain — arrives with a
// budget of HALIPHRON_GRACE_SECONDS, and within that budget the entrypoint
// stops the agent, persists the result, pushes if there is time, and notifies.
// The order is a priority by value: the durable result first, then the code,
// then the message about it.
//
// The push is capped at half the remaining budget, because a push interrupted
// by SIGKILL halfway leaves the branch in a state the next attempt would spend
// longer untangling than a clean repeat would have cost.
func RunWithSignals(parent context.Context, r *Run) int32 {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	done := make(chan int32, 1)
	go func() { done <- r.Execute(ctx) }()

	select {
	case code := <-done:
		return code

	case sig := <-signals:
		r.Cancel()
		r.logf("received %s; %s of grace to persist what exists", sig, r.cfg.Grace)

		// The agent's own context is cancelled, which sends it SIGTERM and
		// gives it the thirty seconds the run phase budgets. The pipeline then
		// carries on through persist, the git phases and notify on its own.
		cancel()

		select {
		case code := <-done:
			return code
		case <-time.After(r.cfg.Grace):
			// Out of budget. SIGKILL is imminent and the exit code will be the
			// platform's, not ours; saying so is the last useful thing left.
			r.logf("the grace period expired before the pipeline finished; "+
				"what reached storage is in %s", r.cfg.StoragePrefix)
			return exitSIGTERM
		}
	}
}

// reportToStderr prints a failure that happened before there was anywhere to
// report it and returns its exit code.
func reportToStderr(err error, fallback int32) int {
	f := classify(err, fallback)
	fmt.Fprintf(os.Stderr, "haliphron entrypoint: %s: %s\n", f.Reason, f.Message())
	return int(f.Code)
}
