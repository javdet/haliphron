package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The runtime contract has a problem the other two do not: its primary
// implementation is a shell script, and nothing about a shell script is checked
// by a compiler. The variable names, the phase names, the exit codes and the
// file layout exist in three places — the Go constants the controller builds
// the container from, the OpenAPI the controller parses the report with, and
// the prose table the person writing the entrypoint actually reads.
//
// These tests are the join. A documentation table that can drift from the code
// is not documentation, it is a hypothesis.

const runtimeDoc = "docs/contracts/agent-runtime.md"

func repoRoot() string { return filepath.Join("..", "..") }

// ---------------------------------------------------------------------------
// The report travels unchanged, so both documents must describe it identically.
// ---------------------------------------------------------------------------

func TestRuntimeAndClusterAPIAgreeOnCompletionReport(t *testing.T) {
	runtimeAPI := loadYAML(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "openapi.yaml"))
	clusterAPI := loadYAML(t, filepath.Join(repoRoot(), "api", "cluster", "v1", "openapi.yaml"))

	rt := &resolver{doc: runtimeAPI}
	cl := &resolver{doc: clusterAPI}

	var diffs []string
	compareWire(t, "CompletionReport",
		rt, dig(t, runtimeAPI, "components", "schemas", "CompletionReport"),
		cl, dig(t, clusterAPI, "components", "schemas", "CompletionReport"),
		&diffs)

	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("the pod writes one report and the backend reads another:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

// compareWire is the two-document twin of compare: both sides carry $ref, and
// neither is the source. Only keys that decide whether a value is accepted are
// considered; prose and examples may differ, and do.
func compareWire(t *testing.T, path string, lr *resolver, ln any, rr *resolver, rn any, diffs *[]string) {
	t.Helper()
	l, r := lr.resolve(t, ln), rr.resolve(t, rn)

	for _, key := range significantKeys {
		a, hasA := l[key]
		b, hasB := r[key]
		if key == "required" || key == "enum" {
			a, b = sortedStrings(a), sortedStrings(b)
		}
		switch {
		case hasA && !hasB:
			*diffs = append(*diffs, fmt.Sprintf("%s: runtime has %s=%v, cluster has none", path, key, a))
		case !hasA && hasB:
			*diffs = append(*diffs, fmt.Sprintf("%s: cluster has %s=%v, runtime has none", path, key, b))
		case hasA && hasB && !equalScalar(a, b):
			*diffs = append(*diffs, fmt.Sprintf("%s: %s runtime=%v cluster=%v", path, key, a, b))
		}
	}

	lp, _ := l["properties"].(map[string]any)
	rp, _ := r["properties"].(map[string]any)
	for name, sub := range lp {
		other, ok := rp[name]
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in the runtime API, missing from the Cluster API", path, name))
			continue
		}
		compareWire(t, path+"."+name, lr, sub, rr, other, diffs)
	}
	for name := range rp {
		if _, ok := lp[name]; !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in the Cluster API, missing from the runtime API", path, name))
		}
	}

	if a, ok := l["items"]; ok {
		if b, ok := r["items"]; ok {
			compareWire(t, path+"[]", lr, a, rr, b, diffs)
		}
	}
}

// ---------------------------------------------------------------------------
// The Go type is what the controller forwards with. A field the schema promises
// and the struct drops is a field that arrives at the backend as absent.
// ---------------------------------------------------------------------------

func TestCompletionReportGoTypeMatchesSchema(t *testing.T) {
	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "openapi.yaml"))
	res := &resolver{doc: doc}

	var diffs []string
	compareGoToSchema(t, res, "CompletionReport",
		reflect.TypeOf(runv1.CompletionReport{}),
		dig(t, doc, "components", "schemas", "CompletionReport"), &diffs)

	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("CompletionReport disagrees with its schema:\n  %s", strings.Join(diffs, "\n  "))
	}
}

func compareGoToSchema(t *testing.T, res *resolver, path string, typ reflect.Type, node any, diffs *[]string) {
	t.Helper()
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	schema := res.resolve(t, node)
	props, _ := schema["properties"].(map[string]any)

	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := jsonName(typ.Field(i))
		if name == "" {
			continue
		}
		seen[name] = true
		sub, ok := props[name]
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in Go, missing from the schema", path, name))
			continue
		}
		compareGoToSchema(t, res, path+"."+name, typ.Field(i).Type, itemsOf(res, t, sub), diffs)
	}
	for name := range props {
		if !seen[name] {
			*diffs = append(*diffs, fmt.Sprintf("%s.%s: in the schema, missing from Go", path, name))
		}
	}
}

