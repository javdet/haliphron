package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The callback's one GET: the files a request arrived with, which the
// controller fetched from the backend before it acknowledged the lease and the
// pod reads in the inputs phase.
//
// The rule worth faking faithfully is the one a permissive fake would drop: a
// key is served only when it is in *this run's* manifest. The allow-list alone
// is not enough — every run's keys look alike, "inputs/1-screenshot.png" — so a
// controller that checked only the shape would hand one run's screenshot to
// another run's pod. Here, as in the real controller, the token picks the run
// and the run's manifest picks what may be read.

// InputFile is one file a test gives a run.
type InputFile struct {
	// Key defaults to runv1.InputFileName(position, Name), as the backend
	// builds it. Set it to place a file under a key of the test's choosing;
	// Prepare refuses one runv1.InputKey refuses, as a controller refuses such
	// a lease. A test that wants the image to see a bad key writes the
	// manifest itself through RunRequest.Secrets.
	Key       string
	Name      string
	MediaType string
	Body      []byte
}

// inputFile is a served input: its manifest entry and its bytes.
type inputFile struct {
	ref  runv1.InputRef
	body []byte
}

// prepareInputs builds the manifest and the served set. The caller holds the
// lock.
func prepareInputs(files []InputFile) (map[string]*inputFile, string) {
	served := make(map[string]*inputFile, len(files))
	manifest := runv1.InputManifest{Inputs: make([]runv1.InputRef, 0, len(files))}
	for i, f := range files {
		key := f.Key
		if key == "" {
			key = runv1.InputFileName(i+1, f.Name)
		}
		if _, err := runv1.InputKey(key); err != nil {
			panic("controlplane: a controller would refuse a lease with this input: " + err.Error())
		}
		if _, dup := served[key]; dup {
			panic("controlplane: two inputs under " + key)
		}
		sum := sha256.Sum256(f.Body)
		ref := runv1.InputRef{
			Key:       key,
			Name:      f.Name,
			MediaType: f.MediaType,
			SizeBytes: int64(len(f.Body)),
			SHA256:    hex.EncodeToString(sum[:]),
		}
		manifest.Inputs = append(manifest.Inputs, ref)
		served[key] = &inputFile{ref: ref, body: append([]byte(nil), f.Body...)}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		panic("controlplane: the input manifest does not marshal: " + err.Error())
	}
	return served, string(body)
}

// handleInput serves GET /inputs.
//
// The answers are the route's, in the order a controller reaches them: 400 for
// a request that names no run or a key no input may have, 401 for a token that
// is missing, unknown or bound to a different run than the query names, 409
// for a run this cluster does not hold, 404 for a key that is not in the run's
// manifest, and the injected 503 for "temporarily unavailable".
func (c *ControlPlane) handleInput(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.URL.Query().Get(runv1.QueryRunID))
	key := r.URL.Query().Get(runv1.QueryKey)
	if runID == "" || key == "" {
		c.callbackError(w, http.StatusBadRequest, "MissingKey", "an input read must name its run and its key")
		return
	}
	if _, err := runv1.InputKey(key); err != nil {
		c.callbackError(w, http.StatusBadRequest, "InvalidKey", err.Error())
		return
	}

	c.mu.Lock()
	run, ok := c.authenticatedRun(w, r)
	if !ok {
		c.mu.Unlock()
		return
	}
	// 401 rather than the relay's 409: the route's contract says a token
	// bound to another run is a token that does not authenticate this read.
	if runID != run.id {
		c.mu.Unlock()
		c.callbackError(w, http.StatusUnauthorized, "RunMismatch",
			"the token is bound to "+string(run.id)+" and the read names "+string(runID))
		return
	}
	run.inputReads++
	in, listed := run.inputs[key]
	if !listed {
		c.mu.Unlock()
		// The key is never echoed into the fake's own log: it carries the
		// person's file name, and the real controller logs neither.
		c.logf("input read for run %s -> 404 not in its manifest", run.id)
		c.callbackError(w, http.StatusNotFound, "InputNotFound", "the key is not in this run's manifest")
		return
	}
	if status := c.inputFault(); status != 0 {
		c.mu.Unlock()
		c.logf("input read for run %s -> injected %d", run.id, status)
		c.callbackError(w, status, "Injected", "injected fault")
		return
	}
	body := in.body
	if c.inputCorruption() {
		// Same length, same digest header, one byte different: the transfer
		// the pod must refuse to write into the work tree.
		body = append([]byte(nil), in.body...)
		if len(body) > 0 {
			body[len(body)/2] ^= 0xff
		}
		c.logf("input read for run %s -> 200 with corrupted bytes", run.id)
	} else {
		c.logf("input read for run %s -> 200 (%d bytes)", run.id, len(body))
	}
	digest := in.ref.SHA256
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set(runv1.HeaderSHA256, digest)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// FailInputs makes GET /inputs answer status, after the token and the manifest
// have been checked, for the next count reads. A non-positive count means
// until cleared. 503 is the "temporarily unavailable" the pod retries.
func (c *ControlPlane) FailInputs(status, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if count <= 0 {
		count = forever
	}
	c.faults.inputStatus = status
	c.faults.inputRemaining = count
}

// CorruptInputs serves the next count reads with one byte flipped, under the
// manifest's size and digest — a spool that went bad, which the pod must catch.
// A non-positive count means until cleared.
func (c *ControlPlane) CorruptInputs(count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if count <= 0 {
		count = forever
	}
	c.faults.inputCorrupt = count
}

// ClearInputFaults restores the route.
func (c *ControlPlane) ClearInputFaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults.inputStatus = 0
	c.faults.inputRemaining = 0
	c.faults.inputCorrupt = 0
}

// inputFault decides whether this read is refused. The caller holds the lock.
func (c *ControlPlane) inputFault() int {
	if c.faults.inputStatus == 0 || c.faults.inputRemaining == 0 {
		return 0
	}
	if c.faults.inputRemaining > 0 {
		c.faults.inputRemaining--
	}
	return c.faults.inputStatus
}

// inputCorruption decides whether this read is served corrupted. The caller
// holds the lock.
func (c *ControlPlane) inputCorruption() bool {
	if c.faults.inputCorrupt == 0 {
		return false
	}
	if c.faults.inputCorrupt > 0 {
		c.faults.inputCorrupt--
	}
	return true
}

// InputReads counts the reads of a run's inputs that authenticated as that
// run, refused and faulted ones included: how a test sees that the pod retried
// a 503 and did not retry a 404.
func (c *ControlPlane) InputReads(id runv1.ULID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if run, ok := c.runs[id]; ok {
		return run.inputReads
	}
	return 0
}
