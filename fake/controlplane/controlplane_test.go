package controlplane_test

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
)

// The fake's own contract tests. They check the half of its behaviour that the
// image will lean on and that a permissive stub would get wrong: that a
// capability is a capability rather than a URL, that a missing checkpoint is a
// 404 and not a 500, and that the callback endpoint refuses the three things it
// exists to refuse.

func newPlane(t *testing.T, opts ...controlplane.Option) *controlplane.ControlPlane {
	t.Helper()
	cp, srv := controlplane.NewServer(opts...)
	t.Cleanup(srv.Close)
	return cp
}

// materialize stages a run into a temporary directory and takes the layout's
// own cleanup, which knows how to get past the 0500 secrets directory. Cleanups
// run last-registered-first, so this one empties the tree before t.TempDir
// tries to.
func materialize(t *testing.T, p *controlplane.Prepared) controlplane.Layout {
	t.Helper()
	layout, err := p.Materialize(t.TempDir())
	t.Cleanup(func() {
		if err := layout.Remove(); err != nil {
			t.Errorf("removing the layout: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return layout
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, body
}

func get(t *testing.T, link clusterv1.PresignedURL) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, link.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return do(t, req)
}

func put(t *testing.T, link clusterv1.PresignedURL, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, link.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return do(t, req)
}

func TestPresignedGETReturnsThePromptTheBackendWrote(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "add a postgres database"})

	resp, body := get(t, p.Bundle.Get[runv1.StorageKeyPrompt])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt GET: got %d, want 200 (%s)", resp.StatusCode, body)
	}
	if string(body) != "add a postgres database" {
		t.Fatalf("prompt body: got %q", body)
	}
	// The digest the pod is told to check against is the digest of what is
	// actually there. If these two ever diverge the image fails every run with
	// PromptDigestMismatch and the cause is the harness.
	if p.Env[runv1.EnvPromptSHA256] == "" {
		t.Fatal("no prompt digest in the environment: the pod cannot verify what it runs")
	}
}

func TestACheckpointThatWasNeverWrittenIs404(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	resp, _ := get(t, p.Bundle.Get[runv1.StorageKeyState])
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("state.json on a first attempt: got %d, want 404 — a 500 here "+
			"would teach the image to treat the normal case as a failure", resp.StatusCode)
	}
}

func TestAGETCapabilityCannotWrite(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	// The signature covers the method. Without that, the split between Get and
	// Put in the bundle is documentation rather than a boundary.
	link := p.Bundle.Get[runv1.StorageKeyPrompt]
	resp, body := put(t, link, "rewritten")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT through a GET link: got %d, want 403 (%s)", resp.StatusCode, body)
	}
	if stored, _ := cp.Object(p.Prefix + runv1.StorageKeyPrompt); string(stored) != "hello" {
		t.Fatalf("the prompt was overwritten through a read capability: %q", stored)
	}
}

func TestATamperedSignatureIsRefused(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	link := p.Bundle.Put[runv1.StorageKeyResult]
	// Repoint the same signature at a different key: the key is in the signed
	// material precisely so that this fails.
	link.URL = strings.Replace(link.URL, runv1.StorageKeyResult, runv1.StorageKeyOutput, 1)
	resp, body := put(t, link, "{}")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT to a key the signature does not cover: got %d, want 403 (%s)", resp.StatusCode, body)
	}
}

func TestAnExpiredSignatureIs403AndAReissuedBundleWorks(t *testing.T) {
	t.Parallel()
	cp := newPlane(t, controlplane.WithSignatureTTL(10*time.Minute))
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	cp.Advance(11 * time.Minute)
	resp, body := put(t, p.Bundle.Put[runv1.StorageKeyResult], "# done")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT with an expired signature: got %d, want 403 (%s)", resp.StatusCode, body)
	}

	// The one failure the cluster repairs by itself: the controller mints a new
	// bundle before the next attempt. That is why the contract classes an
	// expired signature as infra and retries it.
	next := cp.Reissue(p, 2)
	if !next.Bundle.ExpiresAt.After(p.Bundle.ExpiresAt) {
		t.Fatalf("reissued bundle expires at %s, not after the original %s",
			next.Bundle.ExpiresAt, p.Bundle.ExpiresAt)
	}
	resp, body = put(t, next.Bundle.Put[runv1.StorageKeyResult], "# done")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with the reissued signature: got %d, want 200 (%s)", resp.StatusCode, body)
	}
	if next.Env[runv1.EnvAttempt] != "2" {
		t.Fatalf("reissued environment says attempt %q, want 2", next.Env[runv1.EnvAttempt])
	}
}

