// Command haliphron-backend is the control plane: the REST API, the MCP
// server, the Cluster API and the expiry scanners, in one binary.
//
// They are one process because they are one set of use cases over one
// database. The listeners are separate ports rather than paths, because their
// authentication models and their reasons to be reachable differ: the Cluster
// API need not be exposed outward when the clusters sit inside the perimeter,
// and MCP may have to be exposed when REST does not.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/clusterapi"
	"github.com/automagicops/haliphron/backend/config"
	"github.com/automagicops/haliphron/backend/mcp"
	"github.com/automagicops/haliphron/backend/restapi"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/backend/version"
)

// openArtifacts builds whichever half of the ArtifactStore port this
// installation runs.
//
// The default needs no credentials and no external service, which is the point:
// `helm install` with nothing set produces a control plane that can run an
// agent end to end. Choosing object-store mode is a deliberate act, and the
// configuration refuses the halfway state — that mode without a bucket — at
// startup rather than as a run that works for an hour and cannot upload.
func openArtifacts(cfg config.Config, log *slog.Logger) (artifacts.Store, error) {
	if cfg.Artifacts.Mode == runv1.ArtifactModeObjectStore {
		store, err := artifacts.NewS3(cfg.Artifacts.S3)
		if err != nil {
			return nil, err
		}
		log.Info("artifacts: object-store mode",
			"bucket", store.Bucket(), "endpoint", store.Endpoint())
		return store, nil
	}

	store, err := artifacts.NewDisk(artifacts.DiskConfig{Root: cfg.Artifacts.VolumePath})
	if err != nil {
		return nil, err
	}
	log.Info("artifacts: relay mode; results are written to this volume",
		"path", store.Root())
	return store, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "haliphron-backend: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	log.Info("starting", "version", version.Version, "mode", cfg.Mode)

	// The signal context is taken before anything is opened, so a shutdown
	// during a slow migration is a shutdown rather than a kill.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	if len(cfg.KEK) > 0 {
		if err := db.SetKEK(cfg.KEKID, cfg.KEK); err != nil {
			return err
		}
	} else {
		// Said once, at startup, rather than discovered when the first managed
		// secret is written.
		log.Warn("no key encryption key is configured; managed secrets are unavailable")
	}

	if cfg.Migrate {
		// Every replica calls this. The advisory lock inside makes a rollout
		// wait for its schema instead of starting against half of one.
		start := time.Now()
		if err := db.Migrate(ctx); err != nil {
			return err
		}
		log.Info("schema applied", "took", time.Since(start))
	}

	objects, err := openArtifacts(cfg, log)
	if err != nil {
		return err
	}

	service := app.New(app.Options{
		Store:     db,
		Artifacts: objects,
		Git:       app.StoredGitToken{Store: db, Name: cfg.Limits.GitTokenSecret},
		Logger:    log,
		Timings:   cfg.Timings,
		Versions:  cfg.Versions,
		Defaults:  cfg.Defaults,
		Limits:    cfg.Limits,
	})

	var wg sync.WaitGroup
	serve := func(name, addr string, handler http.Handler) {
		server := &http.Server{
			Addr:    addr,
			Handler: handler,
			// The long poll runs for up to maxWaitSeconds, so a read timeout
			// shorter than it would cut every idle cluster's poll and look
			// like network instability from inside the cluster.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			log.Info("listening", "listener", name, "addr", addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("listener failed", "listener", name, "addr", addr, "error", err)
				stop()
			}
		}()
		go func() {
			defer wg.Done()
			<-ctx.Done()
			// A grace period longer than one long poll, so a rollout does not
			// hand every cluster a dropped connection on the way out.
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 40*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				log.Error("listener did not shut down cleanly", "listener", name, "error", err)
			}
		}()
	}

	if cfg.Mode.Serves(config.ModeAPI) {
		serve("public", cfg.PublicAddr, restapi.New(service, log).Handler())
	}
	if cfg.Mode.Serves(config.ModeMCP) {
		serve("mcp", cfg.MCPAddr, mcp.New(service, log).Handler())
	}
	if cfg.Mode.Serves(config.ModeCluster) {
		serve("cluster-api", cfg.ClusterAddr, clusterapi.New(service, log).Handler())
	}
	serve("health", cfg.MetricsAddr, healthHandler(db))

	// The scanners run wherever the Cluster API does: they are the other half
	// of the same deadlines. In a split deployment the api and mcp processes do
	// not sweep, so two replicas do not both expire the same lease — which
	// would be harmless, since both statements are idempotent, but doubles the
	// scan for nothing.
	if cfg.Mode.Serves(config.ModeCluster) {
		wg.Add(3)
		go func() { defer wg.Done(); service.RunSweeper(ctx, cfg.SweepInterval) }()
		go func() { defer wg.Done(); service.RunReaper(ctx, cfg.ReapInterval) }()
		// Retention for the artifact volume. A no-op in object-store mode,
		// where the chart generated lifecycle rules and the store applies them
		// itself.
		go func() {
			defer wg.Done()
			service.ReapArtifacts(ctx, cfg.ReapInterval, app.Retention{
				Logs:    cfg.Artifacts.RetainLogs,
				Results: cfg.Artifacts.RetainResults,
				Other:   cfg.Artifacts.RetainOther,
			})
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
	return nil
}

// healthHandler answers the two probes.
//
// They are deliberately different: liveness says the process is running, and
// readiness says it can serve, which here means the database answers. A
// liveness probe that checks the database restarts every replica during a
// failover, turning a database blip into an outage.
func healthHandler(db *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.DB().PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("database unreachable\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "%s\n", version.Version)
	})
	return mux
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	// JSON by default: these lines are read by a log pipeline far more often
	// than by a person, and a request id that is a field is greppable where one
	// inside a sentence is not.
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
