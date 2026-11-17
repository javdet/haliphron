package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Inputs: the files a person sends with a request, which the agent reads.
//
// The rules are here and nowhere else, so that the chat path and a future REST
// path deliver files by the same ones. The backend downloads, never the pod:
// the pod has no chat credential and runs untrusted code. The type is sniffed
// from the bytes, never believed from a name or a platform. A file that cannot
// be delivered is still named in the prompt, so the agent says it cannot see
// it rather than guessing what was in it. The caps are checked on the bytes
// actually read, never on what a platform declared.
//
// The bytes are stored under runs/{id}/inputs/ before the run is inserted, and
// the manifest is written in the insert. That puts the artifact store on the
// path to starting a run that has inputs, which nothing else is; a run without
// inputs is unchanged.

// The admission defaults. The contract's ceilings, which a deployment may raise
// these to, are clusterv1.MaxInputsPerLease and clusterv1.MaxInputBytesPerLease.
const (
	DefaultMaxInputFiles       = 10
	DefaultMaxInputFileBytes   = 10 << 20
	DefaultMaxInputBytesPerRun = 25 << 20
)

// inputOrphanAge is how long an admission may hold stored inputs without its
// run before the sweeper takes the run as never coming. Far past the chat
// handler's whole budget, so a slow admission is never swept from under itself.
const inputOrphanAge = time.Hour

// InputLimits are the caps in force.
type InputLimits struct {
	MaxFiles       int
	MaxFileBytes   int64
	MaxBytesPerRun int64
}

// InputLimits returns the caps in force, with the defaults filled in.
func (s *Service) InputLimits() InputLimits {
	l := InputLimits{
		MaxFiles:       s.limits.MaxInputFiles,
		MaxFileBytes:   s.limits.MaxInputFileBytes,
		MaxBytesPerRun: s.limits.MaxInputBytesPerRun,
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxInputFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxInputFileBytes
	}
	if l.MaxBytesPerRun <= 0 {
		l.MaxBytesPerRun = DefaultMaxInputBytesPerRun
	}
	l.MaxFiles = min(l.MaxFiles, clusterv1.MaxInputsPerLease)
	l.MaxBytesPerRun = min(l.MaxBytesPerRun, clusterv1.MaxInputBytesPerLease)
	l.MaxFileBytes = min(l.MaxFileBytes, l.MaxBytesPerRun)
	return l
}

// inputImageTypes are the binary types both CLIs can read. Text is accepted
// by family, below.
var inputImageTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// SniffInput decides what a file is from its first bytes and whether the agent
// can be given it. The name and the platform's declared type play no part:
// a screenshot.png that is an executable is an executable.
func SniffInput(body []byte) (mediaType string, readable bool) {
	mediaType = http.DetectContentType(body)
	base, _, _ := strings.Cut(mediaType, ";")
	base = strings.TrimSpace(base)
	return mediaType, inputImageTypes[base] || strings.HasPrefix(base, "text/")
}

// IsImageInput reports whether a sniffed type is a picture, which the prompt
// tells the agent to open with its file-reading tool.
func IsImageInput(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/")
}

