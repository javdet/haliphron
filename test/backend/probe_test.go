package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// probe is a cluster that says exactly what a test tells it to.
//
// FakeController is the right driver for "does a correct controller work", and
// it is the wrong one for most of the decision table: it applies the
// monotonicity rules itself before it speaks, so it will not send a Running
// after a Succeeded, and it will not report under an epoch it knows is stale.
// Those rows exist because the network produces them — reordering, retries, a
// controller that was disconnected while its work was reassigned — and the
// backend's handling of them is what this contract is mostly about.
//
// It signs for real. The token is the one thing a test must not stub: the
// backend's verification is a check a permissive fake would hide.
type probe struct {
	t    *testing.T
	base string
	http *http.Client

	kid       string
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	clusterID runv1.ULID
	version   string
}

func newProbe(t *testing.T, h *harness, name string, opts ...func(*clusterv1.RegisterRequest)) *probe {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate probe key: %v", err)
	}
	p := &probe{
		t: t, base: h.Server.URL, http: h.Server.Client(),
		kid: "kid-" + name, priv: priv, pub: pub, version: "1.0.0",
	}

	req := clusterv1.RegisterRequest{
		Name: name,
		PublicKey: clusterv1.PublicKey{
			KID: p.kid, Alg: clusterv1.KeyAlgorithm,
			Key: base64.RawURLEncoding.EncodeToString(pub),
		},
		ControllerVersion: p.version,
		AgentNamespace:    "haliphron-agents",
		Runtimes:          []runv1.AgentType{runv1.AgentClaudeCode},
		CapacitySlots:     10,
	}
	for _, opt := range opts {
		opt(&req)
	}

	var resp clusterv1.RegisterResponse
	status, problem := p.post("/register", h.BootstrapToken(), req, &resp)
	if problem != nil || status != http.StatusOK {
		t.Fatalf("register probe %s: status %d problem %+v", name, status, problem)
	}
	p.clusterID = resp.ClusterID
	return p
}

// token mints a fresh JWT for one request, as a controller does.
func (p *probe) token() string {
	return p.mintToken(p.clusterID, time.Now(), 2*time.Minute, true)
}

// mintToken is token with every claim under the test's control, for the cases
// about authentication itself.
func (p *probe) mintToken(subject runv1.ULID, iat time.Time, ttl time.Duration, withJTI bool) string {
	return p.mintTokenWithKID(p.kid, subject, iat, ttl, withJTI)
}

