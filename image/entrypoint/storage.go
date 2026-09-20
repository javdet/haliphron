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

// The object-store half of the Uploader port: a bundle of presigned
// capabilities and no credential of its own.
//
// It can write into its own prefix and read nothing at all. The reads are gone
// — the prompt and the checkpoint both arrive in the environment now — and with
// them went the two failure modes that made this path fragile: a run that could
// not start because a presigned GET had expired, and a retry that paid for the
// model again because it could not read a checkpoint it was entitled to.
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
	// No bucket in the ref. Which store holds the object is the installation's
	// business, and a pod that never puts a bucket name in a report cannot leak
	// one. Uploaded is this pod's own PUT: in relay mode the same field is the
	// controller's acknowledgement, and the backend checks it either way before
	// believing the reference.
	return &runv1.ObjectRef{
		Key:         s.bundle.KeyPrefix + key,
		SizeBytes:   int64(len(clean)),
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: contentType,
		Uploaded:    true,
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
		Key:         key,
		SizeBytes:   int64(len(clean)),
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: contentType,
		Uploaded:    true,
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

// Mode is object-store.
func (s *Storage) Mode() runv1.ArtifactMode { return runv1.ArtifactModeObjectStore }

// Describe is the startup line. It names the expiry, because in this mode the
// expiry is the first thing to suspect when an upload fails late in a long run.
func (s *Storage) Describe() string {
	return fmt.Sprintf("object store %s/%s, bundle expires %s",
		s.bundle.Bucket, s.bundle.KeyPrefix,
		s.bundle.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
}

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
