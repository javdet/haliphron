package artifacts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// S3, spoken directly rather than through a vendor SDK.
//
// The reason is proportion. What the backend needs from S3 is four verbs and a
// signature: PUT one object, GET one object, LIST a prefix, and mint presigned
// capabilities. SigV4 is a published algorithm with stable test vectors, and
// the whole of it is the bottom half of this file. The alternative is a
// dependency tree of about forty modules in a binary whose other dependencies
// are a Postgres driver and the standard library, kept current for the sake of
// code paths that are never taken.
//
// It also keeps one property that matters operationally: the same code signs
// for AWS and for MinIO, so an on-prem installation is not a second code path
// that gets tested less.

// S3Config is what the deployment provides.
type S3Config struct {
	Bucket string
	Region string

	// Endpoint is set for MinIO and other S3-compatible stores. Empty means
	// AWS, and the regional endpoint is derived from Region.
	Endpoint string

	AccessKey    string
	SecretKey    string
	SessionToken string

	// PathStyle addresses the bucket as a path segment rather than a
	// subdomain. Mandatory for MinIO and for any endpoint reached by IP, and
	// defaulted to true whenever Endpoint is set.
	PathStyle bool
}

// S3Store is the ArtifactStore over an S3-compatible object store.
type S3Store struct {
	cfg    S3Config
	client *http.Client
	now    func() time.Time
}

// s3Timeout bounds one request from the backend. The objects on this path are
// a prompt and a completion report — kilobytes — so a request that takes longer
// than this is a store that is down, and waiting longer only spends a request
// handler on it.
const s3Timeout = 30 * time.Second

// NewS3 builds a store. It performs no round trip: a control plane that cannot
// start because the bucket is briefly unreachable is a control plane that
// cannot start during an object-store upgrade.
func NewS3(cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("artifacts: bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	} else {
		cfg.PathStyle = true
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("artifacts: endpoint %q: %w", cfg.Endpoint, err)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("artifacts: access key and secret key are required")
	}
	return &S3Store{
		cfg:    cfg,
		client: &http.Client{Timeout: s3Timeout},
		now:    time.Now,
	}, nil
}

// Bucket is the bucket every capability is scoped to.
func (s *S3Store) Bucket() string { return s.cfg.Bucket }

// Scheme is s3, which is what a result_ref written by this store says.
func (s *S3Store) Scheme() string { return runv1.SchemeS3 }

// Mode is object-store.
func (s *S3Store) Mode() runv1.ArtifactMode { return runv1.ArtifactModeObjectStore }

// Endpoint is what the pod addresses. It travels in the bundle because the pod
// has no configuration of its own.
func (s *S3Store) Endpoint() string { return s.cfg.Endpoint }

// Put writes one object, through a capability the backend mints for itself.
// Going through the presigned path rather than a signed request keeps one
// signing implementation instead of two.
func (s *S3Store) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.put(ctx, key, bytes.NewReader(body), int64(len(body)), contentType)
	return err
}

// PutStream writes without buffering.
//
// It exists for symmetry with the disk store rather than for this mode's own
// sake: in object-store mode the backend is not in the artifact data path at
// all — the pod PUTs straight to the store — so the only bytes that come
// through here are the ones a relay-to-object-store migration would move.
// SigV4 with an unsigned payload is what makes streaming possible without
// buffering to compute a body digest.
func (s *S3Store) PutStream(ctx context.Context, key string, body io.Reader, contentType string) (runv1.ObjectRef, error) {
	// The digest is computed on the way past rather than up front, so a caller
	// that handed us a network reader is not asked to produce it twice.
	sum := sha256.New()
	counter := &countingReader{r: io.TeeReader(body, sum)}
	if _, err := s.put(ctx, key, counter, -1, contentType); err != nil {
		return runv1.ObjectRef{}, err
	}
	return runv1.ObjectRef{
		Key:         key,
		SizeBytes:   counter.n,
		SHA256:      hex.EncodeToString(sum.Sum(nil)),
		ContentType: contentType,
		Uploaded:    true,
	}, nil
}

func (s *S3Store) put(ctx context.Context, key string, body io.Reader, length int64, contentType string) (*http.Response, error) {
	signed, err := s.presign(http.MethodPut, key, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.URL, body)
	if err != nil {
		return nil, fmt.Errorf("artifacts: build put %s: %w", key, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = length

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("artifacts: put %s: %s", key, statusDetail(resp))
	}
	return resp, nil
}

// countingReader is how PutStream learns the size it did not know in advance.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Get reads one object. A 404 is ErrNotFound and nothing else: the caller that
// reaches for completion.json has to tell "the pod never wrote it" from "the
// store is broken", and they have opposite consequences.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	signed, err := s.presign(http.MethodGet, key, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: build get %s: %w", key, err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("artifacts: get %s: %s", key, statusDetail(resp))
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxGetBytes))
}

