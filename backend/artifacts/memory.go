package artifacts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Memory is the object-store half of the port, held in a map.
//
// It exists for the contract tests, where the property under test is the
// backend's: that a terminal status without a completion recovers the report
// from the store, that a bundle is scoped to one run's prefix, that the expiry
// arithmetic is what the controller will compare against. None of those need a
// real object store, and a test that needs MinIO running is a test that gets
// skipped.
//
// It reports object-store mode, because that is the half it stands in for. The
// relay half needs no double: Disk over a t.TempDir() is the real
// implementation and costs a line.
//
// The signatures it mints are real HMACs over the same material an S3
// signature covers — method, key and expiry — so that a bundle from here has
// the same shape and the same expiry arithmetic. They lead nowhere: the pod's
// side of storage is tested against FakeControlPlane, which serves them.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memObject

	bucket  string
	baseURL string
	signKey []byte
	now     func() time.Time
}

type memObject struct {
	body        []byte
	contentType string
	modified    time.Time
}

// NewMemory builds an empty store.
func NewMemory(bucket, baseURL string) *Memory {
	return &Memory{
		objects: map[string]memObject{},
		bucket:  bucket,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		signKey: []byte("haliphron-memory-artifacts"),
		now:     time.Now,
	}
}

// Bucket is the bucket name the bundle advertises.
func (m *Memory) Bucket() string { return m.bucket }

// Scheme is s3: this stands in for the object-store half, and a result_ref
// written in a test should have the shape one written in production has.
func (m *Memory) Scheme() string { return runv1.SchemeS3 }

// Mode is object-store.
func (m *Memory) Mode() runv1.ArtifactMode { return runv1.ArtifactModeObjectStore }

// Endpoint is the base URL the bundle advertises.
func (m *Memory) Endpoint() string { return m.baseURL }

// Put stores an object.
func (m *Memory) Put(_ context.Context, key string, body []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{
		body: slices.Clone(body), contentType: contentType, modified: m.now(),
	}
	return nil
}

// PutStream stores an object from a reader.
func (m *Memory) PutStream(ctx context.Context, key string, body io.Reader, contentType string) (runv1.ObjectRef, error) {
	buf, err := io.ReadAll(body)
	if err != nil {
		return runv1.ObjectRef{}, fmt.Errorf("artifacts: read the body for %s: %w", key, err)
	}
	if err := m.Put(ctx, key, buf, contentType); err != nil {
		return runv1.ObjectRef{}, err
	}
	sum := sha256.Sum256(buf)
	return runv1.ObjectRef{
		Key:         key,
		SizeBytes:   int64(len(buf)),
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: contentType,
		Uploaded:    true,
	}, nil
}

// Open streams an object out.
func (m *Memory) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	body, err := m.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// Delete removes an object. A key that was never there is not an error: the
// reaper deleting twice is ordinary.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Get returns an object, or ErrNotFound.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, ErrNotFound)
	}
	return slices.Clone(obj.body), nil
}

// List enumerates a prefix, sorted by key so that a paged reader sees a stable
// order.
func (m *Memory) List(_ context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := slices.Sorted(maps.Keys(m.objects))
	var out []Object
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		obj := m.objects[key]
		out = append(out, Object{
			Key: key, SizeBytes: int64(len(obj.body)), LastModified: obj.modified,
		})
	}
	return out, nil
}

// PresignGet mints a read capability.
func (m *Memory) PresignGet(key string, ttl time.Duration) (clusterv1.PresignedURL, error) {
	return m.presign("GET", key, ttl)
}

// PresignPut mints a write capability.
func (m *Memory) PresignPut(key string, ttl time.Duration) (clusterv1.PresignedURL, error) {
	return m.presign("PUT", key, ttl)
}

// PresignPost mints a capability over a prefix.
func (m *Memory) PresignPost(prefix string, ttl time.Duration, maxSize int64) (clusterv1.PresignedPostPolicy, error) {
	expires := m.now().Add(ttl)
	return clusterv1.PresignedPostPolicy{
		Prefix: prefix,
		URL:    m.baseURL + "/" + m.bucket,
		Fields: map[string]string{
			"key": prefix + "${filename}",
			"sig": m.sign("POST", prefix, expires),
		},
		MaxSizeBytes: maxSize,
		ExpiresAt:    expires,
	}, nil
}

func (m *Memory) presign(method, key string, ttl time.Duration) (clusterv1.PresignedURL, error) {
	if ttl <= 0 {
		return clusterv1.PresignedURL{}, fmt.Errorf("artifacts: presign %s %s: ttl must be positive", method, key)
	}
	expires := m.now().Add(ttl)
	q := url.Values{
		"method": {method},
		"exp":    {strconv.FormatInt(expires.Unix(), 10)},
		"sig":    {m.sign(method, key, expires)},
	}
	return clusterv1.PresignedURL{
		URL:       m.baseURL + "/" + m.bucket + "/" + key + "?" + q.Encode(),
		Method:    method,
		ExpiresAt: expires,
	}, nil
}

// sign covers the method as well as the key. Leaving the method out would let
// a holder of a GET capability turn it into a PUT, which is the whole reason
// the bundle separates the two maps.
func (m *Memory) sign(method, key string, expires time.Time) string {
	mac := hmac.New(sha256.New, m.signKey)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%d", method, m.bucket, key, expires.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}
