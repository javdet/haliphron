// Command fake-controlplane is FakeControlPlane as a process: object storage
// with presigned capabilities and the controller's completion webhook, plus an
// admin listener for staging a run and reading back what happened to it.
//
// It exists so that the agent image can be verified with a `docker run` and
// nothing else — no cluster, no backend, no MinIO. Prepare a run, get a
// directory of secret files and an env file, mount them, run the image, and
// look at what landed in the bucket.
//
//	fake-controlplane -prepare /tmp/run -prompt 'say hello'
//	docker run --rm --read-only \
//	  --env-file /tmp/run/run.env \
//	  -v /tmp/run/secrets:/haliphron/secrets:ro \
//	  -v /tmp/run/role:/haliphron/role:ro \
//	  --tmpfs /workspace --tmpfs /haliphron/run --tmpfs /home/agent --tmpfs /tmp \
//	  --network host --user 1000:1000 \
//	  haliphron/agent:dev
//
// `--network host` is what makes the pod's links resolve, and it does not route
// on Docker Desktop. There, bind the listeners as usual and advertise the name
// the container can reach the host by:
//
//	fake-controlplane -advertise http://host.docker.internal:9000 -prepare /tmp/run
//
// The advertised base URL is signed into nothing — the signature covers the
// method, the key and the expiry — so it can be changed without invalidating a
// bundle already minted.
//
// The staged directory outlives the process and cannot be deleted with `rm -r`
// alone: the secrets directory is 0500, like the mount it stands in for. Either
// `chmod u+w` it first or let a test use Layout.Remove.
//
// The admin listener is not part of any contract and nothing may depend on it.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
)

func main() {
	var (
		addr      = flag.String("addr", ":9000", "storage and runtime API listen address")
		adminAddr = flag.String("admin-addr", ":9001", "admin listen address")
		advertise = flag.String("advertise", "", "base URL the pod should use; defaults to http://127.0.0.1<addr>")
		bucket    = flag.String("bucket", controlplane.DefaultBucket, "bucket named in presigned bundles")
		ttl       = flag.Duration("ttl", time.Hour, "how long a minted bundle stays valid")

		prepare = flag.String("prepare", "", "stage a run into this directory, then keep serving its links")
		prompt  = flag.String("prompt", "say hello", "the prompt for the staged run")
		agent   = flag.String("agent", string(runv1.AgentClaudeCode), "claude-code or codex")
		repo    = flag.String("repo", "", "repository URL, empty for a run without one")
		role    = flag.String("role", "", "role name")
		schema  = flag.String("output-schema", "", "file holding the node's output schema")
	)
	flag.Parse()

	cp := controlplane.New(controlplane.WithBucket(*bucket), controlplane.WithSignatureTTL(*ttl))
	base := *advertise
	if base == "" {
		base = "http://127.0.0.1" + *addr
	}
	cp.SetBaseURL(base)

	api := &http.Server{Addr: *addr, Handler: cp.Handler(), ReadHeaderTimeout: 10 * time.Second}
	admin := &http.Server{Addr: *adminAddr, Handler: adminRoutes(cp), ReadHeaderTimeout: 10 * time.Second}

	go serve(api, "control-plane")
	go serve(admin, "admin")

	if *prepare != "" {
		// Staged and then served: the image is about to be pointed at these
		// links, so the process has to stay up.
		staged := stage(cp, *prepare, *prompt, *agent, *repo, *role, *schema)
		log.Printf("staged run %s into %s", staged.RunID, *prepare)
	}

	log.Printf("fake control plane: storage and runtime API on %s (advertised as %s), admin on %s",
		*addr, base, *adminAddr)

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

func stage(cp *controlplane.ControlPlane, dir, prompt, agent, repo, role, schemaPath string) *controlplane.Prepared {
	req := controlplane.RunRequest{
		Prompt: prompt,
		Agent:  runv1.AgentType(agent),
		Role:   role,
	}
	if repo != "" {
		req.RepoURL = repo
		req.GitProvider = runv1.GitProviderGitHub
		req.BaseBranch = "main"
		req.TargetBranch = "haliphron/local-dev"
	}
	if schemaPath != "" {
		schema, err := os.ReadFile(schemaPath)
		if err != nil {
			log.Fatalf("reading %s: %v", schemaPath, err)
		}
		req.RoleConfig = map[string]string{runv1.RoleConfigKeyOutputSchema: string(schema)}
	}

	p := cp.Prepare(req)
	if _, err := p.Materialize(dir); err != nil {
		log.Fatalf("staging into %s: %v", dir, err)
	}
	return p
}

func adminRoutes(cp *controlplane.ControlPlane) http.Handler {
	mux := http.NewServeMux()

	// POST /admin/runs stages a run and answers with everything needed to start
	// a pod: the environment, the secret material and the bundle.
	mux.HandleFunc("POST /admin/runs", func(w http.ResponseWriter, r *http.Request) {
		var req controlplane.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, cp.Prepare(req))
	})

	mux.HandleFunc("GET /admin/runs/{runID}", func(w http.ResponseWriter, r *http.Request) {
		id := runv1.ULID(r.PathValue("runID"))
		writeJSON(w, map[string]any{
			"keys":    cp.RunKeys(id),
			"reports": cp.Reports(id),
		})
	})

	mux.HandleFunc("GET /admin/runs/{runID}/objects/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := "runs/" + r.PathValue("runID") + "/" + r.PathValue("key")
		body, ok := cp.Object(key)
		if !ok {
			http.Error(w, "no such key: "+key, http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	})

	// The injected faults, so that the failure paths can be watched by hand
	// rather than only asserted in a test.
	mux.HandleFunc("POST /admin/faults/storage", func(w http.ResponseWriter, r *http.Request) {
		var spec struct {
			Method   string `json:"method"`
			KeyMatch string `json:"keyMatch"`
			Status   int    `json:"status"`
			Count    int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cp.FailStorage(spec.Method, spec.KeyMatch, spec.Status, spec.Count)
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("POST /admin/faults/callback/{status}/{count}", func(w http.ResponseWriter, r *http.Request) {
		cp.FailCallback(atoi(r.PathValue("status")), atoi(r.PathValue("count")))
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("POST /admin/faults/clear", func(w http.ResponseWriter, _ *http.Request) {
		cp.ClearStorageFaults()
		cp.ClearCallbackFaults()
		w.WriteHeader(http.StatusAccepted)
	})

	// POST /admin/clock/{duration} expires every signature minted so far, which
	// is how "the bundle outlived its own run" is reproduced without waiting.
	mux.HandleFunc("POST /admin/clock/{duration}", func(w http.ResponseWriter, r *http.Request) {
		d, err := time.ParseDuration(r.PathValue("duration"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cp.Advance(d)
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /admin/log", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Join(cp.Log(), "\n") + "\n"))
	})

	return mux
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