// storeInputs checks a request's files against the caps, stores them under the
// run's prefix and returns the manifest InsertRun writes. Nothing is stored
// when the request breaks a rule: the refusal comes first.
//
// The chat path has already applied the same rules to decide what to say about
// each file, and is expected to hand over only files that pass. They are
// applied again here because this is admission, and admission is the one path
// into the system.
func (s *Service) storeInputs(ctx context.Context, id runv1.ULID, files []run.InputFile) ([]store.RunInput, error) {
	if len(files) == 0 {
		return nil, nil
	}
	limits := s.InputLimits()
	if len(files) > limits.MaxFiles {
		return nil, &run.InvalidRequestError{Field: "inputs",
			Detail: fmt.Sprintf("%d files were sent and at most %d are delivered", len(files), limits.MaxFiles)}
	}
	if s.artifacts == nil {
		return nil, errors.New("app: a run with input files needs the artifact store, and none is configured")
	}

	manifest := make([]store.RunInput, 0, len(files))
	var total int64
	for i, f := range files {
		size := int64(len(f.Body))
		total += size
		switch {
		case size > limits.MaxFileBytes:
			return nil, &run.InvalidRequestError{Field: "inputs",
				Detail: fmt.Sprintf("file %d is %d bytes and the limit is %d", i+1, size, limits.MaxFileBytes)}
		case total > limits.MaxBytesPerRun:
			return nil, &run.InvalidRequestError{Field: "inputs",
				Detail: fmt.Sprintf("the files come to more than %d bytes", limits.MaxBytesPerRun)}
		}
		mediaType, readable := SniffInput(f.Body)
		if !readable {
			return nil, &run.InvalidRequestError{Field: "inputs",
				Detail: fmt.Sprintf("file %d is %s, which is not a type the agent can read", i+1, mediaType)}
		}
		sum := sha256.Sum256(f.Body)
		key := runv1.InputFileName(i+1, f.Name)
		if _, err := runv1.InputKey(key); err != nil {
			// InputFileName always builds a key InputKey accepts; this is the
			// guard that keeps it so.
			return nil, fmt.Errorf("app: built an input key the contract refuses: %w", err)
		}
		manifest = append(manifest, store.RunInput{
			Ordinal:         int16(i + 1),
			Key:             key,
			Name:            f.Name,
			MediaType:       mediaType,
			SizeBytes:       size,
			SHA256:          sum[:],
			SourceMessageID: f.SourceMessageID,
		})
	}

	// The ledger row first: from here on a crash leaves objects the sweeper
	// can find without listing the store.
	if err := s.store.BeginInputUpload(ctx, id); err != nil {
		return nil, err
	}
	for i, in := range manifest {
		if err := s.artifacts.Put(ctx, artifacts.Key(id, in.Key), files[i].Body, in.MediaType); err != nil {
			s.discardInputs(ctx, id, manifest[:i+1])
			return nil, fmt.Errorf("app: store input %d of %s: %w", in.Ordinal, id, err)
		}
	}
	return manifest, nil
}

// discardInputs removes what an admission stored for a run that will not be
// inserted. Best effort: whatever it leaves, the sweeper finds through the
// ledger row, which is kept unless every object went.
func (s *Service) discardInputs(ctx context.Context, id runv1.ULID, stored []store.RunInput) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, in := range stored {
		if err := s.artifacts.Delete(ctx, artifacts.Key(id, in.Key)); err != nil &&
			!errors.Is(err, artifacts.ErrNotFound) {
			s.log.Warn("could not delete the input of a run that was not admitted",
				"run", id, "ordinal", in.Ordinal, "error", err)
			return
		}
	}
	if err := s.store.ForgetInputUpload(ctx, id); err != nil {
		s.log.Warn("could not close the input upload ledger", "run", id, "error", err)
	}
}

// LeaseInput opens one input of a leased run for its controller: the epoch
// fence of /ack, then the manifest, then the object.
//
// A key the manifest does not name and an object the store has lost are the
// same answer, InputNotFound with action fatal: no repeat produces either, and
// the controller refuses the lease rather than start a run without a file its
// agent will be told it has.
func (s *Service) LeaseInput(ctx context.Context, cluster store.Cluster, runID runv1.ULID,
	epoch int64, file string) (runv1.InputRef, io.ReadCloser, error) {

	if _, err := s.store.VerifyOwnership(ctx, runID, cluster.ID, epoch); err != nil {
		return runv1.InputRef{}, nil, s.problemFor(runID, err)
	}
	notFound := &clusterv1.Problem{
		Title: "no such input", Status: http.StatusNotFound,
		Code: clusterv1.CodeInputNotFound, Action: clusterv1.ActionFatal, RunID: runID,
	}
	key, err := runv1.InputKey(runv1.StoragePrefixInputs + file)
	if err != nil {
		return runv1.InputRef{}, nil, notFound
	}
	in, err := s.store.RunInput(ctx, runID, key)
	if errors.Is(err, store.ErrNotFound) {
		return runv1.InputRef{}, nil, notFound
	}
	if err != nil {
		return runv1.InputRef{}, nil, err
	}
	body, err := s.artifacts.Open(ctx, artifacts.Key(runID, in.Key))
	if errors.Is(err, artifacts.ErrNotFound) {
		s.log.Error("a leased run's input is gone from the artifact store",
			"run", runID, "ordinal", in.Ordinal)
		return runv1.InputRef{}, nil, notFound
	}
	if err != nil {
		return runv1.InputRef{}, nil, err
	}
	return in.Ref(), body, nil
}

