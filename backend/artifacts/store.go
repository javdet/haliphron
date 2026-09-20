// Package artifacts is the backend's side of the artifact store: the
// ArtifactStore port of section 5 of the architecture, its two implementations,
// and the bundle a lease carries in the mode that has one.
//
// # Two modes behind one port
//
// Results, logs and artifacts have unbounded size, must outlive the pod, are
// read both by a human in the UI and by another agent, and need retention. That
// is a storage problem, not an object-storage problem, and the distinction is
// the whole reason this package has the shape it does: an installation should
// not have to stand up MinIO to run its first agent.
//
//	relay (default)  pod → controller → backend → a PVC on the backend
//	object-store     pod → S3/MinIO directly, by presigned URL
//
// The key layout is identical in both, so the mode is invisible above the port.
// Everything that used to make object storage mandatory has left this package:
// the prompt is a column in PostgreSQL, the retry checkpoint is a column in
// PostgreSQL, and what remains here is what actually has no ceiling. Nothing on
// the path to *starting* a run touches this store any more — only the path to
// finishing one, and the default mode finishes runs without a bucket.
//
// # What the backend does with it
//
// Two things, and the port is shaped by them rather than by S3's API surface.
// It writes the bytes a controller relays, and it reads runs/{id}/completion.json
// back when a terminal status arrived without a report. In object-store mode it
// also mints the capabilities the pod works through, and in that mode the
// backend is not in the data path at all.
//
// No credential ever reaches the pod in either mode (ADR 14). In relay mode it
// addresses no store and cannot name another run's prefix, because the
// controller stamps the prefix from the CR. In object-store mode the presigned
// bundle is the whole of its access, which is why the bundle is secret material.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// ErrNotFound is a missing object. It is a named error because one caller has
// to tell it from a failure: a run whose terminal status arrived without a
// completion is recoverable only if completion.json is there, and "not there
// yet" is a normal answer that must not be logged as a storage fault.
var ErrNotFound = errors.New("artifacts: object not found")

// ErrPresignUnsupported is what the disk store answers to a request for a
// presigned capability.
//
// It is an error rather than a silent empty bundle because the caller is about
// to hand the answer to a pod: an empty bundle in object-store mode is a run
// that starts, works for an hour and cannot upload anything. The configuration
// that produces it — mode object-store with no object store — is refused at
// startup, and this is the backstop.
var ErrPresignUnsupported = errors.New("artifacts: this store mints no presigned capabilities")

// Object is one stored object, as a listing sees it.
type Object struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ETag         string
}

// Store is the port. Implementations are Disk against a mounted volume, S3
// against S3 or MinIO, and Memory in tests.
//
// Put and Get take whole byte slices and PutStream takes a reader. Both exist
// because the two callers are different: the recovery path reads one small
// JSON document and wants it in memory, while the relay writes a log that can
// be a gigabyte and must not hold it.
type Store interface {
	// Scheme is what a stored runs.result_ref says about this store —
	// runv1.SchemeFile or runv1.SchemeS3. A row read years later names the
	// store that wrote it, so an installation that migrates between modes
	// keeps its old runs readable instead of orphaning them.
	Scheme() string
	// Mode is which half of the port this is.
	Mode() runv1.ArtifactMode
	// Bucket is the object store's bucket, and empty for the disk store. It is
	// part of a result_ref URI and of nothing the pod ever sees.
	Bucket() string
	Endpoint() string

	Put(ctx context.Context, key string, body []byte, contentType string) error
	// PutStream writes without buffering. It returns what landed, so that the
	// caller can put a complete ObjectRef in a response rather than
	// reconstructing one from what it hoped it sent.
	PutStream(ctx context.Context, key string, body io.Reader, contentType string) (runv1.ObjectRef, error)
	Get(ctx context.Context, key string) ([]byte, error)
	// Open streams an object out. The caller closes it. This is what the REST
	// result and log endpoints serve from in relay mode, where there is no
	// presigned URL to redirect to.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	// Delete removes one object. The reaper walks a prefix by age and calls
	// this; in object-store mode lifecycle rules do the same job and the chart
	// generates them instead.
	Delete(ctx context.Context, key string) error

	// The presign methods take no context: they are pure computation over a
	// credential the process already holds, and giving them a context would
	// suggest a round trip that does not happen. That property is what makes a
	// lease cheap to issue — no network call stands between a queued run and a
	// controller holding it. The disk store answers ErrPresignUnsupported.
	PresignGet(key string, ttl time.Duration) (clusterv1.PresignedURL, error)
	PresignPut(key string, ttl time.Duration) (clusterv1.PresignedURL, error)
	PresignPost(prefix string, ttl time.Duration, maxSize int64) (clusterv1.PresignedPostPolicy, error)
}

