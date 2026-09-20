package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The disk store is the default half of the port, so it carries the properties
// an installation gets without configuring anything. The three worth pinning
// are the ones whose absence is invisible until it costs a result: a key cannot
// escape the volume, a half-written object cannot be read, and a listing over a
// prefix behaves the way the object store's does.

func TestDiskRoundTripsAnObject(t *testing.T) {
	store := newDisk(t)
	ctx := context.Background()
	key := Key("01J0000000000000000000000X", runv1.StorageKeyResult)

	ref, err := store.PutStream(ctx, key, strings.NewReader("# done\n"), "text/markdown")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	switch {
	case ref.Key != key:
		t.Errorf("ref key = %q, want %q", ref.Key, key)
	case ref.SizeBytes != 7:
		t.Errorf("sizeBytes = %d, want 7", ref.SizeBytes)
	case !ref.Uploaded:
		t.Error("a ref the store just wrote is not marked uploaded")
	case ref.SHA256 == "":
		t.Error("the ref carries no digest")
	}

	body, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(body) != "# done\n" {
		t.Errorf("read back %q", body)
	}

	if _, err := store.Get(ctx, Key("01J0000000000000000000000X", "absent.md")); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing object gave %v, want ErrNotFound — the recovery path has to tell "+
			"'the pod never wrote it' from 'the store is broken'", err)
	}
}

// A key that escapes the volume writes into the control plane's filesystem
// rather than into one run's prefix. The keys reaching this store come from a
// pod by way of a controller, and both of those stamp the prefix themselves;
// this is the third place the same rule is enforced, which is the point.
func TestDiskRefusesKeysThatEscapeTheVolume(t *testing.T) {
	store := newDisk(t)
	ctx := context.Background()

	for _, key := range []string{
		"../escaped.md",
		"runs/01J/../../escaped.md",
		"/etc/passwd",
		"",
	} {
		if _, err := store.PutStream(ctx, key, strings.NewReader("x"), ""); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}

	// And nothing landed outside.
	parent := filepath.Dir(store.Root())
	if _, err := os.Stat(filepath.Join(parent, "escaped.md")); err == nil {
		t.Fatal("a refused key still wrote outside the volume")
	}
}

// A half-written result.md under the right key is worse than none: the
// CompletedWithoutResult recovery reads it and believes it. The store writes to
// a temporary name and renames, so a reader sees the whole object or nothing.
func TestDiskLeavesNoPartialObjectReadable(t *testing.T) {
	store := newDisk(t)
	ctx := context.Background()
	key := Key("01J0000000000000000000000X", runv1.StorageKeyResult)

	_, err := store.PutStream(ctx, key, failingReader{after: 4}, "text/markdown")
	if err == nil {
		t.Fatal("a reader that fails mid-copy produced no error")
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("the interrupted object is readable: %v", err)
	}

	// Nor did the partial file survive under its temporary name, where the
	// listing would have to learn to skip it.
	objects, err := store.List(ctx, "runs/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("the volume holds %v after an interrupted write", objects)
	}
}

// The log endpoint pages over a prefix, and it is the same call over a bucket
// and over a directory. Sorted, because chunk names sort in the order they were
// written and the cursor is the last key of the previous page.
func TestDiskListsAPrefixInKeyOrder(t *testing.T) {
	store := newDisk(t)
	ctx := context.Background()
	id := runv1.ULID("01J0000000000000000000000X")

	for _, name := range []string{"000002.log", "000010.log", "000001.log"} {
		if err := store.Put(ctx, Key(id, runv1.StoragePrefixChunks+name), []byte("x"), "text/plain"); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	// Another run, which must not appear in the listing.
	other := runv1.ULID("01J0000000000000000000000Y")
	if err := store.Put(ctx, Key(other, runv1.StorageKeyResult), []byte("x"), ""); err != nil {
		t.Fatalf("put other: %v", err)
	}

	objects, err := store.List(ctx, RunPrefix(id)+runv1.StoragePrefixChunks)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, strings.TrimPrefix(o.Key, RunPrefix(id)+runv1.StoragePrefixChunks))
	}
	want := []string{"000001.log", "000002.log", "000010.log"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("listed %v, want %v", keys, want)
	}

	// An empty prefix is an empty listing rather than a failure: a run that has
	// produced nothing yet is the normal state of a running run.
	empty, err := store.List(ctx, RunPrefix("01J0000000000000000000000Z"))
	if err != nil {
		t.Fatalf("list an empty prefix: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty prefix listed %v", empty)
	}
}

