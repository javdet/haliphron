// Command haliphron-dockerhost runs agents on one Linux host with Docker. To
// the control plane it is one more cluster: it registers with a bootstrap
// token, leases work for itself over the Cluster API, and reports back. Nothing
// connects inward to it, the property the whole design exists to preserve.
//
// Subcommands:
//
//	serve              the controller (the default)
//	runs list          the runs this host holds
//	runs get <runID>   one run, without its secrets
//	netcheck           run the agent network check once and print every result
//	healthcheck        ask the running controller's health listener (for compose)
//	version            print the version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	ctrlconfig "github.com/automagicops/haliphron/controller/config"
	ctrlidentity "github.com/automagicops/haliphron/controller/identity"
	"github.com/automagicops/haliphron/controller/spool"

	"github.com/automagicops/haliphron/dockerhost/callback"
	"github.com/automagicops/haliphron/dockerhost/capacity"
	"github.com/automagicops/haliphron/dockerhost/config"
	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/gc"
	"github.com/automagicops/haliphron/dockerhost/identity"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/lease"
	"github.com/automagicops/haliphron/dockerhost/materialize"
	"github.com/automagicops/haliphron/dockerhost/netcheck"
	"github.com/automagicops/haliphron/dockerhost/rawspec"
	"github.com/automagicops/haliphron/dockerhost/reconcile"
	"github.com/automagicops/haliphron/dockerhost/report"
	"github.com/automagicops/haliphron/dockerhost/server"
	"github.com/automagicops/haliphron/dockerhost/state"
	"github.com/automagicops/haliphron/dockerhost/version"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "runs":
		err = runs(ctx, args)
	case "netcheck":
		err = netcheckOnce(ctx)
	case "version":
		fmt.Println(version.Version)
	case "healthcheck":
		err = healthcheck(ctx)
	default:
		err = fmt.Errorf("unknown command %q; use serve, runs list, runs get <id>, netcheck or version", cmd)
	}
	if err != nil {
		slog.Error("haliphron-dockerhost", "error", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// lock takes the single-instance lock. Two controllers on one host would each
// lease work and each start it: double execution, the one thing the epoch
// fence cannot undo once a branch has been pushed twice.
func lock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another haliphron-dockerhost holds %s; exactly one may run per host", path)
	}
	return f, nil
}

func dockerConfigPath() string {
	if p := os.Getenv("HALIPHRON_DOCKER_CONFIG"); p != "" {
		return p
	}
	return "/root/.docker/config.json"
}

func netcheckSettings(cfg config.Config) netcheck.Settings {
	dns := make([]string, 0, len(cfg.DNS))
	for _, d := range cfg.DNS {
		dns = append(dns, d.String())
	}
	return netcheck.Settings{
		Network:          cfg.Network,
		Subnet:           cfg.Subnet,
		Gateway:          cfg.Gateway,
		Bridge:           cfg.Bridge,
		DNS:              dns,
		CallbackPort:     portOf(cfg.CallbackAddr, 8083),
		HealthPort:       portOf(cfg.HealthAddr, 8084),
		MetricsPort:      portOf(cfg.MetricsAddr, 9091),
		PublicURL:        cfg.NetcheckPublic,
		Blocked:          cfg.NetcheckBlocked,
		ControlPlaneMCP:  cfg.ControlPlaneMCP,
		ControlPlaneHost: cfg.ControlPlaneHost,
		ControlPlanePort: cfg.ControlPlanePorts,
		Image:            cfg.NetcheckImage,
		MountDir:         filepath.Join(cfg.RunDir, "netcheck"),
	}
}

