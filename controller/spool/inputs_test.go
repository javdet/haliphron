package spool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The inputs area holds what a person sent, fetched before the lease was
// acknowledged. It must hold exactly those bytes, keep out of the artifact
// path, and refuse any key that is not an input key.

func inputRef(key string, body []byte) runv1.InputRef {
	sum := sha256.Sum256(body)
	return runv1.InputRef{Key: key, Name: "screenshot.png", MediaType: "image/png",
		SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func TestAnInputIsSpooledOnlyWhenItsSizeAndDigestMatchTheManifest(t *testing.T) {
	s := open(t)
	body := []byte("\x89PNG not really")
	ref := inputRef("inputs/1-screenshot.png", body)

	cases := []struct {
		name string
		body []byte
	}{
		{"short", body[:4]},
		{"long", append(append([]byte(nil), body...), 'x')},
		{"same length, other bytes", bytes.ToUpper(body)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.WriteInput(testRun, ref, bytes.NewReader(tc.body))
			if !errors.Is(err, ErrInputMismatch) {
				t.Fatalf("want ErrInputMismatch, got %v", err)
			}
			if s.HasInput(testRun, ref) {
				t.Fatal("a body that does not match the manifest was left in the spool")
			}
			if strings.Contains(err.Error(), "screenshot") {
				t.Fatalf("the error quotes the input's name: %v", err)
			}
		})
	}

	if err := s.WriteInput(testRun, ref, bytes.NewReader(body)); err != nil {
		t.Fatalf("the right bytes were refused: %v", err)
	}
	f, size, err := s.OpenInput(testRun, ref.Key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	got, _ := io.ReadAll(f)
	if size != int64(len(body)) || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes %q", size, got)
	}
}

func TestASpooledInputIsReusedWhenIntactAndDroppedWhenCorrupt(t *testing.T) {
	s := open(t)
	body := []byte("a log line\n")
	ref := inputRef("inputs/2-build.log", body)

	if ok, err := s.InputMatches(testRun, ref); err != nil || ok {
		t.Fatalf("an input never written matched: %t %v", ok, err)
	}
	if err := s.WriteInput(testRun, ref, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.InputMatches(testRun, ref); err != nil || !ok {
		t.Fatalf("an intact input did not match: %t %v", ok, err)
	}

	// Same size, different bytes: what a disk error or a hand edit leaves.
	path := filepath.Join(s.Root(), inputsDir, string(testRun), "2-build.log")
	if err := os.WriteFile(path, []byte("A LOG LINE\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !s.HasInput(testRun, ref) {
		t.Fatal("HasInput is a size check and should still see the file")
	}
	if ok, err := s.InputMatches(testRun, ref); err != nil || ok {
		t.Fatalf("a corrupt input matched: %t %v", ok, err)
	}
	if s.HasInput(testRun, ref) {
		t.Fatal("a corrupt input was left where the callback would serve it")
	}
}

func TestAnInputKeyOutsideTheLayoutIsRefusedBeforeTheFilesystem(t *testing.T) {
	s := open(t)
	for _, key := range []string{
		"inputs/../result.md", "result.md", "inputs/1-a/b", "inputs/.hidden", "inputs/1-x.meta",
		"../../etc/passwd", "inputs/1-..",
	} {
		ref := inputRef(key, []byte("x"))
		if err := s.WriteInput(testRun, ref, strings.NewReader("x")); err == nil {
			t.Errorf("%q was written", key)
		}
		if _, _, err := s.OpenInput(testRun, key); err == nil {
			t.Errorf("%q was opened", key)
		}
	}
	if err := s.WriteInput("not-a-ulid", inputRef("inputs/1-a.png", []byte("x")), strings.NewReader("x")); err == nil {
		t.Error("an input was written for a run that is not a run identifier")
	}
}

func TestInputsAreNotPendingAndDoNotSpendTheArtifactBudget(t *testing.T) {
	root := t.TempDir()
	s, err := Open(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("x"), 1000)
	if err := s.WriteInput(testRun, inputRef("inputs/1-big.txt", body), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("an input is waiting to be forwarded: %v %v", pending, err)
	}
	if s.Spent(testRun) != 0 {
		t.Fatalf("an input spent %d bytes of the artifact budget", s.Spent(testRun))
	}

	// And a restart rebuilds the ledger without counting it either.
	again, err := Open(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.Spent(testRun) != 0 {
		t.Fatalf("after a restart an input spent %d bytes of the budget", again.Spent(testRun))
	}
}

func TestForgettingARunDropsItsInputsToo(t *testing.T) {
	s := open(t)
	body := []byte("x")
	ref := inputRef("inputs/1-a.txt", body)
	if err := s.WriteInput(testRun, ref, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(testRun); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenInput(testRun, ref.Key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run's input survived Forget: %v", err)
	}
}

func TestTheSweepDropsOnlyOldInputsOfRunsNoLongerHeld(t *testing.T) {
	now := time.Now()
	clock := now
	s, err := Open(Config{Root: t.TempDir(), Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	const held, gone, fresh = runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8C1"),
		runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8C2"), runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8C3")
	body := []byte("x")
	ref := inputRef("inputs/1-a.txt", body)

	clock = now.Add(-2 * time.Hour)
	for _, id := range []runv1.ULID{held, gone} {
		if err := s.WriteInput(id, ref, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	if err := s.WriteInput(fresh, ref, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.SweepInputs(func(id runv1.ULID) bool { return id == held }, now.Add(-time.Hour))
	if err != nil || dropped != 1 {
		t.Fatalf("dropped %d, %v; want exactly the old run nobody holds", dropped, err)
	}
	for id, want := range map[runv1.ULID]bool{held: true, gone: false, fresh: true} {
		if got := s.HasInput(id, ref); got != want {
			t.Errorf("%s: present=%t, want %t", id, got, want)
		}
	}
}
