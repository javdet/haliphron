package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// A controller that received its clusterID and crashed before writing it to
// its Secret must be able to repeat the call. Without idempotency on the pair
// the token is spent, the identity is lost, and repairing it takes a human
// with the UI — which is the one failure mode a cluster cannot recover from by
// restarting.
func TestRegisterIsIdempotentOnTheKey(t *testing.T) {
	h := newHarness(t)
	token := h.BootstrapToken()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	request := clusterv1.RegisterRequest{
		Name: "east",
		PublicKey: clusterv1.PublicKey{
			KID: "kid-east", Alg: clusterv1.KeyAlgorithm,
			Key: base64.RawURLEncoding.EncodeToString(pub),
		},
		ControllerVersion: "1.0.0",
		AgentNamespace:    "haliphron-agents",
	}

	first, err := h.App.Register(context.Background(), token, request)
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	second, err := h.App.Register(context.Background(), token, request)
	if err != nil {
		t.Fatalf("repeat registration: %v", err)
	}

	if first.ClusterID != second.ClusterID {
		t.Errorf("repeat registration produced %s, first produced %s", second.ClusterID, first.ClusterID)
	}
	if second.Timings != h.App.Timings() {
		t.Errorf("timings = %+v, want %+v", second.Timings, h.App.Timings())
	}
	// The controller is obliged to compare serverTime with its own clock at
	// startup; skew otherwise surfaces as intermittent 401s under load.
	if second.ServerTime.IsZero() {
		t.Error("serverTime is missing; a controller cannot detect clock skew without it")
	}
}

// A spent token presented with a different key is a stolen token being
// replayed, not a retry.
func TestASpentTokenRefusesADifferentKey(t *testing.T) {
	h := newHarness(t)
	token := h.BootstrapToken()

	register := func(name, kid string) error {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		_, err = h.App.Register(context.Background(), token, clusterv1.RegisterRequest{
			Name: name,
			PublicKey: clusterv1.PublicKey{
				KID: kid, Alg: clusterv1.KeyAlgorithm,
				Key: base64.RawURLEncoding.EncodeToString(pub),
			},
			ControllerVersion: "1.0.0", AgentNamespace: "haliphron-agents",
		})
		return err
	}

	if err := register("east", "kid-east"); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	err := register("west", "kid-west")
	problem, ok := err.(*clusterv1.Problem)
	if !ok {
		t.Fatalf("expected a Problem, got %v", err)
	}
	if problem.Code != clusterv1.CodeBootstrapTokenConsumed {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeBootstrapTokenConsumed)
	}
	if problem.Action != clusterv1.ActionFatal {
		t.Errorf("action = %s, want fatal: a retry cannot mint a token", problem.Action)
	}
}

// Every rejection in this contract has to say what to do about it, because the
// controller is forbidden to infer behaviour from the status code.
func TestAuthenticationFailuresCarryAnAction(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")

	cases := []struct {
		name   string
		token  string
		code   clusterv1.ProblemCode
		action clusterv1.Action
	}{
		{
			// A key this control plane does not hold. Retrying with it cannot
			// help, and neither can waiting: the cluster has to register
			// again, so the action says so.
			name:  "an unknown key sends the controller back to registration",
			token: p.mintTokenWithKID("kid-nobody", p.clusterID, time.Now(), time.Minute, true),
			code:  clusterv1.CodeUnauthenticated, action: clusterv1.ActionReregister,
		},
		{
			name:  "a token whose life exceeds the ceiling is refused",
			token: p.mintToken(p.clusterID, time.Now(), 30*time.Minute, true),
			code:  clusterv1.CodeUnauthenticated, action: clusterv1.ActionRetry,
		},
		{
			name:  "an expired token is refused",
			token: p.mintToken(p.clusterID, time.Now().Add(-10*time.Minute), time.Minute, true),
			code:  clusterv1.CodeUnauthenticated, action: clusterv1.ActionRetry,
		},
		{
			name:  "a token without a jti could never be checked for replay",
			token: p.mintToken(p.clusterID, time.Now(), time.Minute, false),
			code:  clusterv1.CodeUnauthenticated, action: clusterv1.ActionRetry,
		},
		{
			name:  "a subject that is not the key's cluster is somebody else's work",
			token: p.mintToken("01JSOMEONEELSE00000000001", time.Now(), time.Minute, true),
			code:  clusterv1.CodeClusterMismatch, action: clusterv1.ActionAbandon,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", tc.token,
				clusterv1.HeartbeatRequest{FreeSlots: 1}, nil)
			if problem == nil {
				t.Fatalf("expected a rejection, got status %d", status)
			}
			if problem.Code != tc.code {
				t.Errorf("code = %s, want %s", problem.Code, tc.code)
			}
			if problem.Action != tc.action {
				t.Errorf("action = %s, want %s", problem.Action, tc.action)
			}
		})
	}
}

// A revoked cluster stops being able to act within the life of a token already
// issued, which is what makes a revocation list unnecessary: the row is read on
// every request anyway.
func TestRevocationTakesEffectOnTheNextRequest(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")

	if _, status := p.Lease(1, 1); status != http.StatusNoContent {
		t.Fatalf("an empty queue should answer 204, got %d", status)
	}

	if err := h.Store.RevokeCluster(context.Background(), p.clusterID, "decommissioned"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, problem := p.post("/clusters/"+string(p.clusterID)+"/leases", p.token(),
		clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1}, nil)
	if problem == nil {
		t.Fatal("a revoked cluster was still served")
	}
	if problem.Code != clusterv1.CodeClusterRevoked {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeClusterRevoked)
	}
	if problem.Action != clusterv1.ActionFatal {
		t.Errorf("action = %s, want fatal", problem.Action)
	}
}

// The control plane and the chart are upgraded independently, so a version
// divergence is a normal state — up to the window the backend declares, outside
// which the answer must say that no retry will help.
func TestAControllerOutsideTheSupportedRangeIsToldItIsFatal(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	p.version = "0.0.1"

	_, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", p.token(),
		clusterv1.HeartbeatRequest{FreeSlots: 1}, nil)
	if problem == nil {
		t.Fatal("a controller below the supported range was accepted")
	}
	if problem.Code != clusterv1.CodeUnsupportedControllerVersion {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeUnsupportedControllerVersion)
	}
	if problem.Action != clusterv1.ActionFatal {
		t.Errorf("action = %s, want fatal", problem.Action)
	}
}

// The version header is mandatory on every request: in a multi-cluster
// installation there is otherwise no telling which version sent what, and that
// question is always asked after the fact.
func TestTheControllerVersionHeaderIsMandatory(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	p.version = ""

	_, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", p.token(),
		clusterv1.HeartbeatRequest{FreeSlots: 1}, nil)
	if problem == nil {
		t.Fatal("a request without the version header was accepted")
	}
	if problem.Code != clusterv1.CodeInvalidRequest {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeInvalidRequest)
	}
}
