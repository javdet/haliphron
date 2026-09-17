package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

type memStore struct {
	p      Persisted
	ok     bool
	writes int
}

func (m *memStore) Load(context.Context) (Persisted, bool, error) { return m.p, m.ok, nil }

func (m *memStore) Save(_ context.Context, p Persisted) error {
	m.p, m.ok = p, true
	m.writes++
	return nil
}

type recordingRegistrar struct {
	calls int
	keys  []string
	resp  clusterv1.RegisterResponse
	err   error
}

func (r *recordingRegistrar) Register(_ context.Context, _ string, req clusterv1.RegisterRequest) (*clusterv1.RegisterResponse, error) {
	r.calls++
	r.keys = append(r.keys, req.PublicKey.Key)
	if r.err != nil {
		return nil, r.err
	}
	resp := r.resp
	if resp.ClusterID == "" {
		resp.ClusterID = "01J8X4K2ZQ7YB3M9F0R5W6T8CD"
	}
	resp.KeyID = req.PublicKey.KID
	return &resp, nil
}

// TestTheKeyIsWrittenBeforeRegistrationIsAttempted. The failure this ordering
// prevents is terminal and needs a human: the bootstrap token is one-time, so a
// controller that registered successfully and died before persisting its key
// would have spent the token and have no identity. With the key stored first,
// the call is repeatable — it is idempotent on (token, key) — and a restart
// simply asks again.
func TestTheKeyIsWrittenBeforeRegistrationIsAttempted(t *testing.T) {
	store := &memStore{}
	registrar := &recordingRegistrar{err: context.DeadlineExceeded}

	_, _, err := Bootstrap(context.Background(), store, registrar, "hlb_token",
		clusterv1.RegisterRequest{Name: "c"}, nil)
	if err == nil {
		t.Fatal("a failed registration was reported as success")
	}
	if !store.ok || len(store.p.Seed) != ed25519.SeedSize {
		t.Fatal("the key was not persisted before the call went out")
	}
	if store.p.ClusterID != "" {
		t.Fatal("a cluster ID was recorded for a registration that failed")
	}

	// The next start finds the key, sees no cluster ID and repeats the call
	// with the same key — which is what makes the backend's idempotency usable.
	registrar.err = nil
	id, _, err := Bootstrap(context.Background(), store, registrar, "hlb_token",
		clusterv1.RegisterRequest{Name: "c"}, nil)
	if err != nil {
		t.Fatalf("the repeat failed: %v", err)
	}
	if registrar.calls != 2 || registrar.keys[0] != registrar.keys[1] {
		t.Fatalf("the repeat used a different key: %v", registrar.keys)
	}
	if id.ClusterID() == "" || store.p.ClusterID != id.ClusterID() {
		t.Fatal("the cluster ID was not recorded")
	}
}

// TestAKnownIdentityDoesNotRegisterAgain. Registration consumes a one-time
// token an operator had to mint by hand; doing it on every restart would make
// rescheduling a pod an administrative event.
func TestAKnownIdentityDoesNotRegisterAgain(t *testing.T) {
	store := &memStore{}
	registrar := &recordingRegistrar{}
	if _, _, err := Bootstrap(context.Background(), store, registrar, "hlb_token",
		clusterv1.RegisterRequest{Name: "c"}, nil); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// A restart, with no bootstrap token in the environment at all.
	id, resp, err := Bootstrap(context.Background(), store, registrar, "",
		clusterv1.RegisterRequest{Name: "c"}, nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if registrar.calls != 1 {
		t.Fatalf("the restart registered again: %d calls", registrar.calls)
	}
	if resp != nil {
		t.Fatal("a restart produced a registration response it could not have received")
	}
	if id.ClusterID() == "" {
		t.Fatal("the identity was not restored")
	}
}

// TestTheTokenIsWhatTheBackendChecks. The claims are the contract's, and the
// signature is over the key the backend was given. Everything here is checked
// on the other side, so a mistake presents as an authentication failure with no
// clue in it.
func TestTheTokenIsWhatTheBackendChecks(t *testing.T) {
	store := &memStore{}
	registrar := &recordingRegistrar{}
	id, _, err := Bootstrap(context.Background(), store, registrar, "hlb_token",
		clusterv1.RegisterRequest{Name: "c"}, nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	now := time.Now()
	token, err := id.Token(now)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}

	header := decodeSegment[map[string]string](t, parts[0])
	if header["alg"] != clusterv1.SigningAlgorithm {
		t.Fatalf("want %s, got %s", clusterv1.SigningAlgorithm, header["alg"])
	}
	if header["kid"] != id.PublicKey().KID {
		t.Fatal("the header does not name the key the backend was given")
	}

	claims := decodeSegment[map[string]any](t, parts[1])
	if claims["aud"] != clusterv1.TokenAudience {
		t.Fatalf("wrong audience: %v", claims["aud"])
	}
	if claims["sub"] != string(id.ClusterID()) {
		t.Fatalf("sub is not the cluster: %v", claims["sub"])
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	if ttl := exp - iat; ttl <= 0 || ttl > clusterv1.TokenMaxTTLSeconds {
		t.Fatalf("a token valid for %d seconds is outside the contract's ceiling", ttl)
	}
	if claims["jti"] == "" {
		t.Fatal("no jti: the backend is entitled to start checking for replays without warning")
	}

	pub, err := base64.RawURLEncoding.DecodeString(id.PublicKey().Key)
	if err != nil {
		t.Fatalf("decode the public key: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode the signature: %v", err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("the signature does not verify against the key the backend stores")
	}

	second, err := id.Token(now)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if second == token {
		t.Fatal("two tokens minted at the same instant are identical; the jti is not fresh")
	}
}

// TestAnUnregisteredIdentityRefusesToSign. An empty token would be answered
// with a 401 the operator then has to diagnose; saying so here costs nothing.
func TestAnUnregisteredIdentityRefusesToSign(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := id.Token(time.Now()); err == nil {
		t.Fatal("an unregistered identity minted a token")
	}
	var deferred Deferred
	if _, err := deferred.Token(time.Now()); err == nil {
		t.Fatal("a deferred identity minted a token before it was set")
	}
	deferred.Set(id)
	if got := deferred.ClusterID(); got != runv1.ULID("") {
		t.Fatalf("an unregistered identity reported a cluster ID: %q", got)
	}
}

func decodeSegment[T any](t *testing.T, segment string) T {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse segment: %v", err)
	}
	return out
}
