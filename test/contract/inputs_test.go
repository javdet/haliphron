package contract

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Input keys are checked in three places — the backend before it stores, the
// controller before it spools, the pod before it writes into the work tree —
// from one function, for the reason ArtifactKey states. These tests are the
// shape that function promises.

func TestAnInputKeyIsOneFileUnderTheInputsPrefix(t *testing.T) {
	t.Parallel()

	good := []string{
		"inputs/1-screenshot.png",
		"inputs/10-build.log",
		"inputs/999-a",
		"inputs/2-Report_2026-11-17.final.pdf",
	}
	for _, key := range good {
		if _, err := runv1.InputKey(key); err != nil {
			t.Errorf("InputKey(%q) = %v, want accepted", key, err)
		}
	}

	bad := []string{
		"",
		"screenshot.png",
		"inputs/",
		"inputs/screenshot.png",
		"inputs/1-",
		"inputs/-x.png",
		"inputs/1000-x.png",
		"inputs/1-.hidden",
		"inputs/1-_x",
		"inputs/1-a/b.png",
		"inputs/1-../../etc/passwd",
		"inputs/1-a..b",
		"inputs/../result.md",
		"/inputs/1-x.png",
		"inputs//1-x.png",
		"inputs/1-x.png.meta",
		"inputs/1-x.partial",
		"inputs/1-a b.png",
		"inputs/1-ünïcode.png",
		"artifacts/1-x.png",
		"inputs/1-" + strings.Repeat("a", 200),
	}
	for _, key := range bad {
		if _, err := runv1.InputKey(key); err == nil {
			t.Errorf("InputKey(%q) accepted, want refused", key)
		}
	}
}

func TestEveryNameAPersonGivesAFileBecomesAKeyEverySideAccepts(t *testing.T) {
	t.Parallel()

	names := []string{
		"screenshot.png",
		"Screen Shot 2026-11-17 at 10.42.13.png",
		"../../etc/passwd",
		"..",
		".",
		"",
		".env",
		"___",
		"отчёт о сбое.pdf",
		"日本語のファイル.txt",
		"a/b\\c:d*e?f\"g<h>i|j",
		"x.meta",
		"x.meta.meta",
		"y.partial",
		strings.Repeat("long", 100) + ".png",
		strings.Repeat("e", 300),
		"name\nwith\nnewlines.log",
		"-leading-dash.txt",
	}
	for i, name := range names {
		key := runv1.InputFileName(i+1, name)
		if _, err := runv1.InputKey(key); err != nil {
			t.Errorf("InputFileName(%d, %q) = %q, which InputKey refuses: %v", i+1, name, key, err)
		}
		if !strings.HasPrefix(key, runv1.StoragePrefixInputs) {
			t.Errorf("InputFileName(%d, %q) = %q, outside %s", i+1, name, key, runv1.StoragePrefixInputs)
		}
	}

	// A long name is cut keeping its extension, so a PDF stays a .pdf.
	if key := runv1.InputFileName(1, strings.Repeat("x", 400)+".pdf"); !strings.HasSuffix(key, ".pdf") {
		t.Errorf("a long PDF's key is %q; the extension was lost", key)
	}
	// Two files with one name are two keys: the ordinal is the key.
	if runv1.InputFileName(1, "a.png") == runv1.InputFileName(2, "a.png") {
		t.Error("two inputs with the same name got the same key")
	}
}

func TestARunMayNotWriteUnderTheInputsPrefix(t *testing.T) {
	t.Parallel()

	// What the UI shows as what the person sent must not be something the
	// agent can overwrite, on the relay or anywhere else ArtifactKey guards.
	for _, key := range []string{"inputs/1-screenshot.png", "inputs/x", "inputs/"} {
		if _, err := runv1.ArtifactKey(key); err == nil {
			t.Errorf("ArtifactKey(%q) accepted, want refused", key)
		}
	}
}

func TestTheInputsPhaseComesAfterTheCloneAndBeforeTheRole(t *testing.T) {
	t.Parallel()

	clone := slices.Index(runv1.RuntimePhases, runv1.RuntimePhaseClone)
	inputs := slices.Index(runv1.RuntimePhases, runv1.RuntimePhaseInputs)
	role := slices.Index(runv1.RuntimePhases, runv1.RuntimePhaseRole)
	if clone < 0 || inputs != clone+1 || role != inputs+1 {
		t.Fatalf("the phases run clone=%d inputs=%d role=%d; inputs must sit between the clone and the role",
			clone, inputs, role)
	}
	if runv1.RuntimePhaseInputs.Resumable() {
		t.Error("the inputs phase is resumable; its files live in the pod's emptyDir and are gone with it")
	}
}

func TestTheLeaseInputCeilingIsTheSchemas(t *testing.T) {
	t.Parallel()

	doc := loadYAML(t, filepath.Join(repoRoot(), clusterAPIDoc))
	maxItems := dig(t, doc, "components", "schemas", "Lease", "properties", "inputs", "maxItems")
	if fmt.Sprint(maxItems) != fmt.Sprint(clusterv1.MaxInputsPerLease) {
		t.Errorf("Lease.inputs maxItems is %v, MaxInputsPerLease is %d", maxItems, clusterv1.MaxInputsPerLease)
	}
	// The path the controller fetches from is in the schema, as a GET.
	if _, ok := dig(t, doc, "paths", "/leases/{runID}/inputs/{file}").(map[string]any)["get"]; !ok {
		t.Error("the schema has no GET /leases/{runID}/inputs/{file}")
	}
}

func TestTheInputsCallbackRouteIsInTheRuntimeSchema(t *testing.T) {
	t.Parallel()

	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "runtime", "v1", "openapi.yaml"))
	route, ok := dig(t, doc, "paths", runv1.CallbackPathInputs).(map[string]any)
	if !ok {
		t.Fatalf("the runtime schema has no %s path", runv1.CallbackPathInputs)
	}
	if _, ok := route["get"]; !ok {
		t.Errorf("%s is not a GET in the runtime schema", runv1.CallbackPathInputs)
	}
}