// mintTokenWithKID signs under a key identifier the backend may never have
// seen, for the case where a cluster presents a credential this control plane
// does not hold.
func (p *probe) mintTokenWithKID(kid string, subject runv1.ULID, iat time.Time, ttl time.Duration, withJTI bool) string {
	header, _ := json.Marshal(map[string]string{
		"alg": clusterv1.SigningAlgorithm, "typ": "JWT", "kid": kid,
	})
	claims := map[string]any{
		"iss": string(subject), "sub": string(subject),
		"aud": clusterv1.TokenAudience,
		"iat": iat.Unix(), "exp": iat.Add(ttl).Unix(),
	}
	if withJTI {
		jti := make([]byte, 12)
		_, _ = rand.Read(jti)
		claims["jti"] = base64.RawURLEncoding.EncodeToString(jti)
	}
	encoded, _ := json.Marshal(claims)

	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(encoded)
	sig := ed25519.Sign(p.priv, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// post sends one request and decodes either the body or the Problem. It
// returns the Problem rather than an error because every assertion about a
// failure in this contract is about the code and the action, never the number.
func (p *probe) post(path, auth string, body, into any) (int, *clusterv1.Problem) {
	p.t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		p.t.Fatalf("encode %s: %v", path, err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.base+clusterv1.BasePath+path, bytes.NewReader(raw))
	if err != nil {
		p.t.Fatalf("build %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clusterv1.HeaderControllerVersion, p.version)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		p.t.Fatalf("send %s: %v", path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var problem clusterv1.Problem
		if err := json.Unmarshal(payload, &problem); err != nil {
			p.t.Fatalf("%s answered %d with an unparsable body: %s", path, resp.StatusCode, payload)
		}
		return resp.StatusCode, &problem
	}
	if into != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, into); err != nil {
			p.t.Fatalf("decode %s: %v (%s)", path, err, payload)
		}
	}
	return resp.StatusCode, nil
}

// Lease polls for work.
func (p *probe) Lease(slots int32, wait int32) ([]clusterv1.Lease, int) {
	p.t.Helper()
	var out clusterv1.LeaseResponse
	status, problem := p.post("/clusters/"+string(p.clusterID)+"/leases", p.token(),
		clusterv1.LeaseRequest{FreeSlots: slots, WaitSeconds: wait}, &out)
	if problem != nil {
		p.t.Fatalf("lease: %+v", problem)
	}
	return out.Leases, status
}

// LeaseOne takes exactly one unit of work and fails if none arrives.
func (p *probe) LeaseOne() clusterv1.Lease {
	p.t.Helper()
	leases, status := p.Lease(1, 1)
	if len(leases) != 1 {
		p.t.Fatalf("expected one lease, got %d (status %d)", len(leases), status)
	}
	return leases[0]
}

// Ack acknowledges a lease.
func (p *probe) Ack(runID runv1.ULID, epoch int64) (clusterv1.AckResponse, *clusterv1.Problem) {
	p.t.Helper()
	var out clusterv1.AckResponse
	_, problem := p.post("/leases/"+string(runID)+"/ack", p.token(),
		clusterv1.AckRequest{ClusterID: p.clusterID, Epoch: epoch,
			CRName: "agentrun-" + string(runID), Namespace: "haliphron-agents"}, &out)
	return out, problem
}

// Reject refuses a lease, which is the ack that says "I cannot materialise
// this" before anything is spent.
func (p *probe) Reject(runID runv1.ULID, epoch int64, code clusterv1.RejectionCode, message string) (clusterv1.AckResponse, *clusterv1.Problem) {
	p.t.Helper()
	no := false
	var out clusterv1.AckResponse
	_, problem := p.post("/leases/"+string(runID)+"/ack", p.token(),
		clusterv1.AckRequest{
			ClusterID: p.clusterID, Epoch: epoch, Accepted: &no,
			Rejection: &clusterv1.AckRejection{Code: code, Message: message},
		}, &out)
	return out, problem
}

// Report sends one observation on the low-latency path.
func (p *probe) Report(obs clusterv1.RunObservation) (clusterv1.StatusIngestResult, *clusterv1.Problem) {
	p.t.Helper()
	var out clusterv1.StatusIngestResponse
	_, problem := p.post("/ingest/status", p.token(),
		clusterv1.StatusIngestRequest{ClusterID: p.clusterID, Reports: []clusterv1.RunObservation{obs}}, &out)
	if problem != nil {
		return clusterv1.StatusIngestResult{}, problem
	}
	if len(out.Results) != 1 {
		p.t.Fatalf("expected one result, got %d", len(out.Results))
	}
	return out.Results[0], nil
}

// Phase is Report for the common case.
func (p *probe) Phase(runID runv1.ULID, epoch int64, attempt int32, phase runv1.Phase) (clusterv1.StatusIngestResult, *clusterv1.Problem) {
	p.t.Helper()
	observed := time.Now()
	return p.Report(clusterv1.RunObservation{
		RunID: runID, Epoch: epoch, Attempt: attempt, Phase: phase,
		JobName: "agentrun-" + string(runID), ObservedAt: &observed,
	})
}

// Complete sends a completion report.
func (p *probe) Complete(runID runv1.ULID, epoch int64, attempt int32,
	report runv1.CompletionReport) (clusterv1.CompletionIngestResponse, *clusterv1.Problem) {

	p.t.Helper()
	received := time.Now()
	var out clusterv1.CompletionIngestResponse
	_, problem := p.post("/ingest/completion", p.token(), clusterv1.CompletionIngestRequest{
		ClusterID: p.clusterID, RunID: runID, Epoch: epoch, Attempt: attempt,
		ReceivedAt: &received, Completion: report,
	}, &out)
	return out, problem
}

// Heartbeat reports what this cluster holds.
func (p *probe) Heartbeat(complete bool, runs ...clusterv1.RunObservation) clusterv1.HeartbeatResponse {
	p.t.Helper()
	var out clusterv1.HeartbeatResponse
	_, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", p.token(),
		clusterv1.HeartbeatRequest{
			FreeSlots: 10, CapacitySlots: 10, ReportComplete: complete, Runs: runs,
			Controller: &clusterv1.ControllerHealth{Version: p.version},
			Cluster: &clusterv1.ClusterFacts{
				K8sVersion: "v1.34.1", Runtimes: []runv1.AgentType{runv1.AgentClaudeCode},
			},
		}, &out)
	if problem != nil {
		p.t.Fatalf("heartbeat: %+v", problem)
	}
	return out
}

// Observation is a convenience for building one.
func observation(runID runv1.ULID, epoch int64, attempt int32, phase runv1.Phase) clusterv1.RunObservation {
	at := time.Now()
	return clusterv1.RunObservation{
		RunID: runID, Epoch: epoch, Attempt: attempt, Phase: phase, ObservedAt: &at,
	}
}

// completionFor builds a pod's report with the fields the backend promotes.
func completionFor(runID runv1.ULID, attempt int32, cost runv1.MoneyUSD, prURL string) runv1.CompletionReport {
	return runv1.CompletionReport{
		RunID: runID, Attempt: attempt,
		Status: runv1.CompletionSuccess, ExitCode: runv1.ExitSuccess,
		Agent: runv1.AgentClaudeCode, Model: "anthropic/claude-opus-5",
		Summary: "added the endpoint",
		Repo: &runv1.RepoResult{
			Pushed: true, TargetBranch: "haliphron/run-" + string(runID),
			PRURL: prURL, PRAction: runv1.PRActionCreated,
		},
		Usage: &runv1.Usage{
			TotalCostUSD: cost, DurationMs: 42_000, NumTurns: 5,
			InputTokens: 1200, OutputTokens: 800,
		},
	}
}

func fatalIfProblem(t *testing.T, context string, p *clusterv1.Problem) {
	t.Helper()
	if p != nil {
		t.Fatalf("%s: unexpected problem %s/%s: %s", context, p.Code, p.Action, p.Title)
	}
}