// Open streams one object out. The caller closes it.
//
// Unlike Get it is not capped: its caller is the log endpoint streaming a
// gigabyte to a browser, and a cap there would truncate a log rather than
// protect anything — the bytes pass through without being held.
func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	signed, err := s.presign(http.MethodGet, key, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: build open %s: %w", key, err)
	}
	// No client timeout on a stream: s.client bounds a whole request, and a
	// large log legitimately takes longer than s3Timeout to read.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open %s: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("artifacts: open %s: %w", key, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		detail := statusDetail(resp)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("artifacts: open %s: %s", key, detail)
	}
	return resp.Body, nil
}

// Delete removes one object. Retention in this mode is normally ILM or S3
// lifecycle rules generated by the chart; this is for the reaper on an
// installation that has neither.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	signed, err := s.presign(http.MethodDelete, key, nil, time.Minute)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, signed.URL, nil)
	if err != nil {
		return fmt.Errorf("artifacts: build delete %s: %w", key, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("artifacts: delete %s: %w", key, err)
	}
	defer resp.Body.Close()
	// S3 answers 204 to deleting a key that was never there, which is the
	// answer this caller wants: the reaper deleting an object twice is
	// ordinary, and it is not evidence of anything.
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("artifacts: delete %s: %s", key, statusDetail(resp))
	}
	return nil
}

// maxGetBytes caps what the backend will read from storage. The two objects it
// reads are a prompt and a report; anything larger is a mistake, and an
// unbounded read is a way to exhaust the control plane's memory from inside a
// pod.
const maxGetBytes = 8 << 20

// List enumerates a prefix. It exists for one caller — the log endpoint, which
// pages over runs/{id}/logs/chunks/ — and that is why there is no artifacts
// table in the schema: the listing is a LIST by prefix, not a second index to
// keep in agreement with the bucket.
func (s *S3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		query := url.Values{
			"list-type": {"2"},
			"prefix":    {prefix},
			"max-keys":  {"1000"},
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		signed, err := s.presign(http.MethodGet, "", query, time.Minute)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.URL, nil)
		if err != nil {
			return nil, fmt.Errorf("artifacts: build list %s: %w", prefix, err)
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("artifacts: list %s: %w", prefix, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxGetBytes))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("artifacts: list %s: status %d", prefix, resp.StatusCode)
		}
		if readErr != nil {
			return nil, fmt.Errorf("artifacts: list %s: %w", prefix, readErr)
		}

		var parsed listBucketResult
		if err := xml.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("artifacts: list %s: %w", prefix, err)
		}
		for _, c := range parsed.Contents {
			out = append(out, Object{
				Key: c.Key, SizeBytes: c.Size,
				LastModified: c.LastModified, ETag: strings.Trim(c.ETag, `"`),
			})
		}
		if !parsed.IsTruncated || parsed.NextContinuationToken == "" {
			return out, nil
		}
		token = parsed.NextContinuationToken
	}
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
	} `xml:"Contents"`
}

// PresignGet mints a read capability for one key.
func (s *S3Store) PresignGet(key string, ttl time.Duration) (clusterv1.PresignedURL, error) {
	return s.presign(http.MethodGet, key, nil, ttl)
}

// PresignPut mints a write capability for one key. One key, never a prefix:
// the set of objects the pod writes is known in advance, and a PUT capability
// over a prefix would let a compromised agent fill the bucket.
func (s *S3Store) PresignPut(key string, ttl time.Duration) (clusterv1.PresignedURL, error) {
	return s.presign(http.MethodPut, key, nil, ttl)
}

