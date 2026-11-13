// Package rawspec is the Docker host's half of the pruning guard.
//
// On Kubernetes the trap is the CRD: an older structural schema deletes spec
// fields it does not know and answers 201. Here there is no CRD, but there is
// the same trap one step earlier — Go's JSON decoder drops fields the struct
// does not have, silently. A control plane newer than this binary could send a
// setting this binary would never apply, and the run would succeed meaning
// something else.
//
// The lease is decoded by controller/clusterapi, which stays the only place
// that reads a Problem's action and the only place a lease is parsed. So the
// raw bytes are taken here, from the transport underneath it: a RoundTripper
// that, for the lease endpoint only, keeps each lease's raw "spec" object until
// the materialiser asks for it. Nothing in this package logs a body; the bytes
// are held in memory for the seconds between the poll and the materialisation
// and then dropped.
package rawspec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Tap captures raw lease specs. It is safe for concurrent use.
type Tap struct {
	Next http.RoundTripper

	mu    sync.Mutex
	specs map[key]json.RawMessage
}

type key struct {
	runID runv1.ULID
	epoch int64
}

// RoundTrip implements http.RoundTripper.
func (t *Tap) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next().RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(req.URL.Path, clusterv1.BasePath+"/clusters/") ||
		!strings.HasSuffix(req.URL.Path, "/leases") {
		return resp, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, clusterv1.MaxRequestBytes))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if readErr != nil {
		// The client reads the same truncated body and reports a transport
		// error. Nothing to capture.
		return resp, nil
	}
	var envelope struct {
		Leases []struct {
			RunID runv1.ULID      `json:"runID"`
			Epoch int64           `json:"epoch"`
			Spec  json.RawMessage `json:"spec"`
		} `json:"leases"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return resp, nil
	}
	t.mu.Lock()
	if t.specs == nil {
		t.specs = map[key]json.RawMessage{}
	}
	for _, l := range envelope.Leases {
		t.specs[key{l.RunID, l.Epoch}] = l.Spec
	}
	t.mu.Unlock()
	return resp, nil
}

func (t *Tap) next() http.RoundTripper {
	if t.Next != nil {
		return t.Next
	}
	return http.DefaultTransport
}

// Take returns and forgets the raw spec of one lease. ok is false when the
// lease did not pass through this tap — a test that hands leases to the
// materialiser directly — in which case the guard has nothing to compare.
func (t *Tap) Take(runID runv1.ULID, epoch int64) (json.RawMessage, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, ok := t.specs[key{runID, epoch}]
	delete(t.specs, key{runID, epoch})
	return raw, ok
}

// Digest is the SHA-256 of the raw bytes, for the row's diagnostic column.
func Digest(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Dropped names the paths present in the raw spec and lost by decoding it into
// RenderedRunSpec and encoding it again.
//
// One-directional, as on Kubernetes: a field the struct adds is not a loss.
// And a field sent with its zero value — false, 0, "", null, {} or [] — is
// not a loss either: the struct's omitempty drops it on the way back out, and
// the zero value is exactly what an absent field means to the image.
func Dropped(raw []byte, decoded runv1.RenderedRunSpec) []string {
	if len(raw) == 0 {
		return nil
	}
	var sent map[string]any
	if json.Unmarshal(raw, &sent) != nil {
		return nil
	}
	back, err := json.Marshal(decoded)
	if err != nil {
		return nil
	}
	var kept map[string]any
	if json.Unmarshal(back, &kept) != nil {
		return nil
	}
	var out []string
	walk("", sent, kept, &out)
	sort.Strings(out)
	return out
}

func walk(prefix string, sent, kept map[string]any, out *[]string) {
	for k, v := range sent {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		other, ok := kept[k]
		if !ok {
			if !zero(v) {
				*out = append(*out, path)
			}
			continue
		}
		sm, sIsMap := v.(map[string]any)
		km, kIsMap := other.(map[string]any)
		if sIsMap && kIsMap {
			walk(path, sm, km, out)
			continue
		}
		sl, sIsList := v.([]any)
		kl, kIsList := other.([]any)
		if sIsList && kIsList && len(sl) == len(kl) {
			for i := range sl {
				a, aok := sl[i].(map[string]any)
				b, bok := kl[i].(map[string]any)
				if aok && bok {
					walk(path, a, b, out)
				} else if !reflect.DeepEqual(sl[i], kl[i]) {
					*out = append(*out, path)
					break
				}
			}
			continue
		}
		if !reflect.DeepEqual(v, other) {
			*out = append(*out, path)
		}
	}
}

func zero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, inner := range x {
			if !zero(inner) {
				return false
			}
		}
		return true
	}
	return false
}
