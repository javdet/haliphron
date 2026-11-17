package dockerhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"

	"github.com/automagicops/haliphron/dockerhost/state"
)

// Inputs on the Docker host: the same contract as on Kubernetes. Every input
// is fetched into the spool and verified before the ack; the row holds the
// manifest; the container reads its own run's inputs, and only those, from
// the callback.

var (
	hostScreenshot = fakebackend.InputFile{Name: "error dialog.png", Body: []byte("\x89PNG\r\n\x1a\nnot really")}
	hostLog        = fakebackend.InputFile{Name: "deploy.log", Body: []byte("exit status 137\n")}
)

// failing makes the backend answer 503 to requests whose path ends with a
// suffix, switchable while the controller runs.
type failing struct{ suffix atomic.Value }

func (f *failing) set(s string) { f.suffix.Store(s) }

func (f *failing) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if s, _ := f.suffix.Load().(string); s != "" && strings.HasSuffix(r.URL.Path, s) {
			rw.Header().Set("Content-Type", "application/problem+json")
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = rw.Write([]byte(`{"title":"injected","status":503,"code":"Unavailable","action":"backoff"}`))
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func TestAHostAcknowledgesOnlyOnceEveryInputIsSpooled(t *testing.T) {
	w := newWorld(t)
	f := &failing{}
	w.mutate = f.wrap
	id := w.Backend.Enqueue(spec(), fakebackend.WithInputs(hostScreenshot, hostLog))
	manifest := w.Backend.Inputs(id)
	f.set("/inputs/" + manifest[1].File())
	h := start(t, w)

	eventually(t, "the first input spooled", func() bool { return h.Spool.HasInput(id, manifest[0]) })
	time.Sleep(1500 * time.Millisecond)
	if h.state(id).Acked {
		t.Fatal("the lease was acknowledged with an input missing")
	}
	if _, err := h.Store.Get(t.Context(), id); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("a row was written before every input arrived: %v", err)
	}

	f.set("")
	w.Backend.Reassign(id)
	h.running(id, 2, 1)
	if !h.state(id).Acked {
		t.Fatal("a container runs for a lease the backend never saw acknowledged")
	}
	for _, ref := range manifest {
		if ok, _ := h.Spool.InputMatches(id, ref); !ok {
			t.Fatal("an input is not spooled and verified")
		}
	}
	if n := w.Backend.InputFetches(id, manifest[0].Key); n != 1 {
		t.Errorf("an input already held was fetched %d times", n)
	}
}

