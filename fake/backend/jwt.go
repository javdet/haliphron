package backend

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// The fake verifies tokens for real rather than trusting a header.
//
// This is the point of the exercise. The controller's obligations — mint per
// request, keep the key inside the cluster, set aud, keep exp short, put kid in
// the header — are all things that work perfectly against a fake that ignores
// them, and fail on the first request against a backend that does not. A fake
// that skips verification lets a controller pass its whole test suite with a
// bug that only production can find.
//
// Verification only, and Ed25519 only: the fake never mints a cluster token,
// because neither does the real backend. Hand-rolled on the standard library
// rather than pulled from a JWT library, because this is 60 lines of one
// algorithm with no negotiation, and "which algorithms will this library
// accept" is the question that produces CVEs.

type jwtClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud any    `json:"aud"` // a string or an array of strings, per RFC 7519
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

var (
	errMalformedToken = errors.New("malformed token")
	errBadSignature   = errors.New("signature does not verify")
	errUnknownKey     = errors.New("no such key")
)

// parseJWTHeader reads the header without verifying anything: the kid is needed
// to find the key that the verification will then use.
func parseJWTHeader(token string) (jwtHeader, error) {
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
	if h.Alg != clusterv1.SigningAlgorithm {
		// No negotiation. An accepted alg is an alg an attacker can select,
		// and "none" is the classic.
		return jwtHeader{}, errMalformedToken
	}
	return h, nil
}

// verifyJWT checks the signature and returns the claims. Claim semantics —
// audience, lifetime, subject — are checked by the caller, which has the
// request in front of it and knows what the subject is supposed to match.
func verifyJWT(token string, key ed25519.PublicKey) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errMalformedToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, errMalformedToken
	}
	if len(key) != ed25519.PublicKeySize {
		return jwtClaims{}, errUnknownKey
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

// hasAudience accepts both shapes RFC 7519 allows for aud. A controller that
// sends the array form is not wrong, and discovering that in production is an
// expensive way to read the RFC.
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

// lifetimeOK enforces the two rules that keep a cluster token short-lived: it
// must not already have expired, and it must not have been minted with a longer
// life than the contract allows. The skew tolerance applies to both ends —
// unsynchronised clocks are the normal case, and the symptom of getting this
// wrong is intermittent 401s that look like a network fault.
func (c jwtClaims) lifetimeOK(now time.Time, maxTTL, skew time.Duration) bool {
	if c.Exp == 0 || c.Iat == 0 {
		return false
	}
	exp := time.Unix(c.Exp, 0)
	iat := time.Unix(c.Iat, 0)
	if now.After(exp.Add(skew)) {
		return false
	}
	if iat.After(now.Add(skew)) {
		return false
	}
	return exp.Sub(iat) <= maxTTL+skew
}
