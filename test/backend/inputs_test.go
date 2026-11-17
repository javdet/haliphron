package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
)

// Files a person sends with a request: stored at admission, listed in the
// lease, fetched by the controller before it acknowledges.

// pngBytes is the smallest thing http.DetectContentType calls image/png.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// getInput is the controller's fetch of one input.
func (p *probe) getInput(runID runv1.ULID, file string, epoch int64) (int, []byte, http.Header, *clusterv1.Problem) {
	p.t.Helper()
	url := fmt.Sprintf("%s%s/leases/%s/inputs/%s?%s=%d",
		p.base, clusterv1.BasePath, runID, file, clusterv1.QueryEpoch, epoch)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		p.t.Fatalf("build the input request: %v", err)
	}
	req.Header.Set(clusterv1.HeaderControllerVersion, p.version)
	req.Header.Set("Authorization", "Bearer "+p.token())
	resp, err := p.http.Do(req)
	if err != nil {
		p.t.Fatalf("fetch the input: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var problem clusterv1.Problem
		if err := json.Unmarshal(body, &problem); err != nil {
			p.t.Fatalf("the input endpoint answered %d with an unparsable body: %s", resp.StatusCode, body)
		}
		return resp.StatusCode, nil, resp.Header, &problem
	}
	return resp.StatusCode, body, resp.Header, nil
}

func TestARunAdmittedWithFilesCarriesTheirManifestInItsLeaseAndServesThemToItsHolder(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	text := []byte("panic: runtime error: index out of range\n")

	r := h.Submit(func(req *run.SubmitRequest) {
		req.Inputs = []run.InputFile{
			{Name: "Screen Shot.png", Body: pngBytes, SourceMessageID: "post-1"},
			{Name: "crash.log", Body: text},
		}
	})

	lease := p.LeaseOne()
	if lease.RunID != r.ID {
		t.Fatalf("leased %s, want %s", lease.RunID, r.ID)
	}
	if len(lease.Inputs) != 2 {
		t.Fatalf("the lease lists %d inputs, want 2: %+v", len(lease.Inputs), lease.Inputs)
	}
	shot, log := lease.Inputs[0], lease.Inputs[1]
	if shot.Key != "inputs/1-Screen_Shot.png" || shot.Name != "Screen Shot.png" ||
		shot.MediaType != "image/png" || shot.SizeBytes != int64(len(pngBytes)) || shot.SHA256 != digest(pngBytes) {
		t.Errorf("the screenshot's entry is %+v", shot)
	}
	if log.Key != "inputs/2-crash.log" || !strings.HasPrefix(log.MediaType, "text/plain") {
		t.Errorf("the log's entry is %+v", log)
	}
	// The spec carries nothing of them: the names are customer context.
	if spec, _ := json.Marshal(lease.Spec); bytes.Contains(spec, []byte("Screen Shot")) {
		t.Error("an input's name reached the spec")
	}

	status, body, header, problem := p.getInput(r.ID, shot.File(), lease.Epoch)
	if problem != nil || status != http.StatusOK {
		t.Fatalf("fetching the screenshot: %d %+v", status, problem)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Error("the screenshot came back with different bytes")
	}
	if header.Get(clusterv1.HeaderArtifactSHA256) != digest(pngBytes) || header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v, want the digest and no-store", header)
	}

	// An epoch the backend never issued is a defect, not a race.
	if status, _, _, problem := p.getInput(r.ID, shot.File(), lease.Epoch+1); status != http.StatusBadRequest ||
		problem == nil || problem.Action != clusterv1.ActionFatal {
		t.Errorf("an epoch never issued answered %d %+v, want 400 fatal", status, problem)
	}
	// A file the manifest does not name is gone for good.
	status, _, _, problem = p.getInput(r.ID, "3-other.png", lease.Epoch)
	if status != http.StatusNotFound || problem == nil ||
		problem.Code != clusterv1.CodeInputNotFound || problem.Action != clusterv1.ActionFatal {
		t.Errorf("an unknown input answered %d %+v, want 404 InputNotFound fatal", status, problem)
	}
	// So is one whose object the store has lost.
	if err := h.Artifacts.Delete(context.Background(), artifacts.Key(r.ID, log.Key)); err != nil {
		t.Fatal(err)
	}
	status, _, _, problem = p.getInput(r.ID, log.File(), lease.Epoch)
	if status != http.StatusNotFound || problem == nil || problem.Code != clusterv1.CodeInputNotFound {
		t.Errorf("a lost input answered %d %+v, want 404 InputNotFound", status, problem)
	}

	for _, line := range h.Logs.Lines() {
		if strings.Contains(line, "Screen Shot") || strings.Contains(line, "index out of range") {
			t.Errorf("an input's name or bytes are in the log: %s", line)
		}
	}
}

