package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The architecture claims that Lease.spec in the Cluster API and spec in the
// AgentRun CRD are the same schema on two carriers. That claim is worth exactly
// as much as the mechanism that enforces it.
//
// The Go types are the source: they carry the validation markers, and the CRD
// is generated from them. The OpenAPI document is hand-written, because its
// prose is what the backend team reads. This test is the join between the two —
// it compares every key that changes what is accepted, and ignores the ones
// that only change what is read.
//
// A failure here is never cosmetic. A bound present on one side and absent on
// the other means the backend can hand out a lease the controller cannot
// materialise, which surfaces as a run that dies at dispatch for no visible
// reason.

// Keys that decide whether a value is accepted. Anything outside this set —
// description, example, x-kubernetes-* — may differ freely.
var significantKeys = []string{
	"type", "format", "enum", "default", "required",
	"minimum", "maximum", "minLength", "maxLength", "pattern",
	"minItems", "maxItems", "maxProperties",
}

// Fields the controller adds when it turns a lease into a CR. They exist in the
// CRD and cannot exist in RenderedRunSpec: the backend does not know a
// cluster-local callback URL, and the epoch is its own to hand out.
var controllerOnlyFields = map[string]bool{
	"runID": true, "leaseEpoch": true, "materials": true, "callbackURL": true,
}

func TestOpenAPIAndCRDAgreeOnRenderedRunSpec(t *testing.T) {
	root := filepath.Join("..", "..")

	openAPI := loadYAML(t, filepath.Join(root, "api", "cluster", "v1", "openapi.yaml"))
	crd := loadYAML(t, filepath.Join(root, "config", "crd", "bases", "haliphron.io_agentruns.yaml"))

	res := &resolver{doc: openAPI}
	wire := res.resolve(t, dig(t, openAPI, "components", "schemas", "RenderedRunSpec"))

	stored := dig(t, crd, "spec", "versions")
	versions, ok := stored.([]any)
	if !ok || len(versions) == 0 {
		t.Fatal("CRD has no versions")
	}
	crdSpec := dig(t, versions[0], "schema", "openAPIV3Schema", "properties", "spec")

	var diffs []string
	compare(res, t, "spec", crdSpec, wire, &diffs)
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("Cluster API and the CRD disagree about the rendered run spec:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

type resolver struct{ doc map[string]any }

// resolve follows $ref and flattens the single-element allOf that OpenAPI needs
// in order to attach a description to a $ref.
func (r *resolver) resolve(t *testing.T, node any) map[string]any {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if ref, ok := m["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		return r.resolve(t, dig(t, r.doc, "components", "schemas", name))
	}
	if all, ok := m["allOf"].([]any); ok {
		out := map[string]any{}
		for _, part := range all {
			for k, v := range r.resolve(t, part) {
				out[k] = v
			}
		}
		for k, v := range m {
			if k != "allOf" {
				out[k] = v
			}
		}
		return out
	}
	return m
}

func compare(r *resolver, t *testing.T, path string, crdNode, wireNode any, diffs *[]string) {
	t.Helper()
	crdM, _ := crdNode.(map[string]any)
	wireM := r.resolve(t, wireNode)

	for _, key := range significantKeys {
		c, hasC := crdM[key]
		w, hasW := wireM[key]
		if key == "required" {
			c, w = sortedStrings(c), sortedStrings(w)
			if path == "spec" {
				c = withoutControllerFields(c)
			}
			hasC, hasW = hasC && len(c.([]string)) > 0, hasW && len(w.([]string)) > 0
		}
		if key == "enum" {
			c, w = sortedStrings(c), sortedStrings(w)
		}
		switch {
		case hasC && !hasW:
			*diffs = append(*diffs, fmt.Sprintf("%s: CRD has %s=%v, OpenAPI has none", path, key, c))
		case !hasC && hasW:
			*diffs = append(*diffs, fmt.Sprintf("%s: OpenAPI has %s=%v, CRD has none", path, key, w))
		case hasC && hasW && !equalScalar(c, w):
			*diffs = append(*diffs, fmt.Sprintf("%s: %s CRD=%v OpenAPI=%v", path, key, c, w))
		}
	}

	crdProps, _ := crdM["properties"].(map[string]any)
	wireProps, _ := wireM["properties"].(map[string]any)
	seen := map[string]bool{}
	for name, sub := range crdProps {
		if path == "spec" && controllerOnlyFields[name] {
			continue
		}
		seen[name] = true
		w, ok := wireProps[name]
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in the CRD, missing from the Cluster API", path, name))
			continue
		}
		compare(r, t, path+"."+name, sub, w, diffs)
	}
	for name := range wireProps {
		if !seen[name] {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in the Cluster API, missing from the CRD", path, name))
		}
	}

	if c, ok := crdM["items"]; ok {
		if w, ok := wireM["items"]; ok {
			compare(r, t, path+"[]", c, w, diffs)
		}
	}
	if c, ok := crdM["additionalProperties"]; ok {
		if w, ok := wireM["additionalProperties"]; ok {
			if _, isBool := w.(bool); !isBool {
				compare(r, t, path+"{}", c, w, diffs)
			}
		}
	}
}

// withoutControllerFields drops the fields the controller adds from a required
// list, so that the CRD's "the controller must fill these in" does not read as
// a disagreement with the Cluster API.
func withoutControllerFields(v any) any {
	list, ok := v.([]string)
	if !ok {
		return v
	}
	out := make([]string, 0, len(list))
	for _, name := range list {
		if !controllerOnlyFields[name] {
			out = append(out, name)
		}
	}
	return out
}

func equalScalar(a, b any) bool {
	an, aok := toFloat(a)
	bn, bok := toFloat(b)
	if aok && bok {
		return an == bn
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func sortedStrings(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	sort.Strings(out)
	return out
}

func dig(t *testing.T, node any, path ...string) any {
	t.Helper()
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("path %v: not an object at %q", path, key)
		}
		node, ok = m[key]
		if !ok {
			t.Fatalf("path %v: missing %q", path, key)
		}
	}
	return node
}

func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}
