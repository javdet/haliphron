package entrypoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The inputs phase: the files a person sent with the request, read from the
// controller into DirInputs so the agent can open them.
//
// From the controller and from nowhere else. The controller fetched every file
// from the backend and checked it before it acknowledged the lease, so this
// phase never waits on the control plane and works while it is down; and the
// pod is given no credential for the chat server or the store, which is the
// whole of why the files make this detour.
//
// Everything about a file except its count and size is customer data — the
// name a person gave a screenshot is as much theirs as its pixels — and the
// run log is uploaded. So the log says "2 inputs, 3.1 MiB" and failures name an
// input by its position in the manifest, never by its key or its name.

// fetchedInput is one input as later phases see it: where it is and what it
// sniffed as at admission.
type fetchedInput struct {
	Path      string
	MediaType string
	SizeBytes int64
}

// inputTries is how many times one file is asked for before the phase gives
// up, and inputBackoff the pauses between them. Short: the controller is a
// Service in the same cluster, and what this rides out is its restart or a
// spool that is momentarily busy, not a WAN.
var inputBackoff = []time.Duration{250 * time.Millisecond, time.Second, 2 * time.Second}

// inputTryTimeout bounds one transfer. Generous against the 10 MiB per-file cap
// over a cluster network; what it stops is a transfer that stalls forever, in
// a phase that has no deadline of its own.
const inputTryTimeout = 2 * time.Minute

// phaseInputs fetches the manifest's files into DirInputs, each verified
// against its size and digest and left read-only.
//
// A failure is ExitStorage, infra and retriable: the controller held every file
// when it acknowledged, so a transfer that failed is one that can succeed. A
// malformed manifest never gets here — LoadSecrets refused it as config.
func phaseInputs(ctx context.Context, r *Run) error {
	switch {
	case r.secrets.Inputs == nil:
		return skip("this run has no inputs")
	case len(r.secrets.Inputs) == 0:
		return skip("the input manifest lists no files")
	case r.resumed:
		// The model is not called again, so nothing would open the files.
		// Fetching them anyway would only add a way for a run whose result is
		// already durable to fail.
		return skip("resuming: the agent will not run again and needs no inputs")
	case r.callback == nil:
		return fail(runv1.ExitConfig, "MissingConfiguration",
			"the run has inputs and no callback URL to read them from")
	}

	if err := os.MkdirAll(r.layout.Inputs, 0o755); err != nil {
		return failWrap(runv1.ExitStorage, "InputUnwritable", err, "creating %s", r.layout.Inputs)
	}

	total := len(r.secrets.Inputs)
	fetched := make([]fetchedInput, 0, total)
	var bytes int64
	for i, ref := range r.secrets.Inputs {
		path, err := r.fetchInput(ctx, i+1, total, ref)
		if err != nil {
			return err
		}
		fetched = append(fetched, fetchedInput{Path: path, MediaType: ref.MediaType, SizeBytes: ref.SizeBytes})
		bytes += ref.SizeBytes
	}
	r.inputs = fetched

	images := len(r.imageInputs())
	r.logf("inputs: %d files, %d bytes, verified, in %s (%d images)", total, bytes, r.layout.Inputs, images)
	r.warnIfInputsUnreadable()
	return nil
}

// fetchInput reads one file, retrying what is worth retrying, and returns where
// it was written. ordinal and total name the input in messages.
func (r *Run) fetchInput(ctx context.Context, ordinal, total int, ref runv1.InputRef) (string, error) {
	var last error
	for try := 0; ; try++ {
		path, retry, err := r.fetchInputOnce(ctx, ordinal, total, ref)
		if err == nil {
			return path, nil
		}
		last = err
		if !retry || try >= len(inputBackoff) {
			return "", last
		}
		r.logf("input %d of %d: try %d failed, retrying in %s", ordinal, total, try+1, inputBackoff[try])
		select {
		case <-ctx.Done():
			return "", failWrap(runv1.ExitStorage, "InputUnavailable", ctx.Err(),
				"input %d of %d was not read before the run was stopped", ordinal, total)
		case <-time.After(inputBackoff[try]):
		}
	}
}

