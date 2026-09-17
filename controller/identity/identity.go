// Package identity is the cluster's credential: an Ed25519 pair generated
// inside the cluster, kept in a Secret, and never sent anywhere but the public
// half.
//
// This is the mechanical half of ADR 1. The control plane has no way into the
// cluster, and it must also have no way to *become* the cluster: if the backend
// could issue a token on a cluster's behalf, then compromising the backend
// would hand the attacker every cluster attached to it, and the threat model
// the whole pull design rests on would be fiction. So the backend stores a
// public key and nothing else, and the controller signs its own short-lived
// tokens.
package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// DefaultTokenTTL is comfortably inside the contract's 300 second ceiling. A
// shorter token costs nothing — it is minted per request — and bounds the value
// of one intercepted header.
const DefaultTokenTTL = 120 * time.Second

// Identity holds the key and the registered cluster ID. It implements
// clusterapi.Signer.
type Identity struct {
	mu        sync.RWMutex
	kid       string
	seed      []byte
	priv      ed25519.PrivateKey
	clusterID runv1.ULID
	ttl       time.Duration
}

// Persisted is what survives a restart. It lives in a Secret in the
// controller's own namespace: the private key must outlive the pod, or every
// rescheduling would need a fresh bootstrap token from a human.
type Persisted struct {
	KID       string
	Seed      []byte
	ClusterID runv1.ULID
}

// Store is where Persisted lives. An interface because the controller's own
// namespace is a Kubernetes detail and the protocol is not: the registration
// sequence below is worth testing without an API server.
type Store interface {
	// Load returns the stored identity, or ok false when there is none yet.
	Load(ctx context.Context) (Persisted, bool, error)
	Save(ctx context.Context, p Persisted) error
}

// Generate mints a new pair. The kid is derived from the public key rather than
// invented, so the same key always presents the same identifier — a controller
// that registers twice with one key must not look like two keys to the backend.
func Generate() (*Identity, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	return fromSeed(seed, "")
}

func fromSeed(seed []byte, kid string) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	id := &Identity{kid: kid, seed: seed, priv: priv, ttl: DefaultTokenTTL}
	if id.kid == "" {
		id.kid = keyID(priv.Public().(ed25519.PublicKey))
	}
	return id, nil
}

// keyID is the first 128 bits of the public key, base64url. Short enough for a
// JWT header, long enough not to collide, and derived so that it cannot drift
// from the key it names.
func keyID(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub[:16])
}

// ClusterID is the registered identity, empty until Bootstrap has run.
func (i *Identity) ClusterID() runv1.ULID {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.clusterID
}

// PublicKey is the half that leaves the cluster.
func (i *Identity) PublicKey() clusterv1.PublicKey {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pub := i.priv.Public().(ed25519.PublicKey)
	return clusterv1.PublicKey{
		KID: i.kid,
		Alg: clusterv1.KeyAlgorithm,
		Key: base64.RawURLEncoding.EncodeToString(pub),
	}
}

// Persisted is the form to write to the Store.
func (i *Identity) Persisted() Persisted {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return Persisted{KID: i.kid, Seed: i.seed, ClusterID: i.clusterID}
}

// Token mints a JWT for one request. A fresh jti every time: the backend is
// entitled to switch on the replay check without telling anyone, and a
// controller that reused a jti would start failing authentication for a reason
// invisible from inside the cluster.
func (i *Identity) Token(now time.Time) (string, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.clusterID == "" {
		return "", errors.New("identity: not registered yet")
	}
	return i.sign(i.clusterID, now, i.ttl)
}

