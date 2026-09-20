// Command haliphron-controller runs inside a customer's cluster. It is the only
// component of the system that does, and the only one that ever holds a git
// token in memory.
//
// It does four things, each as a runnable of its own: it leases work from the
// control plane, it reconciles that work into Jobs and watches what becomes of
// them, it receives the pods' completion reports, and it tells the control
// plane everything it saw. Nothing here connects inward from outside: the
// backend has no route into this process, which is the property the whole
// design exists to preserve.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"

	"github.com/automagicops/haliphron/controller/agentrun"
	"github.com/automagicops/haliphron/controller/callback"
	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	"github.com/automagicops/haliphron/controller/identity"
	"github.com/automagicops/haliphron/controller/launcher"
	"github.com/automagicops/haliphron/controller/lease"
	"github.com/automagicops/haliphron/controller/materialize"
	"github.com/automagicops/haliphron/controller/report"
	"github.com/automagicops/haliphron/controller/spool"
	"github.com/automagicops/haliphron/controller/version"
)

func main() {
	if err := run(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("the controller stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	// controller-runtime speaks logr; giving it this handler keeps one log
	// format for the whole process instead of two that have to be correlated.
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	log.Info("haliphron controller starting",
		"version", version.Version, "cluster", cfg.ClusterName,
		"agentNamespace", cfg.AgentNamespace, "backend", cfg.BackendURL)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		agentrunv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("build the scheme: %w", err)
		}
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("read the kubernetes configuration: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		// The cache is scoped to two namespaces: the agents' and the
		// controller's own. A cluster-wide cache would make the controller's
		// memory a function of the cluster's size and would need permissions
		// nobody should grant it.
		Cache: cache.Options{
			DefaultNamespaces: namespaces(cfg),
		},
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:  cfg.HealthAddr,
		LeaderElection:          true,
		LeaderElectionID:        "haliphron-controller." + agentrunv1alpha1.GroupName,
		LeaderElectionNamespace: cfg.OwnNamespace,
	})
	if err != nil {
		return fmt.Errorf("build the manager: %w", err)
	}

	// The identity is established before the manager starts, so a direct
	// client is needed: the manager's own client reads through a cache that is
	// not running yet.
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build a direct client: %w", err)
	}

	signer := &identity.Deferred{}
	api, err := clusterapi.New(clusterapi.Options{
		BaseURL:           cfg.BackendURL,
		ControllerVersion: version.Version,
		Signer:            signer,
		Logger:            log,
	})
	if err != nil {
		return err
	}

	store := identity.NewSecretStore(direct, cfg.OwnNamespace, cfg.IdentitySecret)
	id, registration, err := identity.Bootstrap(ctx, store, api, cfg.BootstrapToken,
		clusterv1.RegisterRequest{
			Name:              cfg.ClusterName,
			Labels:            cfg.ClusterLabels,
			ControllerVersion: version.Version,
			AgentNamespace:    cfg.AgentNamespace,
			Runtimes:          cfg.Runtimes,
			CapacitySlots:     cfg.CapacitySlots,
			CRDVersions:       []string{agentrunv1alpha1.GroupVersion.Version},
		}, log)
	if err != nil {
		return err
	}
	signer.Set(id)

	timings := config.NewTimings()
	if registration != nil {
		timings.Set(&registration.Timings)
	}

	builder := launcher.Builder{
		Namespace:            cfg.AgentNamespace,
		ClusterID:            id.ClusterID(),
		GraceSeconds:         cfg.GraceSeconds,
		DeadlineSlackSeconds: cfg.DeadlineSlack,
		ServiceAccountName:   cfg.ServiceAccount,
	}

	// The spool holds what a pod handed this controller until the backend has
	// taken it. It is opened whatever the mode: the mode is a per-run fact that
	// arrives in a lease, so a controller that only opened it on demand would
	// have to do so in the middle of a pod's upload.
	//
	// Opening it also reclaims what a previous process left behind. That is the
	// whole reason it is a directory and not a map: the acknowledgement this
	// controller gives a pod is a promise the bytes outlive the pod, and a
	// restart that cleared the spool would break it for anything not yet
	// forwarded.
	artifactSpool, err := spool.Open(spool.Config{Root: cfg.SpoolPath})
	if err != nil {
		return err
	}
	log.Info("the artifact spool is open", "path", artifactSpool.Root())

	reporter, err := report.New(report.Config{
		API:       api,
		K8s:       mgr.GetClient(),
		Timings:   timings,
		Namespace: cfg.AgentNamespace,
		ClusterID: id.ClusterID(),
		Capacity:  cfg.CapacitySlots,
		Version:   version.Version,
		Spool:     artifactSpool,
		Log:       log,
	})
	if err != nil {
		return err
	}

	reconciler, err := agentrun.New(agentrun.Config{
		Client:          mgr.GetClient(),
		Scheme:          scheme,
		Recorder:        mgr.GetEventRecorderFor("haliphron-controller"),
		ClusterID:       id.ClusterID(),
		Builder:         builder,
		Notifier:        reporter,
		Bundles:         api,
		StartupDeadline: cfg.StartupDeadline,
		Log:             log,
	})
	if err != nil {
		return err
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("wire the AgentRun controller: %w", err)
	}

	materializer := &materialize.Materializer{
		Client:         mgr.GetClient(),
		Namespace:      cfg.AgentNamespace,
		ClusterID:      id.ClusterID(),
		CallbackURL:    cfg.CallbackURL,
		RunURLTemplate: cfg.RunURLTemplate,
		Builder:        builder,
		PreflightJob:   cfg.PreflightJob,
		Log:            log,
	}

	leaseLoop, err := lease.New(lease.Loop{
		API:          api,
		K8s:          mgr.GetClient(),
		Materializer: materializer,
		Timings:      timings,
		Namespace:    cfg.AgentNamespace,
		ClusterID:    id.ClusterID(),
		Capacity:     cfg.CapacitySlots,
		Runtimes:     cfg.Runtimes,
		Log:          log,
	})
	if err != nil {
		return err
	}

	callbackServer, err := callback.New(callback.Config{
		K8s:            mgr.GetClient(),
		Namespace:      cfg.AgentNamespace,
		Sink:           reporter,
		Addr:           cfg.CallbackAddr,
		CallbackURL:    cfg.CallbackURL,
		MaxBytesPerRun: cfg.MaxArtifactBytesPerRun,
		Spent:          reporter.Spent,
		Log:            log,
	})
	if err != nil {
		return err
	}

	for _, runnable := range []manager.Runnable{
		leaseLoop, reporter, reporter.Heartbeat(), callbackServer,
	} {
		if err := mgr.Add(runnable); err != nil {
			return fmt.Errorf("add a runnable: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add the health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add the readiness check: %w", err)
	}

	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run the manager: %w", err)
	}
	return nil
}

// namespaces is what the cache watches. The agents' namespace holds the runs;
// the controller's own holds the identity Secret and the lease used for leader
// election.
func namespaces(cfg config.Config) map[string]cache.Config {
	out := map[string]cache.Config{cfg.AgentNamespace: {}}
	if cfg.OwnNamespace != "" && cfg.OwnNamespace != cfg.AgentNamespace {
		out[cfg.OwnNamespace] = cache.Config{}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