// postChunk uploads through the prefix capability the way the image will: a
// multipart form carrying the policy back verbatim.
func postChunk(t *testing.T, policy clusterv1.PresignedPostPolicy, key, body string) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	for name, value := range policy.Fields {
		if name == "key" {
			value = key
		}
		if err := form.WriteField(name, value); err != nil {
			t.Fatalf("form field %s: %v", name, err)
		}
	}
	part, err := form.CreateFormFile("file", filepath.Base(key))
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := io.WriteString(part, body); err != nil {
		t.Fatalf("form body: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("form close: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, policy.URL, &buf)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	return do(t, req)
}

func TestAPOSTPolicyCoversItsPrefixAndNothingElse(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	mine := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})
	theirs := cp.Prepare(controlplane.RunRequest{Prompt: "somebody else's run"})

	policy := mine.Bundle.Post[0]
	key := policy.Prefix + "000000.log"
	if resp, body := postChunk(t, policy, key, "chunk zero"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk inside the prefix: got %d, want 204 (%s)", resp.StatusCode, body)
	}
	if stored, ok := cp.Object(key); !ok || string(stored) != "chunk zero" {
		t.Fatalf("chunk not stored under %s: %q", key, stored)
	}

	// The whole point of scoping the capability to a prefix: a compromised pod
	// cannot write into another run.
	outside := theirs.Prefix + runv1.StoragePrefixChunks + "000000.log"
	resp, body := postChunk(t, policy, outside, "not mine")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("chunk outside the prefix: got %d, want 403 (%s)", resp.StatusCode, body)
	}
	if _, ok := cp.Object(outside); ok {
		t.Fatal("a run wrote into another run's prefix")
	}
}

func TestChunkKeysSortInUploadOrder(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})
	policy := p.Bundle.Post[0]

	// Six digits with leading zeros, so that listing — which is lexicographic —
	// gives the order the log was written in. Without the padding the tenth
	// chunk sorts between the first and the second.
	for _, seq := range []string{"000000", "000001", "000002", "000010"} {
		if resp, body := postChunk(t, policy, policy.Prefix+seq+".log", seq); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("chunk %s: got %d (%s)", seq, resp.StatusCode, body)
		}
	}
	got := cp.Keys(policy.Prefix)
	want := []string{
		policy.Prefix + "000000.log", policy.Prefix + "000001.log",
		policy.Prefix + "000002.log", policy.Prefix + "000010.log",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chunks list as %v, want %v", got, want)
	}
}

func postReport(t *testing.T, cp *controlplane.ControlPlane, token string, report runv1.CompletionReport) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cp.BaseURL()+"/runtime/v1/completion", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Haliphron-Contract", runv1.ContractVersion)
	return do(t, req)
}

func reportFor(p *controlplane.Prepared) runv1.CompletionReport {
	return runv1.CompletionReport{
		RunID:    p.RunID,
		Attempt:  p.Attempt,
		Status:   runv1.CompletionSuccess,
		ExitCode: runv1.ExitSuccess,
		Agent:    runv1.AgentClaudeCode,
	}
}

