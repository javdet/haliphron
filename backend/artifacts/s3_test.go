package artifacts

import (
	"net/url"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The AWS documentation's own worked example for a query-authenticated
// signature. Hand-rolled signing is worth exactly as much as the vector that
// pins it: without this test the code is a plausible-looking transcription of
// a specification, and the first evidence of a mistake is a 403 inside a pod
// forty minutes into a run.
func TestPresignMatchesTheAWSWorkedExample(t *testing.T) {
	store := &S3Store{
		cfg: S3Config{
			Bucket:    "examplebucket",
			Region:    "us-east-1",
			Endpoint:  "https://s3.amazonaws.com",
			AccessKey: "AKIAIOSFODNN7EXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}

	signed, err := store.PresignGet("test.txt", 24*time.Hour)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	if got := parsed.Query().Get("X-Amz-Signature"); got != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
	if got, want := parsed.Host, "examplebucket.s3.amazonaws.com"; got != want {
		t.Errorf("virtual-host addressing: host = %s, want %s", got, want)
	}
	if got, want := parsed.Path, "/test.txt"; got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
}

// MinIO and anything reached by IP cannot use virtual-host addressing, and an
// endpoint that sets one style while the signature assumes the other produces
// a signature over a path the store never sees.
func TestEndpointImpliesPathStyle(t *testing.T) {
	store, err := NewS3(S3Config{
		Bucket: "haliphron", Region: "us-east-1",
		Endpoint:  "http://minio.haliphron.svc:9000",
		AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	signed, err := store.PresignPut("runs/X/result.md", time.Hour)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !strings.HasPrefix(signed.URL, "http://minio.haliphron.svc:9000/haliphron/runs/X/result.md?") {
		t.Errorf("path-style URL expected, got %s", signed.URL)
	}
	if signed.Method != "PUT" {
		t.Errorf("method = %s, want PUT", signed.Method)
	}
}

// The signature is minted before the body exists, so S3 caps a query-authenticated
// link at a week. A bundle asking for more has to fail here, not in a pod.
func TestPresignRejectsAnImpossibleTTL(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PresignGet("runs/X/prompt.txt", 8*24*time.Hour); err == nil {
		t.Fatal("a TTL beyond the S3 maximum was accepted")
	}
	if _, err := store.PresignGet("runs/X/prompt.txt", 0); err == nil {
		t.Fatal("a zero TTL was accepted")
	}
}

// The bundle's expiry is what the controller compares against the expected
// duration of the next attempt. Reporting anything but the minimum would let a
// bundle be judged fresh while the key the pod reads first has expired.
func TestBundleCarriesTheContractsKeysAndTheEarliestExpiry(t *testing.T) {
	store := NewMemory("haliphron", "http://storage.invalid")
	id := runv1.ULID("01J0000000000000000000000X")

	bundle, err := Bundle(store, id, time.Hour)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}

	for _, key := range []string{
		runv1.StorageKeyOutput, runv1.StorageKeyResult, runv1.StorageKeyState,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		if _, ok := bundle.Put[key]; !ok {
			t.Errorf("bundle has no PUT capability for %s", key)
		}
	}
	// Two mandatory reads: without prompt.txt the pod has no task, and without
	// state.json an idempotent retry cannot know the agent phase is done.
	for _, key := range []string{runv1.StorageKeyPrompt, runv1.StorageKeyState} {
		if _, ok := bundle.Get[key]; !ok {
			t.Errorf("bundle has no GET capability for %s", key)
		}
	}
	if len(bundle.Post) != 1 || !strings.HasSuffix(bundle.Post[0].Prefix, runv1.StoragePrefixChunks) {
		t.Errorf("bundle has no POST policy over the log chunk prefix: %+v", bundle.Post)
	}
	if want := "runs/" + string(id) + "/"; bundle.KeyPrefix != want {
		t.Errorf("keyPrefix = %s, want %s", bundle.KeyPrefix, want)
	}

	for key, u := range bundle.Put {
		if u.ExpiresAt.Before(bundle.ExpiresAt) {
			t.Errorf("%s expires at %s, before the bundle's stated %s", key, u.ExpiresAt, bundle.ExpiresAt)
		}
	}
}

func newTestStore(t *testing.T) *S3Store {
	t.Helper()
	store, err := NewS3(S3Config{
		Bucket: "haliphron", Region: "eu-central-1",
		AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}
