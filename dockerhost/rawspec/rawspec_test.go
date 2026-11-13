package rawspec

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

func decode(t *testing.T, raw string) runv1.RenderedRunSpec {
	t.Helper()
	var s runv1.RenderedRunSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAFieldThisBinaryDoesNotKnowIsNamed(t *testing.T) {
	raw := `{"agent":"claude-code","promptSHA256":"a","model":"m","image":"i","repo":{},
		"runtime":{"timeoutSeconds":60,"sandbox":{"profile":"strict"}},"newTopLevel":"x"}`
	got := Dropped([]byte(raw), decode(t, raw))
	if !slices.Equal(got, []string{"newTopLevel", "runtime.sandbox"}) {
		t.Fatalf("got %v", got)
	}
}

func TestZeroValuesAndKnownFieldsAreNotLosses(t *testing.T) {
	raw := `{"agent":"claude-code","promptSHA256":"a","model":"m","image":"i",
		"repo":{"url":"u","submodules":false,"createPR":false},"runtime":{"timeoutSeconds":60,"maxTurns":0,
		"env":[{"name":"A","value":"1"}],"resources":{}},"futureFlag":false,"futureList":[]}`
	if got := Dropped([]byte(raw), decode(t, raw)); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestTheTapKeepsEachLeasesRawSpecUntilTaken(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"leases":[{"runID":"R1","epoch":4,"spec":{"agent":"x","extra":1}}],"serverTime":"2026-01-01T00:00:00Z"}`)
	}))
	defer backend.Close()
	tap := &Tap{}
	client := &http.Client{Transport: tap}
	resp, err := client.Post(backend.URL+clusterv1.BasePath+"/clusters/C/leases", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"extra":1`) {
		t.Fatal("the client did not get the body back")
	}
	raw, ok := tap.Take("R1", 4)
	if !ok || !strings.Contains(string(raw), `"extra":1`) {
		t.Fatalf("raw %s ok %v", raw, ok)
	}
	if _, ok := tap.Take("R1", 4); ok {
		t.Fatal("a raw spec was kept after it was taken")
	}
}
