package inputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/spool"
)

const testRun = runv1.ULID("01J8X4K2ZQ7YB3M9F0R5W6T8CD")

// stubAPI serves inputs from a map, or fails the way it is told to.
type stubAPI struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	fail    map[string]error
	fetches map[string]int
}

func (s *stubAPI) LeaseInput(_ context.Context, runID runv1.ULID, epoch int64, file string) (*clusterapi.InputBody, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches[file]++
	if err := s.fail[file]; err != nil {
		return nil, err
	}
	body, ok := s.bodies[file]
	if !ok {
		return nil, &clusterv1.Problem{Code: clusterv1.CodeInputNotFound, Action: clusterv1.ActionFatal}
	}
	return &clusterapi.InputBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), SizeBytes: int64(len(body))}, nil
}

func ref(n int, name string, body []byte) runv1.InputRef {
	sum := sha256.Sum256(body)
	return runv1.InputRef{Key: runv1.InputFileName(n, name), Name: name, MediaType: "text/plain",
		SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func setup(t *testing.T, files map[string][]byte) (*Fetcher, *stubAPI, clusterv1.Lease) {
	t.Helper()
	sp, err := spool.Open(spool.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	api := &stubAPI{bodies: map[string][]byte{}, fail: map[string]error{}, fetches: map[string]int{}}
	lease := clusterv1.Lease{RunID: testRun, Epoch: 3}
	names := []string{"screenshot.png", "build.log"}
	for i, name := range names {
		body, ok := files[name]
		if !ok {
			continue
		}
		r := ref(i+1, name, body)
		api.bodies[r.File()] = body
		lease.Inputs = append(lease.Inputs, r)
	}
	return &Fetcher{API: api, Spool: sp}, api, lease
}

func TestEveryInputIsSpooledAndVerifiedBeforeFetchReturns(t *testing.T) {
	f, api, lease := setup(t, map[string][]byte{"screenshot.png": []byte("png"), "build.log": []byte("log\n")})
	rejection, err := f.Fetch(context.Background(), lease)
	if err != nil || rejection != nil {
		t.Fatalf("fetch: %v %+v", err, rejection)
	}
	for _, r := range lease.Inputs {
		if ok, err := f.Spool.InputMatches(lease.RunID, r); err != nil || !ok {
			t.Fatalf("an input is not in the spool after a successful fetch: %v", err)
		}
		if api.fetches[r.File()] != 1 {
			t.Fatalf("an input was fetched %d times", api.fetches[r.File()])
		}
	}
}

func TestAFetchAfterARestartReusesIntactInputsAndRefetchesACorruptOne(t *testing.T) {
	f, api, lease := setup(t, map[string][]byte{"screenshot.png": []byte("png"), "build.log": []byte("log\n")})
	if _, err := f.Fetch(context.Background(), lease); err != nil {
		t.Fatal(err)
	}

	// The same volume, a new process.
	sp, err := spool.Open(spool.Config{Root: f.Spool.Root()})
	if err != nil {
		t.Fatal(err)
	}
	f = &Fetcher{API: api, Spool: sp}
	corrupt := filepath.Join(sp.Root(), "_inputs", string(testRun), lease.Inputs[1].File())
	if err := os.WriteFile(corrupt, []byte("LOG\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if rejection, err := f.Fetch(context.Background(), lease); err != nil || rejection != nil {
		t.Fatalf("fetch after restart: %v %+v", err, rejection)
	}
	if n := api.fetches[lease.Inputs[0].File()]; n != 1 {
		t.Errorf("an intact input was fetched again (%d fetches)", n)
	}
	if n := api.fetches[lease.Inputs[1].File()]; n != 2 {
		t.Errorf("a corrupt input was not fetched again (%d fetches)", n)
	}
	if ok, _ := sp.InputMatches(testRun, lease.Inputs[1]); !ok {
		t.Error("the refetched input does not match")
	}
}

func TestAnInputTheBackendCannotProduceRefusesTheLease(t *testing.T) {
	f, api, lease := setup(t, map[string][]byte{"screenshot.png": []byte("png")})
	delete(api.bodies, lease.Inputs[0].File())

	rejection, err := f.Fetch(context.Background(), lease)
	if err != nil {
		t.Fatalf("a fatal answer came back as an error, which would leave the lease unacknowledged: %v", err)
	}
	if rejection == nil || rejection.Code != clusterv1.RejectMaterializationFailed {
		t.Fatalf("want MaterializationFailed, got %+v", rejection)
	}
	if strings.Contains(rejection.Message, "screenshot") {
		t.Fatalf("the rejection quotes the input's name: %q", rejection.Message)
	}
}

func TestBytesThatDoNotMatchTheManifestRefuseTheLease(t *testing.T) {
	f, api, lease := setup(t, map[string][]byte{"build.log": []byte("log\n")})
	api.bodies[lease.Inputs[0].File()] = []byte("LOG\n")

	rejection, err := f.Fetch(context.Background(), lease)
	if err != nil || rejection == nil || rejection.Code != clusterv1.RejectMaterializationFailed {
		t.Fatalf("want a MaterializationFailed refusal, got %+v, %v", rejection, err)
	}
	if f.Spool.HasInput(testRun, lease.Inputs[0]) {
		t.Fatal("the mismatched bytes were kept")
	}

	// A stated length that differs is refused before the transfer.
	api.bodies[lease.Inputs[0].File()] = []byte("a longer log\n")
	if rejection, err := f.Fetch(context.Background(), lease); err != nil || rejection == nil {
		t.Fatalf("want a refusal on the length, got %+v, %v", rejection, err)
	}
}

func TestAnAbandonOrAnUnreachableBackendMeansNoAckAtAll(t *testing.T) {
	for name, failure := range map[string]error{
		"stale epoch": &clusterv1.Problem{Code: clusterv1.CodeEpochMismatch, Action: clusterv1.ActionAbandon},
		"unavailable": &clusterv1.Problem{Code: clusterv1.CodeUnavailable, Action: clusterv1.ActionBackoff},
		"transport":   &clusterapi.TransportError{Op: "leases/inputs", Err: errors.New("connection refused")},
	} {
		t.Run(name, func(t *testing.T) {
			f, api, lease := setup(t, map[string][]byte{"screenshot.png": []byte("png")})
			api.fail[lease.Inputs[0].File()] = failure
			rejection, err := f.Fetch(context.Background(), lease)
			if err == nil || rejection != nil {
				t.Fatalf("want an error and no rejection, got %+v, %v", rejection, err)
			}
		})
	}
}

func TestAManifestOutsideTheContractIsRefusedAsAnInvalidSpec(t *testing.T) {
	good := ref(1, "a.png", []byte("x"))
	many := make([]runv1.InputRef, clusterv1.MaxInputsPerLease+1)
	for i := range many {
		many[i] = ref(i+1, "a.png", []byte("x"))
	}
	huge := good
	huge.SizeBytes = clusterv1.MaxInputBytesPerLease + 1
	escaping := good
	escaping.Key = "inputs/../result.md"
	upper := good
	upper.SHA256 = strings.ToUpper(good.SHA256)
	negative := good
	negative.SizeBytes = -1

	cases := map[string][]runv1.InputRef{
		"too many":          many,
		"too large":         {huge},
		"an escaping key":   {escaping},
		"a repeated key":    {good, good},
		"an uppercase hash": {upper},
		"a negative size":   {negative},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			rejection := Validate(in)
			if rejection == nil || rejection.Code != clusterv1.RejectInvalidSpec ||
				len(rejection.Fields) != 1 || rejection.Fields[0] != "inputs" {
				t.Fatalf("want InvalidSpec on inputs, got %+v", rejection)
			}
		})
	}
	if rejection := Validate([]runv1.InputRef{good}); rejection != nil {
		t.Fatalf("a valid manifest was refused: %+v", rejection)
	}
}

func TestConfirmRefetchesAnInputRemovedAfterTheFetch(t *testing.T) {
	f, api, lease := setup(t, map[string][]byte{"screenshot.png": []byte("png")})
	if _, err := f.Fetch(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if rejection, err := f.Confirm(context.Background(), lease); err != nil || rejection != nil {
		t.Fatal(err, rejection)
	}
	if api.fetches[lease.Inputs[0].File()] != 1 {
		t.Fatal("Confirm fetched an input that was still there")
	}
	if err := f.Spool.ForgetInputs(testRun); err != nil {
		t.Fatal(err)
	}
	if rejection, err := f.Confirm(context.Background(), lease); err != nil || rejection != nil {
		t.Fatal(err, rejection)
	}
	if !f.Spool.HasInput(testRun, lease.Inputs[0]) || api.fetches[lease.Inputs[0].File()] != 2 {
		t.Fatal("Confirm did not fetch again an input that had gone")
	}
}
