package backend_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
)

// A minimal controller, only as much as the fake's own tests need: a key pair,
// a token minted per request, and one call per endpoint.
//
// It mints tokens for real. Testing the fake against a client that fakes its
// authentication too would leave the one thing both sides must agree on — how a
// cluster proves who it is — checked by nobody.

type client struct {
	t       *testing.T
	baseURL string
	http    *http.Client

	kid     string
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	cluster runv1.ULID

	version string
	// ttl and audience are overridable so a test can present the tokens a
	// backend must refuse.
	ttl      time.Duration
	audience string
	// now is the backend's clock rather than wall time. A test that skips an
	// hour to expire a lease would otherwise also expire every token it mints
	// next, and the resulting 401 looks like an authentication bug.
	now func() time.Time
}

func newClient(t *testing.T, baseURL, kid string) *client {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &client{
		t: t, baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second},
		kid: kid, pub: pub, priv: priv,
		version: "1.2.0", ttl: 120 * time.Second, audience: clusterv1.TokenAudience,
		now: time.Now,
	}
}

// tokenNonce feeds the jti. Package level and atomic because the concurrency
// tests mint from several goroutines through one client, and a counter on the
// client would also stop the struct being copyable — which one test needs in
// order to forge a subject.
var tokenNonce atomic.Int64