func TestAHostRefusesALeaseWhoseInputIsGoneOrDoesNotMatch(t *testing.T) {
	for name, spoil := range map[string]func(*fakebackend.Backend, runv1.ULID, runv1.InputRef){
		"gone": func(b *fakebackend.Backend, id runv1.ULID, ref runv1.InputRef) { b.DropInput(id, ref.Key) },
		"altered": func(b *fakebackend.Backend, id runv1.ULID, ref runv1.InputRef) {
			b.CorruptInput(id, ref.Key, []byte("exit status 138\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			id := w.Backend.Enqueue(spec(), fakebackend.WithInputs(hostScreenshot, hostLog))
			manifest := w.Backend.Inputs(id)
			spoil(w.Backend, id, manifest[1])
			h := start(t, w)

			eventually(t, "the lease refused", func() bool { return slices.Contains(h.state(id).Excluded, h.ClusterID) })
			if msg := h.state(id).Message; !strings.Contains(msg, "MaterializationFailed") ||
				strings.Contains(msg, "deploy") {
				t.Fatalf("refusal message %q", msg)
			}
			if ctrs, _ := w.Docker.Count(); ctrs != 0 {
				t.Fatal("a refused lease started a container")
			}
			if _, err := h.Store.Get(t.Context(), id); err == nil {
				t.Fatal("a refused lease left a row")
			}
			if h.Spool.HasInput(id, manifest[0]) {
				t.Fatal("a refused lease's inputs were kept")
			}
		})
	}
}

func TestTheHostsContainerReadsItsOwnInputsAndNothingElse(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	id := w.Backend.Enqueue(spec(), fakebackend.WithInputs(hostScreenshot, hostLog))
	other := w.Backend.Enqueue(spec(), fakebackend.WithInputs(fakebackend.InputFile{Name: "theirs.txt", Body: []byte("no\n")}))
	h.running(id, 1, 1)
	h.running(other, 1, 1)
	mine, theirs := w.Backend.Inputs(id), w.Backend.Inputs(other)

	// The secret files carry the manifest; the container's spec does not.
	raw, err := os.ReadFile(filepath.Join(w.attemptDir(id, 1, 1), "secrets", runv1.SecretKeyInputs))
	if err != nil {
		t.Fatalf("no %s among the secret files: %v", runv1.SecretKeyInputs, err)
	}
	var manifest runv1.InputManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Inputs) != 2 || manifest.Inputs[0] != mine[0] {
		t.Fatalf("the manifest file: %+v %v", manifest, err)
	}
	c, _ := w.Docker.Container(h.running(id, 1, 1))
	for _, kv := range c.spec.Env {
		if strings.Contains(kv, "error dialog") || strings.Contains(kv, mine[0].Key) {
			t.Fatal("an input's name is in the container's environment")
		}
	}

	code, body, header := h.getInput(id, mine[1].Key, h.token(id, 1, 1))
	if code != http.StatusOK || !bytes.Equal(body, hostLog.Body) || header.Get(runv1.HeaderSHA256) != mine[1].SHA256 ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the run's own input: %d %q %v", code, body, header)
	}
	for _, tc := range []struct {
		name  string
		key   string
		token string
		want  int
	}{
		{"another run's key", theirs[0].Key, h.token(id, 1, 1), http.StatusNotFound},
		{"another run's token", mine[0].Key, h.token(other, 1, 1), http.StatusUnauthorized},
		{"a key outside the layout", "inputs/../completion.json", h.token(id, 1, 1), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := h.getInput(id, tc.key, tc.token); code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, code)
			}
		})
	}
}

func TestAHostRestartedBetweenFetchAndAckRepeatsTheAckWithoutFetchingAgain(t *testing.T) {
	w := newWorld(t)
	f := &failing{}
	f.set("/ack")
	w.mutate = f.wrap
	h := start(t, w)
	id := w.Backend.Enqueue(spec(), fakebackend.WithInputs(hostScreenshot, hostLog))
	manifest := w.Backend.Inputs(id)
	eventually(t, "the row written", func() bool {
		_, err := h.Store.Get(t.Context(), id)
		return err == nil
	})
	h.stop()
	f.set("")

	h2 := start(t, w)
	h2.running(id, 1, 1)
	if !h2.state(id).Acked {
		t.Fatal("the acknowledgement was not repeated")
	}
	for _, ref := range manifest {
		if n := w.Backend.InputFetches(id, ref.Key); n != 1 {
			t.Errorf("an input still in the spool was fetched %d times", n)
		}
	}
}

func TestARunsInputsGoWhenItsRowIsRemoved(t *testing.T) {
	w := newWorld(t)
	h := start(t, w)
	sp := spec()
	zero := int32(0)
	sp.TTLSecondsAfterFinished = &zero
	id := w.Backend.Enqueue(sp, fakebackend.WithInputs(hostScreenshot))
	ref := w.Backend.Inputs(id)[0]
	name := h.running(id, 1, 1)
	if !h.Spool.HasInput(id, ref) {
		t.Fatal("the input is not spooled")
	}
	h.postCompletion(id, 1, 1, runv1.CompletionSuccess, 0)
	w.Docker.Exit(name, 0, false)
	eventually(t, "the row reaped", func() bool {
		_, err := h.Store.Get(t.Context(), id)
		return errors.Is(err, state.ErrNotFound)
	})
	if h.Spool.HasInput(id, ref) {
		t.Fatal("the input of a removed run is still in the spool")
	}
}

func (h *harness) getInput(id runv1.ULID, key, token string) (int, []byte, http.Header) {
	h.t.Helper()
	q := url.Values{runv1.QueryRunID: {string(id)}, runv1.QueryKey: {key}}
	req, _ := http.NewRequest(http.MethodGet, h.Callback.URL+runv1.CallbackPathInputs+"?"+q.Encode(), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header
}
