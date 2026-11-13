// Package dockerhost drives the real Docker host controller against
// FakeBackend and an in-memory Docker daemon.
//
// FakeBackend is the only way to exercise the failures the protocol is built
// around — an epoch rise, a cancel, an abandon, an outage — because a backend
// that agrees with us proves nothing. The fake daemon is the only way to
// exercise what a real daemon does on its own and at a moment of its choosing:
// a process that exits, an OOM kill, a restart that kills everything, a
// container someone removed. The test plays the agent itself, posting to the
// callback with the token it reads from the secret files, exactly as the
// entrypoint does.
package dockerhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"

	"github.com/automagicops/haliphron/controller/clusterapi"
	ctrlconfig "github.com/automagicops/haliphron/controller/config"
	ctrlidentity "github.com/automagicops/haliphron/controller/identity"
	"github.com/automagicops/haliphron/controller/spool"

	"github.com/automagicops/haliphron/dockerhost/callback"
	"github.com/automagicops/haliphron/dockerhost/identity"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/lease"
	"github.com/automagicops/haliphron/dockerhost/materialize"
	"github.com/automagicops/haliphron/dockerhost/rawspec"
	"github.com/automagicops/haliphron/dockerhost/reconcile"
	"github.com/automagicops/haliphron/dockerhost/report"
	"github.com/automagicops/haliphron/dockerhost/state"
)

const (
	controllerVersion = "0.1.0"
	agentImage        = "javdet/haliphron-agent:test"
	network           = "haliphron-agents"
)

// world is what survives a controller restart: the control plane, the daemon
// and the host's directories.
type world struct {
	Backend    *fakebackend.Backend
	BackendURL string
	Docker     *fakeDocker
	Dir        string
	mutate     func(http.Handler) http.Handler
}

func newWorld(t *testing.T, opts ...fakebackend.Option) *world {
	t.Helper()
	return newWorldWithTimings(t, func(*clusterv1.Timings) {}, opts...)
}

func newWorldWithTimings(t *testing.T, adjust func(*clusterv1.Timings), opts ...fakebackend.Option) *world {
	t.Helper()
	timing := clusterv1.DefaultTimings()
	timing.MaxWaitSeconds = 1
	timing.HeartbeatIntervalSeconds = 1
	adjust(&timing)
	backend := fakebackend.New(append([]fakebackend.Option{fakebackend.WithTimings(timing)}, opts...)...)
	w := &world{Backend: backend, Docker: newFakeDocker(), Dir: t.TempDir()}
	w.Docker.AddImage(agentImage)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var h http.Handler = backend.Handler()
		if w.mutate != nil {
			h = w.mutate(h)
		}
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)
	w.BackendURL = srv.URL
	return w
}

func (w *world) attemptDir(runID runv1.ULID, epoch int64, attempt int32) string {
	return filepath.Join(w.Dir, "run", "runs", strings.ToLower(string(runID)),
		fmt.Sprint(epoch), fmt.Sprintf("a%d", attempt))
}

// harness is one controller process.
type harness struct {
	t      *testing.T
	w      *world
	cancel context.CancelFunc
	wg     sync.WaitGroup

	Store     *state.Store
	Reporter  *report.Reporter
	ClusterID runv1.ULID
	Callback  *httptest.Server
	Seen      *recordingNotifier
}

// recordingNotifier passes observations to the reporter and keeps a copy, so a
// test can see what an earlier attempt was reported as after a later attempt
// has replaced it in the row.
type recordingNotifier struct {
	next reconcile.Notifier
	mu   sync.Mutex
	obs  []clusterv1.RunObservation
}

func (n *recordingNotifier) Observed(o clusterv1.RunObservation) {
	n.mu.Lock()
	n.obs = append(n.obs, o)
	n.mu.Unlock()
	n.next.Observed(o)
}

func (n *recordingNotifier) Forget(id runv1.ULID) { n.next.Forget(id) }

// Reported reports whether an observation with this attempt and reason went
// out.
func (n *recordingNotifier) Reported(id runv1.ULID, attempt int32, reason string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, o := range n.obs {
		if o.RunID == id && o.Attempt == attempt && o.Reason == reason {
			return true
		}
	}
	return false
}

type fixedHost struct{ slots int32 }

func (h fixedHost) FreeSlots(context.Context) (int32, error) { return h.slots, nil }
func (h fixedHost) Facts(context.Context) clusterv1.ClusterFacts {
	return clusterv1.ClusterFacts{DockerVersion: "27.3.1", CRDVersions: []string{"v1alpha1"}}
}