// RunInputs is a run's manifest as the REST API lists it. ErrNotFound for a
// run that does not exist, so an unknown run is not an empty list.
func (s *Service) RunInputs(ctx context.Context, id runv1.ULID) ([]store.RunInput, error) {
	if _, err := s.store.RunByID(ctx, id); err != nil {
		return nil, err
	}
	return s.store.RunInputs(ctx, id)
}

// InputServedType is the Content-Type a run's input is served to a browser
// with. The sniffed type for the pictures and PDFs the agent reads, and plain
// text for the whole text family: an input sniffed as text/html or text/xml
// is the person's file, and served as what it sniffed as it would run in the
// UI's origin.
func InputServedType(mediaType string) string {
	base, _, _ := strings.Cut(mediaType, ";")
	if inputImageTypes[strings.TrimSpace(base)] {
		return strings.TrimSpace(base)
	}
	if strings.HasPrefix(strings.TrimSpace(base), "text/") {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// OpenRunInput streams one input of a run to a reader of the run, by its file
// name. ErrNotFound for a name the manifest does not have.
func (s *Service) OpenRunInput(ctx context.Context, id runv1.ULID, file string) (store.RunInput, io.ReadCloser, error) {
	key, err := runv1.InputKey(runv1.StoragePrefixInputs + file)
	if err != nil {
		return store.RunInput{}, nil, store.ErrNotFound
	}
	in, err := s.store.RunInput(ctx, id, key)
	if err != nil {
		return store.RunInput{}, nil, err
	}
	body, err := s.artifacts.Open(ctx, artifacts.Key(id, in.Key))
	if errors.Is(err, artifacts.ErrNotFound) {
		return store.RunInput{}, nil, store.ErrNotFound
	}
	if err != nil {
		return store.RunInput{}, nil, err
	}
	return in, body, nil
}

// SweepOrphanInputs deletes the inputs of admissions that stored files and
// never inserted their run, and returns how many prefixes it cleared.
func (s *Service) SweepOrphanInputs(ctx context.Context) (int, error) {
	if s.artifacts == nil {
		return 0, nil
	}
	ids, err := s.store.OrphanInputUploads(ctx, inputOrphanAge, 100)
	if err != nil {
		return 0, err
	}
	cleared := 0
	for _, id := range ids {
		prefix := artifacts.RunPrefix(id) + runv1.StoragePrefixInputs
		objects, err := s.artifacts.List(ctx, prefix)
		if err != nil {
			s.log.Warn("could not list orphaned inputs", "run", id, "error", err)
			continue
		}
		ok := true
		for _, obj := range objects {
			if err := s.artifacts.Delete(ctx, obj.Key); err != nil && !errors.Is(err, artifacts.ErrNotFound) {
				s.log.Warn("could not delete an orphaned input", "run", id, "error", err)
				ok = false
			}
		}
		if !ok {
			continue
		}
		if err := s.store.ForgetInputUpload(ctx, id); err != nil {
			return cleared, err
		}
		cleared++
	}
	return cleared, nil
}
