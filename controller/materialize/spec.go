package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// HashSpec digests the rendered spec exactly as it arrived in the lease. Go
// marshals struct fields in declaration order and map keys sorted, so the
// encoding is stable without a canonicaliser.
//
// The digest goes into an annotation because annotations are the one part of
// the object a structural schema does not prune. That is the whole trick: an
// API server whose CRD predates a spec field accepts the object, silently drops
// the field and answers 201, and the only evidence left is the hash of what was
// sent sitting next to the object that no longer contains it.
func HashSpec(spec runv1.RenderedRunSpec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LostFields names what the write dropped: the paths present in what was sent
// and absent or altered in what came back.
//
// The comparison is deliberately one-directional. A field the API server *added*
// is a default being applied — ttlSecondsAfterFinished, baseBranch, createPR —
// and a bidirectional comparison would report every defaulted field as pruned
// and reject every lease that omitted an optional value. Only a loss matters,
// because only a loss means the run would execute as something other than what
// was admitted.
func LostFields(sent, stored runv1.RenderedRunSpec) []string {
	var a, b map[string]any
	rawSent, err := json.Marshal(sent)
	if err != nil {
		return nil
	}
	rawStored, err := json.Marshal(stored)
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(rawSent, &a); err != nil {
		return nil
	}
	if err := json.Unmarshal(rawStored, &b); err != nil {
		return nil
	}

	var out []string
	diffPaths("", a, b, &out)
	sort.Strings(out)
	return out
}

// diffPaths walks the sent object and records every leaf the stored object does
// not reproduce. Naming the path is the point: a rejection that says "something
// was pruned" sends an operator to read CRD YAML, one that says
// runtime.mcpServers does not.
func diffPaths(prefix string, sent, stored map[string]any, out *[]string) {
	for key, value := range sent {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		other, ok := stored[key]
		if !ok {
			*out = append(*out, path)
			continue
		}
		sentChild, sentIsMap := value.(map[string]any)
		storedChild, storedIsMap := other.(map[string]any)
		if sentIsMap && storedIsMap {
			diffPaths(path, sentChild, storedChild, out)
			continue
		}
		if !reflect.DeepEqual(value, other) {
			*out = append(*out, path)
		}
	}
}