func start(t *testing.T, w *world) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, w: w, cancel: cancel}
	t.Cleanup(h.stop)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	store, err := state.Open(filepath.Join(w.Dir, "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	h.Store = store

	tap := &rawspec.Tap{Next: clusterapi.DefaultTransport()}
	signer := &ctrlidentity.Deferred{}
	api, err := clusterapi.New(clusterapi.Options{BaseURL: w.BackendURL, ControllerVersion: controllerVersion,
		Signer: signer, HTTPClient: &http.Client{Transport: tap}, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	id, reg, err := ctrlidentity.Bootstrap(ctx, identity.FileStore{Path: filepath.Join(w.Dir, "state", "identity.json")},
		api, w.Backend.BootstrapToken(), clusterv1.RegisterRequest{
			Name: "docker-box", ControllerVersion: controllerVersion, AgentNamespace: network,
			CapacitySlots: 4, CRDVersions: []string{"v1alpha1"},
			RuntimeKind: clusterv1.RuntimeDocker, DockerVersion: "27.3.1",
		}, log)
	if err != nil {
		t.Fatal(err)
	}
	signer.Set(id)
	h.ClusterID = id.ClusterID()
	timings := ctrlconfig.NewTimings()
	if reg != nil {
		timings.Set(&reg.Timings)
	} else {
		tm := clusterv1.DefaultTimings()
		tm.MaxWaitSeconds, tm.HeartbeatIntervalSeconds = 1, 1
		timings.Set(&tm)
	}

	sp, err := spool.Open(spool.Config{Root: filepath.Join(w.Dir, "spool")})
	if err != nil {
		t.Fatal(err)
	}
	reporter := &report.Reporter{API: api, Store: store, Timings: timings, Spool: sp, Host: fixedHost{slots: 4},
		ClusterID: h.ClusterID, Capacity: 4, Version: controllerVersion, Log: log}
	h.Reporter = reporter

	cb := &callback.Server{Store: store, Sink: reporter, Spent: reporter.Spent, Log: log}
	h.Callback = httptest.NewServer(cb.Handler())

	h.Seen = &recordingNotifier{next: reporter}
	reconciler := &reconcile.Reconciler{
		Store: store, Engine: w.Docker, Notifier: h.Seen, Artifacts: reporter, Bundles: api,
		Host: launcher.Host{
			ClusterID: h.ClusterID, CallbackURL: h.Callback.URL, Network: network, DNS: []string{"10.0.0.53"},
			GraceSeconds: 1, DeadlineSlackSeconds: 600, AttemptDir: w.attemptDir,
		},
		StartupDeadline: 30 * time.Second,
		RetryBase:       200 * time.Millisecond,
		Log:             log,
	}
	driver := &reconcile.Driver{Reconciler: reconciler, Engine: w.Docker, Store: store, Resync: 300 * time.Millisecond, Log: log}
	reporter.Kicker = driver
	loop := &lease.Loop{
		API: api, Store: store, Timings: timings, Kicker: driver, Slots: fixedHost{slots: 4},
		Materializer: &materialize.Materializer{Store: store, RawSpecs: tap, CallbackURL: h.Callback.URL, Log: log},
		Network:      network, ClusterID: h.ClusterID, Capacity: 4, Log: log,
	}
	for _, run := range []func(context.Context) error{driver.Start, reporter.Start, reporter.Heartbeat, loop.Start} {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			_ = run(ctx)
		}()
	}
	return h
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.wg.Wait()
	h.cancel = nil
	h.Callback.Close()
	_ = h.Store.Close()
}

// eventually polls a condition, the way an operator watching a run would.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func spec() runv1.RenderedRunSpec {
	return runv1.RenderedRunSpec{
		Agent: "claude-code", Model: "claude-opus-5-5", Image: agentImage,
		Repo:    runv1.RepoSpec{URL: "https://git.example.com/a/b.git", Provider: "gitlab", BaseBranch: "main"},
		Runtime: runv1.RuntimeSpec{TimeoutSeconds: 600, Resources: runv1.Resources{CPU: "1", Memory: "2Gi"}},
	}
}

func (h *harness) state(id runv1.ULID) fakebackend.RunState {
	s, _ := h.w.Backend.RunState(id)
	return s
}

// running waits for the attempt's container to run and returns its name.
func (h *harness) running(id runv1.ULID, epoch int64, attempt int32) string {
	h.t.Helper()
	name := launcher.ContainerName(id, epoch, attempt)
	eventually(h.t, name+" running", func() bool {
		c, ok := h.w.Docker.Container(name)
		return ok && c.state.Status == "running"
	})
	return name
}

func (h *harness) token(id runv1.ULID, epoch int64, attempt int32) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.w.attemptDir(id, epoch, attempt), "secrets", runv1.SecretKeyCallbackToken))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func (h *harness) post(path, token string, body []byte, header map[string]string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.Callback.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) postPhase(id runv1.ULID, epoch int64, attempt int32, phase runv1.RuntimePhase) {
	h.t.Helper()
	raw, _ := json.Marshal(runv1.PhaseReport{RunID: id, Attempt: attempt, Phase: phase, Outcome: runv1.PhaseOutcomeOK})
	if code := h.post(runv1.CallbackPathPhase, h.token(id, epoch, attempt), raw, nil); code != http.StatusAccepted {
		h.t.Fatalf("phase report answered %d", code)
	}
}

func (h *harness) postCompletion(id runv1.ULID, epoch int64, attempt int32, status runv1.CompletionStatus, code int32) {
	h.t.Helper()
	raw, _ := json.Marshal(runv1.CompletionReport{RunID: id, Attempt: attempt, Status: status, ExitCode: code,
		Agent: "claude-code", CompletedPhases: []runv1.RuntimePhase{runv1.RuntimePhaseRun, runv1.RuntimePhasePersist},
		Repo: &runv1.RepoResult{PRURL: "https://git.example.com/a/b/-/merge_requests/1"}})
	if got := h.post(runv1.CallbackPathCompletion, h.token(id, epoch, attempt), raw, nil); got != http.StatusAccepted {
		h.t.Fatalf("completion answered %d", got)
	}
}

func (h *harness) postArtifact(id runv1.ULID, epoch int64, attempt int32, key string, body []byte) int {
	h.t.Helper()
	sum := sha256.Sum256(body)
	q := url.Values{runv1.QueryRunID: {string(id)}, runv1.QueryKey: {key}, runv1.QueryAttempt: {fmt.Sprint(attempt)}}
	return h.post(runv1.CallbackPathArtifacts+"?"+q.Encode(), h.token(id, epoch, attempt), body,
		map[string]string{runv1.HeaderSHA256: hex.EncodeToString(sum[:]), "Content-Type": "text/markdown"})
}