// The reaper deletes by prefix age, and a volume that kept one empty directory
// per run runs out of inodes long before it runs out of bytes.
func TestDiskDeleteCollectsEmptyDirectories(t *testing.T) {
	store := newDisk(t)
	ctx := context.Background()
	id := runv1.ULID("01J0000000000000000000000X")
	key := Key(id, runv1.StoragePrefixChunks+"000001.log")

	if err := store.Put(ctx, key, []byte("x"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "runs", string(id))); err == nil {
		t.Error("the run's directory survived its last object")
	}
	// Deleting twice is ordinary for a reaper and is not an error.
	if err := store.Delete(ctx, key); err != nil {
		t.Errorf("a second delete failed: %v", err)
	}
}

// A ref read years later has to say which store wrote it, or an installation
// that switched modes finds its old runs unreadable for no visible reason.
func TestRefCarriesItsScheme(t *testing.T) {
	disk := newDisk(t)
	mem := NewMemory("haliphron", "http://storage.invalid")
	key := "runs/01J0000000000000000000000X/result.md"

	for _, tc := range []struct {
		store Store
		want  string
	}{
		{disk, "file://runs/01J0000000000000000000000X/result.md"},
		{mem, "s3://haliphron/runs/01J0000000000000000000000X/result.md"},
	} {
		ref := Ref(tc.store, key)
		if ref != tc.want {
			t.Errorf("Ref = %q, want %q", ref, tc.want)
		}
		scheme, bucket, got, ok := ParseRef(ref)
		if !ok || got != key {
			t.Errorf("ParseRef(%q) = (%q, %q, %q, %v)", ref, scheme, bucket, got, ok)
		}
	}

	if _, _, _, ok := ParseRef("runs/01J/result.md"); ok {
		t.Error("a bare key parsed as a ref; a row written before the scheme existed must not look like one")
	}
}

// NewDisk proves the volume is writable at startup. A PVC that failed to bind,
// or mounted read-only because the access mode did not match the storage class,
// is otherwise discovered by the first run that finishes — an hour and one model
// bill after the mistake was made.
func TestNewDiskRefusesAnUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(root, 0o500); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test relies on")
	}
	if _, err := NewDisk(DiskConfig{Root: root}); err == nil {
		t.Fatal("an unwritable root was accepted")
	} else if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("the message does not name the problem: %v", err)
	}
}

// The disk store mints no capabilities, and says so rather than answering with
// an empty bundle a pod would discover an hour into its work.
func TestDiskRefusesToPresign(t *testing.T) {
	store := newDisk(t)
	if _, err := store.PresignPut("runs/x/result.md", time.Hour); !errors.Is(err, ErrPresignUnsupported) {
		t.Errorf("PresignPut gave %v", err)
	}
	if _, err := store.PresignGet("runs/x/result.md", time.Hour); !errors.Is(err, ErrPresignUnsupported) {
		t.Errorf("PresignGet gave %v", err)
	}
	if _, err := store.PresignPost("runs/x/", time.Hour, 1); !errors.Is(err, ErrPresignUnsupported) {
		t.Errorf("PresignPost gave %v", err)
	}
}

func newDisk(t *testing.T) *Disk {
	t.Helper()
	store, err := NewDisk(DiskConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("disk store: %v", err)
	}
	return store
}

// failingReader delivers a few bytes and then fails, standing in for a relay
// whose connection dropped mid-transfer.
type failingReader struct{ after int }

func (f failingReader) Read(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("the connection dropped")
	}
	n := copy(p, bytes.Repeat([]byte("x"), min(f.after, len(p))))
	return n, io.ErrUnexpectedEOF
}
