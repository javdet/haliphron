package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Inputs: a lease's attached files, fetched before the lease is acknowledged.
//
// The rules are the controller's obligations under the Cluster API contract,
// implemented here a second time and independently, so that a backend is
// tested against a peer that reads the contract for itself:
//
//   - a manifest past the ceilings, with a key outside the allow-list, a key
//     twice, or a digest that is not one, is refused with InvalidSpec;
//   - an input the backend answers with a fatal action (InputNotFound), or
//     whose bytes do not match the manifest, is refused with
//     MaterializationFailed;
//   - anything else — abandon, retry, backoff, a transport failure — means no
//     acknowledgement at all, and the backend reassigns after the deadline.
//
// The fetched bytes are kept in memory, standing in for the spool. Notes name
// runs and counts, never an input's name or key.

// fetchOutcome is what fetchInputs decided.
type fetchOutcome int

const (
	fetchDone   fetchOutcome = iota // every input is held and verified
	fetchRefuse                     // send a negative ack with the rejection
	fetchSkip                       // send nothing; the lease expires
)

func (c *Controller) fetchInputs(ctx context.Context, lease clusterv1.Lease) (fetchOutcome, *clusterv1.AckRejection) {
	if rejection := validateInputs(lease.Inputs); rejection != nil {
		return fetchRefuse, rejection
	}
	for i, ref := range lease.Inputs {
		n := i + 1
		c.mu.Lock()
		held, ok := c.inputs[lease.RunID][ref.Key]
		c.mu.Unlock()
		if ok && digestOf(held) == ref.SHA256 && int64(len(held)) == ref.SizeBytes {
			continue
		}

		body, problem, err := c.getInput(ctx, lease, ref)
		switch {
		case err != nil:
			c.note("input %d of run=%s not fetched: transport error", n, lease.RunID)
			return fetchSkip, nil
		case problem != nil && problem.Action == clusterv1.ActionFatal:
			return fetchRefuse, &clusterv1.AckRejection{
				Code:    clusterv1.RejectMaterializationFailed,
				Message: fmt.Sprintf("input %d of %d could not be fetched: %s", n, len(lease.Inputs), problem.Code),
				Fields:  []string{"inputs"},
			}
		case problem != nil:
			c.note("input %d of run=%s not fetched: %s, action %s", n, lease.RunID, problem.Code, problem.Action)
			return fetchSkip, nil
		}
		if int64(len(body)) != ref.SizeBytes || digestOf(body) != ref.SHA256 {
			return fetchRefuse, &clusterv1.AckRejection{
				Code:    clusterv1.RejectMaterializationFailed,
				Message: fmt.Sprintf("input %d of %d does not match the size and digest the lease names", n, len(lease.Inputs)),
				Fields:  []string{"inputs"},
			}
		}
		c.mu.Lock()
		if c.inputs[lease.RunID] == nil {
			c.inputs[lease.RunID] = map[string][]byte{}
		}
		c.inputs[lease.RunID][ref.Key] = body
		c.mu.Unlock()
	}
	return fetchDone, nil
}

// getInput is one GET. Like call, it returns the decoded Problem rather than
// an opaque error, so that the action decides.
func (c *Controller) getInput(ctx context.Context, lease clusterv1.Lease, ref runv1.InputRef) ([]byte, *clusterv1.Problem, error) {
	c.mu.Lock()
	auth := c.authed()
	c.mu.Unlock()
	file := runv1.InputRef{Key: ref.Key}.File()
	target := c.baseURL + clusterv1.BasePath + "/leases/" + url.PathEscape(string(lease.RunID)) +
		"/inputs/" + url.PathEscape(file) + "?" + url.Values{
		clusterv1.QueryEpoch: {strconv.FormatInt(lease.Epoch, 10)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set(clusterv1.HeaderControllerVersion, c.version)
	req.Header.Set("Authorization", "Bearer "+auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var p clusterv1.Problem
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, clusterv1.MaxRequestBytes))
		if err := json.Unmarshal(raw, &p); err != nil || p.Code == "" {
			return nil, nil, fmt.Errorf("inputs: status %d, unparsable body", resp.StatusCode)
		}
		return nil, &p, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ref.SizeBytes+1))
	if err != nil {
		return nil, nil, err
	}
	return body, nil, nil
}

func validateInputs(in []runv1.InputRef) *clusterv1.AckRejection {
	refuse := func(msg string) *clusterv1.AckRejection {
		return &clusterv1.AckRejection{Code: clusterv1.RejectInvalidSpec, Message: msg, Fields: []string{"inputs"}}
	}
	if len(in) > clusterv1.MaxInputsPerLease {
		return refuse(fmt.Sprintf("%d inputs, more than %d", len(in), clusterv1.MaxInputsPerLease))
	}
	seen := map[string]bool{}
	var total int64
	for i, ref := range in {
		if _, err := runv1.InputKey(ref.Key); err != nil {
			return refuse(fmt.Sprintf("input %d has a key outside the contract's layout", i+1))
		}
		if seen[ref.Key] {
			return refuse(fmt.Sprintf("input %d repeats a key", i+1))
		}
		seen[ref.Key] = true
		if len(ref.SHA256) != 64 {
			return refuse(fmt.Sprintf("input %d carries no SHA-256 digest", i+1))
		}
		if _, err := hex.DecodeString(ref.SHA256); err != nil || ref.SHA256 != lowerHex(ref.SHA256) {
			return refuse(fmt.Sprintf("input %d carries no lowercase hex SHA-256 digest", i+1))
		}
		if ref.SizeBytes < 0 {
			return refuse(fmt.Sprintf("input %d has a negative size", i+1))
		}
		total += ref.SizeBytes
		if total > clusterv1.MaxInputBytesPerLease {
			return refuse("the inputs come to more than the contract's ceiling")
		}
	}
	return nil
}

func lowerHex(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'F' {
			out[i] = c + 32
		}
	}
	return string(out)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Input is what this controller fetched for a run under one key, for a test to
// assert on. It stands in for the spool the pod reads from.
func (c *Controller) Input(id runv1.ULID, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.inputs[id][key]
	return append([]byte(nil), b...), ok
}
