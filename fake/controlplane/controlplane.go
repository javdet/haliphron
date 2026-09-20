// Package controlplane is FakeControlPlane: object storage with presigned
// capabilities plus the controller's completion webhook, in one process.
//
// It is the third of the three fakes named in the phasing plan, and the only
// one the agent image needs. The image has no lease and no reconciliation: it
// receives a directory of secret files, a handful of environment variables and
// an address to knock on, so its entire outside world is a bucket it writes
// through signed links and an endpoint that accepts one report. That is what
// this package is, and it is why the image track can be verified with a single
// `docker run` rather than a cluster.
//
// It serves both halves of the ArtifactStore port, because the image implements
// both. In relay mode it accepts the pod's artifacts on the same endpoint that
// takes its completion, which is what a controller does; in object-store mode
// it serves signed links, which is what S3 does. Relay is the default, matching
// what an installation gets without configuration.
//
// What it is not: MinIO. Objects live in a map and the links are signed with
// HMAC over the method, the key and the expiry. What the image must get right
// is the shape of the exchange and the answers it gets when things go wrong —
// 403 on an expired signature, a POST policy that refuses a key outside its
// prefix, a 413 on a run that has spent its artifact budget — and those are
// implemented exactly, because every one of them is a row in the contract's
// test checklist. Where a real S3 or a real controller would differ in a way
// the image can observe, the comment says so.
//
// A fake that is merely permissive is worse than no fake: it teaches the image
// habits MinIO will reject.
package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Default sizes. The POST ceiling matters: a log chunk that grows without
// bound is the failure mode this policy field exists to stop, and a fake that
// never enforces it lets the image ship without ever having handled a 413.
const (
	DefaultBucket       = "haliphron"
	DefaultMaxPostBytes = 32 << 20
	// maxObjectBytes caps a single PUT. Storage here is a map in a test
	// process; an image bug that streams a gigabyte should fail the test, not
	// the machine.
	maxObjectBytes = 256 << 20
)

// artifactModeOrRelay is the configured mode, defaulting to relay.
func (c *ControlPlane) artifactModeOrRelay() runv1.ArtifactMode {
	if c.artifactMode == runv1.ArtifactModeObjectStore {
		return runv1.ArtifactModeObjectStore
	}
	return runv1.ArtifactModeRelay
}

// ControlPlane is the fake. Safe for concurrent use: the entrypoint uploads log
// chunks from a background goroutine while the main one is still running the
// agent, and "the final log and the chunks agree" is a property worth testing
// rather than serialising away.
type ControlPlane struct {
	mu sync.Mutex

	bucket  string
	baseURL string
	signKey []byte
	ttl     time.Duration

	// artifactMode is which half of the port this fake presents. Relay by
	// default, matching what an installation gets without configuration: the
	// image must work against the default path without being told anything.
	artifactMode   runv1.ArtifactMode
	maxBytesPerRun int64

	objects map[string]*object
	runs    map[runv1.ULID]*runState
	// byToken indexes the callback tokens. The controller mints one per run and
	// binds it to that run; checking the binding is the whole reason the token
	// exists, so the index is not an optimisation.
	byToken map[string]runv1.ULID

	faults faults
	ids    ulidGen
	offset time.Duration
	log    []string
}

// object is one stored object. The digest is computed on write so that a test
// can assert "what the image uploaded is what the image said it uploaded"
// without re-reading the body.
type object struct {
	body        []byte
	contentType string
	sha256      string
	writtenAt   time.Time
}

// runState is everything the fake knows about one prepared run.
type runState struct {
	id            runv1.ULID
	attempt       int32
	callbackToken string
	secrets       map[string]string
	bundle        clusterv1.ArtifactBundle
	// reports accumulate in arrival order, including duplicates: at-least-once
	// delivery means the image may send the same report twice, and a test that
	// cannot see the repeat cannot check that the second one was a duplicate
	// rather than a second run.
	reports []ReceivedReport

	// phases are the entrypoint phases this run reported getting through, in
	// arrival order. It is the assertion surface that replaced reading
	// state.json back out of the bucket — and a better one, because it records
	// what the pod said when it said it rather than what survived until the pod
	// next chose to save.
	phases []runv1.RuntimePhase
	// relayed is how many bytes this run has pushed through the relay, for the
	// per-run budget.
	relayed int64
}

// ReceivedReport is one delivery of the completion webhook, as it arrived.
type ReceivedReport struct {
	Report     runv1.CompletionReport
	Body       []byte
	Attempt    int
	ReceivedAt time.Time
	// Idempotency is the Idempotency-Key header, verbatim. The contract makes
	// it optional and says what it must contain when present; an empty string
	// here is a legal answer and a wrong one is not.
	Idempotency string
	Status      int
	Duplicate   bool
}

// Option configures the fake at construction.
type Option func(*ControlPlane)

// WithBucket names the bucket the presigned links point into.
func WithBucket(name string) Option {
	return func(c *ControlPlane) { c.bucket = name }
}

// WithSignatureTTL sets how long a minted bundle stays valid. Tests that want
// an expired signature move the clock with Advance instead of setting this to
// something implausible: the image is allowed to notice a bundle that was born
// expired and refuse to start, and then the test proves nothing.
func WithSignatureTTL(d time.Duration) Option {
	return func(c *ControlPlane) { c.ttl = d }
}