// RunPrefix is the per-run prefix. Every capability in an object-store bundle
// is scoped to it, and in relay mode it is what the controller and the backend
// each stamp onto a key the pod handed them — so in neither mode can a
// compromised pod reach another run's results.
func RunPrefix(id runv1.ULID) string { return fmt.Sprintf(runv1.StoragePrefixRun, string(id)) }

// Key is one object under a run's prefix.
func Key(id runv1.ULID, name string) string { return RunPrefix(id) + name }

// Ref renders a stored key as the URI runs.result_ref holds.
//
// The scheme is the point: file://runs/01J8.../result.md against
// s3://haliphron/runs/01J8.../result.md. Without it a reader has to assume the
// mode configured today, and an installation that switched modes last year
// would find its old runs unreadable for no reason anybody could see.
func Ref(s Store, key string) string {
	if s.Scheme() == runv1.SchemeS3 {
		return runv1.SchemeS3 + "://" + s.Bucket() + "/" + key
	}
	return runv1.SchemeFile + "://" + key
}

// ParseRef is Ref backwards: the key, and the scheme it was written under.
//
// A ref whose scheme is not this store's is not an error here. It is an older
// run from before a migration, and the caller decides whether it can serve it —
// which is a decision about configuration, not about parsing.
func ParseRef(ref string) (scheme, bucket, key string, ok bool) {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme == "" {
		return "", "", "", false
	}
	switch u.Scheme {
	case runv1.SchemeFile:
		// file://runs/… — the authority is the first path segment, because a
		// relative key has no host to put there.
		return u.Scheme, "", strings.TrimPrefix(u.Host+u.Path, "/"), true
	case runv1.SchemeS3:
		return u.Scheme, u.Host, strings.TrimPrefix(u.Path, "/"), true
	}
	return "", "", "", false
}

// putKeys are the objects the pod writes, and the only ones it is given a PUT
// capability for in object-store mode.
//
// completion.json is the one that makes the report survivable. The webhook and
// its forwarding both live in the controller's memory; a crash between them
// loses the cost and the PR link, and neither result.md nor output.json carries
// either of those.
//
// state.json is gone from this list, and so is prompt.txt from a list of reads
// that no longer exists. Those two were the reason the bundle needed GET
// capabilities at all, and removing them removes exit code 21's most common
// cause along with one whole class of "the run failed and the reason was a URL
// expiry".
var putKeys = []string{
	runv1.StorageKeyOutput,
	runv1.StorageKeyResult,
	runv1.StorageKeyCompletion,
	runv1.StorageKeyAgentLog,
}

// MaxChunkBytes bounds one presigned POST upload. A log chunk that exceeds it
// is a bug in the entrypoint's chunking, and an unbounded POST policy is a way
// to fill a customer's bucket from inside an agent.
const MaxChunkBytes = 64 << 20

// Bundle says how one run's artifacts reach durable storage.
//
// In relay mode that is one field and no round trip: the pod posts to the
// controller Service it already posts its completion to. Everything below the
// early return is object-store mode.
//
// ttl is derived by the caller from the run's own timeout, multiplied by
// Timings.ArtifactTTLMultiplier. It is deliberately not "long enough to be
// safe": a signature that outlives the work it was minted for is a capability
// lying around in a Secret, and the controller can ask for a fresh bundle
// whenever the one it holds falls short.
func Bundle(s Store, id runv1.ULID, ttl time.Duration, maxBytesPerRun int64) (clusterv1.ArtifactBundle, error) {
	if maxBytesPerRun <= 0 {
		maxBytesPerRun = clusterv1.DefaultMaxBytesPerRun
	}
	if s.Mode() != runv1.ArtifactModeObjectStore {
		return clusterv1.ArtifactBundle{
			Mode:           runv1.ArtifactModeRelay,
			MaxBytesPerRun: maxBytesPerRun,
		}, nil
	}

	prefix := RunPrefix(id)
	bundle := clusterv1.ArtifactBundle{
		Mode:           runv1.ArtifactModeObjectStore,
		MaxBytesPerRun: maxBytesPerRun,
		Bucket:         s.Bucket(),
		Endpoint:       s.Endpoint(),
		KeyPrefix:      prefix,
		Put:            make(map[string]clusterv1.PresignedURL, len(putKeys)),
	}

	for _, name := range putKeys {
		url, err := s.PresignPut(prefix+name, ttl)
		if err != nil {
			return clusterv1.ArtifactBundle{}, fmt.Errorf("presign put %s: %w", name, err)
		}
		bundle.Put[name] = url
	}

	for _, p := range []string{runv1.StoragePrefixChunks, runv1.StoragePrefixArtifacts} {
		post, err := s.PresignPost(prefix+p, ttl, MaxChunkBytes)
		if err != nil {
			return clusterv1.ArtifactBundle{}, fmt.Errorf("presign post %s: %w", p, err)
		}
		bundle.Post = append(bundle.Post, post)
	}

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
	for _, p := range b.Post {
		consider(p.ExpiresAt)
	}
	return min
}
