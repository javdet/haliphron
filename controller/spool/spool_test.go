package spool

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

const testRun = runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8CD")

// The spool's whole purpose is the promise the acknowledgement makes: the bytes
// are on a disk that is not the pod's. Everything below is that promise stated
// as a property — it survives a restart, it is not released early, and it
// cannot be used to write outside the run it belongs to.

func TestWriteThenOpenRoundTrips(t *testing.T) {
	s := open(t)

	entry, err := s.Write(Entry{
		RunID: testRun, Epoch: 1, Attempt: 1,
		Key: runv1.StorageKeyResult, ContentType: "text/markdown",
	}, strings.NewReader("# done\n"), 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	switch {
	case entry.SizeBytes != 7:
		t.Errorf("sizeBytes = %d, want 7", entry.SizeBytes)
	case entry.SHA256 == "":
		t.Error("the entry carries no digest, so the backend cannot verify the relay")
	case entry.At.IsZero():
		t.Error("the entry has no timestamp, so Pending cannot order")
	}

	body, err := s.Open(entry)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "# done\n" {
		t.Errorf("read back %q", got)
	}
}

// A controller that restarted between acknowledging a pod's upload and
// forwarding it holds the only copy of that object. Clearing the directory on
// startup would break the promise the acknowledgement made, and the pod that
// trusted it is long gone.
func TestOpenReclaimsWhatAPreviousProcessLeft(t *testing.T) {
	root := t.TempDir()
	first, err := Open(Config{Root: root})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.Write(Entry{
		RunID: testRun, Epoch: 1, Attempt: 1, Key: runv1.StorageKeyResult,
	}, strings.NewReader("survive me"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, err := Open(Config{Root: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	pending, err := second.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("a restart left %d pending entries, want 1", len(pending))
	}
	got := pending[0]
	switch {
	case got.RunID != testRun:
		t.Errorf("runID = %q", got.RunID)
	case got.Key != runv1.StorageKeyResult:
		t.Errorf("key = %q", got.Key)
	case got.Epoch != 1 || got.Attempt != 1:
		t.Errorf("epoch/attempt = %d/%d", got.Epoch, got.Attempt)
	}

	// The ledger is rebuilt too. Starting it at zero would hand a run its whole
	// budget again on every restart, and the cap would stop being a cap.
	if second.Spent(testRun) != int64(len("survive me")) {
		t.Errorf("spent = %d after a restart, want the bytes on disk", second.Spent(testRun))
	}

	body, err := second.Open(got)
	if err != nil {
		t.Fatalf("open a reclaimed entry: %v", err)
	}
	defer func() { _ = body.Close() }()
	if raw, _ := io.ReadAll(body); string(raw) != "survive me" {
		t.Errorf("reclaimed %q", raw)
	}
}

// Oldest first, because the forwarder should drain in the order the pod
// produced things: a reader watching a run's chunks arrive out of order is a
// reader seeing a log that did not happen.
func TestPendingIsOldestFirst(t *testing.T) {
	now := time.Now()
	s, err := Open(Config{Root: t.TempDir(), Clock: func() time.Time { now = now.Add(time.Second); return now }})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for _, key := range []string{"logs/chunks/000001.log", "logs/chunks/000002.log", runv1.StorageKeyResult} {
		if _, err := s.Write(Entry{RunID: testRun, Epoch: 1, Attempt: 1, Key: key},
			strings.NewReader("x"), 0); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
	}

	pending, err := s.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	var keys []string
	for _, e := range pending {
		keys = append(keys, e.Key)
	}
	want := "logs/chunks/000001.log,logs/chunks/000002.log,result.md"
	if strings.Join(keys, ",") != want {
		t.Errorf("pending order %v, want %s", keys, want)
	}
}

// Done is what the forwarder calls after the backend has acknowledged. Calling
// it before would throw away the only copy of a result a pod was told was safe.
func TestDoneReleasesTheEntryAndTheBudget(t *testing.T) {
	s := open(t)
	entry, err := s.Write(Entry{RunID: testRun, Epoch: 1, Attempt: 1, Key: runv1.StorageKeyResult},
		strings.NewReader("done"), 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.Spent(testRun) != 4 {
		t.Fatalf("spent = %d before forwarding", s.Spent(testRun))
	}

	if err := s.Done(entry); err != nil {
		t.Fatalf("done: %v", err)
	}
	pending, _ := s.Pending()
	if len(pending) != 0 {
		t.Errorf("%d entries survived being forwarded", len(pending))
	}
	if s.Spent(testRun) != 0 {
		t.Errorf("spent = %d after forwarding, want 0", s.Spent(testRun))
	}
	// The sidecar goes with it, or the next Pending re-reads an entry whose
	// object is gone on every sweep.
	if left, _ := filepath.Glob(filepath.Join(s.Root(), string(testRun), "*")); len(left) != 0 {
		t.Errorf("the run's directory still holds %v", left)
	}
}

// Forget is called on abandon: the run belongs to another cluster now, and
// forwarding our copy would overwrite theirs under the same key.
func TestForgetDiscardsARunsSpool(t *testing.T) {
	s := open(t)
	for _, id := range []runv1.ULID{testRun, "01J8X4K2ZQ7YB3M9F0R5W6T8CE"} {
		if _, err := s.Write(Entry{RunID: id, Epoch: 1, Attempt: 1, Key: runv1.StorageKeyResult},
			strings.NewReader("x"), 0); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := s.Forget(testRun); err != nil {
		t.Fatalf("forget: %v", err)
	}
	pending, _ := s.Pending()
	if len(pending) != 1 || pending[0].RunID == testRun {
		t.Errorf("after forgetting one run, pending is %+v", pending)
	}
	if s.Spent(testRun) != 0 {
		t.Error("the forgotten run still holds budget")
	}
}

// The budget is enforced one hop from the pod so the transfer is not paid for
// twice. The second check — after the copy — is for a body that claims a
// kilobyte in its Content-Length and delivers a gigabyte.
func TestWriteRefusesBeyondTheBudget(t *testing.T) {
	s := open(t)

	if _, err := s.Write(Entry{RunID: testRun, Epoch: 1, Attempt: 1, Key: "logs/chunks/000001.log"},
		strings.NewReader(strings.Repeat("x", 60)), 100); err != nil {
		t.Fatalf("a write inside the budget failed: %v", err)
	}

	_, err := s.Write(Entry{RunID: testRun, Epoch: 1, Attempt: 1, Key: "logs/chunks/000002.log"},
		strings.NewReader(strings.Repeat("x", 60)), 100)
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a write past the budget gave %v, want ErrBudgetSpent", err)
	}
	// And nothing of it was kept: a partial object under a real key is worse
	// than none.
	if s.Spent(testRun) != 60 {
		t.Errorf("spent = %d after a refused write, want 60", s.Spent(testRun))
	}
	pending, _ := s.Pending()
	if len(pending) != 1 {
		t.Errorf("a refused write left %d entries", len(pending))
	}
}

// A truncated relay is refused rather than stored: a half-written result.md
// under the right key is worse than none, because the recovery path would read
// it and believe it.
func TestWriteRefusesABodyThatDoesNotMatchItsDigest(t *testing.T) {
	s := open(t)
	_, err := s.Write(Entry{
		RunID: testRun, Epoch: 1, Attempt: 1, Key: runv1.StorageKeyResult,
		SHA256: strings.Repeat("a", 64),
	}, strings.NewReader("not what the digest says"), 0)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("a mismatched body was spooled anyway: %+v", pending)
	}
}

// The key comes from a pod, which is the least trusted component in the system.
// It is checked here, and again by the backend, and again by the disk store.
func TestWriteRefusesKeysThatEscapeTheRun(t *testing.T) {
	s := open(t)
	for _, key := range []string{
		"../../etc/passwd",
		"../other-run/result.md",
		"/absolute.md",
		"result.md.meta",
		"",
	} {
		if _, err := s.Write(Entry{RunID: testRun, Epoch: 1, Attempt: 1, Key: key},
			strings.NewReader("x"), 0); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
	// A run identifier that is not one cannot be a path either.
	if _, err := s.Write(Entry{RunID: "../..", Epoch: 1, Attempt: 1, Key: "result.md"},
		strings.NewReader("x"), 0); err == nil {
		t.Error("a run identifier that is a path was accepted")
	}

	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatalf("read the spool: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("refused writes left %d entries in the spool root", len(entries))
	}
}

func open(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("open the spool: %v", err)
	}
	return s
}
