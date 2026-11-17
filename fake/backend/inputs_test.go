package backend_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
)

// The inputs endpoint: what a controller fetches before it acknowledges. The
// fake must refuse what the real backend refuses — a stale epoch, a file the
// manifest does not name, an object that is gone — and with the same actions,
// or a controller built against it learns to start runs it should refuse.

var attached = backend.InputFile{Name: "the screenshot.png", Body: []byte("\x89PNG\r\n\x1a\nbytes")}

func (c *client) getInput(runID runv1.ULID, epoch int64, file string) response {
	c.t.Helper()
	target := c.baseURL + clusterv1.BasePath + "/leases/" + string(runID) + "/inputs/" + url.PathEscape(file) +
		"?" + url.Values{clusterv1.QueryEpoch: {strconv.FormatInt(epoch, 10)}}.Encode()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(clusterv1.HeaderControllerVersion, c.version)
	req.Header.Set("Authorization", "Bearer "+c.token())
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("GET input: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: out, headers: resp.Header}
}

func TestALeaseCarriesItsInputsAndTheirBytesAreServedUnderItsEpoch(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec(), backend.WithInputs(attached))
	lease := c.mustLease()
	if len(lease.Inputs) != 1 {
		t.Fatalf("the lease carries %d inputs", len(lease.Inputs))
	}
	ref := lease.Inputs[0]
	sum := sha256.Sum256(attached.Body)
	if ref.Key != "inputs/1-the_screenshot.png" || ref.Name != attached.Name ||
		ref.SizeBytes != int64(len(attached.Body)) || ref.SHA256 != hex.EncodeToString(sum[:]) ||
		ref.MediaType != "image/png" {
		t.Fatalf("the manifest entry: %+v", ref)
	}

	resp := c.getInput(id, lease.Epoch, ref.File())
	if resp.status != http.StatusOK || string(resp.body) != string(attached.Body) {
		t.Fatalf("status %d body %q", resp.status, resp.body)
	}
	if resp.headers.Get("Cache-Control") != "no-store" ||
		resp.headers.Get(clusterv1.HeaderArtifactSHA256) != ref.SHA256 ||
		resp.headers.Get("Content-Length") != strconv.Itoa(len(attached.Body)) {
		t.Fatalf("headers %v", resp.headers)
	}
	if b.InputFetches(id, ref.Key) != 1 {
		t.Fatal("the fetch was not counted")
	}
}

func TestAnInputReadUnderAStaleEpochIsAnAbandon(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec(), backend.WithInputs(attached))
	lease := c.mustLease()
	b.Reassign(id)

	resp := c.getInput(id, lease.Epoch, lease.Inputs[0].File())
	if resp.status != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeEpochMismatch || p.Action != clusterv1.ActionAbandon {
		t.Fatalf("want EpochMismatch/abandon, got %s/%s", p.Code, p.Action)
	}
}

func TestAnInputTheRunDoesNotHaveIsNotFoundAndFatal(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec(), backend.WithInputs(attached))
	lease := c.mustLease()

	for name, file := range map[string]string{
		"a file the manifest does not name": "2-other.png",
		"a name outside the layout":         "..%2Fresult.md",
	} {
		t.Run(name, func(t *testing.T) {
			resp := c.getInput(id, lease.Epoch, file)
			if resp.status != http.StatusNotFound {
				t.Fatalf("want 404, got %d body %s", resp.status, resp.body)
			}
			if p := resp.problem(t); p.Code != clusterv1.CodeInputNotFound || p.Action != clusterv1.ActionFatal {
				t.Fatalf("want InputNotFound/fatal, got %s/%s", p.Code, p.Action)
			}
		})
	}

	// In the manifest, gone from the store: the same answer.
	b.DropInput(id, lease.Inputs[0].Key)
	resp := c.getInput(id, lease.Epoch, lease.Inputs[0].File())
	if p := resp.problem(t); resp.status != http.StatusNotFound || p.Code != clusterv1.CodeInputNotFound ||
		p.Action != clusterv1.ActionFatal {
		t.Fatalf("a dropped object: %d %s/%s", resp.status, p.Code, p.Action)
	}
}

func TestAnInputIsServedOnlyToTheClusterHoldingTheRun(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	other := join(t, b, c, "kid-b", "cluster-b")
	id := b.Enqueue(sampleSpec(), backend.WithInputs(attached))
	lease := c.mustLease()

	resp := other.getInput(id, lease.Epoch, lease.Inputs[0].File())
	if resp.status != http.StatusForbidden {
		t.Fatalf("want 403, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeRunLeasedByAnotherCluster || p.Action != clusterv1.ActionAbandon {
		t.Fatalf("want RunLeasedByAnotherCluster/abandon, got %s/%s", p.Code, p.Action)
	}
}
