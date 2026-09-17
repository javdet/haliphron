package entrypoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The pod's entire access to storage: a bundle of presigned capabilities and no
// credential of its own. It can write into its own prefix and read two keys,
// and that is the whole of ADR 14.
//
// Every failure here is exit 21, class infra, retryable — and that classing is
// the point of the code existing at all (R3). An expired signature expressed as
// exit 30 is not retried, and a run whose result had already been obtained dies
// for good, even though the controller repairs exactly this by reissuing the
// bundle before the next attempt. It is the one failure the cluster fixes by
// itself, so it is obliged to be retryable.

// Storage is the presigned client.
type Storage struct {
	bundle   clusterv1.ArtifactBundle
	client   *http.Client
	redactor *Redactor
}

// maxGetBytes caps what a presigned GET may return. Generous against any
// legitimate prompt or checkpoint, and finite.
const maxGetBytes = 64 << 20

// storageTimeout bounds one request. Generous, because a two-gigabyte log on a
// slow link is a real case and the phase that uploads it is not on the critical
// path of anything; bounded, because the alternative is a pod that hangs until
// activeDeadlineSeconds and reports nothing at all.
const storageTimeout = 5 * time.Minute

// NewStorage builds a client over a bundle.
func NewStorage(bundle clusterv1.ArtifactBundle, redactor *Redactor) *Storage {
	return &Storage{
		bundle:   bundle,
		client:   &http.Client{Timeout: storageTimeout},
		redactor: redactor,
	}
}

// ErrNotFound is a 404 from a presigned GET. It is separate from every other
// storage failure because of one caller: a 404 on state.json is the normal
// answer on a first attempt, and an entrypoint that treated it as a failure
// would fail every run it ever made.
var ErrNotFound = errors.New("no such key")

// Get fetches one of the bundle's readable keys.
func (s *Storage) Get(ctx context.Context, key string) ([]byte, error) {
	link, ok := s.bundle.Get[key]
	if !ok {
		return nil, fail(runv1.ExitStorage, "MissingCapability",
			"the bundle has no presigned GET for %s", key)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "MalformedCapability", err, "GET %s", key)
	}
	for name, value := range link.Headers {
		req.Header.Set(name, value)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// Unreachable storage. Retryable: the pod cannot tell a DNS hiccup from
		// a MinIO restart, and neither is a reason to abandon a run.
		return nil, failWrap(runv1.ExitStorage, "StorageUnreachable", s.scrub(err),
			"GET %s", key)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capped. The two keys this bundle can read are the prompt and the
	// checkpoint, and the prompt's content came from outside the system: an
	// object far larger than either has any business being takes down the one
	// process that still has to persist a paid-for result.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxGetBytes+1))
	if readErr == nil && int64(len(body)) > maxGetBytes {
		return nil, fail(runv1.ExitStorage, "ObjectTooLarge",
			"GET %s returned more than %d bytes", key, maxGetBytes)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	case resp.StatusCode == http.StatusForbidden:
		// S3 answers 403 to an expired signature and to a forged one alike, so
		// the pod cannot distinguish them and must not try. Both are repaired
		// by a fresh bundle, which the controller mints before the next attempt.
		return nil, fail(runv1.ExitStorage, "PresignedAccessDenied",
			"GET %s was refused: the signature has expired or does not verify "+
				"(the bundle expires at %s)", key, s.bundle.ExpiresAt.Format(time.RFC3339))
	case resp.StatusCode != http.StatusOK:
		return nil, fail(runv1.ExitStorage, "StorageError",
			"GET %s: %s", key, s.summarise(resp.StatusCode, body))
	case readErr != nil:
		return nil, failWrap(runv1.ExitStorage, "StorageError", s.scrub(readErr),
			"reading the body of GET %s", key)
	}
	return body, nil
}

// Put writes one of the bundle's known keys and returns a reference to what
// landed. The body is redacted on the way out: this is the last place a secret
// can be stopped from becoming durable.
func (s *Storage) Put(ctx context.Context, key string, body []byte, contentType string) (*runv1.ObjectRef, error) {
	link, ok := s.bundle.Put[key]
	if !ok {
		return nil, fail(runv1.ExitStorage, "MissingCapability",
			"the bundle has no presigned PUT for %s", key)
	}
	clean := s.redactor.Bytes(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, link.URL, bytes.NewReader(clean))
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "MalformedCapability", err, "PUT %s", key)
	}
	// The bundle's headers are part of the signed material and must be sent
	// verbatim or the signature will not verify.
	for name, value := range link.Headers {
		req.Header.Set(name, value)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(clean))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "StorageUnreachable", s.scrub(err), "PUT %s", key)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, fail(runv1.ExitStorage, "PresignedAccessDenied",
			"PUT %s was refused: the signature has expired or does not verify "+
				"(the bundle expires at %s)", key, s.bundle.ExpiresAt.Format(time.RFC3339))
	case resp.StatusCode >= 300:
		return nil, fail(runv1.ExitStorage, "StorageError",
			"PUT %s: %s", key, s.summarise(resp.StatusCode, answer))
	}

	sum := sha256.Sum256(clean)
	return &runv1.ObjectRef{
		Bucket:      s.bundle.Bucket,
		Key:         s.bundle.KeyPrefix + key,
		SizeBytes:   int64(len(clean)),
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: contentType,
	}, nil
}

