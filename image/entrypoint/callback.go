package entrypoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Callback is everything this pod says to its controller: the phases it gets
// through, the artifacts it produces in relay mode, and the report at the end.
//
// One client for all three because they share what matters — the base URL from
// the CR, the per-run bearer token, and the fact that none of them may ever be
// pointed at the control plane. The pod has no credential for the backend and
// must not have one; the controller is a Service twelve inches away that
// forwards what it is given.

// Callback posts to the controller's in-cluster Service.
type Callback struct {
	base  string
	token string
	// attempt is a property of the pod rather than of any one message, so it
	// is held here rather than threaded through every call.
	attempt int32

	// client bounds a short message. Artifacts do not use it: an upload of a
	// gigabyte over a link this pod does not choose is not a thing to put a
	// whole-request deadline on, and cutting one would report a storage
	// failure on a result that was arriving.
	client *http.Client
	stream *http.Client

	redactor *Redactor
	log      func(string, ...any)
}

// NewCallback builds the client. A pod with no callback URL is legal only in a
// test; in a cluster the controller refuses to materialise a lease without one,
// because the alternative is every run finishing as CompletedWithoutResult.
func NewCallback(cfg *Config, secrets *Secrets, redactor *Redactor, log func(string, ...any)) *Callback {
	if cfg.CallbackURL == "" {
		return nil
	}
	return &Callback{
		base:     strings.TrimSuffix(cfg.CallbackURL, "/"),
		token:    secrets.CallbackToken,
		attempt:  cfg.Attempt,
		client:   &http.Client{Timeout: 30 * time.Second},
		stream:   &http.Client{},
		redactor: redactor,
		log:      log,
	}
}

// URL is the base, for a log line.
func (c *Callback) URL() string { return c.base }

// Phase reports one entrypoint phase as it completes.
//
// This is what replaced writing runs/{id}/state.json, and the difference is not
// only where the checkpoint lives. The object was written when the pod chose to
// save it, so a pod killed between two saves recorded nothing; a report at the
// moment a phase completes is held by something that outlives the pod, so an
// OOM between two phases still leaves the one that finished on the record.
//
// A failure here is logged and swallowed. The cost of losing one report is that
// a later attempt redoes a phase it need not have — for the run phase, one
// extra model bill — and the cost of failing the run over it is the whole run.
// The two are not close.
func (c *Callback) Phase(ctx context.Context, report runv1.PhaseReport) {
	body, err := json.Marshal(report)
	if err != nil {
		c.log("could not encode the phase report for %s: %v", report.Phase, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+runv1.CallbackPathPhase, bytes.NewReader(body))
	if err != nil {
		c.log("could not build the phase report for %s: %v", report.Phase, err)
		return
	}
	c.authorise(req, "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.log("phase %s was not reported: %v", report.Phase, c.redactor.String(err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		c.log("phase %s was not recorded: the controller answered %d", report.Phase, resp.StatusCode)
	}
}

// Artifact uploads one object and waits for the acknowledgement.
//
// The wait is the point and it is the one place in this file where blocking is
// correct. The acknowledgement means the bytes are on a disk that is not this
// pod's, and until it arrives the only copy of what the run produced is in a
// container that is about to be deleted. That is principle P5, and it is what
// makes an object store optional rather than what an object store was for.
//
// The digest goes in a header and is verified on the far side, so a transfer
// that was cut is refused rather than stored: a half-written result.md under
// the right key is worse than none, because the recovery path would read it and
// believe it.
func (c *Callback) Artifact(ctx context.Context, key string, attempt int32,
	body []byte, contentType string) (*runv1.ObjectRef, error) {

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	q := url.Values{
		runv1.QueryKey:     {key},
		runv1.QueryAttempt: {strconv.Itoa(int(attempt))},
	}
	target := c.base + runv1.CallbackPathArtifacts + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "RelayUnreachable", err, "building the upload of %s", key)
	}
	c.authorise(req, contentType)
	req.Header.Set(runv1.HeaderSHA256, digest)
	req.ContentLength = int64(len(body))

	resp, err := c.stream.Do(req)
	if err != nil {
		// The controller is restarting, or the Service has no endpoints. Infra
		// and retryable: the next attempt finds it back, which is the whole
		// reason this class exists.
		return nil, failWrap(runv1.ExitStorage, "RelayUnreachable",
			errorf("%s", c.redactor.String(err.Error())), "uploading %s", key)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	switch {
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		// The run's artifact budget, reached. The one refusal the entrypoint
		// can act on: it drops the object and carries on rather than failing a
		// run over an attachment. Reported as an error so the caller decides —
		// the caller for result.md decides differently from the caller for a
		// log chunk.
		return nil, fail(runv1.ExitStorage, "ArtifactBudgetSpent",
			"the controller refused %s: %s", key, summarise(answer))
	case resp.StatusCode == http.StatusConflict:
		// This run is configured for object storage and should not be relaying
		// at all. A defect rather than a condition, and a retry repeats it.
		return nil, fail(runv1.ExitConfig, "ArtifactModeMismatch",
			"the controller says this run uploads to object storage: %s", summarise(answer))
	case resp.StatusCode >= 300:
		return nil, fail(runv1.ExitStorage, "RelayRefused",
			"the controller answered %d to %s: %s", resp.StatusCode, key, summarise(answer))
	}

	var ack runv1.ArtifactAck
	if err := json.Unmarshal(answer, &ack); err != nil {
		return nil, failWrap(runv1.ExitStorage, "RelayRefused", err,
			"decoding the acknowledgement for %s", key)
	}
	if ack.Ref.Key == "" {
		return nil, fail(runv1.ExitStorage, "RelayRefused",
			"the controller acknowledged %s without saying where it went", key)
	}
	return &ack.Ref, nil
}