func TestAStaleEpochCannotReadARunsInputs(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	r := h.Submit(func(req *run.SubmitRequest) {
		req.Inputs = []run.InputFile{{Name: "a.png", Body: pngBytes}}
	})
	lease := p.LeaseOne()

	if _, problem := p.Reject(r.ID, lease.Epoch, clusterv1.RejectMaterializationFailed, "test"); problem != nil {
		t.Fatalf("reject: %+v", problem)
	}
	status, _, _, problem := p.getInput(r.ID, lease.Inputs[0].File(), lease.Epoch)
	if status != http.StatusConflict || problem == nil || problem.Code != clusterv1.CodeEpochMismatch ||
		problem.Action != clusterv1.ActionAbandon {
		t.Errorf("the refusing cluster's epoch answered %d %+v, want 409 abandon", status, problem)
	}
}

func TestAFileTheAgentCannotReadIsRefusedAtAdmissionAndNothingIsStored(t *testing.T) {
	h := newHarness(t)
	elf := append([]byte("\x7fELF\x02\x01\x01"), bytes.Repeat([]byte{0}, 64)...)

	_, err := h.App.Submit(context.Background(), run.SubmitRequest{
		Prompt: "look", CreatedBy: "test", CreatedVia: "api",
		Inputs: []run.InputFile{{Name: "screenshot.png", Body: elf}},
	}, app.SubmitOptions{})
	var invalid *run.InvalidRequestError
	if !errors.As(err, &invalid) || invalid.Field != "inputs" {
		t.Fatalf("an executable named .png was admitted: %v", err)
	}
	if objects, _ := h.Artifacts.List(context.Background(), "runs/"); len(objects) != 0 {
		t.Errorf("%d objects stored for a refused request", len(objects))
	}
}

