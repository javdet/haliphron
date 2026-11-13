package dockerhost

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"

	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/state"
)

func TestTheHostRegistersAsADockerRuntimeWithItsNetworkAsTheAgentNamespace(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	clusters := w.Backend.Clusters()
	if len(clusters) != 1 {
		t.Fatalf("%d clusters", len(clusters))
	}
	c := clusters[0]
	if c.ID != h.ClusterID || c.RuntimeKind != clusterv1.RuntimeDocker || c.AgentNamespace != network ||
		c.DockerVersion != "27.3.1" {
		t.Fatalf("%+v", c)
	}
}

func TestARunIsLeasedAcknowledgedStartedAndReportedEndToEnd(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec(), fakebackend.WithPrompt("fix the flaky test"),
		fakebackend.WithRoleConfig(map[string]string{runv1.RoleConfigKeySystemPrompt: "be terse"}))

	name := h.running(id, 1, 1)
	if !h.state(id).Acked {
		t.Fatal("a container is running for a lease the backend never saw acknowledged")
	}
	c, _ := w.Docker.Container(name)
	if !slices.Contains(c.spec.Env, runv1.EnvPrompt+"=fix the flaky test") {
		t.Fatal("the prompt is not in the environment")
	}
	for _, kv := range c.spec.Env {
		if strings.Contains(kv, "ghs_fake_token_value") {
			t.Fatal("the git token leaked into the environment")
		}
	}
	dir := w.attemptDir(id, 1, 1)
	token, err := os.ReadFile(filepath.Join(dir, "secrets", runv1.SecretKeyGitToken))
	if err != nil || string(token) != "ghs_fake_token_value" {
		t.Fatalf("git token file: %q %v", token, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "role", runv1.RoleConfigKeySystemPrompt)); err != nil {
		t.Fatal("the role file was not written")
	}

	eventually(t, "Running reported", func() bool { return h.state(id).Phase == runv1.PhaseRunning })

	if code := h.postArtifact(id, 1, 1, runv1.StorageKeyResult, []byte("# done\n")); code != http.StatusOK {
		t.Fatalf("artifact answered %d", code)
	}
	h.postPhase(id, 1, 1, runv1.RuntimePhaseRun)
	h.postCompletion(id, 1, 1, runv1.CompletionSuccess, 0)
	w.Docker.Exit(name, 0, false)

	eventually(t, "Succeeded with the completion", func() bool {
		s := h.state(id)
		return s.TerminalPhase == runv1.PhaseSucceeded && s.Completion != nil
	})
	if body, ok := w.Backend.Artifact(id, runv1.StorageKeyResult); !ok || string(body) != "# done\n" {
		t.Fatalf("the relayed artifact did not reach the backend: %q", body)
	}
	if !w.Backend.ArtifactPrecededCompletion(id) {
		t.Fatal("the completion arrived ahead of the artifact it names")
	}
	eventually(t, "the attempt's container, volumes and secrets removed", func() bool {
		ctrs, vols := w.Docker.Count()
		_, err := os.Stat(dir)
		return ctrs == 0 && vols == 0 && os.IsNotExist(err)
	})
	eventually(t, "the outcome recorded as delivered", func() bool {
		row, err := h.Store.Get(t.Context(), id)
		return err == nil && reconcileSettled(row)
	})
}

func reconcileSettled(row state.Run) bool {
	st := row.Status
	return st.Reported.Phase == runv1.PhaseSucceeded && st.Reported.CompletionDelivered &&
		st.ArtifactsRelayed != nil && *st.ArtifactsRelayed && st.PRURL != ""
}

func TestNothingStartsBeforeTheAcknowledgement(t *testing.T) {
	w := newWorld(t)
	var blockAck = true
	w.mutate = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if blockAck && strings.HasSuffix(r.URL.Path, "/ack") {
				rw.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(rw, r)
		})
	}
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	eventually(t, "the row written", func() bool {
		_, err := h.Store.Get(t.Context(), id)
		return err == nil
	})
	time.Sleep(2 * time.Second)
	if ctrs, _ := w.Docker.Count(); ctrs != 0 {
		t.Fatal("a container was created for an unacknowledged lease")
	}
}

