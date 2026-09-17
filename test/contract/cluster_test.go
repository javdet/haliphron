package contract

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The Cluster API has the same problem as the runtime contract and a different
// shape of it: the OpenAPI is hand-written because its prose is what the two
// teams read, and the Go types are hand-written because they carry the reasons
// the schema cannot. Neither generates the other, so nothing but a test stops
// them diverging — and the way that divergence presents is a field the backend
// fills and the controller never sees, on a protocol where the two sides are
// deployed and upgraded separately.

const clusterAPIDoc = "api/cluster/v1/openapi.yaml"

func TestClusterAPIGoTypesMatchSchema(t *testing.T) {
	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "cluster", "v1", "openapi.yaml"))
	res := &resolver{doc: doc}

	// Every message either side puts on the wire. RenderedRunSpec and
	// CompletionReport are absent on purpose: they are run/v1 types travelling
	// on this carrier, and they already have their own drift tests.
	cases := []struct {
		schema string
		typ    reflect.Type
	}{
		{"Problem", reflect.TypeOf(clusterv1.Problem{})},
		{"RegisterRequest", reflect.TypeOf(clusterv1.RegisterRequest{})},
		{"RegisterResponse", reflect.TypeOf(clusterv1.RegisterResponse{})},
		{"PublicKey", reflect.TypeOf(clusterv1.PublicKey{})},
		{"Timings", reflect.TypeOf(clusterv1.Timings{})},
		{"LeaseRequest", reflect.TypeOf(clusterv1.LeaseRequest{})},
		{"LeaseResponse", reflect.TypeOf(clusterv1.LeaseResponse{})},
		{"Lease", reflect.TypeOf(clusterv1.Lease{})},
		{"ArtifactBundleRequest", reflect.TypeOf(clusterv1.ArtifactBundleRequest{})},
		{"ArtifactBundle", reflect.TypeOf(clusterv1.ArtifactBundle{})},
		{"PresignedURL", reflect.TypeOf(clusterv1.PresignedURL{})},
		{"PresignedPostPolicy", reflect.TypeOf(clusterv1.PresignedPostPolicy{})},
		{"AckRequest", reflect.TypeOf(clusterv1.AckRequest{})},
		{"AckResponse", reflect.TypeOf(clusterv1.AckResponse{})},
		{"HeartbeatRequest", reflect.TypeOf(clusterv1.HeartbeatRequest{})},
		{"HeartbeatResponse", reflect.TypeOf(clusterv1.HeartbeatResponse{})},
		{"RunObservation", reflect.TypeOf(clusterv1.RunObservation{})},
		{"ControllerHealth", reflect.TypeOf(clusterv1.ControllerHealth{})},
		{"ClusterFacts", reflect.TypeOf(clusterv1.ClusterFacts{})},
		{"Command", reflect.TypeOf(clusterv1.Command{})},
		{"StatusIngestRequest", reflect.TypeOf(clusterv1.StatusIngestRequest{})},
		{"StatusIngestResponse", reflect.TypeOf(clusterv1.StatusIngestResponse{})},
		{"StatusIngestResult", reflect.TypeOf(clusterv1.StatusIngestResult{})},
		{"CompletionIngestRequest", reflect.TypeOf(clusterv1.CompletionIngestRequest{})},
		{"CompletionIngestResponse", reflect.TypeOf(clusterv1.CompletionIngestResponse{})},
	}

	var diffs []string
	for _, tc := range cases {
		compareGoToSchema(t, res, tc.schema, tc.typ,
			dig(t, doc, "components", "schemas", tc.schema), &diffs)
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("the Cluster API types and %s disagree:\n  %s",
			clusterAPIDoc, strings.Join(diffs, "\n  "))
	}
}

