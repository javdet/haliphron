// Package server holds the host-level glue: the answer to "how much room is
// there and is it safe to take work" that the lease loop and the heartbeat
// both ask, and the health and metrics listeners.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/capacity"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Gate is the network check's verdict.
type Gate interface{ Healthy() error }

// Host answers the lease loop and the heartbeat.
type Host struct {
	Capacity      *capacity.Checker
	Network       Gate
	Runtimes      []runv1.AgentType
	DockerVersion string
	Log           *slog.Logger
}

// FreeSlots is zero while the host must not take work: the network check
// failing, the disk under its floor, or every slot in use.
func (h *Host) FreeSlots(ctx context.Context) (int32, error) {
	if h.Network != nil && h.Network.Healthy() != nil {
		return 0, nil
	}
	snap, err := h.Capacity.Take(ctx)
	if err != nil {
		return 0, err
	}
	if snap.DiskCriticalLow() && h.Log != nil {
		h.Log.Warn("free disk is under the critical floor; running runs continue, no new work is taken",
			"free", snap.DiskFree, "critical", snap.DiskCritical)
	}
	return snap.FreeSlots(), nil
}

// Facts implements report.Host. QuotaExhausted is what stops the backend
// assigning work here, as an exhausted ResourceQuota does on Kubernetes.
func (h *Host) Facts(ctx context.Context) clusterv1.ClusterFacts {
	f := clusterv1.ClusterFacts{
		Runtimes:      h.Runtimes,
		CRDVersions:   []string{agentrunv1alpha1.GroupVersion.Version},
		DockerVersion: h.DockerVersion,
	}
	if h.Network != nil && h.Network.Healthy() != nil {
		f.QuotaExhausted = true
		return f
	}
	if snap, err := h.Capacity.Take(ctx); err == nil && snap.DiskLow() {
		f.QuotaExhausted = true
	}
	return f
}

// Health serves /healthz and /readyz. Ready means the network check passes:
// a load balancer has nothing to do with this process, but an operator's
// monitoring does.
type Health struct {
	Addr    string
	Network Gate
}

// Start serves until the context ends.
func (h *Health) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.Network != nil {
			if err := h.Network.Healthy(); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	return serve(ctx, h.Addr, mux)
}

// Metrics serves a handful of gauges in the Prometheus text format, written
// by hand: four numbers do not justify a client library.
type Metrics struct {
	Addr     string
	Store    *state.Store
	Host     *Host
	Network  Gate
	Capacity *capacity.Checker
}

// Start serves until the context ends.
func (m *Metrics) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", m.handle)
	return serve(ctx, m.Addr, mux)
}

func (m *Metrics) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder
	rows, err := m.Store.List(ctx)
	if err == nil {
		counts := map[string]int{}
		for _, row := range rows {
			phase := string(row.Status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			counts[phase]++
		}
		phases := make([]string, 0, len(counts))
		for p := range counts {
			phases = append(phases, p)
		}
		sort.Strings(phases)
		b.WriteString("# HELP haliphron_dockerhost_runs Runs held on this host, by phase.\n# TYPE haliphron_dockerhost_runs gauge\n")
		for _, p := range phases {
			fmt.Fprintf(&b, "haliphron_dockerhost_runs{phase=%q} %d\n", p, counts[p])
		}
	}
	ok := 1
	if m.Network != nil && m.Network.Healthy() != nil {
		ok = 0
	}
	fmt.Fprintf(&b, "# HELP haliphron_dockerhost_netcheck_ok 1 when the agent network check passes.\n"+
		"# TYPE haliphron_dockerhost_netcheck_ok gauge\nhaliphron_dockerhost_netcheck_ok %d\n", ok)
	if m.Capacity != nil {
		if snap, err := m.Capacity.Take(ctx); err == nil {
			fmt.Fprintf(&b, "# TYPE haliphron_dockerhost_disk_free_bytes gauge\nhaliphron_dockerhost_disk_free_bytes %d\n", snap.DiskFree)
			fmt.Fprintf(&b, "# TYPE haliphron_dockerhost_free_slots gauge\nhaliphron_dockerhost_free_slots %d\n", snap.FreeSlots())
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	if addr == "" || addr == "0" {
		<-ctx.Done()
		return nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case <-ctx.Done():
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
