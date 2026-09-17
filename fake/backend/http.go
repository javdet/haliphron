package backend

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

func (b *Backend) routes() http.Handler {
	mux := http.NewServeMux()
	p := clusterv1.BasePath
	mux.HandleFunc("POST "+p+"/register", b.handleRegister)
	mux.HandleFunc("POST "+p+"/clusters/{clusterID}/leases", b.handleLeases)
	mux.HandleFunc("POST "+p+"/clusters/{clusterID}/heartbeat", b.handleHeartbeat)
	mux.HandleFunc("POST "+p+"/leases/{runID}/ack", b.handleAck)
	mux.HandleFunc("POST "+p+"/leases/{runID}/artifacts", b.handleArtifacts)
	mux.HandleFunc("POST "+p+"/ingest/status", b.handleIngestStatus)
	mux.HandleFunc("POST "+p+"/ingest/completion", b.handleIngestCompletion)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !b.preflight(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// preflight applies what every endpoint owes the caller before it looks at the
// body: the injected faults, the body ceiling and the version window. Each
// answers with an action, so the controller never has to guess from the number.
func (b *Backend) preflight(w http.ResponseWriter, r *http.Request) bool {
	b.mu.Lock()
	f := b.faults
	versions := b.versions
	b.mu.Unlock()

	switch {
	case f.unavailable:
		// Migrations, a restart, no Postgres. The controller keeps running what
		// it holds and comes back — the whole point of ADR 6.
		w.Header().Set("Retry-After", "5")
		b.writeProblem(w, http.StatusServiceUnavailable, clusterv1.Problem{
			Title: "backend unavailable", Code: clusterv1.CodeUnavailable,
			Action: clusterv1.ActionBackoff, RetryAfterSeconds: 5,
		})
		return false
	case f.rateLimited:
		w.Header().Set("Retry-After", "1")
		b.writeProblem(w, http.StatusTooManyRequests, clusterv1.Problem{
			Title: "too many requests", Code: clusterv1.CodeRateLimited,
			Action: clusterv1.ActionBackoff, RetryAfterSeconds: 1,
		})
		return false
	}

	if r.ContentLength > clusterv1.MaxRequestBytes {
		b.writeProblem(w, http.StatusRequestEntityTooLarge, clusterv1.Problem{
			Title: "body exceeds 1 MiB", Code: clusterv1.CodePayloadTooLarge,
			Action: clusterv1.ActionFatal,
		})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, clusterv1.MaxRequestBytes)

	// Mandatory on every request, including /register: in a multi-cluster
	// installation there is otherwise no telling which version sent what, and
	// that question is always asked after the fact.
	version := r.Header.Get(clusterv1.HeaderControllerVersion)
	if version == "" {
		b.writeProblem(w, http.StatusBadRequest, clusterv1.Problem{
			Title:  "missing " + clusterv1.HeaderControllerVersion,
			Code:   clusterv1.CodeInvalidRequest,
			Action: clusterv1.ActionFatal,
		})
		return false
	}
	if !versionInRange(version, versions) {
		b.writeProblem(w, http.StatusUnprocessableEntity, clusterv1.Problem{
			Title: "controller " + version + " outside supported range " +
				versions.Min + ".." + versions.Max,
			Code:   clusterv1.CodeUnsupportedControllerVersion,
			Action: clusterv1.ActionFatal,
		})
		return false
	}
	return true
}

// authenticate verifies the JWT and returns the calling cluster. wantCluster is
// the identity the path or body claims; an empty value means the endpoint is
// run-scoped and the caller checks ownership itself.
func (b *Backend) authenticate(w http.ResponseWriter, r *http.Request, wantCluster runv1.ULID) (*cluster, bool) {
	token, ok := bearer(r)
	if !ok {
		b.unauthenticated(w, "missing bearer token", clusterv1.ActionRetry)
		return nil, false
	}

	header, err := parseJWTHeader(token)
	if err != nil {
		b.unauthenticated(w, "malformed token", clusterv1.ActionRetry)
		return nil, false
	}

	b.mu.Lock()
	clusterID, known := b.byKID[header.Kid]
	var c *cluster
	if known {
		c = b.clusters[clusterID]
	}
	now := b.now()
	b.mu.Unlock()

	if c == nil {
		// The key is not one we hold. Retrying with the same key cannot help;
		// the cluster has to register again.
		b.unauthenticated(w, "unknown key "+header.Kid, clusterv1.ActionReregister)
		return nil, false
	}

	claims, err := verifyJWT(token, c.key)
	if err != nil {
		b.unauthenticated(w, "signature does not verify", clusterv1.ActionRetry)
		return nil, false
	}
	if !claims.hasAudience(clusterv1.TokenAudience) {
		b.unauthenticated(w, "wrong audience", clusterv1.ActionRetry)
		return nil, false
	}
	if !claims.lifetimeOK(now,
		clusterv1.TokenMaxTTLSeconds*time.Second,
		clusterv1.ClockSkewToleranceSeconds*time.Second) {
		b.unauthenticated(w, "token lifetime rejected", clusterv1.ActionRetry)
		return nil, false
	}
	if claims.Jti == "" {
		// The replay cache is not built in phase 1, but a token without a jti
		// could never be checked later, so the field is mandatory now.
		b.unauthenticated(w, "missing jti", clusterv1.ActionRetry)
		return nil, false
	}
	if claims.Sub != string(c.id) {
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "subject does not match the key's cluster", Code: clusterv1.CodeClusterMismatch,
			Action: clusterv1.ActionAbandon, ClusterID: c.id,
		})
		return nil, false
	}
	if c.revoked {
		// Revocation is a status change and takes effect within the lifetime of
		// a token already issued. Repeats are pointless, so: fatal.
		b.writeProblem(w, http.StatusUnauthorized, clusterv1.Problem{
			Title: "cluster revoked", Code: clusterv1.CodeClusterRevoked,
			Action: clusterv1.ActionFatal, ClusterID: c.id,
		})
		return nil, false
	}
	if wantCluster != "" && wantCluster != c.id {
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "token is for a different cluster", Code: clusterv1.CodeClusterMismatch,
			Action: clusterv1.ActionAbandon, ClusterID: c.id,
		})
		return nil, false
	}
	return c, true
}

