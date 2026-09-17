// Command fake-backend serves the Cluster API in memory, for developing a
// controller without a control plane.
//
// It is the same implementation the contract tests run against, with one
// addition: an admin listener on a separate port for putting work into the
// queue and reading back what happened to it. That is not part of any contract
// and no component may depend on it — it exists because a controller polling an
// empty queue forever is not a development environment.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
)

func main() {
	var (
		addr      = flag.String("addr", ":8082", "Cluster API listen address")
		adminAddr = flag.String("admin-addr", ":8083", "admin listen address")
		bucket    = flag.String("bucket", "haliphron", "bucket named in presigned bundles")
		endpoint  = flag.String("storage-endpoint", "http://minio:9000", "object storage endpoint")
	)
	flag.Parse()

	b := backend.New(backend.WithStorage(*bucket, *endpoint))

	api := &http.Server{
		Addr:    *addr,
		Handler: b.Handler(),
		// Longer than the long poll's ceiling, or the ingress-shaped failure
		// this fake exists to avoid gets reproduced by the fake itself.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	admin := &http.Server{Addr: *adminAddr, Handler: adminRoutes(b), ReadHeaderTimeout: 10 * time.Second}

	go serve(api, "cluster-api")
	go serve(admin, "admin")

	log.Printf("fake backend: cluster-api on %s, admin on %s, bootstrap token %s",
		*addr, *adminAddr, b.BootstrapToken())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	_ = api.Close()
	_ = admin.Close()
}

func serve(s *http.Server, name string) {
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("%s: %v", name, err)
	}
}

func adminRoutes(b *backend.Backend) http.Handler {
	mux := http.NewServeMux()

	// POST /admin/runs with a RenderedRunSpec body queues work, the way
	// admission would after the public API accepted a request.
	mux.HandleFunc("POST /admin/runs", func(w http.ResponseWriter, r *http.Request) {
		var spec runv1.RenderedRunSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := b.Enqueue(spec)
		writeJSON(w, map[string]string{"runID": string(id)})
	})

	mux.HandleFunc("GET /admin/runs/{runID}", func(w http.ResponseWriter, r *http.Request) {
		state, ok := b.RunState(runv1.ULID(r.PathValue("runID")))
		if !ok {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		writeJSON(w, state)
	})

	mux.HandleFunc("POST /admin/runs/{runID}/cancel", func(w http.ResponseWriter, r *http.Request) {
		b.Cancel(runv1.ULID(r.PathValue("runID")))
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /admin/clusters", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.Clusters())
	})

	// The injected faults, so an operator can watch a controller survive a
	// control plane that went away — the property ADR 6 claims and nobody
	// checks by hand.
	mux.HandleFunc("POST /admin/faults/unavailable/{on}", func(w http.ResponseWriter, r *http.Request) {
		b.SetUnavailable(r.PathValue("on") == "true")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /admin/faults/drop-long-polls/{on}", func(w http.ResponseWriter, r *http.Request) {
		b.SetDropLongPolls(r.PathValue("on") == "true")
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