// authorise sets the two headers every callback carries.
//
// The bearer token is the controller's rather than the backend's: the callback
// URL is cluster-local, and without a token any pod in the agent namespace
// could post a forged completion — or a forged phase report, which would let it
// mark another run's expensive phase as done and have that run's next attempt
// skip its model call.
func (c *Callback) authorise(req *http.Request, contentType string) {
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Haliphron-Contract", runv1.ContractVersion)
}

// summarise renders a refusal for a log line, bounded.
func summarise(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 256 {
		text = text[:256]
	}
	if text == "" {
		return "(no detail)"
	}
	return text
}

// Relay is the Uploader over the controller's Service.
//
// It is the default, and it is the thinner of the two implementations by some
// distance: there is no signature to mint, no expiry to compare against the
// run's budget, no bucket to address and no endpoint to resolve. That asymmetry
// is most of the argument for the mode — the failure modes that were specific to
// presigned access simply do not exist on this path.
type Relay struct {
	callback *Callback
	// redactor is applied to every byte on the way out, exactly as it is on the
	// presigned path. This is the last place a secret can be stopped from
	// becoming durable, and it is a property of the *port* rather than of
	// either implementation: a relay that skipped it would make "the agent
	// printed its environment" a thirty-day retention problem in the default
	// mode and not in the other.
	redactor *Redactor
}

// Mode is relay.
func (r *Relay) Mode() runv1.ArtifactMode { return runv1.ArtifactModeRelay }

// Put writes one of the layout's fixed keys.
func (r *Relay) Put(ctx context.Context, key string, body []byte, contentType string) (*runv1.ObjectRef, error) {
	return r.callback.Artifact(ctx, key, r.attempt(), r.redactor.Bytes(body), contentType)
}

// PostUnder writes a key named at runtime.
func (r *Relay) PostUnder(ctx context.Context, prefix, suffix string, body []byte, contentType string) (*runv1.ObjectRef, error) {
	return r.callback.Artifact(ctx, prefix+suffix, r.attempt(), r.redactor.Bytes(body), contentType)
}

// Describe is the startup line. No expiry, because nothing expires.
func (r *Relay) Describe() string {
	return fmt.Sprintf("relay through %s", r.callback.URL())
}

// attempt is carried on the client rather than passed through every call: it is
// a property of the pod, not of the object.
func (r *Relay) attempt() int32 { return r.callback.attempt }
