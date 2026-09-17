package clusterapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The seven endpoints. Each returns either a decoded response or an error that
// carries an Action; no method interprets a status code on the caller's behalf,
// with the single exception of the 204 on /leases, which is not a failure at
// all and is reported as a nil response.

// MaxClockSkew is how far the controller's clock may differ from the control
// plane's before registration logs an error. Beyond it, tokens start being
// refused under load and the symptom — occasional 401s — costs half a day of
// diagnostics if nobody said so at startup.
const MaxClockSkew = 30 * time.Second

// Register exchanges a one-time bootstrap token for a cluster identity. It is
// idempotent on (bootstrapToken, publicKey), which is what saves a controller
// that received a 200 and crashed before persisting the clusterID: it simply
// calls again with the same key.
func (c *Client) Register(ctx context.Context, bootstrapToken string, req clusterv1.RegisterRequest) (*clusterv1.RegisterResponse, error) {
	var out clusterv1.RegisterResponse
	if _, err := c.do(ctx, "register", "/register", bootstrapToken, req, &out); err != nil {
		return nil, err
	}
	c.checkClockSkew(out.ServerTime)
	c.log.Info("registered with the control plane",
		"clusterID", out.ClusterID, "name", out.Name, "keyID", out.KeyID,
		"heartbeatInterval", out.Timings.HeartbeatIntervalSeconds,
		"leaseTTL", out.Timings.LeaseTTLSeconds, "ackTimeout", out.Timings.AckTimeoutSeconds)
	return &out, nil
}

// checkClockSkew logs, and only logs. Refusing to start on skew would take a
// cluster offline over something an operator can fix in a minute, and the
// backend's 60 second tolerance means small drift is harmless.
func (c *Client) checkClockSkew(serverTime time.Time) {
	if serverTime.IsZero() {
		return
	}
	skew := c.now().Sub(serverTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		c.log.Error("clock skew against the control plane exceeds the tolerance; "+
			"expect authentication failures until it is corrected",
			"skew", skew.Round(time.Second), "serverTime", serverTime)
	}
}

// Lease is the long poll. A nil response with a nil error is the contract's 204:
// no work available, and the caller re-polls immediately and without backoff —
// backoff here would turn a long poll back into polling.
//
// The context must carry a deadline longer than req.WaitSeconds; the caller
// owns that arithmetic because only it knows what the backend clamped the wait
// to.
func (c *Client) Lease(ctx context.Context, req clusterv1.LeaseRequest) (*clusterv1.LeaseResponse, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/clusters/%s/leases", c.signer.ClusterID())
	var out clusterv1.LeaseResponse
	status, err := c.do(ctx, "leases", path, auth, req, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	// The count and nothing else. The body holds a git token, a model key and
	// signed URLs into somebody's bucket.
	if len(out.Leases) > 0 {
		c.log.Info("leases received", "count", len(out.Leases))
	}
	return &out, nil
}

// Ack reports that the lease became durable in the cluster: the Secret, the
// ConfigMap and the AgentRun exist, and the work now survives a controller
// restart. A negative ack says the opposite — nothing was created and nothing
// was spent — which before delta D16 could only be expressed by burning a run.
func (c *Client) Ack(ctx context.Context, runID runv1.ULID, req clusterv1.AckRequest) (*clusterv1.AckResponse, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	if req.ClusterID == "" {
		req.ClusterID = c.signer.ClusterID()
	}
	var out clusterv1.AckResponse
	if _, err := c.do(ctx, "ack", "/leases/"+string(runID)+"/ack", auth, req, &out); err != nil {
		return nil, err
	}
	c.log.Info("lease acknowledged",
		"runID", runID, "epoch", req.Epoch, "accepted", req.IsAccepted(), "status", out.Status)
	return &out, nil
}

// Artifacts mints a fresh presigned bundle for an active lease. Deliberately
// not idempotent: the whole point of the call is an expiry later than the one
// the caller already holds, and an expired signature surfaces as a lost result
// on work that actually succeeded.
func (c *Client) Artifacts(ctx context.Context, runID runv1.ULID, req clusterv1.ArtifactBundleRequest) (*clusterv1.ArtifactBundle, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	var out clusterv1.ArtifactBundle
	if _, err := c.do(ctx, "artifacts", "/leases/"+string(runID)+"/artifacts", auth, req, &out); err != nil {
		return nil, err
	}
	c.log.Info("artifact bundle reissued",
		"runID", runID, "epoch", req.Epoch, "attempt", req.Attempt, "expiresAt", out.ExpiresAt)
	return &out, nil
}

// Heartbeat proves the cluster is alive, renews every listed lease, reconciles
// state and collects commands, in one call because they are one interval.
func (c *Client) Heartbeat(ctx context.Context, req clusterv1.HeartbeatRequest) (*clusterv1.HeartbeatResponse, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/clusters/%s/heartbeat", c.signer.ClusterID())
	var out clusterv1.HeartbeatResponse
	if _, err := c.do(ctx, "heartbeat", path, auth, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IngestStatus is the low-latency path for a phase change. It carries the same
// observations the next heartbeat would have carried and obeys the same rules
// on the far side; losing one costs an interval, not a report.
func (c *Client) IngestStatus(ctx context.Context, req clusterv1.StatusIngestRequest) (*clusterv1.StatusIngestResponse, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	if req.ClusterID == "" {
		req.ClusterID = c.signer.ClusterID()
	}
	var out clusterv1.StatusIngestResponse
	if _, err := c.do(ctx, "ingest/status", "/ingest/status", auth, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IngestCompletion forwards the pod's report unchanged. The controller does not
// get to edit what the pod said: the same bytes are in storage as
// completion.json, and three copies that disagree would be worse than none.
func (c *Client) IngestCompletion(ctx context.Context, req clusterv1.CompletionIngestRequest) (*clusterv1.CompletionIngestResponse, error) {
	auth, err := c.authed()
	if err != nil {
		return nil, err
	}
	if req.ClusterID == "" {
		req.ClusterID = c.signer.ClusterID()
	}
	var out clusterv1.CompletionIngestResponse
	if _, err := c.do(ctx, "ingest/completion", "/ingest/completion", auth, req, &out); err != nil {
		return nil, err
	}
	c.log.Info("completion forwarded",
		"runID", req.RunID, "epoch", req.Epoch, "attempt", req.Attempt,
		"accepted", out.Accepted, "duplicate", out.Duplicate)
	return &out, nil
}