// The enums are the other half. A code or an action the backend can emit and
// the controller cannot name is a branch that falls through to a default, and
// the default for an unrecognised instruction is to guess.
func TestProblemCodesMatchSchema(t *testing.T) {
	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "cluster", "v1", "openapi.yaml"))

	assertEnumMatches(t, "Problem.code",
		enumOf(t, dig(t, doc, "components", "schemas", "Problem", "properties", "code")),
		[]string{
			string(clusterv1.CodeInvalidRequest), string(clusterv1.CodeUnauthenticated),
			string(clusterv1.CodeClusterRevoked), string(clusterv1.CodeClusterMismatch),
			string(clusterv1.CodeClusterNameTaken), string(clusterv1.CodeBootstrapTokenInvalid),
			string(clusterv1.CodeBootstrapTokenConsumed), string(clusterv1.CodeUnsupportedControllerVersion),
			string(clusterv1.CodeRunNotFound), string(clusterv1.CodeEpochMismatch),
			string(clusterv1.CodeRunLeasedByAnotherCluster), string(clusterv1.CodeRunTerminal),
			string(clusterv1.CodePhaseRegression), string(clusterv1.CodeAttemptRegression),
			string(clusterv1.CodeLeaseExpired), string(clusterv1.CodePayloadTooLarge),
			string(clusterv1.CodeRateLimited), string(clusterv1.CodeUnavailable),
			string(clusterv1.CodeInternal),
		})

	assertEnumMatches(t, "Problem.action",
		enumOf(t, dig(t, doc, "components", "schemas", "Problem", "properties", "action")),
		[]string{
			string(clusterv1.ActionRetry), string(clusterv1.ActionBackoff),
			string(clusterv1.ActionAbandon), string(clusterv1.ActionResync),
			string(clusterv1.ActionReregister), string(clusterv1.ActionFatal),
		})

	assertEnumMatches(t, "Command.type",
		enumOf(t, dig(t, doc, "components", "schemas", "Command", "properties", "type")),
		[]string{string(clusterv1.CommandCancel), string(clusterv1.CommandAbandon)})

	assertEnumMatches(t, "AckRequest.rejection.code",
		enumOf(t, dig(t, doc, "components", "schemas", "AckRequest",
			"properties", "rejection", "properties", "code")),
		[]string{
			string(clusterv1.RejectInvalidSpec), string(clusterv1.RejectSpecFieldsPruned),
			string(clusterv1.RejectQuotaExhausted), string(clusterv1.RejectImageNotAllowed),
			string(clusterv1.RejectMaterializationFailed),
		})
}

// The backend's own status names never appear in the OpenAPI — they are strings
// in a description — but the controller reads them out of AckResponse.status
// and StatusIngestResult.appliedStatus, and the mapping from an observed phase
// is the contract's table in section 5.
func TestPhaseToStatusMapping(t *testing.T) {
	want := map[runv1.Phase]string{
		runv1.PhasePending:   clusterv1.StatusDispatched,
		runv1.PhaseStarting:  clusterv1.StatusStarting,
		runv1.PhaseRunning:   clusterv1.StatusRunning,
		runv1.PhaseSucceeded: clusterv1.StatusSucceeded,
		runv1.PhaseFailed:    clusterv1.StatusFailed,
		runv1.PhaseTimedOut:  clusterv1.StatusTimedOut,
		runv1.PhaseCancelled: clusterv1.StatusCancelled,
	}
	for phase, status := range want {
		if got := clusterv1.StatusForPhase(phase); got != status {
			t.Errorf("phase %s maps to %q, want %q", phase, got, status)
		}
	}
	// Unknown is the backend's alone. A controller able to report it could
	// declare a run lost that it is in fact still running.
	if got := clusterv1.StatusForPhase("Unknown"); got != "" {
		t.Errorf("a backend-only status was produced from a phase: %q", got)
	}
}

// The defaults are handed out at registration rather than read from each
// cluster's values.yaml, so every installation agrees on what staleAfter means.
// They are stated twice — in the schema and in Go — and the pair below is the
// only thing keeping the two copies equal.
func TestDefaultTimingsMatchTheSchema(t *testing.T) {
	doc := loadYAML(t, filepath.Join(repoRoot(), "api", "cluster", "v1", "openapi.yaml"))
	props, ok := dig(t, doc, "components", "schemas", "Timings", "properties").(map[string]any)
	if !ok {
		t.Fatal("Timings has no properties")
	}

	timings := clusterv1.DefaultTimings()
	got := map[string]float64{
		"heartbeatIntervalSeconds": float64(timings.HeartbeatIntervalSeconds),
		"staleAfterSeconds":        float64(timings.StaleAfterSeconds),
		"leaseTTLSeconds":          float64(timings.LeaseTTLSeconds),
		"ackTimeoutSeconds":        float64(timings.AckTimeoutSeconds),
		"maxWaitSeconds":           float64(timings.MaxWaitSeconds),
		"maxLeasesPerPoll":         float64(timings.MaxLeasesPerPoll),
		"artifactTTLMultiplier":    float64(timings.ArtifactTTLMultiplier),
	}
	for name, value := range got {
		schema, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s: not in the schema", name)
			continue
		}
		want, ok := toFloat(schema["default"])
		if !ok {
			t.Errorf("%s: the schema states no default", name)
			continue
		}
		if want != value {
			t.Errorf("%s: Go says %v, the schema says %v", name, value, want)
		}
	}
}

func enumOf(t *testing.T, node any) []string {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatal("not a schema object")
	}
	list, ok := m["enum"].([]any)
	if !ok {
		t.Fatal("no enum")
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func assertEnumMatches(t *testing.T, what string, schema, gone []string) {
	t.Helper()
	sort.Strings(schema)
	sorted := append([]string(nil), gone...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(schema, sorted) {
		t.Errorf("%s: schema has %v, Go has %v", what, schema, sorted)
	}
}
