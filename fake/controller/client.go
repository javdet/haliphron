package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The Cluster API as the controller speaks it.
//
// The key is generated here and the private half never leaves: that is the
// whole basis of ADR 1, and a fake that took a credential from the backend
// would be testing a protocol nobody is going to deploy. Tokens are minted per
// request and live for two minutes.

type keyPair struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKeyPair(kid string) (keyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return keyPair{}, fmt.Errorf("generate cluster key: %w", err)
	}
	return keyPair{kid: kid, pub: pub, priv: priv}, nil
}

// token mints a short-lived JWT for one request. A new jti every time, because
// the backend is entitled to start checking for replays without warning.
func (k keyPair) token(subject runv1.ULID, now time.Time, ttl time.Duration) string {
	header, _ := json.Marshal(map[string]string{
		"alg": clusterv1.SigningAlgorithm, "typ": "JWT", "kid": k.kid,
	})
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	claims, _ := json.Marshal(map[string]any{
		"iss": string(subject), "sub": string(subject),
		"aud": clusterv1.TokenAudience,
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti),
	})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(k.priv, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// call is one request to the Cluster API. It returns the decoded Problem rather
// than an opaque error, because every decision the controller makes on failure
// comes from Problem.action — never from the status code.
func (c *Controller) call(ctx context.Context, path, auth string, body, into any) (int, *clusterv1.Problem, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+clusterv1.BasePath+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clusterv1.HeaderControllerVersion, c.version)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A dropped long poll arrives here, and so does a backend that went
		// away. Neither is fatal: the work already taken is played out, and
		// the reports accumulate.
		c.note("transport error on %s: %v", path, err)
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var p clusterv1.Problem
		if err := json.Unmarshal(payload, &p); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("%s: status %d, unparsable body", path, resp.StatusCode)
		}
		return resp.StatusCode, &p, nil
	}
	if into != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, into); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil, nil
}

// authed is the token for the current identity.
func (c *Controller) authed() string {
	return c.key.token(c.clusterID, c.now(), c.tokenTTL)
}

// Register exchanges the bootstrap token for an identity and adopts the
// operational parameters the control plane hands back. Safe to call twice: the
// call is idempotent on the key, which is what saves a controller that crashed
// before persisting the clusterID.
func (c *Controller) Register(ctx context.Context, bootstrapToken string) error {
	req := clusterv1.RegisterRequest{
		Name: c.name,
		PublicKey: clusterv1.PublicKey{
			KID: c.key.kid, Alg: clusterv1.KeyAlgorithm,
			Key: base64.RawURLEncoding.EncodeToString(c.key.pub),
		},
		ControllerVersion: c.version,
		AgentNamespace:    c.namespace,
		K8sVersion:        c.k8sVersion,
		Runtimes:          c.runtimes,
		CapacitySlots:     c.capacity,
		CRDVersions:       c.crdVersions,
	}

	var out clusterv1.RegisterResponse
	status, problem, err := c.call(ctx, "/register", bootstrapToken, req, &out)
	if err != nil {
		return err
	}
	if problem != nil {
		return fmt.Errorf("register: %w", problem)
	}
	if status != http.StatusOK {
		return fmt.Errorf("register: unexpected status %d", status)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterID = out.ClusterID
	c.timings = out.Timings
	if out.TokenMaxTTLSeconds > 0 {
		ttl := time.Duration(out.TokenMaxTTLSeconds) * time.Second
		if c.tokenTTL > ttl {
			c.tokenTTL = ttl
		}
	}

	// The contract obliges the controller to compare clocks at startup and say
	// something. Skew otherwise surfaces as intermittent 401s under load, and
	// half a day goes into diagnosing a network that is fine.
	if skew := out.ServerTime.Sub(c.now()); skew > 30*time.Second || skew < -30*time.Second {
		c.noteLocked("clock skew of %s against the control plane", skew)
	}
	return nil
}
