// Package clusterapi is the backend's side of the Cluster API: the listener on
// :8082 that controllers lease work from.
//
// It is a separate listener from the public API and not the same thing behind a
// path prefix. The two have different authentication models, different
// consumers and different reasons to be reachable at all: this one need not be
// exposed through the ingress when the clusters sit inside the perimeter, and
// making that a deployment decision requires it to be a separate port.
package clusterapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// Token verification, on the standard library.
//
// Ed25519 and nothing else. An algorithm the backend is willing to negotiate
// is an algorithm an attacker can negotiate down, and "none" is the classic;
// there is exactly one signature scheme in this protocol and no reason to
// parse a second. That also makes a JWT library unnecessary here: what a
// library adds is algorithm agility and a parser for claims this contract does
// not use, and what it adds with them is the question "which algorithms will
// this accept", which is where the CVEs in this area come from.
//
// The backend never mints one of these. It holds only public keys, which is the
// property ADR 1 rests on: a compromised control plane gets what the control
// plane already knows and no ability to act as a cluster.

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	KID string `json:"kid"`
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	// Aud is a string or an array of strings, per RFC 7519. A controller that
	// sends the array form is not wrong, and finding that out in production is
	// an expensive way to read the RFC.
	Aud any    `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

var (
	errMalformedToken = errors.New("malformed token")
	errBadSignature   = errors.New("signature does not verify")
)

// parseHeader reads the header without verifying anything: the kid is what
// finds the key that the verification then uses.
func parseHeader(token string) (jwtHeader, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtHeader{}, errMalformedToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, errMalformedToken
	}
	var h jwtHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return jwtHeader{}, errMalformedToken
	}
	if h.Alg != clusterv1.SigningAlgorithm || h.KID == "" {
		return jwtHeader{}, errMalformedToken
	}
	return h, nil
}

// verify checks the signature and returns the claims. The claim semantics are
// checked by the caller, which has the request in front of it and knows what
// the subject is supposed to match.
func verify(token string, key ed25519.PublicKey) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errMalformedToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, errMalformedToken
	}
	if len(key) != ed25519.PublicKeySize {
		return jwtClaims{}, errBadSignature
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return jwtClaims{}, errBadSignature
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, errMalformedToken
	}
	var c jwtClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return jwtClaims{}, errMalformedToken
	}
	return c, nil
}

// hasAudience accepts both shapes the RFC allows.
func (c jwtClaims) hasAudience(want string) bool {
	switch v := c.Aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// lifetimeOK bounds the token's life and tolerates a bounded clock skew.
//
// Both halves matter. The ceiling is what makes a self-signed cluster token
// acceptable at all: it is five minutes of authority, not a credential. The
// tolerance is what keeps a cluster whose clock drifts by twenty seconds from
// producing intermittent 401s that get diagnosed as a network fault —
// registration hands back serverTime so the controller can notice the drift
// itself and say so.
func (c jwtClaims) lifetimeOK(now time.Time, maxTTL, skew time.Duration) bool {
	if c.Iat == 0 || c.Exp == 0 {
		return false
	}
	iat := time.Unix(c.Iat, 0)
	exp := time.Unix(c.Exp, 0)

	switch {
	case exp.Sub(iat) > maxTTL:
		return false
	case now.After(exp.Add(skew)):
		return false
	case now.Before(iat.Add(-skew)):
		return false
	}
	return true
}
