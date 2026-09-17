package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Storage, as far as the pod is concerned: three verbs over signed links and no
// credential of its own. The pod holds no access key, cannot list, and cannot
// reach a key outside its own prefix — that is ADR 14, and it is the property
// this half exists to make testable.
//
// The signature covers the method, the bucket, the key and the expiry. Leaving
// the method out would let a pod that was handed a GET link turn it into a PUT,
// which is exactly the capability the split between Get and Put in the bundle
// is there to deny.

// sign produces the signature for one capability.
func (c *ControlPlane) sign(method, key string, expires time.Time) string {
	mac := hmac.New(sha256.New, c.signKey)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%d", method, c.bucket, key, expires.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}

// presign mints one capability: a URL the pod can use once or many times until
// the expiry, and for nothing else.
func (c *ControlPlane) presign(method, key string, expires time.Time) clusterv1.PresignedURL {
	q := url.Values{
		"method": {method},
		"exp":    {strconv.FormatInt(expires.Unix(), 10)},
		"sig":    {c.sign(method, key, expires)},
	}
	return clusterv1.PresignedURL{
		URL:       c.baseURL + "/storage/" + c.bucket + "/" + key + "?" + q.Encode(),
		Method:    method,
		ExpiresAt: expires,
	}
}

// postPolicy is the signed document behind a presigned POST. It travels to the
// pod inside the bundle's Fields and comes back verbatim, which is what lets
// the fake check a key it never saw at minting time.
type postPolicy struct {
	Prefix  string `json:"prefix"`
	Expires int64  `json:"expires"`
	MaxSize int64  `json:"maxSize"`
}

func (c *ControlPlane) signPolicy(encoded string) string {
	mac := hmac.New(sha256.New, c.signKey)
	fmt.Fprintf(mac, "POST\n%s\n%s", c.bucket, encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

// presignPost mints a capability over a prefix rather than a key: log chunks
// and free-form artifacts have names nobody knows in advance.
func (c *ControlPlane) presignPost(prefix string, expires time.Time, maxSize int64) clusterv1.PresignedPostPolicy {
	doc, err := json.Marshal(postPolicy{Prefix: prefix, Expires: expires.Unix(), MaxSize: maxSize})
	if err != nil {
		panic("controlplane: policy does not marshal: " + err.Error())
	}
	encoded := base64.StdEncoding.EncodeToString(doc)
	return clusterv1.PresignedPostPolicy{
		Prefix: prefix,
		URL:    c.baseURL + "/storage/" + c.bucket,
		Fields: map[string]string{
			// ${filename} is S3's own placeholder, kept because the image will
			// meet it again against the real thing.
			"key":       prefix + "${filename}",
			"policy":    encoded,
			"signature": c.signPolicy(encoded),
		},
		MaxSizeBytes: maxSize,
		ExpiresAt:    expires,
	}
}

// storageError writes the answer S3 would, in the shape the image must parse:
// a status code, and a code string in the body. The image classifies on the
// status; the body is there so that a failing test prints something a person
// can act on.
func (c *ControlPlane) storageError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// authorize verifies a signed link. Every failure here is a 403, as S3 answers:
// an expired signature and a forged one are indistinguishable to the caller,
// and the image is obliged to treat both as exit 21 rather than guessing which
// it was.
func (c *ControlPlane) authorize(r *http.Request, method, bucket, key string) error {
	if bucket != c.bucket {
		return errors.New("no such bucket")
	}
	q := r.URL.Query()
	if q.Get("method") != method {
		return errors.New("signature is not for this method")
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		return errors.New("malformed expiry")
	}
	if c.now().After(time.Unix(exp, 0)) {
		return errors.New("signature expired")
	}
	want := c.sign(method, key, time.Unix(exp, 0))
	if !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		return errors.New("signature does not verify")
	}
	return nil
}

func (c *ControlPlane) handleGet(w http.ResponseWriter, r *http.Request) {
	bucket, key := r.PathValue("bucket"), r.PathValue("key")

	c.mu.Lock()
	defer c.mu.Unlock()

	if status, code := c.storageFault(http.MethodGet, key); status != 0 {
		c.logf("GET %s -> injected %d", key, status)
		c.storageError(w, status, code, "injected fault")
		return
	}
	if err := c.authorize(r, http.MethodGet, bucket, key); err != nil {
		c.logf("GET %s -> 403 %v", key, err)
		c.storageError(w, http.StatusForbidden, "AccessDenied", err.Error())
		return
	}
	obj, ok := c.objects[key]
	if !ok {
		// The normal answer for state.json on a first attempt. An image that
		// treats it as a failure fails every run it ever makes.
		c.logf("GET %s -> 404", key)
		c.storageError(w, http.StatusNotFound, "NoSuchKey", "no such key: "+key)
		return
	}
	c.logf("GET %s -> 200 (%d bytes)", key, len(obj.body))
	if obj.contentType != "" {
		w.Header().Set("Content-Type", obj.contentType)
	}
	w.Header().Set("ETag", `"`+obj.sha256+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
	_, _ = w.Write(obj.body)
}

func (c *ControlPlane) handlePut(w http.ResponseWriter, r *http.Request) {
	bucket, key := r.PathValue("bucket"), r.PathValue("key")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxObjectBytes+1))
	if err != nil {
		c.storageError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	if len(body) > maxObjectBytes {
		c.storageError(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "object exceeds the fake's ceiling")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if status, code := c.storageFault(http.MethodPut, key); status != 0 {
		c.logf("PUT %s -> injected %d", key, status)
		c.storageError(w, status, code, "injected fault")
		return
	}
	if err := c.authorize(r, http.MethodPut, bucket, key); err != nil {
		c.logf("PUT %s -> 403 %v", key, err)
		c.storageError(w, http.StatusForbidden, "AccessDenied", err.Error())
		return
	}
	c.store(key, body, r.Header.Get("Content-Type"))
	c.logf("PUT %s -> 200 (%d bytes)", key, len(body))
	w.Header().Set("ETag", `"`+c.objects[key].sha256+`"`)
	w.WriteHeader(http.StatusOK)
}

// handlePost is the prefix capability. The key arrives in the form rather than
// the path, so the check is against the policy the pod sent back — which is
// the only reason the policy is signed at all.
func (c *ControlPlane) handlePost(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		c.storageError(w, http.StatusBadRequest, "MalformedPOSTRequest", err.Error())
		return
	}
	encoded := r.FormValue("policy")
	key := r.FormValue("key")

	c.mu.Lock()
	defer c.mu.Unlock()

	if status, code := c.storageFault(http.MethodPost, key); status != 0 {
		c.logf("POST %s -> injected %d", key, status)
		c.storageError(w, status, code, "injected fault")
		return
	}
	if bucket != c.bucket {
		c.storageError(w, http.StatusForbidden, "AccessDenied", "no such bucket")
		return
	}
	if !hmac.Equal([]byte(c.signPolicy(encoded)), []byte(r.FormValue("signature"))) {
		c.logf("POST %s -> 403 policy signature", key)
		c.storageError(w, http.StatusForbidden, "AccessDenied", "policy signature does not verify")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		c.storageError(w, http.StatusBadRequest, "MalformedPolicyDocument", err.Error())
		return
	}
	var policy postPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		c.storageError(w, http.StatusBadRequest, "MalformedPolicyDocument", err.Error())
		return
	}
	if c.now().After(time.Unix(policy.Expires, 0)) {
		c.logf("POST %s -> 403 expired", key)
		c.storageError(w, http.StatusForbidden, "AccessDenied", "policy expired")
		return
	}
	// The condition that makes the capability a capability: a pod handed a
	// policy over runs/A/logs/chunks/ must not be able to write runs/B/.
	if !strings.HasPrefix(key, policy.Prefix) {
		c.logf("POST %s -> 403 outside prefix %s", key, policy.Prefix)
		c.storageError(w, http.StatusForbidden, "AccessDenied",
			"key "+key+" is outside the policy prefix "+policy.Prefix)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		c.storageError(w, http.StatusBadRequest, "MalformedPOSTRequest", "no file part: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxObjectBytes+1))
	if err != nil {
		c.storageError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	if policy.MaxSize > 0 && int64(len(body)) > policy.MaxSize {
		c.logf("POST %s -> 413 (%d > %d)", key, len(body), policy.MaxSize)
		c.storageError(w, http.StatusRequestEntityTooLarge, "EntityTooLarge",
			"body exceeds the policy's maxSize")
		return
	}
	c.store(key, body, header.Header.Get("Content-Type"))
	c.logf("POST %s -> 204 (%d bytes)", key, len(body))
	w.WriteHeader(http.StatusNoContent)
}

// store writes an object. The caller holds the lock.
func (c *ControlPlane) store(key string, body []byte, contentType string) {
	sum := sha256.Sum256(body)
	c.objects[key] = &object{
		body:        body,
		contentType: contentType,
		sha256:      hex.EncodeToString(sum[:]),
		writtenAt:   c.now(),
	}
}

// Put writes an object behind the pod's back. It is how a test arranges the
// world the image wakes up in — a checkpoint from a previous attempt, a prompt
// whose digest does not match what the pod was told.
func (c *ControlPlane) Put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store(key, body, "")
}

// Object returns what is stored under a key.
func (c *ControlPlane) Object(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), obj.body...), true
}

// Keys lists what has been written under a prefix, sorted. The pod cannot list;
// the test can, and "the chunks are numbered continuously across attempts" is
// not checkable any other way.
func (c *ControlPlane) Keys(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// RunKeys lists everything written under one run's prefix.
func (c *ControlPlane) RunKeys(id runv1.ULID) []string {
	return c.Keys(fmt.Sprintf(runv1.StoragePrefixRun, id))
}

// Delete removes an object. It exists for one checklist row: a checkpoint that
// claims run succeeded while the result it points at is gone must produce a
// full run, not a resumed one that reports someone else's work.
func (c *ControlPlane) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, key)
}