// WithArtifactMode selects which half of the ArtifactStore port the image is
// exercised against.
//
// Relay is the default, matching what an installation gets with nothing
// configured. A test that sets object-store is exercising the optimisation
// rather than the path every installation takes, and both are worth a pass:
// the image implements both, and the contract test runs the checklist twice.
func WithArtifactMode(mode runv1.ArtifactMode) Option {
	return func(c *ControlPlane) { c.artifactMode = mode }
}

// WithArtifactBudget caps what one run may store. Small values are the point: a
// 413 on a spent budget is otherwise only reachable by uploading a gigabyte.
func WithArtifactBudget(bytes int64) Option {
	return func(c *ControlPlane) { c.maxBytesPerRun = bytes }
}

// New builds a fake with no runs in it. The caller must set the base URL before
// preparing a run — presigned links have to point somewhere — which is why
// NewServer exists and is what almost every caller should use.
func New(opts ...Option) *ControlPlane {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a fake
		// that silently signs with zeros would pass every test that matters.
		panic("controlplane: no entropy for the signing key: " + err.Error())
	}
	c := &ControlPlane{
		bucket:  DefaultBucket,
		signKey: key,
		ttl:     time.Hour,
		objects: map[string]*object{},
		runs:    map[runv1.ULID]*runState{},
		byToken: map[string]runv1.ULID{},
		// Relay, matching the real default. A fake whose default differed would
		// let the image pass its contract tests against a path most
		// installations never take.
		artifactMode:   runv1.ArtifactModeRelay,
		maxBytesPerRun: clusterv1.DefaultMaxBytesPerRun,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewServer starts the fake on a loopback listener and points its links at
// itself. The caller closes the server.
//
// The image reaches storage over the network by construction — presigned links
// are URLs — so there is no in-process shortcut to offer, and every test pays
// for a real listener whether it wants one or not.
func NewServer(opts ...Option) (*ControlPlane, *httptest.Server) {
	c := New(opts...)
	srv := httptest.NewServer(c.Handler())
	c.SetBaseURL(srv.URL)
	return c, srv
}

// SetBaseURL tells the fake where it can be reached. Links minted before this
// call point at the empty string and are useless, which is loud enough.
func (c *ControlPlane) SetBaseURL(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = u
}

// BaseURL is the address callers hand to the image.
func (c *ControlPlane) BaseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL
}

// Bucket is the bucket name in the minted bundles.
func (c *ControlPlane) Bucket() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bucket
}

// Handler serves both halves: the storage endpoint under /storage and the
// controller's runtime API under its own base path. They share a process
// because the image talks to both and nothing is learned by making a test start
// two servers.
func (c *ControlPlane) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /storage/{bucket}/{key...}", c.handleGet)
	mux.HandleFunc("PUT /storage/{bucket}/{key...}", c.handlePut)
	mux.HandleFunc("POST /storage/{bucket}", c.handlePost)
	// The three callback paths, under the base the CR advertises. They are
	// named by the contract's own constants rather than spelled out, so a fake
	// and an image built from one contract agree on the routes by construction.
	mux.HandleFunc("POST /runtime/v1"+runv1.CallbackPathCompletion, c.handleCompletion)
	mux.HandleFunc("POST /runtime/v1"+runv1.CallbackPathPhase, c.handlePhase)
	mux.HandleFunc("POST /runtime/v1"+runv1.CallbackPathArtifacts, c.handleRelay)
	return mux
}

// now is the fake's clock, offset by Advance.
func (c *ControlPlane) now() time.Time { return time.Now().Add(c.offset) }

// Advance moves the fake's clock forward. Every signature minted before the
// call expires that much sooner, which is how a test reproduces "the bundle
// outlived its own run" without sleeping through the TTL.
func (c *ControlPlane) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// Logf records what the fake did, for a test that fails and needs to say why.
func (c *ControlPlane) logf(format string, args ...any) {
	c.log = append(c.log, time.Now().Format(time.RFC3339Nano)+" "+fmt.Sprintf(format, args...))
}

// Log returns the fake's own trace, oldest first.
func (c *ControlPlane) Log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

// ulidGen produces identifiers that sort by creation order. The entropy is a
// counter, not random bytes: a test that fails on the eleventh run should print
// the same identifier when it is re-run.
type ulidGen struct{ counter uint64 }

// crockford is the ULID alphabet: no I, L, O or U, so a transcribed identifier
// cannot turn into a different valid one.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func (g *ulidGen) next(now time.Time) runv1.ULID {
	g.counter++
	n := g.counter
	b := make([]byte, 0, 26)
	ms := uint64(now.UnixMilli())
	for shift := 45; shift >= 0; shift -= 5 {
		b = append(b, crockford[(ms>>uint(shift))&0x1f])
	}
	for shift := 75; shift >= 0; shift -= 5 {
		var v uint64
		if shift < 64 {
			v = (n >> uint(shift)) & 0x1f
		}
		b = append(b, crockford[v])
	}
	return runv1.ULID(b)
}

// randomToken mints a bearer credential. Callback tokens are the one thing here
// that must not be guessable: a test proving "a pod cannot post a completion
// for somebody else's run" is meaningless against a counter.
func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("controlplane: no entropy for a callback token: " + err.Error())
	}
	return hex.EncodeToString(b)
}