// itemsOf descends into an array schema so that a []T in Go is compared against
// the element schema rather than against the array wrapper.
func itemsOf(res *resolver, t *testing.T, node any) any {
	m := res.resolve(t, node)
	if items, ok := m["items"]; ok {
		return items
	}
	return node
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

// ---------------------------------------------------------------------------
// Doc tables against Go. The image is written from the tables.
// ---------------------------------------------------------------------------

func TestContractEnvMatchesDocumentedTable(t *testing.T) {
	rows := markdownTable(t, runtimeDoc, "| Variable |")
	documented := make([]string, 0, len(rows))
	for _, row := range rows {
		documented = append(documented, unquote(row[0]))
	}
	assertSameSet(t, "environment variables", runv1.ContractEnv, documented)
}

func TestRuntimePhasesMatchDocumentedTable(t *testing.T) {
	rows := markdownTable(t, runtimeDoc, "| # | Phase |")
	if len(rows) != len(runv1.RuntimePhases) {
		t.Fatalf("phase table has %d rows, RuntimePhases has %d", len(rows), len(runv1.RuntimePhases))
	}
	for i, row := range rows {
		if got, want := strconv.Itoa(i+1), row[0]; got != want {
			t.Errorf("phase row %d is numbered %q; the order is contract, so the numbers are too", i+1, want)
		}
		if got, want := unquote(row[1]), string(runv1.RuntimePhases[i]); got != want {
			t.Errorf("phase %d: documented %q, Go has %q", i+1, got, want)
		}
	}
}

func TestExitCodeTableMatchesGo(t *testing.T) {
	rows := markdownTable(t, runtimeDoc, "| Code | Class |")

	documented := map[int32]string{}
	sawSignals := false
	for _, row := range rows {
		class := unquote(row[1])
		if strings.HasPrefix(row[0], ">") {
			sawSignals = true
			// The row covers every code above 128; check the two that matter.
			for _, code := range []int32{137, 143} {
				if got := string(runv1.FailureClassForExitCode(code)); got != class {
					t.Errorf("exit %d: documented %q, Go says %q", code, class, got)
				}
			}
			continue
		}
		code, err := strconv.Atoi(row[0])
		if err != nil {
			t.Fatalf("exit code table: %q is not a code", row[0])
		}
		documented[int32(code)] = class
	}
	if !sawSignals {
		t.Error("exit code table says nothing about signals; every eviction lands there")
	}

	for code, class := range documented {
		if got := string(runv1.FailureClassForExitCode(code)); got != class {
			t.Errorf("exit %d: documented %q, Go says %q", code, class, got)
		}
	}
	for _, code := range runv1.ExitCodes {
		if _, ok := documented[code]; !ok {
			t.Errorf("exit %d exists in Go and is documented nowhere", code)
		}
	}
}

// ---------------------------------------------------------------------------
// The JSON Schemas the image validates against.
// ---------------------------------------------------------------------------

func TestStateSchemaCoversEveryPhase(t *testing.T) {
	schema := loadJSON(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "state.schema.json"))
	props, ok := dig(t, schema, "properties", "phases", "properties").(map[string]any)
	if !ok {
		t.Fatal("state.schema.json: phases has no properties")
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	assertSameSet(t, "checkpoint phases", phaseStrings(), keys)

	// additionalProperties:false is the point of the enumeration: a phase
	// renamed in Go and not here must fail loudly, not be accepted as an extra.
	if v, ok := dig(t, schema, "properties", "phases", "additionalProperties").(bool); !ok || v {
		t.Error("phases accepts unknown keys, which makes the enumeration decorative")
	}
}

func TestRuntimePhaseEnumMatchesGo(t *testing.T) {
	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "openapi.yaml"))
	enum := dig(t, doc, "components", "schemas", "RuntimePhase", "enum")
	assertSameSet(t, "RuntimePhase enum", phaseStrings(), toStrings(enum))
}

func TestOutputEnvelopeIsEntrypointFilled(t *testing.T) {
	schema := loadJSON(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "output.schema.json"))

	// Everything required in the envelope is something the entrypoint knows and
	// the model would have to be told to produce. That asymmetry is the whole
	// decision (R5); if `data` ever becomes required, the model is back on the
	// hook for an envelope field and the decision has quietly been reversed.
	required := toStrings(dig(t, schema, "required"))
	for _, field := range required {
		if field == "summary" || field == "artifacts" {
			t.Errorf("%q is required; the entrypoint cannot always produce it", field)
		}
	}
	props := dig(t, schema, "properties", "data")
	if _, ok := props.(map[string]any)["default"]; !ok {
		t.Error("data has no default; a run without a declared node schema produces no payload and must still be valid")
	}
}

// ---------------------------------------------------------------------------

func phaseStrings() []string {
	out := make([]string, 0, len(runv1.RuntimePhases))
	for _, p := range runv1.RuntimePhases {
		out = append(out, string(p))
	}
	return out
}

func assertSameSet(t *testing.T, what string, want, got []string) {
	t.Helper()
	inWant := map[string]bool{}
	for _, v := range want {
		inWant[v] = true
	}
	inGot := map[string]bool{}
	for _, v := range got {
		inGot[v] = true
	}
	var problems []string
	for _, v := range want {
		if !inGot[v] {
			problems = append(problems, fmt.Sprintf("%q is in Go and not in the contract", v))
		}
	}
	for _, v := range got {
		if !inWant[v] {
			problems = append(problems, fmt.Sprintf("%q is in the contract and not in Go", v))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%s have drifted:\n  %s", what, strings.Join(problems, "\n  "))
	}
}

// markdownTable returns the body rows of the table whose header line starts
// with the given prefix. Cells are trimmed; the alignment row is dropped.
func markdownTable(t *testing.T, path, headerPrefix string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, headerPrefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no table starting with %q", path, headerPrefix)
	}
	var rows [][]string
	for _, line := range lines[start+1:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := splitRow(line)
		if len(cells) == 0 || strings.HasPrefix(cells[0], "---") {
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: table %q is empty", path, headerPrefix)
	}
	return rows
}

func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func unquote(cell string) string { return strings.Trim(cell, "`") }

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func loadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}