// PostUnder writes to a key whose name was not known when the bundle was
// minted: a log chunk, or a file the agent chose to keep.
//
// suffix is relative to the policy's prefix. The policy is sent back verbatim
// because that is what the store verifies the key against — the capability is
// over a prefix, and a pod that could name any key would not be scoped to its
// own run at all.
func (s *Storage) PostUnder(ctx context.Context, prefix, suffix string, body []byte, contentType string) (*runv1.ObjectRef, error) {
	policy, ok := s.policyFor(prefix)
	if !ok {
		return nil, fail(runv1.ExitStorage, "MissingCapability",
			"the bundle has no presigned POST policy covering %s", prefix)
	}
	clean := s.redactor.Bytes(body)
	key := policy.Prefix + suffix
	if policy.MaxSizeBytes > 0 && int64(len(clean)) > policy.MaxSizeBytes {
		return nil, fail(runv1.ExitStorage, "ObjectTooLarge",
			"%s is %d bytes and the policy allows %d", key, len(clean), policy.MaxSizeBytes)
	}

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	for name, value := range policy.Fields {
		if name == "key" {
			value = key
		}
		if err := form.WriteField(name, value); err != nil {
			return nil, failWrap(runv1.ExitStorage, "StorageError", err, "building the POST for %s", key)
		}
	}
	part, err := form.CreateFormFile("file", path.Base(key))
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "StorageError", err, "building the POST for %s", key)
	}
	if _, err := part.Write(clean); err != nil {
		return nil, failWrap(runv1.ExitStorage, "StorageError", err, "building the POST for %s", key)
	}
	if err := form.Close(); err != nil {
		return nil, failWrap(runv1.ExitStorage, "StorageError", err, "building the POST for %s", key)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, policy.URL, &buf)
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "MalformedCapability", err, "POST %s", key)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "StorageUnreachable", s.scrub(err), "POST %s", key)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, fail(runv1.ExitStorage, "PresignedAccessDenied",
			"POST %s was refused: the policy has expired, does not verify, or does not cover this key", key)
	case resp.StatusCode >= 300:
		return nil, fail(runv1.ExitStorage, "StorageError",
			"POST %s: %s", key, s.summarise(resp.StatusCode, answer))
	}

	sum := sha256.Sum256(clean)
	return &runv1.ObjectRef{
		Bucket:      s.bundle.Bucket,
		Key:         key,
		SizeBytes:   int64(len(clean)),
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: contentType,
	}, nil
}

// policyFor finds the POST capability covering a prefix relative to the run.
func (s *Storage) policyFor(relative string) (clusterv1.PresignedPostPolicy, bool) {
	want := s.bundle.KeyPrefix + relative
	for _, policy := range s.bundle.Post {
		if policy.Prefix == want {
			return policy, true
		}
	}
	// A policy over a broader prefix still covers this one. Accepting it costs
	// nothing and saves a pod from failing because the backend minted one
	// capability over the run instead of two over its subdirectories.
	for _, policy := range s.bundle.Post {
		if strings.HasPrefix(want, policy.Prefix) {
			return policy, true
		}
	}
	return clusterv1.PresignedPostPolicy{}, false
}

// ExpiresAt is when the bundle stops working. Reported in the log at startup so
// that "the upload failed at minute fifty" has an obvious first suspect.
func (s *Storage) ExpiresAt() time.Time { return s.bundle.ExpiresAt }

// summarise renders a store's refusal without letting its echo of the request
// carry a signature into the log.
func (s *Storage) summarise(status int, body []byte) string {
	text := strings.TrimSpace(string(s.redactor.Bytes(body)))
	if len(text) > 256 {
		text = text[:256]
	}
	if text == "" {
		return http.StatusText(status)
	}
	return fmt.Sprintf("%d %s: %s", status, http.StatusText(status), text)
}

// scrub redacts an error's text. net/http puts the whole URL into its errors,
// signature and all, and that error is on its way into a message that becomes
// durable.
func (s *Storage) scrub(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(s.redactor.String(err.Error()))
}
