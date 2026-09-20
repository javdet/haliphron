// Package spool is the controller's copy of what a pod handed it.
//
// It exists because of principle P5, and because of the one reading of P5 that
// the previous design got wrong. The rule is that a paid-for result must not
// exist only in the filesystem of a pod about to be deleted. It was read as "so
// it must be in an object store", and it is not: it must be somewhere that
// outlives the pod. The controller's own volume is somewhere that outlives the
// pod, and it is already in the cluster.
//
// So in relay mode the pod POSTs an artifact here, this package writes it and
// only then is the upload acknowledged. From that moment the pod may exit: the
// controller owns delivery onward, and it survives both the pod's deletion and
// a backend outage. That is the whole of what makes object storage optional.
//
// What this is not is a second system of record. Nothing reads a spooled object
// for its contents; the controller forwards it and deletes it. A spool that
// grows without bound is a disk that fills, so the budget is enforced on the
// way in and the sweep on the way out is not optional.
package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ErrBudgetSpent is the per-run artifact cap, reached.
//
// It is refused here rather than by the backend so that the transfer is not
// paid for twice: the controller is one hop from the pod and the backend is
// across whatever network separates the cluster from the control plane.
var ErrBudgetSpent = errors.New("spool: this run has spent its artifact budget")

// ErrDigestMismatch is a body that does not hash to what the pod said.
//
// Refused rather than kept, and the reason is the same one the backend has for
// the same check: a half-written result.md under the right key is worse than
// none, because the CompletedWithoutResult recovery path would read it and
// believe it.
var ErrDigestMismatch = errors.New("spool: the body does not match its digest")

// Entry is one spooled object, waiting to be forwarded.
type Entry struct {
	RunID   runv1.ULID
	Epoch   int64
	Attempt int32
	// Key is relative to the run's prefix — "result.md", "logs/chunks/7.log".
	// The prefix itself is never stored, because the pod never gets to choose
	// it: the controller stamps it from the CR it authenticated against, and
	// the backend stamps it again from the envelope it authenticated.
	Key         string
	ContentType string
	SHA256      string
	SizeBytes   int64
	At          time.Time

	// path is where the bytes are. Unexported: a caller that could name a file
	// here could name a file anywhere, and the only thing anyone needs from an
	// entry is to open it.
	path string
}

// Spool is a directory of pending artifacts.
type Spool struct {
	root  string
	perm  os.FileMode
	clock func() time.Time

	// mu guards the byte ledger only. The filesystem is not guarded: keys are
	// unique per run and a pod writes one at a time, and two writers for one
	// key would be two attempts of one run running at once — which the
	// duplicate guard in the database exists to make impossible.
	mu    sync.Mutex
	spent map[runv1.ULID]int64
}

// Config is what a Spool needs.
type Config struct {
	// Root is the controller's own volume. An emptyDir is a legitimate choice
	// and a PVC is the better one: with an emptyDir a controller restart loses
	// whatever had not been forwarded yet, which turns an acknowledged upload
	// into a lost result — exactly the failure the acknowledgement promises
	// will not happen.
	Root     string
	FileMode os.FileMode
	Clock    func() time.Time
}

// Open prepares the spool and reclaims what a previous process left behind.
//
// Reclaiming rather than clearing is the point of the directory existing. A
// controller that restarted between acknowledging a pod's upload and forwarding
// it holds the only copy of that object; deleting it on startup would break the
// promise the acknowledgement made, and the pod that trusted it is long gone.
func Open(cfg Config) (*Spool, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("spool: the artifact spool needs a root path")
	}
	perm := cfg.FileMode
	if perm == 0 {
		perm = 0o640
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("spool: resolve %s: %w", cfg.Root, err)
	}
	if err := os.MkdirAll(root, perm|0o111); err != nil {
		return nil, fmt.Errorf("spool: create %s: %w", root, err)
	}

	probe := filepath.Join(root, ".haliphron-writable")
	if err := os.WriteFile(probe, []byte("ok"), perm); err != nil {
		return nil, fmt.Errorf("spool: %s is not writable; "+
			"relay mode needs a writable volume for the controller: %w", root, err)
	}
	_ = os.Remove(probe)

	s := &Spool{root: root, perm: perm, clock: clock, spent: map[runv1.ULID]int64{}}

	// The ledger is rebuilt from what is on disk rather than started at zero.
	// Otherwise a controller that restarts mid-run hands the run its whole
	// budget again, and the cap stops being a cap.
	pending, err := s.Pending()
	if err != nil {
		return nil, err
	}
	for _, e := range pending {
		s.spent[e.RunID] += e.SizeBytes
	}
	return s, nil
}