func portOf(addr string, fallback int) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return fallback
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return fallback
	}
	return n
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	if !cfg.InsecureNetwork && cfg.NetcheckImage == "" {
		return errors.New("config: HALIPHRON_NETCHECK_IMAGE must be set (normally the agent image); " +
			"the network check needs an image with curl to probe from")
	}

	lockFile, err := lock(cfg.LockPath())
	if err != nil {
		return err
	}
	defer func() { _ = lockFile.Close() }()

	engine, err := docker.NewClient(cfg.DockerHost)
	if err != nil {
		return err
	}
	ver, err := engine.Version(ctx)
	if err != nil {
		return fmt.Errorf("reach the docker daemon at %s: %w", cfg.DockerHost, err)
	}
	log.Info("haliphron dockerhost starting",
		"version", version.Version, "cluster", cfg.ClusterName, "network", cfg.Network,
		"backend", cfg.BackendURL, "docker", ver.Version, "insecureNetwork", cfg.InsecureNetwork)
	if cfg.InsecureNetwork {
		log.Warn("HALIPHRON_INSECURE_SKIP_NETCHECK is set: agent isolation on this host is NOT verified; " +
			"the cluster registers with the label " + config.LabelInsecureNetwork + "=true")
	}

	store, err := state.Open(cfg.StatePath())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	auths, err := docker.LoadAuths(dockerConfigPath())
	if err != nil {
		return err
	}

	tap := &rawspec.Tap{Next: clusterapi.DefaultTransport()}
	signer := &ctrlidentity.Deferred{}
	api, err := clusterapi.New(clusterapi.Options{
		BaseURL:           cfg.BackendURL,
		ControllerVersion: version.Version,
		Signer:            signer,
		HTTPClient:        &http.Client{Transport: tap},
		Logger:            log,
	})
	if err != nil {
		return err
	}

	id, registration, err := ctrlidentity.Bootstrap(ctx, identity.FileStore{Path: cfg.IdentityPath()}, api,
		cfg.BootstrapToken, clusterv1.RegisterRequest{
			Name:              cfg.ClusterName,
			Labels:            cfg.Labels(),
			ControllerVersion: version.Version,
			// "Where agents run": on this runtime, the agents network.
			AgentNamespace: cfg.Network,
			Runtimes:       cfg.Runtimes,
			CapacitySlots:  cfg.CapacitySlots,
			CRDVersions:    []string{agentrunv1alpha1.GroupVersion.Version},
			RuntimeKind:    clusterv1.RuntimeDocker,
			DockerVersion:  ver.Version,
		}, log)
	if err != nil {
		return err
	}
	signer.Set(id)
	timings := ctrlconfig.NewTimings()
	if registration != nil {
		timings.Set(&registration.Timings)
	}

	artifactSpool, err := spool.Open(spool.Config{Root: cfg.SpoolPath})
	if err != nil {
		return err
	}

	checker := &netcheck.Checker{
		Engine: engine, Settings: netcheckSettings(cfg), Auths: auths,
		Disabled: cfg.InsecureNetwork, Interval: cfg.NetcheckInterval, Log: log,
	}
	capChecker := &capacity.Checker{Engine: engine, Store: store, Config: capacity.Config{
		Slots: cfg.CapacitySlots, ReserveCPU: cfg.ReserveCPU, ReserveMemory: cfg.ReserveMemory,
		DefaultCPU: cfg.DefaultCPU, DefaultMem: cfg.DefaultMem, DefaultDisk: cfg.DefaultDisk,
		DiskPath: cfg.DiskPath, DiskFloor: cfg.DiskFloor, DiskCritical: cfg.DiskCritical,
	}}
	host := &server.Host{Capacity: capChecker, Network: checker, Runtimes: cfg.Runtimes,
		DockerVersion: ver.Version, Log: log}

	dns := make([]string, 0, len(cfg.DNS))
	for _, d := range cfg.DNS {
		dns = append(dns, d.String())
	}
	reporter := &report.Reporter{
		API: api, Store: store, Timings: timings, Spool: artifactSpool, Host: host,
		ClusterID: id.ClusterID(), Capacity: cfg.CapacitySlots, Version: version.Version, Log: log,
	}
	reconciler := &reconcile.Reconciler{
		Store: store, Engine: engine, Notifier: reporter, Artifacts: reporter, Bundles: api, Auths: auths,
		Host: launcher.Host{
			ClusterID: id.ClusterID(), CallbackURL: cfg.CallbackURL, Network: cfg.Network, DNS: dns,
			GraceSeconds: cfg.GraceSeconds, DeadlineSlackSeconds: cfg.DeadlineSlack,
			PidsLimit: cfg.PidsLimit, Nofile: cfg.Nofile,
			LogDriver: cfg.LogDriver, LogMaxSize: cfg.LogMaxSize, LogMaxFile: cfg.LogMaxFile,
			AttemptDir: cfg.AttemptDir,
		},
		StartupDeadline: cfg.StartupDeadline,
		ChownFiles:      os.Geteuid() == 0,
		Log:             log,
	}
	driver := &reconcile.Driver{
		Reconciler: reconciler, Engine: engine, Store: store, Log: log,
		OnDaemonReconnect: checker.Trigger,
	}
	reporter.Kicker = driver

	loop := &lease.Loop{
		API: api, Store: store, Timings: timings, Kicker: driver, Slots: host,
		Materializer: &materialize.Materializer{
			Store: store, Capacity: capChecker, Network: checker, RawSpecs: tap,
			Runtimes: cfg.Runtimes, CallbackURL: cfg.CallbackURL, Log: log,
		},
		Network: cfg.Network, ClusterID: id.ClusterID(), Capacity: cfg.CapacitySlots, Runtimes: cfg.Runtimes, Log: log,
	}
	cb := &callback.Server{
		Store: store, Sink: reporter, Addr: cfg.CallbackAddr, Base: callback.BaseOf(cfg.CallbackURL),
		MaxBytesPerRun: cfg.MaxArtifactBytesPerRun, Spent: reporter.Spent, Log: log,
	}
	collector := &gc.Collector{Engine: engine, Store: store, Keep: cfg.ImageKeep, Log: log}

	runnables := map[string]func(context.Context) error{
		"callback":  cb.Start,
		"netcheck":  checker.Start,
		"reconcile": driver.Start,
		"ingest":    reporter.Start,
		"heartbeat": reporter.Heartbeat,
		"lease":     loop.Start,
		"gc":        collector.Start,
		"health":    (&server.Health{Addr: cfg.HealthAddr, Network: checker}).Start,
		"metrics":   (&server.Metrics{Addr: cfg.MetricsAddr, Store: store, Host: host, Network: checker, Capacity: capChecker}).Start,
	}
	return runAll(ctx, log, runnables)
}