func TestARefusedInsertTakesItsStoredFilesWithIt(t *testing.T) {
	h := newHarness(t)

	// A parent that does not exist: the insert fails on its foreign key after
	// the files were stored.
	_, err := h.App.Submit(context.Background(), run.SubmitRequest{
		Prompt: "look", CreatedBy: "test", CreatedVia: "api",
		ParentRunID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Depth: 1,
		Inputs: []run.InputFile{{Name: "a.png", Body: pngBytes}},
	}, app.SubmitOptions{})
	if err == nil {
		t.Fatal("a run with a parent that does not exist was admitted")
	}
	if objects, _ := h.Artifacts.List(context.Background(), "runs/"); len(objects) != 0 {
		t.Errorf("%d objects outlived the refused insert", len(objects))
	}
	var ledger int
	if err := h.Store.DB().QueryRow(`SELECT count(*) FROM run_input_uploads`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 0 {
		t.Errorf("%d upload ledger rows outlived an admission that cleaned up after itself", ledger)
	}
}

func TestTheSweeperDeletesTheFilesOfAnAdmissionThatDiedBeforeItsInsert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orphan := runv1.ULID("01JBBBBBBBBBBBBBBBBBBBBBBB")
	key := artifacts.Key(orphan, "inputs/1-a.png")

	// What a process that died between the upload and the insert leaves.
	if err := h.Store.BeginInputUpload(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := h.Artifacts.Put(ctx, key, pngBytes, "image/png"); err != nil {
		t.Fatal(err)
	}
	// Young: it may be an admission still in progress.
	if n, err := h.App.SweepOrphanInputs(ctx); err != nil || n != 0 {
		t.Fatalf("a fresh upload was swept: %d, %v", n, err)
	}

	if _, err := h.Store.DB().Exec(
		`UPDATE run_input_uploads SET started_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	// A run that did commit is never swept, whatever its ledger row says.
	kept := h.Submit(func(req *run.SubmitRequest) {
		req.Inputs = []run.InputFile{{Name: "b.png", Body: pngBytes}}
	})

	if n, err := h.App.SweepOrphanInputs(ctx); err != nil || n != 1 {
		t.Fatalf("swept %d prefixes (%v), want the orphan's", n, err)
	}
	if _, err := h.Artifacts.Get(ctx, key); !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("the orphaned input is still stored: %v", err)
	}
	if _, err := h.Artifacts.Get(ctx, artifacts.Key(kept.ID, "inputs/1-b.png")); err != nil {
		t.Errorf("an admitted run's input was swept: %v", err)
	}
}

func TestAScreenshotAttachedToAMentionIsDeliveredAndEveryOtherFileIsNamed(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	bot.Connect(t, h)

	exe := append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 64)...)
	huge := bytes.Repeat([]byte("x"), 11<<20)
	files := []any{
		f.File("file-shot", "checkout error.png", pngBytes, int64(len(pngBytes))),
		f.File("file-exe", "fix.exe", exe, int64(len(exe))),
		// The server says a hundred bytes; the cap is checked on what is read.
		f.File("file-liar", "small.txt", huge, 100),
		f.File("file-declared", "big.png", pngBytes, 50<<20),
	}
	f.Emit(mmPost{ID: "post0000000000000000000001", UserID: "user-alice", ChannelID: mmChannel,
		Message:  "@devops_duty why does checkout fail? see the screenshot",
		FileIDs:  []string{"file-shot", "file-exe", "file-liar", "file-declared"},
		Metadata: map[string]any{"files": files}}, "O", []string{mmBotID})
	if got := bot.Outcome(t); got != app.ChatStarted {
		t.Fatalf("outcome = %s, want started", got)
	}

	r := h.runs(t)[0]
	prompt := h.prompt(t, r.ID)
	for _, want := range []string{
		"why does checkout fail? see the screenshot\n\n--- attached files ---\n",
		runv1.DirInputs + "/1-checkout_error.png: \"checkout error.png\", image/png",
		"from @alice",
		"- not available: \"fix.exe\" (a application/octet-stream file, which the agent cannot read)",
		"- not available: \"small.txt\" (larger than the 10 MiB limit)",
		"- not available: \"big.png\" (larger than the 10 MiB limit)",
		"--- end of attached files ---\n",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
	if f.Downloads("file-declared") != 0 {
		t.Error("a file declared over the cap was downloaded anyway")
	}

	inputs, err := h.Store.RunInputs(context.Background(), r.ID)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("the run has %d inputs (%v), want the screenshot alone", len(inputs), err)
	}
	if inputs[0].SourceMessageID != "post0000000000000000000001" || inputs[0].MediaType != "image/png" {
		t.Errorf("the input is %+v", inputs[0])
	}
	stored, err := h.Artifacts.Get(context.Background(), artifacts.Key(r.ID, inputs[0].Key))
	if err != nil || !bytes.Equal(stored, pngBytes) {
		t.Errorf("the stored screenshot: %v", err)
	}
}

func TestAMentionCarryingOnlyAFileIsARequest(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	bot.Connect(t, h)

	f.Emit(mmPost{ID: "post0000000000000000000001", UserID: "user-alice", ChannelID: mmChannel,
		Message: "@devops_duty", FileIDs: []string{"file-shot"},
		Metadata: map[string]any{"files": []any{f.File("file-shot", "shot.png", pngBytes, 72)}}},
		"O", []string{mmBotID})
	if got := bot.Outcome(t); got != app.ChatStarted {
		t.Fatalf("outcome = %s, want a mention with a file to start a run", got)
	}
	prompt := h.prompt(t, h.runs(t)[0].ID)
	if !strings.Contains(prompt, "look at the attached files below") {
		t.Errorf("the prompt does not say what to act on:\n%s", prompt)
	}
}

func TestAFileEarlierInTheThreadIsDeliveredAndTheBotsOwnFilesAreNot(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	bot.Connect(t, h)

	f.Seed(mmPost{ID: "root00000000000000000000001", UserID: "user-bob", ChannelID: mmChannel,
		Message: "the deploy log", CreateAt: 1000, FileIDs: []string{"file-log"},
		Metadata: map[string]any{"files": []any{f.File("file-log", "deploy.log", []byte("error: boom\n"), 12)}}})
	f.Seed(mmPost{ID: "bot000000000000000000000001", UserID: mmBotID, ChannelID: mmChannel,
		RootID: "root00000000000000000000001", Message: "here is mine", CreateAt: 2000,
		FileIDs:  []string{"file-bot"},
		Metadata: map[string]any{"files": []any{f.File("file-bot", "bot.png", pngBytes, 72)}}})
	f.Emit(mmPost{ID: "post0000000000000000000002", UserID: "user-alice", ChannelID: mmChannel,
		RootID: "root00000000000000000000001", Message: "@devops_duty what failed?", CreateAt: 3000},
		"O", []string{mmBotID})
	if got := bot.Outcome(t); got != app.ChatStarted {
		t.Fatalf("outcome = %s", got)
	}

	r := h.runs(t)[0]
	inputs, err := h.Store.RunInputs(context.Background(), r.ID)
	if err != nil || len(inputs) != 1 || inputs[0].Name != "deploy.log" {
		t.Fatalf("inputs = %+v (%v), want the thread's log alone", inputs, err)
	}
	prompt := h.prompt(t, r.ID)
	if !strings.Contains(prompt, "\"deploy.log\"") || !strings.Contains(prompt, "from @bob") {
		t.Errorf("the prompt does not name the thread's file:\n%s", prompt)
	}
	if strings.Contains(prompt, "bot.png") || f.Downloads("file-bot") != 0 {
		t.Error("the bot's own file was offered to the agent")
	}
}

func TestARunsInputsAreListedAndServedAsDownloadsByTheRESTAPI(t *testing.T) {
	h := newHarness(t)
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	r := h.Submit(func(req *run.SubmitRequest) {
		req.Inputs = []run.InputFile{{Name: "shot.png", Body: pngBytes}, {Name: "page.html", Body: html}}
	})
	token := h.Token("runs:read")

	get := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, h.Public.URL+"/api/v1/runs/"+string(r.ID)+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	resp := get("/inputs")
	var list struct {
		Inputs []struct {
			File      string `json:"file"`
			Name      string `json:"name"`
			MediaType string `json:"media_type"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"inputs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list.Inputs) != 2 {
		t.Fatalf("list = %+v (%v)", list, err)
	}
	if list.Inputs[0].File != "1-shot.png" || list.Inputs[0].Name != "shot.png" {
		t.Errorf("the first input is %+v", list.Inputs[0])
	}

	resp = get("/inputs/2-page.html")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, html) {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	// An HTML file a person attached is never rendered in the UI's origin.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") {
		t.Errorf("an HTML input is served as %s", ct)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v, want an attachment with nosniff", resp.Header)
	}
	if resp := get("/inputs/9-none.png"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown input answered %d", resp.StatusCode)
	}
}