// fetchInputOnce is one try. retry says whether another could succeed: a
// transport failure, a cut body or a 5xx could; a refusal the controller would
// repeat, or bytes that are not the bytes the manifest names, could not.
func (r *Run) fetchInputOnce(ctx context.Context, ordinal, total int, ref runv1.InputRef) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, inputTryTimeout)
	defer cancel()

	resp, err := r.callback.Input(ctx, ref.Key)
	if err != nil {
		return "", true, failWrap(runv1.ExitStorage, "InputUnavailable", err,
			"input %d of %d: the controller could not be reached", ordinal, total)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", true, fail(runv1.ExitStorage, "InputUnavailable",
			"input %d of %d: the controller answered %d", ordinal, total, resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		// The key passed this image's allow-list and the token is the run's
		// own, so the controller disagreeing is not something another try
		// fixes here. Still infra: a fresh pod and a fresh look at the CR is
		// what the controller's retry gives, and it is cheap.
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", false, fail(runv1.ExitStorage, "InputRefused",
			"input %d of %d: the controller answered %d: %s", ordinal, total, resp.StatusCode, summarise(answer))
	default:
		// 404, the key is not in the copy of the manifest the controller
		// holds; 409, the controller holds no such run. Either way it does
		// not have the file now and will not have it a second from now.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", false, fail(runv1.ExitStorage, "InputUnavailable",
			"input %d of %d: the controller answered %d", ordinal, total, resp.StatusCode)
	}

	if got := resp.Header.Get(runv1.HeaderSHA256); got != "" && !strings.EqualFold(got, ref.SHA256) {
		return "", false, fail(runv1.ExitStorage, "InputDigestMismatch",
			"input %d of %d: the controller vouches for a different digest than the manifest", ordinal, total)
	}

	tmp, err := os.CreateTemp(r.layout.Inputs, ".incoming-*")
	if err != nil {
		return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
			"input %d of %d: creating a file in %s", ordinal, total, r.layout.Inputs)
	}
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	// One byte past the declared size is read, so that a body longer than the
	// manifest says is caught without reading all of it.
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, ref.SizeBytes+1))
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
				"input %d of %d: writing it into %s", ordinal, total, r.layout.Inputs)
		}
		// The connection went mid-body. Worth another try.
		return "", true, failWrap(runv1.ExitStorage, "InputUnavailable",
			errorf("%s", r.redactor.String(err.Error())),
			"input %d of %d: the transfer was cut after %d bytes", ordinal, total, n)
	}
	if n != ref.SizeBytes {
		return "", false, fail(runv1.ExitStorage, "InputDigestMismatch",
			"input %d of %d: read %d bytes and the manifest says %d", ordinal, total, n, ref.SizeBytes)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != ref.SHA256 {
		return "", false, fail(runv1.ExitStorage, "InputDigestMismatch",
			"input %d of %d: the bytes read are not the bytes the manifest names", ordinal, total)
	}

	if err := tmp.Close(); err != nil {
		return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
			"input %d of %d: closing it", ordinal, total)
	}
	// Read-only to the agent. What the person sent is evidence the agent is
	// asked to look at, not a scratch file it may edit.
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
			"input %d of %d: making it read-only", ordinal, total)
	}
	// An earlier attempt in the same emptyDir may have left the file, read-only.
	// The directory is writable, so it can be removed; rename would replace it
	// anyway on Linux, and saying so explicitly costs one call.
	path := filepath.Join(r.layout.Inputs, ref.File())
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
			"input %d of %d: replacing an earlier copy", ordinal, total)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", false, failWrap(runv1.ExitStorage, "InputUnwritable", err,
			"input %d of %d: moving it into place", ordinal, total)
	}
	keep = true
	return path, false, nil
}

// imageInputs are the fetched inputs that sniffed as an image, for a CLI that
// takes images as attachments rather than as files to open.
func (r *Run) imageInputs() []string {
	var out []string
	for _, in := range r.inputs {
		if strings.HasPrefix(in.MediaType, "image/") {
			out = append(out, in.Path)
		}
	}
	return out
}

// warnIfInputsUnreadable says so when the agent may have no way to open what
// it was sent.
//
// claude-code opens images and PDFs with its Read tool, and a role's deny list
// can take that away: "Read" outright, or "Read(...)" for some paths, which
// may or may not cover these. The run goes ahead — refusing it would be
// deciding for the role — and the log says why the answer may not mention the
// screenshot. codex is given its images on the command line and reads the rest
// through its shell, so there is no single tool to lose. opencode is given its
// images too, but reads a PDF or a text file with its read tool, and the deny
// list is translated onto that tool.
func (r *Run) warnIfInputsUnreadable() {
	if r.cfg.Agent == runv1.AgentCodex {
		return
	}
	for _, denied := range r.cfg.DeniedTools {
		if denied == "Read" || strings.HasPrefix(denied, "Read(") {
			r.logf("warning: the role denies %s; the agent may not be able to open the %d attached files",
				denied, len(r.inputs))
			return
		}
	}
}