// runAll runs every component until the context ends or one of them fails;
// a failure stops the rest, and the process exits so the supervisor restarts
// it. Restarting is uneventful by design: everything is in the row.
func runAll(ctx context.Context, log *slog.Logger, runnables map[string]func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for name, run := range runnables {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := run(ctx)
			if err != nil && ctx.Err() == nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", name, err)
				}
				mu.Unlock()
				log.Error("a component stopped", "component", name, "error", err)
			}
			cancel()
		}()
	}
	wg.Wait()
	return firstErr
}

func runs(ctx context.Context, args []string) error {
	cfg, err := config.Load(func(k string) string {
		// The runs subcommand reads the store and nothing else; it must work
		// with only the state directory configured.
		v := os.Getenv(k)
		if v == "" && (k == config.EnvBackendURL || k == config.EnvClusterName) {
			return "-"
		}
		if v == "" && k == config.EnvDNS {
			return "127.0.0.1"
		}
		return v
	})
	if err != nil {
		return err
	}
	store, err := state.OpenReadOnly(cfg.StatePath())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if len(args) == 0 || args[0] == "list" {
		rows, err := store.List(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RUN ID\tEPOCH\tATTEMPT\tPHASE\tREASON\tCONTAINER\tACKED\tREPORTED\tAGE")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%t\t%s\t%s\n",
				r.RunID, r.Epoch, r.CurrentAttempt(), orDash(string(r.Status.Phase)), orDash(r.Status.Reason),
				orDash(r.Status.ContainerName), r.IsAcknowledged(), orDash(string(r.Status.Reported.Phase)),
				time.Since(r.CreatedAt).Round(time.Second))
		}
		return w.Flush()
	}
	if args[0] == "get" && len(args) == 2 {
		r, err := store.Get(ctx, runv1.ULID(args[1]))
		if err != nil {
			return err
		}
		// Never the materials and never the callback token: this command is
		// for reading a run's state, and its output lands in terminals and
		// tickets.
		out := map[string]any{
			"runID": r.RunID, "epoch": r.Epoch, "spec": r.Spec, "status": r.Status,
			"leasedAt": r.LeasedAt, "ackedAt": r.AckedAt, "cancelRequestedAt": r.CancelRequestedAt,
			"cancelReason": r.CancelReason, "abandonedAt": r.AbandonedAt, "createdAt": r.CreatedAt,
			"terminalAt": r.TerminalAt, "artifactMode": r.ArtifactMode(), "specRawSHA256": r.SpecRawSHA256,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	return errors.New("usage: runs list | runs get <runID>")
}

func netcheckOnce(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	engine, err := docker.NewClient(cfg.DockerHost)
	if err != nil {
		return err
	}
	auths, err := docker.LoadAuths(dockerConfigPath())
	if err != nil {
		return err
	}
	c := &netcheck.Checker{Engine: engine, Settings: netcheckSettings(cfg), Auths: auths}
	n, err := engine.NetworkInspect(ctx, cfg.Network)
	if err != nil {
		return fmt.Errorf("inspect network %s: %w", cfg.Network, err)
	}
	if err := netcheck.CheckNetwork(n, c.Settings); err != nil {
		fmt.Println("FAIL network configuration:", err)
	} else {
		fmt.Println("ok   network configuration")
	}
	results, err := c.Probe(ctx)
	if err != nil {
		return err
	}
	for _, r := range results {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
		}
		target := r.URL
		if target == "" {
			target = r.Shell
		}
		fmt.Printf("%s %-28s expect=%-7s exit=%-3d %s %s\n", mark, r.Name, r.Expect, r.Exit, target, r.Why)
	}
	return netcheck.Failures(results)
}

// healthcheck is compose's probe: the image has no shell and no curl.
func healthcheck(ctx context.Context) error {
	addr := os.Getenv(config.EnvHealthAddr)
	if addr == "" {
		addr = "127.0.0.1:8084"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
