package clusterapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

func TestAnInputIsStreamedWithItsEpochAndTheCallsUsualHeaders(t *testing.T) {
	var got *http.Request
	client, logged := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Length", "5")
		w.Header().Set(clusterv1.HeaderArtifactSHA256, "abc")
		_, _ = w.Write([]byte("bytes"))
	})
	body, err := client.LeaseInput(context.Background(), "01J8X4K2ZQ7YB3M9F0R5W6T8CD", 4, "1-screenshot.png")
	if err != nil {
		t.Fatalf("lease input: %v", err)
	}
	defer func() { _ = body.Close() }()
	raw, _ := io.ReadAll(body)
	if string(raw) != "bytes" || body.SizeBytes != 5 || body.SHA256 != "abc" {
		t.Fatalf("got %q, size %d, digest %q", raw, body.SizeBytes, body.SHA256)
	}
	switch {
	case got.Method != http.MethodGet:
		t.Errorf("method %s", got.Method)
	case got.URL.Path != clusterv1.BasePath+"/leases/01J8X4K2ZQ7YB3M9F0R5W6T8CD/inputs/1-screenshot.png":
		t.Errorf("path %s", got.URL.Path)
	case got.URL.Query().Get(clusterv1.QueryEpoch) != "4":
		t.Errorf("epoch %q", got.URL.Query().Get(clusterv1.QueryEpoch))
	case got.Header.Get("Authorization") != "Bearer token":
		t.Error("the call is not authenticated")
	case got.Header.Get(clusterv1.HeaderControllerVersion) == "":
		t.Error("the controller version header is missing")
	}
	if strings.Contains(logged.String(), "screenshot") {
		t.Fatalf("the input's name was logged: %s", logged.String())
	}
}

func TestAnInputThatIsGoneComesBackAsAFatalProblem(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(clusterv1.Problem{
			Title: "no such input", Code: clusterv1.CodeInputNotFound, Action: clusterv1.ActionFatal,
		})
	})
	_, err := client.LeaseInput(context.Background(), "01J8X4K2ZQ7YB3M9F0R5W6T8CD", 1, "1-a.png")
	p, ok := ProblemOf(err)
	if !ok || p.Code != clusterv1.CodeInputNotFound || ActionOf(err) != clusterv1.ActionFatal {
		t.Fatalf("want InputNotFound/fatal, got %v", err)
	}
}

func TestATransportFailureOnAnInputDoesNotQuoteTheURL(t *testing.T) {
	client, err := New(Options{BaseURL: "http://127.0.0.1:1", ControllerVersion: "0.1.0",
		Signer: stubSigner{id: "01J8X4K2ZQ7YB3M9F0R5W6T8CD"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LeaseInput(context.Background(), "01J8X4K2ZQ7YB3M9F0R5W6T8CD", 1, "1-salary-review.pdf")
	if err == nil {
		t.Fatal("a refused connection was not an error")
	}
	if ActionOf(err) != clusterv1.ActionRetry {
		t.Fatalf("a transport failure is not retry: %v", err)
	}
	if strings.Contains(err.Error(), "salary") {
		t.Fatalf("the error quotes the file name: %v", err)
	}
}