func TestTheCompletionEndpointRefusesWhatItExistsToRefuse(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	mine := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})
	theirs := cp.Prepare(controlplane.RunRequest{Prompt: "somebody else's run"})

	tests := []struct {
		name   string
		token  string
		report runv1.CompletionReport
		want   int
	}{
		{"no token at all", "", reportFor(mine), http.StatusUnauthorized},
		{"a token nobody minted", "deadbeef", reportFor(mine), http.StatusUnauthorized},
		{"another run's token", theirs.CallbackToken, reportFor(mine), http.StatusConflict},
		{"its own token", mine.CallbackToken, reportFor(mine), http.StatusAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postReport(t, cp, tc.token, tc.report)
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestARepeatedReportIsAcceptedAndMarkedDuplicate(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	// At-least-once delivery: an image that retries a report it never saw
	// acknowledged is behaving correctly, and the receiver is obliged to be
	// idempotent on (runID, attempt) rather than to record a second run.
	for i := range 2 {
		resp, body := postReport(t, cp, p.CallbackToken, reportFor(p))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("delivery %d: got %d, want 202 (%s)", i, resp.StatusCode, body)
		}
	}
	reports := cp.Reports(p.RunID)
	if len(reports) != 2 {
		t.Fatalf("saw %d deliveries, want 2", len(reports))
	}
	if reports[0].Duplicate {
		t.Fatal("the first delivery was marked a duplicate")
	}
	if !reports[1].Duplicate {
		t.Fatal("the repeat was not marked a duplicate")
	}
}

func TestAReportFromAnEarlierAttemptCannotOverwriteALaterOne(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	later := reportFor(p)
	later.Attempt = 2
	if resp, body := postReport(t, cp, p.CallbackToken, later); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("attempt 2: got %d (%s)", resp.StatusCode, body)
	}
	// A delayed report from attempt 1 arriving after attempt 2 succeeded would
	// move the run backwards. Ordering is by attempt, never by arrival: cluster
	// clocks are not synchronised.
	earlier := reportFor(p)
	earlier.Attempt = 1
	if resp, body := postReport(t, cp, p.CallbackToken, earlier); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale attempt 1: got %d, want 409 (%s)", resp.StatusCode, body)
	}
	accepted, ok := cp.AcceptedReport(p.RunID)
	if !ok || accepted.Attempt != 2 {
		t.Fatalf("accepted report is attempt %d, want 2", accepted.Attempt)
	}
}

func TestInjectedFaultsAreNarrowedByMethodAndKey(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	// The scenario the contract's phase ordering exists for: persist succeeds,
	// what comes after it does not. A blanket fault cannot express it, and
	// without it the "the result is already durable" rows are untestable.
	cp.FailStorage(http.MethodPut, runv1.StorageKeyCompletion, http.StatusForbidden, 0)

	if resp, body := put(t, p.Bundle.Put[runv1.StorageKeyResult], "# done"); resp.StatusCode != http.StatusOK {
		t.Fatalf("result.md: got %d, want 200 (%s)", resp.StatusCode, body)
	}
	if resp, _ := put(t, p.Bundle.Put[runv1.StorageKeyCompletion], "{}"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("completion.json: got %d, want the injected 403", resp.StatusCode)
	}

	cp.ClearStorageFaults()
	if resp, body := put(t, p.Bundle.Put[runv1.StorageKeyCompletion], "{}"); resp.StatusCode != http.StatusOK {
		t.Fatalf("after clearing: got %d, want 200 (%s)", resp.StatusCode, body)
	}
}

func TestACallbackFaultExpiresAfterItsCount(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	cp.FailCallback(http.StatusServiceUnavailable, 2)
	for i := range 2 {
		if resp, _ := postReport(t, cp, p.CallbackToken, reportFor(p)); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("delivery %d: got %d, want the injected 503", i, resp.StatusCode)
		}
	}
	if resp, body := postReport(t, cp, p.CallbackToken, reportFor(p)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the third delivery: got %d, want 202 (%s)", resp.StatusCode, body)
	}
	if got := cp.CallbackAttempts(); got != 3 {
		t.Fatalf("the endpoint counted %d deliveries, want 3 — an image's retry "+
			"budget is only checkable if the refused ones are counted too", got)
	}
}