func TestAnInfraFailureIsANewAttemptThatCarriesTheCheckpoint(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	name := h.running(id, 1, 1)
	h.postPhase(id, 1, 1, runv1.RuntimePhaseRun)
	w.Docker.Exit(name, int(runv1.ExitStorage), false)

	second := h.running(id, 1, 2)
	c, _ := w.Docker.Container(second)
	if !slices.Contains(c.spec.Env, runv1.EnvCompletedPhases+"=run") {
		t.Fatalf("attempt 2 does not carry the checkpoint: %v", c.spec.Env)
	}
	if s := h.state(id); s.Epoch != 1 {
		t.Fatalf("a local retry raised the epoch to %d", s.Epoch)
	}
	if _, ok := w.Docker.Container(name); ok {
		t.Fatal("the failed attempt's container was left behind")
	}
	eventually(t, "attempt 2 reported", func() bool { return h.state(id).Attempt == 2 })
}

func TestAnAgentFailureIsNotRetried(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	name := h.running(id, 1, 1)
	w.Docker.Exit(name, int(runv1.ExitAgentError), false)
	eventually(t, "Failed agent reported", func() bool {
		s := h.state(id)
		return s.TerminalPhase == runv1.PhaseFailed && s.FailureClass == runv1.FailureAgent
	})
	time.Sleep(time.Second)
	if _, ok := w.Docker.Container(launcher.ContainerName(id, 1, 2)); ok {
		t.Fatal("an agent failure was replayed")
	}
}

func TestAnOOMKillIsReportedAndRetried(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	name := h.running(id, 1, 1)
	w.Docker.Exit(name, 137, true)
	h.running(id, 1, 2)
	row, _ := h.Store.Get(t.Context(), id)
	if row.Status.Retry.InfraRetries != 1 {
		t.Fatalf("retries %d", row.Status.Retry.InfraRetries)
	}
	if !h.Seen.Reported(id, 1, "OOMKilled") {
		t.Fatal("attempt 1 was not reported as OOMKilled")
	}
}

func TestADaemonRestartIsAnInfraFailureThatIsRetried(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	h.running(id, 1, 1)
	w.Docker.RestartDaemon()
	h.running(id, 1, 2)
	if !h.Seen.Reported(id, 1, "HostRestarted") {
		t.Fatal("attempt 1 was not reported as HostRestarted")
	}
}

func TestARemovedContainerIsContainerDisappearedAndRetried(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	name := h.running(id, 1, 1)
	eventually(t, "Running recorded", func() bool {
		row, err := h.Store.Get(t.Context(), id)
		return err == nil && row.Status.Phase == runv1.PhaseRunning
	})
	w.Docker.Remove(name)
	h.running(id, 1, 2)
	eventually(t, "the vanished attempt's secret files removed", func() bool {
		_, err := os.Stat(w.attemptDir(id, 1, 1))
		return os.IsNotExist(err)
	})
}

func TestACancelStopsTheContainerAndReportsCancelled(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	h.running(id, 1, 1)
	w.Backend.Cancel(id)
	eventually(t, "Cancelled reported", func() bool { return h.state(id).TerminalPhase == runv1.PhaseCancelled })
	row, err := h.Store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status.Stop == nil || row.Status.Stop.Reason != state.StopCancel {
		t.Fatalf("the stop was not recorded before it was sent: %+v", row.Status.Stop)
	}
	time.Sleep(500 * time.Millisecond)
	if _, ok := w.Docker.Container(launcher.ContainerName(id, 1, 2)); ok {
		t.Fatal("a cancelled run was retried")
	}
}