func (c *client) token() string {
	now := c.now()
	header := map[string]string{"alg": clusterv1.SigningAlgorithm, "typ": "JWT", "kid": c.kid}
	claims := map[string]any{
		"iss": string(c.cluster), "sub": string(c.cluster), "aud": c.audience,
		"iat": now.Unix(), "exp": now.Add(c.ttl).Unix(),
		"jti": "jti-" + strconv.FormatInt(tokenNonce.Add(1), 10),
	}
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sig := ed25519.Sign(c.priv, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type response struct {
	status  int
	body    []byte
	headers http.Header
}

func (r response) problem(t *testing.T) clusterv1.Problem {
	t.Helper()
	var p clusterv1.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("decode problem from %q: %v", r.body, err)
	}
	return p
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (c *client) post(path string, auth string, body any) response {
	c.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("encode: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+clusterv1.BasePath+path, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.version != "" {
		req.Header.Set(clusterv1.HeaderControllerVersion, c.version)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: out, headers: resp.Header}
}

func (c *client) register(token, name string) response {
	return c.post("/register", token, clusterv1.RegisterRequest{
		Name: name,
		PublicKey: clusterv1.PublicKey{
			KID: c.kid, Alg: clusterv1.KeyAlgorithm,
			Key: base64.RawURLEncoding.EncodeToString(c.pub),
		},
		ControllerVersion: c.version,
		AgentNamespace:    "haliphron-agents",
		CapacitySlots:     10,
	})
}

// mustRegister registers and adopts the returned identity, which every later
// token must name as its subject.
func (c *client) mustRegister(token, name string) clusterv1.RegisterResponse {
	c.t.Helper()
	resp := c.register(token, name)
	if resp.status != http.StatusOK {
		c.t.Fatalf("register: status %d body %s", resp.status, resp.body)
	}
	var out clusterv1.RegisterResponse
	resp.decode(c.t, &out)
	c.cluster = out.ClusterID
	return out
}

func (c *client) lease(req clusterv1.LeaseRequest) response {
	return c.post("/clusters/"+string(c.cluster)+"/leases", c.token(), req)
}

// poll asks for work with a short wait, the usual shape in these tests.
func (c *client) poll(slots int32) ([]clusterv1.Lease, response) {
	c.t.Helper()
	resp := c.lease(clusterv1.LeaseRequest{FreeSlots: slots, WaitSeconds: 1})
	if resp.status == http.StatusNoContent {
		return nil, resp
	}
	if resp.status != http.StatusOK {
		return nil, resp
	}
	var out clusterv1.LeaseResponse
	resp.decode(c.t, &out)
	return out.Leases, resp
}

// mustLease polls until it gets exactly one lease, failing rather than hanging.
func (c *client) mustLease() clusterv1.Lease {
	c.t.Helper()
	for i := 0; i < 5; i++ {
		leases, resp := c.poll(1)
		if len(leases) == 1 {
			return leases[0]
		}
		if resp.status != http.StatusNoContent {
			c.t.Fatalf("lease: status %d body %s", resp.status, resp.body)
		}
	}
	c.t.Fatal("no lease issued")
	return clusterv1.Lease{}
}

func (c *client) ack(runID runv1.ULID, epoch int64) response {
	return c.post("/leases/"+string(runID)+"/ack", c.token(), clusterv1.AckRequest{
		ClusterID: c.cluster, Epoch: epoch,
		CRName: "ar-" + string(runID), Namespace: "haliphron-agents",
	})
}

func (c *client) mustAck(runID runv1.ULID, epoch int64) clusterv1.AckResponse {
	c.t.Helper()
	resp := c.ack(runID, epoch)
	if resp.status != http.StatusOK {
		c.t.Fatalf("ack: status %d body %s", resp.status, resp.body)
	}
	var out clusterv1.AckResponse
	resp.decode(c.t, &out)
	return out
}

func (c *client) rejectAck(runID runv1.ULID, epoch int64, code clusterv1.RejectionCode, msg string) response {
	no := false
	return c.post("/leases/"+string(runID)+"/ack", c.token(), clusterv1.AckRequest{
		ClusterID: c.cluster, Epoch: epoch, Accepted: &no,
		Rejection: &clusterv1.AckRejection{Code: code, Message: msg},
	})
}

func (c *client) artifacts(runID runv1.ULID, epoch int64, attempt int32) response {
	return c.post("/leases/"+string(runID)+"/artifacts", c.token(),
		clusterv1.ArtifactBundleRequest{Epoch: epoch, Attempt: attempt})
}

func (c *client) heartbeat(req clusterv1.HeartbeatRequest) clusterv1.HeartbeatResponse {
	c.t.Helper()
	resp := c.post("/clusters/"+string(c.cluster)+"/heartbeat", c.token(), req)
	if resp.status != http.StatusOK {
		c.t.Fatalf("heartbeat: status %d body %s", resp.status, resp.body)
	}
	var out clusterv1.HeartbeatResponse
	resp.decode(c.t, &out)
	return out
}

func (c *client) ingest(reports ...clusterv1.RunObservation) clusterv1.StatusIngestResponse {
	c.t.Helper()
	resp := c.post("/ingest/status", c.token(), clusterv1.StatusIngestRequest{
		ClusterID: c.cluster, Reports: reports,
	})
	if resp.status != http.StatusOK {
		c.t.Fatalf("ingest: status %d body %s", resp.status, resp.body)
	}
	var out clusterv1.StatusIngestResponse
	resp.decode(c.t, &out)
	return out
}

func (c *client) completion(req clusterv1.CompletionIngestRequest) response {
	req.ClusterID = c.cluster
	return c.post("/ingest/completion", c.token(), req)
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// serve mounts a fake on a test server and returns its base URL.
func serve(t *testing.T, b *backend.Backend) string {
	t.Helper()
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// start brings up a fake and one registered cluster, the state every test but
// the registration ones begins from.
func start(t *testing.T, opts ...backend.Option) (*backend.Backend, *client) {
	t.Helper()
	b := backend.New(opts...)
	c := newClient(t, serve(t, b), "kid-a")
	c.now = b.Now
	c.mustRegister(b.BootstrapToken(), "cluster-a")
	return b, c
}

// timingsWith is the contract's defaults with one thing changed, so a test can
// shorten a window without restating six numbers it does not care about.
func timingsWith(mutate func(*clusterv1.Timings)) clusterv1.Timings {
	t := clusterv1.DefaultTimings()
	mutate(&t)
	return t
}

// join registers a second cluster against the same fake, for the tests about
// work moving between them.
func join(t *testing.T, b *backend.Backend, c *client, kid, name string) *client {
	t.Helper()
	other := newClient(t, c.baseURL, kid)
	other.now = b.Now
	other.mustRegister(b.IssueBootstrapToken(), name)
	return other
}

func sampleSpec() runv1.RenderedRunSpec {
	return runv1.RenderedRunSpec{
		Agent: runv1.AgentClaudeCode,
		Model: "anthropic/claude-opus-5",
		Image: "ghcr.io/automagicops/agent-runtime:v1",
		Repo: runv1.RepoSpec{
			URL: "https://github.com/acme/widgets.git", Provider: runv1.GitProviderGitHub,
			BaseBranch: "main", TargetBranch: "haliphron/01j8-add-rds",
		},
		Runtime: runv1.RuntimeSpec{TimeoutSeconds: 3600},
	}
}

func observation(l clusterv1.Lease, phase runv1.Phase) clusterv1.RunObservation {
	return clusterv1.RunObservation{
		RunID: l.RunID, Epoch: l.Epoch, Attempt: l.Attempt, Phase: phase,
	}
}