// Write spools one object and returns what landed.
//
// budget is the run's remaining allowance as the lease stated it; zero or less
// means unbounded. The check happens before the copy and again after, because
// the pod sends a Content-Length it is not obliged to honour: a body that
// claims a kilobyte and delivers a gigabyte is the case the second check is
// for.
func (s *Spool) Write(e Entry, body io.Reader, budget int64) (Entry, error) {
	if budget > 0 && s.Spent(e.RunID) >= budget {
		return Entry{}, fmt.Errorf("%w: %d bytes already spooled of %d",
			ErrBudgetSpent, s.Spent(e.RunID), budget)
	}

	path, err := s.pathFor(e.RunID, e.Key)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), s.perm|0o111); err != nil {
		return Entry{}, fmt.Errorf("spool: create the directory for %s: %w", e.Key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return Entry{}, fmt.Errorf("spool: open a temporary file for %s: %w", e.Key, err)
	}
	tmpName := tmp.Name()
	// Removed unless the rename below claims it. A spool that keeps its
	// partials accumulates one per interrupted upload, and the sweep cannot
	// tell them from objects it has not forwarded yet.
	defer func() { _ = os.Remove(tmpName) }()

	limit := budget - s.Spent(e.RunID)
	reader := body
	if budget > 0 {
		// One extra byte, so that reaching exactly the limit is distinguishable
		// from exceeding it.
		reader = io.LimitReader(body, limit+1)
	}

	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, sum), reader)
	if err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("spool: write %s: %w", e.Key, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("spool: sync %s: %w", e.Key, err)
	}
	if err := tmp.Close(); err != nil {
		return Entry{}, fmt.Errorf("spool: close %s: %w", e.Key, err)
	}

	if budget > 0 && written > limit {
		return Entry{}, fmt.Errorf("%w: this object alone exceeds the remaining %d bytes",
			ErrBudgetSpent, limit)
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	if e.SHA256 != "" && !strings.EqualFold(e.SHA256, digest) {
		return Entry{}, fmt.Errorf("%w: %s hashes to %s and the pod said %s",
			ErrDigestMismatch, e.Key, digest, e.SHA256)
	}

	if err := os.Chmod(tmpName, s.perm); err != nil {
		return Entry{}, fmt.Errorf("spool: chmod %s: %w", e.Key, err)
	}
	// Atomic within one filesystem, which is what makes an interrupted upload
	// invisible to the forwarder rather than half of an object it sends on.
	if err := os.Rename(tmpName, path); err != nil {
		return Entry{}, fmt.Errorf("spool: publish %s: %w", e.Key, err)
	}

	out := e
	out.SizeBytes = written
	out.SHA256 = digest
	out.At = s.clock()
	out.path = path

	if err := s.writeMeta(out); err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	s.spent[e.RunID] += written
	s.mu.Unlock()
	return out, nil
}

// Open reads a spooled object back. The caller closes it.
func (s *Spool) Open(e Entry) (io.ReadCloser, error) {
	path := e.path
	if path == "" {
		var err error
		if path, err = s.pathFor(e.RunID, e.Key); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spool: open %s: %w", e.Key, err)
	}
	return f, nil
}

// Done drops one forwarded object, and the run's directory once it is empty.
//
// Called only after the backend has acknowledged. That ordering is the relay's
// at-least-once guarantee: a controller that deleted first and failed to
// forward would have thrown away the only copy of a result the pod was told was
// safe.
func (s *Spool) Done(e Entry) error {
	path, err := s.pathFor(e.RunID, e.Key)
	if err != nil {
		return err
	}
	size := e.SizeBytes
	if size == 0 {
		if info, statErr := os.Stat(path); statErr == nil {
			size = info.Size()
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: delete %s: %w", e.Key, err)
	}
	_ = os.Remove(path + metaSuffix)

	s.mu.Lock()
	if remaining := s.spent[e.RunID] - size; remaining > 0 {
		s.spent[e.RunID] = remaining
	} else {
		delete(s.spent, e.RunID)
	}
	s.mu.Unlock()

	s.pruneDirs(filepath.Dir(path))
	return nil
}

// Forget drops everything spooled for a run.
//
// Called when the run is abandoned — it belongs to another cluster now, and the
// copy that cluster's pod produced is the one that counts — and when the CR is
// collected. Without it the spool keeps the artifacts of every run the
// controller ever lost.
func (s *Spool) Forget(id runv1.ULID) error {
	dir, err := s.runDir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: discard the spool of %s: %w", id, err)
	}
	s.mu.Lock()
	delete(s.spent, id)
	s.mu.Unlock()
	return nil
}

// Pending is everything waiting to be forwarded, oldest first.
//
// Oldest first because the forwarder should drain in the order the pod produced
// things: a log chunk that overtakes the result it belongs to is harmless, and
// a reader watching a run's chunks arrive out of order is not.
func (s *Spool) Pending() ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return filepath.SkipAll
		case err != nil:
			return err
		case entry.IsDir() || !strings.HasSuffix(path, metaSuffix):
			return nil
		}
		e, readErr := s.readMeta(path)
		if readErr != nil {
			// A metadata file that does not parse is a partial write from a
			// process that died mid-save. Its object cannot be forwarded —
			// nobody knows which run it belongs to — so it is dropped rather
			// than left to be re-read on every sweep.
			_ = os.Remove(path)
			_ = os.Remove(strings.TrimSuffix(path, metaSuffix))
			return nil
		}
		if _, statErr := os.Stat(e.path); statErr != nil {
			_ = os.Remove(path)
			return nil
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("spool: list what is pending: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Key < out[j].Key
		}
		return out[i].At.Before(out[j].At)
	})
	return out, nil
}

