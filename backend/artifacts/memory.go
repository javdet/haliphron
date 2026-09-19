package artifacts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// Memory is the ArtifactStore held in a map.
//
// It exists for the contract tests, where the properties under test are the
// backend's: that the prompt is written before the run is admitted, that its
// digest is what the pod will verify against, that a terminal status without a
// completion recovers the report from storage. None of those need a real
// object store, and a test that needs MinIO running is a test that gets
// skipped.
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
