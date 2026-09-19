// Package artifacts is the backend's side of shared storage: the ArtifactStore
// port of section 5 of the architecture, its S3 implementation, and the bundle
// of presigned capabilities a lease carries.
//
// The backend touches storage in exactly three places, and the port is shaped
// by them rather than by S3's API surface: it writes runs/{id}/prompt.txt at
// admission, it reads runs/{id}/completion.json back when a terminal status
// arrived without a report, and it mints the capabilities the pod works
// through. Everything else in the prefix is written by the pod and read by a
// human through a presigned link.
//
// No credential ever reaches the pod (ADR 14). The bundle is the whole of its
// access, which is why the bundle is secret material: a presigned URL is a
// bearer capability on somebody else's prefix, and it belongs in the per-run
// Secret rather than in the CR or an environment variable.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ErrNotFound is a missing object. It is a named error because one caller has
// to tell it from a failure: a run whose terminal status arrived without a
// completion is recoverable only if completion.json is there, and "not there
// yet" is a normal answer that must not be logged as a storage fault.
var ErrNotFound = errors.New("artifacts: object not found")

// Object is one stored object, as a listing sees it.
type Object struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ETag         string
}

// Store is the port. Implementations are s3.Store against S3 or MinIO, and
// Memory in tests.
//
// The presign methods take no context: they are pure computation over a
// credential the process already holds, and giving them a context would suggest
// a round trip that does not happen. That property is what makes a lease cheap
// to issue — no network call stands between a queued run and a controller
// holding it.
type Store interface {
	// Bucket and Endpoint travel in the bundle so the pod can address the same
	// object store the signature was minted against.
	Bucket() string
	Endpoint() string

	Put(ctx context.Context, key string, body []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]Object, error)

	PresignGet(key string, ttl time.Duration) (clusterv1.PresignedURL, error)
	PresignPut(key string, ttl time.Duration) (clusterv1.PresignedURL, error)
	PresignPost(prefix string, ttl time.Duration, maxSize int64) (clusterv1.PresignedPostPolicy, error)
}

// RunPrefix is the per-run prefix. Every capability in a bundle is scoped to
// it, so a compromised pod cannot reach another run's results.
func RunPrefix(id runv1.ULID) string { return fmt.Sprintf(runv1.StoragePrefixRun, string(id)) }

// Key is one object under a run's prefix.
func Key(id runv1.ULID, name string) string { return RunPrefix(id) + name }

// putKeys are the objects the pod writes, and the only ones it is given a PUT
// capability for.
//
// completion.json is the one that makes the report survivable. The webhook and
// its forwarding both live in the controller's memory; a crash between them
// loses the cost and the PR link, and neither result.md nor output.json carries
// either of those.
var putKeys = []string{
	runv1.StorageKeyOutput,
	runv1.StorageKeyResult,
	runv1.StorageKeyState,
	runv1.StorageKeyCompletion,
	runv1.StorageKeyAgentLog,
}

// getKeys are the two objects the pod reads. prompt.txt, without which it has
// no task, and state.json, without which an idempotent retry is impossible: a
// pod that cannot learn the agent phase already finished pays for the model a
// second time. A 404 on state.json is the normal first-attempt answer.
var getKeys = []string{
	runv1.StorageKeyPrompt,
	runv1.StorageKeyState,
}

// MaxChunkBytes bounds one presigned POST upload. A log chunk that exceeds it
// is a bug in the entrypoint's chunking, and an unbounded POST policy is a way
// to fill a customer's bucket from inside an agent.
const MaxChunkBytes = 64 << 20

// Bundle mints the capabilities for one run.
//
// ttl is derived by the caller from the run's own timeout, multiplied by
// Timings.ArtifactTTLMultiplier. It is deliberately not "long enough to be
// safe": a signature that outlives the work it was minted for is a capability
// lying around in a Secret, and the controller can ask for a fresh bundle
// whenever the one it holds falls short.
func Bundle(s Store, id runv1.ULID, ttl time.Duration) (clusterv1.ArtifactBundle, error) {
	prefix := RunPrefix(id)
	bundle := clusterv1.ArtifactBundle{
		Bucket:    s.Bucket(),
		Endpoint:  s.Endpoint(),
		KeyPrefix: prefix,
		Put:       make(map[string]clusterv1.PresignedURL, len(putKeys)),
		Get:       make(map[string]clusterv1.PresignedURL, len(getKeys)),
	}

	for _, name := range putKeys {
		url, err := s.PresignPut(prefix+name, ttl)
		if err != nil {
			return clusterv1.ArtifactBundle{}, fmt.Errorf("presign put %s: %w", name, err)
		}
		bundle.Put[name] = url
	}
	for _, name := range getKeys {
		url, err := s.PresignGet(prefix+name, ttl)
		if err != nil {
			return clusterv1.ArtifactBundle{}, fmt.Errorf("presign get %s: %w", name, err)
		}
		bundle.Get[name] = url
	}

	post, err := s.PresignPost(prefix+runv1.StoragePrefixChunks, ttl, MaxChunkBytes)
	if err != nil {
		return clusterv1.ArtifactBundle{}, fmt.Errorf("presign post %s: %w", runv1.StoragePrefixChunks, err)
	}
	bundle.Post = []clusterv1.PresignedPostPolicy{post}

	// ExpiresAt is the minimum over every link, because that is what the
	// controller compares against the expected duration of the next attempt.
	// Reporting the maximum would let a bundle be judged fresh while the key
	// the pod needs first has already expired.
	bundle.ExpiresAt = minExpiry(bundle)
	return bundle, nil
}

func minExpiry(b clusterv1.ArtifactBundle) time.Time {
	var min time.Time
	consider := func(t time.Time) {
		if min.IsZero() || t.Before(min) {
			min = t
		}
	}
	for _, u := range b.Put {
		consider(u.ExpiresAt)
	}
	for _, u := range b.Get {
		consider(u.ExpiresAt)
	}
	for _, p := range b.Post {
		consider(p.ExpiresAt)
	}
	return min
}