func TestMaterializeReproducesTheMountsTheControllerCreates(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{
		Prompt:     "hello",
		Role:       "coder",
		RoleConfig: map[string]string{runv1.RoleConfigKeyOutputSchema: `{"type":"object"}`},
	})

	layout := materialize(t, p)

	// The five keys of the per-run Secret, as files. Not envFrom: none of these
	// names is a valid variable name, and an image that expected variables
	// would start with no git token and no links to storage.
	for _, key := range []string{
		runv1.SecretKeyGitToken, runv1.SecretKeyLLMAPIKey, runv1.SecretKeyMCPConfig,
		runv1.SecretKeyPresigned, runv1.SecretKeyCallbackToken,
	} {
		info, err := os.Stat(filepath.Join(layout.SecretsDir, key))
		if err != nil {
			t.Fatalf("secret %s: %v", key, err)
		}
		// 0400 in the cluster. An image that writes into its own secret mount
		// works against a laxer harness and fails in production.
		if perm := info.Mode().Perm(); perm != 0o400 {
			t.Fatalf("secret %s has mode %v, want 0400", key, perm)
		}
	}

	var bundle clusterv1.ArtifactBundle
	raw, err := os.ReadFile(filepath.Join(layout.SecretsDir, runv1.SecretKeyPresigned))
	if err != nil {
		t.Fatalf("presigned.json: %v", err)
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("presigned.json does not parse: %v", err)
	}
	if bundle.Get[runv1.StorageKeyPrompt].URL == "" {
		t.Fatal("no presigned GET for prompt.txt: the pod has no task")
	}
	if bundle.Get[runv1.StorageKeyState].URL == "" {
		t.Fatal("no presigned GET for state.json: an idempotent retry becomes impossible")
	}

	schema := filepath.Join(layout.RoleDir, runv1.RoleConfigKeyOutputSchema)
	if _, err := os.Stat(schema); err != nil {
		t.Fatalf("the node's output schema did not reach the role mount: %v", err)
	}

	envFile, err := os.ReadFile(layout.EnvFile)
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	// Nothing secret may be in the environment: it is visible in `kubectl
	// describe pod` and inherited by the agent, which is the one process here
	// assumed capable of exfiltrating what it can read.
	for _, secret := range []string{
		p.Secrets[runv1.SecretKeyGitToken],
		p.Secrets[runv1.SecretKeyLLMAPIKey],
		p.CallbackToken,
	} {
		if bytes.Contains(envFile, []byte(secret)) {
			t.Fatalf("a secret reached the environment file: %q", secret)
		}
	}
	if bytes.Contains(envFile, []byte("sig=")) {
		t.Fatal("a presigned signature reached the environment file")
	}
}

func TestEveryContractVariableIsEitherSetOrOptional(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{
		Prompt: "hello", RepoURL: "https://example.invalid/org/repo.git",
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main",
		TargetBranch: "haliphron/abc-feature", CreatePR: true,
		Role: "coder", PermissionMode: "acceptEdits",
		AllowedTools: []string{"Read", "Edit"}, DeniedTools: []string{"WebFetch"},
		MaxTurns: 40, CloneDepth: 1, LogChunkSeconds: 5,
	})

	// A variable this harness sets that the contract does not define means the
	// image is being taught something the controller will never say.
	known := map[string]bool{}
	for _, name := range runv1.ContractEnv {
		known[name] = true
	}
	for name := range p.Env {
		if !known[name] {
			t.Errorf("%s is set by the harness but is not in the contract", name)
		}
	}
	// The mandatory ones, from the table in section 2.1.
	for _, name := range []string{
		runv1.EnvContract, runv1.EnvRunID, runv1.EnvAttempt, runv1.EnvCallbackURL,
		runv1.EnvGraceSeconds, runv1.EnvAgent, runv1.EnvModel,
		runv1.EnvTimeoutSeconds, runv1.EnvPromptSHA256,
	} {
		if p.Env[name] == "" {
			t.Errorf("%s is required by the contract and was not set", name)
		}
	}
}

func TestAnEnvOverrideCanRemoveARequiredVariable(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	// The only way to reach the checklist row "a required variable is missing →
	// 30 before any network call".
	p := cp.Prepare(controlplane.RunRequest{
		Prompt: "hello",
		Env:    map[string]string{runv1.EnvPromptSHA256: ""},
	})
	if _, ok := p.Env[runv1.EnvPromptSHA256]; ok {
		t.Fatal("an empty override left the variable in place")
	}
}

func TestSecretsAreNotWorldReadableOnDisk(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})
	layout := materialize(t, p)
	err := filepath.WalkDir(layout.SecretsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable beyond its owner: %v", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestAMaterializedLayoutCanBeRemovedWithoutBeingRoot(t *testing.T) {
	t.Parallel()
	cp := newPlane(t)
	p := cp.Prepare(controlplane.RunRequest{Prompt: "hello"})

	// Not t.TempDir with the helper: this test owns the removal it is checking.
	root := filepath.Join(t.TempDir(), "mounts")
	layout, err := p.Materialize(root)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := layout.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the layout survived Remove: %v", err)
	}
	// Idempotent, because it runs from a cleanup that may follow another one.
	if err := layout.Remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
