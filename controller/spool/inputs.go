package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The inputs area: files a person sent with a request, fetched from the
// backend before the lease was acknowledged and read back by the run's pod
// over the callback.
//
// It is the opposite traffic to the rest of the spool — inbound and read, not
// outbound and forwarded — and is kept apart for that reason: under
// <root>/_inputs/<runID>/<file>, invisible to Pending, with no envelope and no
// place in the artifact ledger. A run's inputs do not spend its artifact
// budget; the lease's own ceilings bound them before a byte is fetched.
//
// The leading underscore is what keeps the area out of any run's way: a run's
// directory is named by its ULID, whose alphabet has no underscore.
//
// Nothing here logs or returns an input's key in an error. The key is built
// from the name a person gave the file, and a log line saying which file did
// not verify is a log line quoting them.

const inputsDir = "_inputs"

// ErrInputMismatch is a fetched or spooled input whose size or digest is not
// what the lease's manifest says. Refused rather than kept: the agent is told
// the file is the one the person sent, and a truncated screenshot under that
// name is worse than none.
var ErrInputMismatch = errors.New("spool: the input does not match its manifest entry")

// inputRunDir is the directory holding one run's inputs.
func (s *Spool) inputRunDir(id runv1.ULID) (string, error) {
	// runDir is the ULID check; the artifact directory itself is not used.
	if _, err := s.runDir(id); err != nil {
		return "", err
	}
	return filepath.Join(s.root, inputsDir, string(id)), nil
}

// inputPath resolves an input's key under the run's inputs directory. The key
// is checked with the one allow-list inputs have and refused, never repaired:
// a key the backend should never have built is a defect to report, and a
// cleaned-up path would be a file the pod is not told about.
func (s *Spool) inputPath(id runv1.ULID, key string) (string, error) {
	dir, err := s.inputRunDir(id)
	if err != nil {
		return "", err
	}
	if _, err := runv1.InputKey(key); err != nil {
		return "", fmt.Errorf("spool: an input key was refused: %w", err)
	}
	return filepath.Join(dir, runv1.InputRef{Key: key}.File()), nil
}

// WriteInput spools one input atomically: a temporary file, synced, checked
// against ref's exact size and digest, then renamed into place. A body that
// is short, long or different leaves nothing behind and returns
// ErrInputMismatch.
func (s *Spool) WriteInput(id runv1.ULID, ref runv1.InputRef, body io.Reader) error {
	path, err := s.inputPath(id, ref.Key)
	if err != nil {
		return err
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("%w: a negative size", ErrInputMismatch)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, s.perm|0o111); err != nil {
		return fmt.Errorf("spool: create the inputs directory of %s: %w", id, err)
	}

	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return fmt.Errorf("spool: open a temporary file for an input of %s: %w", id, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// One byte past the stated size, so that a body that keeps going is told
	// apart from one that stops exactly where the manifest says.
	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, ref.SizeBytes+1))
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spool: write an input of %s: %w", id, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spool: sync an input of %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("spool: close an input of %s: %w", id, err)
	}
	if written != ref.SizeBytes {
		return fmt.Errorf("%w: %d bytes arrived and the manifest says %d", ErrInputMismatch, written, ref.SizeBytes)
	}
	if digest := hex.EncodeToString(sum.Sum(nil)); digest != ref.SHA256 {
		return fmt.Errorf("%w: the bytes hash to %s and the manifest says %s", ErrInputMismatch, digest, ref.SHA256)
	}
	if err := os.Chmod(tmpName, s.perm); err != nil {
		return fmt.Errorf("spool: chmod an input of %s: %w", id, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("spool: publish an input of %s: %w", id, err)
	}
	s.touchInputs(id)
	return nil
}

// InputMatches reports whether the spool already holds ref's bytes: the file
// exists, has exactly ref's size and hashes to ref's digest. It is what lets a
// controller that restarted between fetching and acknowledging skip the
// transfer, while one whose copy is corrupt or cut short fetches it again.
//
// A missing file is false with no error; a file that does not match is false
// too, and is removed so that nothing can serve it in the meantime.
func (s *Spool) InputMatches(id runv1.ULID, ref runv1.InputRef) (bool, error) {
	path, err := s.inputPath(id, ref.Key)
	if err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("spool: open an input of %s: %w", id, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("spool: stat an input of %s: %w", id, err)
	}
	if info.Size() != ref.SizeBytes {
		_ = os.Remove(path)
		return false, nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return false, fmt.Errorf("spool: read an input of %s: %w", id, err)
	}
	if hex.EncodeToString(sum.Sum(nil)) != ref.SHA256 {
		_ = os.Remove(path)
		return false, nil
	}
	s.touchInputs(id)
	return true, nil
}

// HasInput is the cheap half of InputMatches: the file is there with the
// right size. It answers "did anything remove it since it was verified", not
// "is it intact".
func (s *Spool) HasInput(id runv1.ULID, ref runv1.InputRef) bool {
	path, err := s.inputPath(id, ref.Key)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == ref.SizeBytes
}

// OpenInput opens one spooled input for reading, with its size. The caller
// closes it. A missing input is an error wrapping fs.ErrNotExist.
func (s *Spool) OpenInput(id runv1.ULID, key string) (*os.File, int64, error) {
	path, err := s.inputPath(id, key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("spool: open an input of %s: %w", id, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("spool: stat an input of %s: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("spool: an input of %s is not a regular file: %w", id, fs.ErrNotExist)
	}
	return f, info.Size(), nil
}

// ForgetInputs drops every input spooled for a run.
func (s *Spool) ForgetInputs(id runv1.ULID) error {
	dir, err := s.inputRunDir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: discard the inputs of %s: %w", id, err)
	}
	return nil
}

// SweepInputs drops the inputs of runs this controller no longer holds: those
// keep reports false for and whose directory has not been written or verified
// since before. It returns how many runs' inputs it dropped.
//
// It is the net under the explicit paths, for a lease whose inputs were
// fetched and that was then never materialised — the API server refused the
// write, the backend took the work elsewhere. The age is what keeps it off a
// lease being handled right now, whose inputs exist before its run does.
func (s *Spool) SweepInputs(keep func(runv1.ULID) bool, before time.Time) (int, error) {
	area := filepath.Join(s.root, inputsDir)
	entries, err := os.ReadDir(area)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("spool: list the inputs area: %w", err)
	}
	dropped := 0
	for _, entry := range entries {
		id := runv1.ULID(entry.Name())
		if _, err := s.runDir(id); err != nil || !entry.IsDir() {
			// Not something this package wrote. Removed: the area is ours.
			_ = os.RemoveAll(filepath.Join(area, entry.Name()))
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(before) || keep(id) {
			continue
		}
		if err := s.ForgetInputs(id); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

// touchInputs stamps the run's inputs directory as just verified, which is
// what SweepInputs measures age by. Best effort: a missed stamp makes a sweep
// earlier, and the lease loop checks the inputs are still there before it
// acknowledges.
func (s *Spool) touchInputs(id runv1.ULID) {
	dir, err := s.inputRunDir(id)
	if err != nil {
		return
	}
	now := s.clock()
	_ = os.Chtimes(dir, now, now)
}
