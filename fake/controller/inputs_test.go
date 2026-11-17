package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
	"github.com/automagicops/haliphron/fake/controller"
)

// Inputs: the controller fetches every attached file before it acknowledges,
// and refuses a lease whose file is gone or does not match. Run against
// FakeBackend, this is where the two fakes' readings of the inputs endpoint
// are checked against each other.

var photo = backend.InputFile{Name: "photo of the rack.jpg", Body: []byte("\xff\xd8\xff\xe0 not a real jpeg")}

func TestTheFakeControllerFetchesEveryInputBeforeItAcknowledges(t *testing.T) {
	t.Parallel()
	b, c := setup(t)
	id := b.Enqueue(sampleSpec(), backend.WithInputs(photo))
	sync(t, c, 1)

	ref := b.Inputs(id)[0]
	if got, ok := c.Input(id, ref.Key); !ok || string(got) != string(photo.Body) {
		t.Fatal("the input was not fetched")
	}
	if state, _ := b.RunState(id); !state.Acked {
		t.Fatal("the lease was not acknowledged once its input was held")
	}

	var manifest runv1.InputManifest
	for _, s := range c.Objects().Secrets {
		if s.Name == agentrunv1alpha1.SecretName(id) {
			_ = json.Unmarshal(s.Data[runv1.SecretKeyInputs], &manifest)
		}
	}
	if len(manifest.Inputs) != 1 || manifest.Inputs[0] != ref {
		t.Fatalf("the Secret's input manifest: %+v", manifest)
	}
	cr, _ := c.AgentRun(id)
	raw, _ := json.Marshal(cr)
	if strings.Contains(string(raw), "rack") {
		t.Fatal("an input's name reached the AgentRun")
	}
}

func TestTheFakeControllerRefusesALeaseWhoseInputIsGoneOrAltered(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(*backend.Backend, runv1.ULID, string){
		"gone":    func(b *backend.Backend, id runv1.ULID, key string) { b.DropInput(id, key) },
		"altered": func(b *backend.Backend, id runv1.ULID, key string) { b.CorruptInput(id, key, []byte("other")) },
	} {
		t.Run(name, func(t *testing.T) {
			b, c := setup(t)
			id := b.Enqueue(sampleSpec(), backend.WithInputs(photo))
			spoil(b, id, b.Inputs(id)[0].Key)
			sync(t, c, 1)

			state, _ := b.RunState(id)
			if state.Acked || !slices.Contains(state.Excluded, c.ClusterID()) ||
				!strings.Contains(state.Message, string(clusterv1.RejectMaterializationFailed)) {
				t.Fatalf("want a MaterializationFailed refusal, got %+v", state)
			}
			if _, ok := c.AgentRun(id); ok {
				t.Fatal("a refused lease was materialised")
			}
		})
	}
}

func TestTheFakeControllerSendsNoAckWhenAnInputCannotBeFetchedYet(t *testing.T) {
	t.Parallel()
	b := backend.New()
	handler := b.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/inputs/") {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"title":"injected","status":503,"code":"Unavailable","action":"backoff"}`))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := controller.New(srv.URL, controller.WithClock(b.Now))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Register(context.Background(), b.BootstrapToken()); err != nil {
		t.Fatal(err)
	}

	id := b.Enqueue(sampleSpec(), backend.WithInputs(photo))
	sync(t, c, 1)

	state, _ := b.RunState(id)
	if state.Acked || len(state.Excluded) != 0 {
		t.Fatalf("an unreachable input produced an answer: %+v", state)
	}
	if _, ok := c.AgentRun(id); ok {
		t.Fatal("the lease was materialised without its input")
	}
	for _, sent := range c.SentFor(id) {
		if sent.Kind == controller.ReportAck {
			t.Fatal("an acknowledgement was sent")
		}
	}
}
