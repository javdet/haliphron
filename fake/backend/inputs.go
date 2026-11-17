package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Inputs: the files a person attached to a request, which the controller
// fetches before it acknowledges. The fake keeps the bytes in memory beside
// the manifest and serves them on GET /leases/{runID}/inputs/{file} with the
// rules the real endpoint has — the epoch fence of /ack, and InputNotFound,
// action fatal, for a file the manifest does not name or whose object is gone.

// InputFile is one file a test attaches to a run.
type InputFile struct {
	// Name is what the person called it. The key is built from it the way the
	// real backend builds it, with runv1.InputFileName.
	Name string
	Body []byte
}

// WithInputs attaches files to the run. Each becomes an entry of the lease's
// Inputs — key, name, the sniffed media type, size and digest — and its bytes
// are served on the inputs endpoint.
func WithInputs(files ...InputFile) RunOption {
	return func(r *run) {
		r.inputs = nil
		r.inputBytes = map[string][]byte{}
		r.inputFetches = map[string]int{}
		for i, f := range files {
			key := runv1.InputFileName(i+1, f.Name)
			sum := sha256.Sum256(f.Body)
			r.inputs = append(r.inputs, runv1.InputRef{
				Key:       key,
				Name:      f.Name,
				MediaType: http.DetectContentType(f.Body),
				SizeBytes: int64(len(f.Body)),
				SHA256:    hex.EncodeToString(sum[:]),
			})
			r.inputBytes[key] = append([]byte(nil), f.Body...)
		}
	}
}

// Inputs is the manifest a run's leases carry.
func (b *Backend) Inputs(id runv1.ULID) []runv1.InputRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runs[id]; ok {
		return append([]runv1.InputRef(nil), r.inputs...)
	}
	return nil
}

// DropInput removes an input's object while its manifest entry stays: the
// state after retention or an operator deleted it from the store. The
// endpoint then answers InputNotFound, action fatal.
func (b *Backend) DropInput(id runv1.ULID, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runs[id]; ok {
		delete(r.inputBytes, key)
	}
}

// CorruptInput replaces an input's stored bytes and leaves the manifest as it
// was, so that what is served no longer matches the digest the lease names.
func (b *Backend) CorruptInput(id runv1.ULID, key string, body []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runs[id]; ok && r.inputBytes != nil {
		r.inputBytes[key] = append([]byte(nil), body...)
	}
}

// InputFetches is how many times an input was served, successfully or not.
// The property worth asserting is that a controller does not fetch twice what
// it already holds.
func (b *Backend) InputFetches(id runv1.ULID, key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runs[id]; ok {
		return r.inputFetches[key]
	}
	return 0
}

// handleInput serves one input of a leased run. The order of checks is the
// real backend's: the epoch first, under the run's current ownership, then
// the manifest, then the object.
func (b *Backend) handleInput(w http.ResponseWriter, r *http.Request) {
	runID := runv1.ULID(r.PathValue("runID"))
	epoch, err := strconv.ParseInt(r.URL.Query().Get(clusterv1.QueryEpoch), 10, 64)
	if err != nil || epoch < 1 {
		b.badRequest(w, "an input is read under the run's current epoch")
		return
	}
	c, ok := b.authenticate(w, r, "")
	if !ok {
		return
	}

	b.mu.Lock()
	b.sweep()
	run, ok := b.runs[runID]
	if !ok {
		b.mu.Unlock()
		b.runNotFound(w, runID)
		return
	}
	if epoch != run.epoch {
		// The work moved on: abandon. An ack for this lease would be refused
		// for the same reason, so the controller sends none.
		b.epochProblem(w, run, epoch)
		b.mu.Unlock()
		return
	}
	if run.holder != c.id {
		b.mu.Unlock()
		b.writeProblem(w, http.StatusForbidden, clusterv1.Problem{
			Title: "run is leased by another cluster", Code: clusterv1.CodeRunLeasedByAnotherCluster,
			Action: clusterv1.ActionAbandon, RunID: runID, CurrentEpoch: run.epoch,
		})
		return
	}

	notFound := clusterv1.Problem{
		Title: "no such input", Code: clusterv1.CodeInputNotFound,
		Action: clusterv1.ActionFatal, RunID: runID,
	}
	key, err := runv1.InputKey(runv1.StoragePrefixInputs + r.PathValue("file"))
	if err != nil {
		b.mu.Unlock()
		b.writeProblem(w, http.StatusNotFound, notFound)
		return
	}
	if run.inputFetches == nil {
		run.inputFetches = map[string]int{}
	}
	run.inputFetches[key]++
	var ref runv1.InputRef
	found := false
	for _, in := range run.inputs {
		if in.Key == key {
			ref, found = in, true
			break
		}
	}
	body, stored := run.inputBytes[key]
	b.mu.Unlock()

	if !found || !stored {
		b.writeProblem(w, http.StatusNotFound, notFound)
		return
	}
	// Identifiers only: the key is built from the name a person gave the file.
	b.mu.Lock()
	b.logf("input served run=%s epoch=%d bytes=%d", runID, epoch, len(body))
	b.mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set(clusterv1.HeaderArtifactSHA256, ref.SHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
