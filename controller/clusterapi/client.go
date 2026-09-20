// Package clusterapi is the controller's half of contract 1: the transport,
// the token, and the translation of a failure into the one thing the contract
// says the controller may act on.
//
// The rule that shapes this package is section 7 of the Cluster API contract:
// the controller never infers behaviour from a status code. Every failure
// leaves here as a *clusterv1.Problem carrying an Action, or as a
// TransportError when there was no answer to read at all. Callers switch on
// the action and on nothing else — that is what stops the two independently
// written sides from disagreeing about whether a 409 means "try again" or
// "give up".
//
// The second rule is section 9: the bodies of /leases and /leases/{id}/artifacts
// are secret material. Nothing in this package logs a request or response body
// at any level, and there is deliberately no debug mode that would. A logging
// mistake here costs an hour-long git token and a bearer capability on a
// bucket prefix.
package clusterapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Signer mints the bearer token for one request. The private key never leaves
// the cluster, so this is an interface rather than a field: the client holds no
// key material and cannot be made to leak any.
type Signer interface {
	// ClusterID is the identity the backend registered, and the sub claim the
	// backend checks against the path. Empty before registration.
	ClusterID() runv1.ULID
	// Token returns a JWT valid for at most TokenMaxTTLSeconds.
	Token(now time.Time) (string, error)
}

// Client is the Cluster API as the controller speaks it. Safe for concurrent
// use: the lease loop, the heartbeat and the ingest queue all call it.
type Client struct {
	base    string
	version string
	signer  Signer
	http    *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// Options configures a Client. BaseURL and ControllerVersion are mandatory;
// the rest have defaults.
type Options struct {
	// BaseURL is the control plane's Cluster API root, without BasePath.
	BaseURL string
	// ControllerVersion travels on every request, including /register: in a
	// multi-cluster installation there is otherwise no telling which version
	// sent what, and that question is only ever asked after the fact.
	ControllerVersion string
	Signer            Signer
	// HTTPClient must have no Timeout of its own: the long poll runs for up to
	// MaxWaitSeconds and every call carries its own context deadline instead.
	HTTPClient *http.Client
	Logger     *slog.Logger
	Clock      func() time.Time
}

// New builds a client. It refuses an HTTP client with a Timeout, because that
// timeout would cut the long poll and the symptom — periodic disconnects on an
// idle queue — reads as network instability rather than as a misconfiguration.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("clusterapi: BaseURL is required")
	}
	if opts.ControllerVersion == "" {
		return nil, errors.New("clusterapi: ControllerVersion is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: DefaultTransport()}
	}
	if hc.Timeout != 0 {
		return nil, errors.New("clusterapi: HTTPClient.Timeout must be zero; calls carry context deadlines")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Client{
		base:    opts.BaseURL,
		version: opts.ControllerVersion,
		signer:  opts.Signer,
		http:    hc,
		log:     log,
		now:     clock,
	}, nil
}

// DefaultTransport is tuned for one long poll plus a trickle of short calls to
// a single host. The idle connection pool is small on purpose: a controller
// that holds dozens of connections to the control plane looks like a leak on
// the other side of the perimeter.
func DefaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 90 * time.Second
	t.ResponseHeaderTimeout = 0
	return t
}

// TransportError is a call that produced no answer to interpret: a refused
// connection, a dropped long poll, a proxy that closed mid-body. It is separate
// from Problem because the contract treats the two differently — a Problem says
// what to do, while this is the case where the controller decides for itself,
// and the decision is always "carry on with the work already held".
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return "cluster-api " + e.Op + ": " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ProblemOf extracts the contract's verdict from an error, if it carries one.
func ProblemOf(err error) (*clusterv1.Problem, bool) {
	var p *clusterv1.Problem
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

// ActionOf is what the controller does about an error. A transport failure maps
// to retry, which is the only honest answer: nothing was heard, so nothing is
// known, and the alternative — treating silence as abandonment — is how a
// ten-second network glitch turns into a lost hour of agent work.
func ActionOf(err error) clusterv1.Action {
	if p, ok := ProblemOf(err); ok {
		return p.Action
	}
	return clusterv1.ActionRetry
}

// RetryAfter is the delay the backend asked for, if it asked for one.
func RetryAfter(err error) (time.Duration, bool) {
	if p, ok := ProblemOf(err); ok && p.RetryAfterSeconds > 0 {
		return time.Duration(p.RetryAfterSeconds) * time.Second, true
	}
	return 0, false
}

// do is the one place a request is built and a response is interpreted. It
// never logs a body: see the package comment.
func (c *Client) do(ctx context.Context, op, path, auth string, body, into any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("cluster-api %s: encode request: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+clusterv1.BasePath+path, bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("cluster-api %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(clusterv1.HeaderControllerVersion, c.version)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &TransportError{Op: op, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, clusterv1.MaxRequestBytes))
	if err != nil {
		// A body that stops mid-read is the dropped long poll again, just
		// later in the exchange.
		return resp.StatusCode, &TransportError{Op: op, Err: err}
	}

	if resp.StatusCode >= 400 {
		return resp.StatusCode, problemFrom(op, resp.StatusCode, payload)
	}
	if into != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, into); err != nil {
			// The length and the status, never the payload: a malformed
			// /leases response is still a response full of secrets.
			return resp.StatusCode, fmt.Errorf("cluster-api %s: decode response (%d bytes, status %d): %w",
				op, len(payload), resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

// problemFrom decodes the contract's error shape. A body that is not a Problem
// is still a failure with an action: fatal, because a control plane answering
// 4xx in some other shape is not something a repeat will fix.
func problemFrom(op string, status int, payload []byte) error {
	var p clusterv1.Problem
	if err := json.Unmarshal(payload, &p); err != nil || p.Code == "" {
		return &clusterv1.Problem{
			Title:  fmt.Sprintf("%s: status %d with no problem document", op, status),
			Status: int32(status),
			Code:   clusterv1.CodeInternal,
			Action: clusterv1.ActionFatal,
		}
	}
	if p.Status == 0 {
		p.Status = int32(status)
	}
	if p.Action == "" {
		// An action the backend forgot to set is the one case where the
		// controller has to choose, and retry is the choice that loses least.
		p.Action = clusterv1.ActionRetry
	}
	return &p
}

// streamer is the client for the one endpoint whose body has no ceiling worth
// timing out on.
//
// It shares the transport — the connection pool, the TLS configuration, the
// proxy settings — and differs only in having no whole-request deadline. Two
// http.Clients over one Transport is the supported way to say that; a second
// Transport would be a second connection pool to the same backend.
func (c *Client) streamer() *http.Client {
	return &http.Client{Transport: c.http.Transport, CheckRedirect: c.http.CheckRedirect}
}

// authed mints the token for the current identity.
func (c *Client) authed() (string, error) {
	if c.signer == nil {
		return "", errors.New("clusterapi: no signer; register first")
	}
	token, err := c.signer.Token(c.now())
	if err != nil {
		return "", fmt.Errorf("cluster-api: mint token: %w", err)
	}
	return token, nil
}