func (b *Backend) unauthenticated(w http.ResponseWriter, detail string, action clusterv1.Action) {
	b.writeProblem(w, http.StatusUnauthorized, clusterv1.Problem{
		Title: "unauthenticated", Detail: detail,
		Code: clusterv1.CodeUnauthenticated, Action: action,
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

// decode reads a JSON body. Unknown fields are accepted on purpose: the
// compatibility rule says both sides ignore what they do not recognise, and a
// fake that rejected them would teach the controller that a newer control plane
// is a broken one.
func decode(r *http.Request, into any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func (b *Backend) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSecret is writeJSON for the two responses carrying secret material. The
// header is not decoration: a cache between the controller and the backend
// holding a lease body is a git token on disk in a customer's cluster.
func (b *Backend) writeSecret(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	b.writeJSON(w, status, body)
}

func (b *Backend) writeProblem(w http.ResponseWriter, status int, p clusterv1.Problem) {
	p.Status = int32(status)
	if p.Type == "" {
		p.Type = clusterv1.ProblemTypeBase + kebab(string(p.Code))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

func kebab(code string) string {
	var out strings.Builder
	for i, r := range code {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r + 32)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// versionInRange compares dotted SemVer cores. Pre-release and build metadata
// are ignored: a controller built from a branch is still that minor version,
// and refusing it would make a fake unusable for the people most likely to be
// running one.
func versionInRange(v string, r clusterv1.VersionRange) bool {
	return compareVersions(v, r.Min) >= 0 && compareVersions(v, r.Max) <= 0
}

func compareVersions(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		switch {
		case as[i] < bs[i]:
			return -1
		case as[i] > bs[i]:
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}
		}
		out[i] = n
	}
	return out
}

func decodeKey(raw string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(raw)
}