// The lease of the new epoch arrives while the old epoch is still being
// removed, and is left unacknowledged — the same rule as a Terminating
// AgentRun on Kubernetes. The ack deadline then reissues it at a higher epoch,
// which is the self-healing path; the test shortens that deadline.
func TestAReassignedRunIsRemovedAndTheNewEpochStartsClean(t *testing.T) {
	w := newWorldWithTimings(t, func(tm *clusterv1.Timings) { tm.AckTimeoutSeconds = 2 })
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	old := h.running(id, 1, 1)
	w.Backend.Reassign(id)
	eventually(t, "epoch 1 removed", func() bool {
		_, ok := w.Docker.Container(old)
		return !ok
	})
	var row state.Run
	eventually(t, "a later epoch running", func() bool {
		r, err := h.Store.Get(t.Context(), id)
		if err != nil || r.Epoch < 2 {
			return false
		}
		row = r
		c, ok := w.Docker.Container(launcher.ContainerName(id, r.Epoch, 1))
		return ok && c.state.Status == "running"
	})
	if row.Status.Attempt > 1 {
		t.Fatalf("the new epoch started at attempt %d; attempts reset with the epoch", row.Status.Attempt)
	}
	if s := h.state(id); !s.Acked || s.Epoch != row.Epoch {
		t.Fatalf("backend epoch %d acked %v, host epoch %d", s.Epoch, s.Acked, row.Epoch)
	}
}

func TestAControllerRestartReattachesToARunningContainer(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	name := h.running(id, 1, 1)
	eventually(t, "Running reported", func() bool { return h.state(id).Phase == runv1.PhaseRunning })
	h.stop()

	h2 := start(t, w)
	if h2.ClusterID != h.ClusterID {
		t.Fatal("the restart registered a new cluster")
	}
	h2.postCompletion(id, 1, 1, runv1.CompletionSuccess, 0)
	w.Docker.Exit(name, 0, false)
	eventually(t, "Succeeded after the restart", func() bool {
		s := h2.state(id)
		return s.TerminalPhase == runv1.PhaseSucceeded && s.Completion != nil
	})
	if _, ok := w.Docker.Container(launcher.ContainerName(id, 1, 2)); ok {
		t.Fatal("the restart started a second attempt")
	}
}

func TestALeaseWithASpecFieldThisBinaryDoesNotKnowIsRefused(t *testing.T) {
	w := newWorld(t)
	w.mutate = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/leases") {
				next.ServeHTTP(rw, r)
				return
			}
			rec := &recorder{header: http.Header{}}
			next.ServeHTTP(rec, r)
			body := rec.body.Bytes()
			if rec.status == http.StatusOK {
				var doc map[string]any
				if json.Unmarshal(body, &doc) == nil {
					for _, l := range doc["leases"].([]any) {
						l.(map[string]any)["spec"].(map[string]any)["futureSetting"] = "strict"
					}
					body, _ = json.Marshal(doc)
				}
			}
			for k, v := range rec.header {
				rw.Header()[k] = v
			}
			rw.Header().Del("Content-Length")
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			rw.WriteHeader(rec.status)
			_, _ = io.Copy(rw, bytes.NewReader(body))
		})
	}
	h := start(t, w)
	id := w.Backend.Enqueue(spec())
	eventually(t, "the lease refused", func() bool { return slices.Contains(h.state(id).Excluded, h.ClusterID) })
	if ctrs, _ := w.Docker.Count(); ctrs != 0 {
		t.Fatal("a refused lease started a container")
	}
	if _, err := h.Store.Get(t.Context(), id); err == nil {
		t.Fatal("a refused lease left a row")
	}
}

type recorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.status = code }

func TestACallbackWithAnotherRunsTokenIsRefused(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	a := w.Backend.Enqueue(spec())
	b := w.Backend.Enqueue(spec())
	h.running(a, 1, 1)
	h.running(b, 1, 1)
	raw, _ := json.Marshal(runv1.PhaseReport{RunID: a, Attempt: 1, Phase: runv1.RuntimePhaseRun, Outcome: runv1.PhaseOutcomeOK})
	if code := h.post(runv1.CallbackPathPhase, h.token(b, 1, 1), raw, nil); code != http.StatusUnauthorized {
		t.Fatalf("run b's token marked run a's phase: %d", code)
	}
	if code := h.postArtifact(a, 1, 1, "../escaped.md", []byte("x")); code != http.StatusBadRequest {
		t.Fatalf("a key escaping the run's prefix answered %d", code)
	}
}