// PresignPost mints a capability over a prefix, for the objects whose names
// nobody knows when the lease is issued: log chunks, and free-form artifacts.
// The policy bounds both the prefix and the size, which is what keeps a prefix
// capability from being the hole a key capability is not.
func (s *S3Store) PresignPost(prefix string, ttl time.Duration, maxSize int64) (clusterv1.PresignedPostPolicy, error) {
	now := s.now().UTC()
	expires := now.Add(ttl)
	scope := s.scope(now)
	credential := s.cfg.AccessKey + "/" + scope

	conditions := []any{
		map[string]string{"bucket": s.cfg.Bucket},
		[]any{"starts-with", "$key", prefix},
		map[string]string{"x-amz-algorithm": sigV4Algorithm},
		map[string]string{"x-amz-credential": credential},
		map[string]string{"x-amz-date": now.Format(amzDateFormat)},
		[]any{"content-length-range", 0, maxSize},
	}
	if s.cfg.SessionToken != "" {
		conditions = append(conditions, map[string]string{"x-amz-security-token": s.cfg.SessionToken})
	}

	doc, err := json.Marshal(map[string]any{
		"expiration": expires.Format(time.RFC3339),
		"conditions": conditions,
	})
	if err != nil {
		return clusterv1.PresignedPostPolicy{}, fmt.Errorf("artifacts: encode post policy: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(doc)

	fields := map[string]string{
		// ${filename} is substituted by the store from the uploaded part's
		// name, which is how one capability covers chunk 1 and chunk 400.
		"key":              prefix + "${filename}",
		"policy":           encoded,
		"x-amz-algorithm":  sigV4Algorithm,
		"x-amz-credential": credential,
		"x-amz-date":       now.Format(amzDateFormat),
		"x-amz-signature":  hex.EncodeToString(hmacSHA256(s.signingKey(now), []byte(encoded))),
	}
	if s.cfg.SessionToken != "" {
		fields["x-amz-security-token"] = s.cfg.SessionToken
	}

	return clusterv1.PresignedPostPolicy{
		Prefix:       prefix,
		URL:          s.bucketURL(),
		Fields:       fields,
		MaxSizeBytes: maxSize,
		ExpiresAt:    expires,
	}, nil
}

// ---------------------------------------------------------------------------
// SigV4
// ---------------------------------------------------------------------------

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	dateFormat     = "20060102"

	// UNSIGNED-PAYLOAD is required for query-string authentication: the
	// signature is minted before the body exists, and for a GET there is no
	// body at all.
	unsignedPayload = "UNSIGNED-PAYLOAD"

	// MaxPresignTTL is S3's own ceiling on a query-authenticated signature.
	// A bundle asking for longer is a configuration mistake that would
	// otherwise surface as a 403 in a pod an hour into its work.
	MaxPresignTTL = 7 * 24 * time.Hour
)

// presign is the whole of SigV4 query-string authentication.
func (s *S3Store) presign(method, key string, extra url.Values, ttl time.Duration) (clusterv1.PresignedURL, error) {
	if ttl <= 0 {
		return clusterv1.PresignedURL{}, fmt.Errorf("artifacts: presign %s %s: ttl must be positive", method, key)
	}
	if ttl > MaxPresignTTL {
		return clusterv1.PresignedURL{}, fmt.Errorf(
			"artifacts: presign %s %s: ttl %s exceeds the S3 maximum of %s", method, key, ttl, MaxPresignTTL)
	}

	endpoint, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return clusterv1.PresignedURL{}, fmt.Errorf("artifacts: endpoint %q: %w", s.cfg.Endpoint, err)
	}
	host, path := s.address(endpoint, key)

	now := s.now().UTC()
	expires := now.Add(ttl)

	query := url.Values{}
	for k, v := range extra {
		query[k] = v
	}
	query.Set("X-Amz-Algorithm", sigV4Algorithm)
	query.Set("X-Amz-Credential", s.cfg.AccessKey+"/"+s.scope(now))
	query.Set("X-Amz-Date", now.Format(amzDateFormat))
	query.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")
	if s.cfg.SessionToken != "" {
		query.Set("X-Amz-Security-Token", s.cfg.SessionToken)
	}

	canonicalQuery := canonicalQueryString(query)
	canonicalRequest := strings.Join([]string{
		method,
		path,
		canonicalQuery,
		"host:" + host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		now.Format(amzDateFormat),
		s.scope(now),
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(now), []byte(stringToSign)))

	return clusterv1.PresignedURL{
		URL: endpoint.Scheme + "://" + host + path + "?" + canonicalQuery +
			"&X-Amz-Signature=" + signature,
		Method:    method,
		ExpiresAt: expires,
	}, nil
}

// address returns the host and the canonical path for a key, in whichever
// addressing style this endpoint uses.
func (s *S3Store) address(endpoint *url.URL, key string) (host, path string) {
	host = endpoint.Host
	base := strings.TrimSuffix(endpoint.Path, "/")
	if s.cfg.PathStyle {
		path = base + "/" + s.cfg.Bucket
	} else {
		host = s.cfg.Bucket + "." + endpoint.Host
		path = base
	}
	if key != "" {
		path += "/" + uriEncode(key, false)
	}
	if path == "" {
		path = "/"
	}
	return host, path
}

func (s *S3Store) bucketURL() string {
	endpoint, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return s.cfg.Endpoint
	}
	host, path := s.address(endpoint, "")
	return endpoint.Scheme + "://" + host + path
}

func (s *S3Store) scope(now time.Time) string {
	return now.Format(dateFormat) + "/" + s.cfg.Region + "/s3/aws4_request"
}

// signingKey is the four-step derivation. The chain is what makes a leaked
// signing key useless outside one day, one region and one service.
func (s *S3Store) signingKey(now time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), []byte(now.Format(dateFormat)))
	k = hmacSHA256(k, []byte(s.cfg.Region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

// canonicalQueryString encodes exactly as SigV4 requires: sorted by key, then
// by value, with AWS's encoding rather than net/url's. The difference is one
// character — a space is %20 here and "+" there — and it produces a signature
// that verifies everywhere except against a key with a space in it.
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b strings.Builder
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		slices.Sort(values)
		for _, v := range values {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(uriEncode(k, true))
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

// uriEncode is AWS's encoding rule: unreserved characters pass, everything else
// is percent-encoded with uppercase hex, and a slash is encoded only outside a
// path.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// statusDetail is the first part of an error body, for a log line that says
// what the store objected to rather than only that it did.
func statusDetail(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return "status " + strconv.Itoa(resp.StatusCode)
	}
	return "status " + strconv.Itoa(resp.StatusCode) + ": " + detail
}