// Spent is how many bytes a run has spooled and not yet had forwarded.
func (s *Spool) Spent(id runv1.ULID) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent[id]
}

// Root is the directory, for a log line at startup.
func (s *Spool) Root() string { return s.root }

// metaSuffix names the sidecar holding an entry's envelope.
//
// A sidecar rather than a header inside the object, because the object is
// forwarded byte for byte: the backend verifies the same digest the pod sent,
// and a controller that prefixed its own bookkeeping onto the bytes would break
// that comparison for the sake of saving a file.
const metaSuffix = ".meta"

func (s *Spool) writeMeta(e Entry) error {
	// A fixed line format rather than JSON: the fields are five scalars, the
	// reader is ten lines below, and a dependency-free format keeps the one
	// file that has to survive a crash trivially inspectable with cat.
	body := fmt.Sprintf("%s\n%d\n%d\n%s\n%s\n%s\n%d\n%s\n",
		e.RunID, e.Epoch, e.Attempt, e.Key, e.ContentType, e.SHA256,
		e.SizeBytes, e.At.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(e.path+metaSuffix, []byte(body), s.perm); err != nil {
		return fmt.Errorf("spool: record the envelope of %s: %w", e.Key, err)
	}
	return nil
}

func (s *Spool) readMeta(path string) (Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 8 {
		return Entry{}, fmt.Errorf("spool: %s is truncated", path)
	}
	e := Entry{
		RunID:       runv1.ULID(lines[0]),
		Key:         lines[3],
		ContentType: lines[4],
		SHA256:      lines[5],
		path:        strings.TrimSuffix(path, metaSuffix),
	}
	if _, err := fmt.Sscanf(lines[1], "%d", &e.Epoch); err != nil {
		return Entry{}, err
	}
	if _, err := fmt.Sscanf(lines[2], "%d", &e.Attempt); err != nil {
		return Entry{}, err
	}
	if _, err := fmt.Sscanf(lines[6], "%d", &e.SizeBytes); err != nil {
		return Entry{}, err
	}
	if e.At, err = time.Parse(time.RFC3339Nano, lines[7]); err != nil {
		return Entry{}, err
	}
	if e.RunID == "" || e.Key == "" {
		return Entry{}, fmt.Errorf("spool: %s names no run or key", path)
	}
	return e, nil
}

func (s *Spool) runDir(id runv1.ULID) (string, error) {
	if id == "" {
		return "", errors.New("spool: an entry must name its run")
	}
	// The identifier's own alphabet is the check: a ULID is 26 characters of
	// Crockford base32, so a value that passes contains no separator and no
	// dot, and cannot be a path.
	for _, c := range string(id) {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return "", fmt.Errorf("spool: %q is not a run identifier", id)
		}
	}
	return filepath.Join(s.root, string(id)), nil
}

// pathFor resolves a run's key under the spool and refuses anything that
// escapes it.
//
// The key comes from a pod, which is the least trusted component in the system.
// It is checked here, and again by the backend, and again by the disk store —
// three places, because a path traversal at any one of them writes into a
// filesystem that is not the run's.
func (s *Spool) pathFor(id runv1.ULID, key string) (string, error) {
	dir, err := s.runDir(id)
	if err != nil {
		return "", err
	}
	switch {
	case key == "":
		return "", errors.New("spool: the empty key names no object")
	case strings.HasSuffix(key, metaSuffix):
		// Otherwise an object named "x.meta" is indistinguishable from the
		// sidecar of an object named "x", and the sweep would forward one and
		// delete the other.
		return "", fmt.Errorf("spool: %q is a reserved name", key)
	case strings.HasPrefix(key, "/"):
		return "", fmt.Errorf("spool: key %q is absolute; keys are relative to the run", key)
	case key != path.Clean(key):
		return "", fmt.Errorf("spool: key %q is not normalised", key)
	case key == ".." || strings.HasPrefix(key, "../") || strings.Contains(key, "/../"):
		return "", fmt.Errorf("spool: key %q escapes the run's spool", key)
	}

	// Refused rather than sanitised, and the difference matters. Rooting the key
	// and calling filepath.Clean — the obvious implementation — turns "../x"
	// into "/x" and writes it at the top of the run's directory: nothing
	// escapes, and the object has silently left the key the backend will be
	// told about. A key that is not what it should be is a defect upstream, and
	// the useful answer is to say so.
	full := filepath.Join(dir, filepath.FromSlash(key))
	if !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("spool: key %q escapes the run's spool", key)
	}
	return full, nil
}

// pruneDirs removes directories left empty, up to the run's own.
func (s *Spool) pruneDirs(dir string) {
	for dir != s.root && strings.HasPrefix(dir, s.root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