func (i *Identity) sign(sub runv1.ULID, now time.Time, ttl time.Duration) (string, error) {
	header, err := json.Marshal(map[string]string{
		"alg": clusterv1.SigningAlgorithm,
		"typ": "JWT",
		"kid": i.kid,
	})
	if err != nil {
		return "", fmt.Errorf("identity: encode header: %w", err)
	}
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("identity: jti: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": string(sub),
		"sub": string(sub),
		"aud": clusterv1.TokenAudience,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti),
	})
	if err != nil {
		return "", fmt.Errorf("identity: encode claims: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(i.priv, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// adopt records the identity the backend assigned.
func (i *Identity) adopt(clusterID runv1.ULID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clusterID = clusterID
}

// Registrar is the one Cluster API call this package makes.
type Registrar interface {
	Register(ctx context.Context, bootstrapToken string, req clusterv1.RegisterRequest) (*clusterv1.RegisterResponse, error)
}

// Bootstrap returns the cluster's identity, registering only if it has to, and
// the operational parameters the control plane handed back.
//
// The ordering is the whole point. The key is written to the Store *before*
// /register is called, so that a controller which crashes after a successful
// registration still holds the key that registration was keyed on — and the
// call is idempotent on (bootstrapToken, publicKey), so repeating it returns
// the same clusterID. Without that ordering the failure is terminal: the
// one-time token is spent, the cluster has no identity, and the cure is a human
// with the UI.
func Bootstrap(ctx context.Context, store Store, reg Registrar, bootstrapToken string, req clusterv1.RegisterRequest, log *slog.Logger) (*Identity, *clusterv1.RegisterResponse, error) {
	if log == nil {
		log = slog.Default()
	}

	stored, found, err := store.Load(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("identity: load: %w", err)
	}

	var id *Identity
	switch {
	case found:
		if id, err = fromSeed(stored.Seed, stored.KID); err != nil {
			return nil, nil, err
		}
		if stored.ClusterID != "" {
			// Already registered. No call: registration is a one-time exchange
			// and repeating it on every restart would spend bootstrap tokens
			// the operator has to mint by hand.
			id.adopt(stored.ClusterID)
			log.Info("identity loaded", "clusterID", stored.ClusterID, "kid", id.kid)
			return id, nil, nil
		}
		log.Info("identity found without a cluster ID; repeating registration", "kid", id.kid)
	default:
		if id, err = Generate(); err != nil {
			return nil, nil, err
		}
		if err := store.Save(ctx, id.Persisted()); err != nil {
			return nil, nil, fmt.Errorf("identity: persist key before registering: %w", err)
		}
		log.Info("identity generated", "kid", id.kid)
	}

	if bootstrapToken == "" {
		return nil, nil, errors.New("identity: no cluster ID and no bootstrap token")
	}
	req.PublicKey = id.PublicKey()
	resp, err := reg.Register(ctx, bootstrapToken, req)
	if err != nil {
		return nil, nil, fmt.Errorf("identity: register: %w", err)
	}
	id.adopt(resp.ClusterID)
	if resp.TokenMaxTTLSeconds > 0 {
		ttl := time.Duration(resp.TokenMaxTTLSeconds) * time.Second
		if ttl < id.ttl {
			id.ttl = ttl
		}
	}
	if err := store.Save(ctx, id.Persisted()); err != nil {
		// Recoverable: the next start repeats the registration with the same
		// key and gets the same ID back. Worth an error in the log all the
		// same, because a controller that cannot write its own Secret will
		// fail in more interesting ways later.
		log.Error("could not persist the cluster ID; registration will be repeated on restart",
			"error", err, "clusterID", resp.ClusterID)
	}
	return id, resp, nil
}

// Deferred is a Signer whose identity appears later.
//
// The ordering it solves is small and unavoidable: the Cluster API client is
// what performs /register, and the identity it will sign with is what
// /register produces. Rather than let the client be constructed twice, or hold
// a mutable field on it, the client is handed this and the real identity
// arrives in it a moment later.
type Deferred struct {
	mu sync.RWMutex
	id *Identity
}

// Set installs the registered identity.
func (d *Deferred) Set(id *Identity) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.id = id
}

// ClusterID implements clusterapi.Signer.
func (d *Deferred) ClusterID() runv1.ULID {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.id == nil {
		return ""
	}
	return d.id.ClusterID()
}

// Token implements clusterapi.Signer. Before registration it fails rather than
// returning an empty token: an unauthenticated request would be answered with a
// 401, and diagnosing that is harder than reading this message.
func (d *Deferred) Token(now time.Time) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.id == nil {
		return "", errors.New("identity: the cluster has not registered yet")
	}
	return d.id.Token(now)
}
