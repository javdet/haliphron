package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/automagicops/haliphron/backend/app"
)

// A file's private URL is fetched with the bot token, and the token goes to
// Slack's own file hosts and nowhere else.

func clientFor(t *testing.T, base string) *Client {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(Config{BaseURL: u, BotToken: testBotToken, AppToken: testAppToken})
}

func TestAFileIsDownloadedWithTheBotTokenUpToOneBytePastTheCap(t *testing.T) {
	t.Parallel()
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	c := clientFor(t, srv.URL)
	body, err := c.Download(context.Background(), app.ChatFile{ID: "F1", URL: srv.URL + "/files-pri/F1/shot.png"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 11 {
		t.Errorf("read %d bytes of a 100-byte file with a cap of 10, want 11", len(body))
	}
	if auth.Load() != "Bearer "+testBotToken {
		t.Errorf("the download was sent with %q", auth.Load())
	}
}

func TestTheBotTokenIsNeverSentToAHostOutsideSlack(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer foreign.Close()

	c := clientFor(t, "https://slack.com/api")
	_, err := c.Download(context.Background(), app.ChatFile{ID: "F1", URL: foreign.URL + "/steal"}, 10)
	var unavailable *app.ChatFileError
	if !errors.As(err, &unavailable) {
		t.Fatalf("a foreign download URL answered %v, want a ChatFileError", err)
	}
	if hits.Load() != 0 {
		t.Error("the foreign host was contacted")
	}
}

func TestARedirectAwayFromSlackIsNotFollowed(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
	}))
	defer foreign.Close()
	// The foreign server is reached by a name the client does not trust.
	target := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	c := clientFor(t, srv.URL)
	_, err := c.Download(context.Background(), app.ChatFile{ID: "F1", URL: srv.URL + "/files-pri/F1/x"}, 10)
	var unavailable *app.ChatFileError
	if !errors.As(err, &unavailable) {
		t.Fatalf("a redirect to a foreign host answered %v, want a ChatFileError", err)
	}
	if leaked.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestASignInPageInsteadOfTheFileSaysTheScopeIsMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>sign in</html>"))
	}))
	defer srv.Close()

	c := clientFor(t, srv.URL)
	_, err := c.Download(context.Background(),
		app.ChatFile{ID: "F1", MediaType: "image/png", URL: srv.URL + "/files-pri/F1/x"}, 1<<20)
	var unavailable *app.ChatFileError
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Reason, "files:read") {
		t.Fatalf("a sign-in page answered %v, want a reason naming files:read", err)
	}
}

func TestAFileShareMessageCarriesItsFiles(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"type":"event_callback","event":{"type":"message","subtype":"file_share",
		"channel":"C1","channel_type":"channel","user":"U1","text":"<@UBOT> look","ts":"1700000000.000100",
		"files":[{"id":"F1","name":"shot.png","mimetype":"image/png","size":10,
		          "url_private_download":"https://files.slack.com/files-pri/T1-F1/download/shot.png"},
		         {"id":"F2","name":"gone.png","mode":"tombstone"}]}}`)
	m, ok, err := decodeEvent(payload, Identity{UserID: "UBOT"})
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if m.System {
		t.Error("a file share was taken for a system message")
	}
	if len(m.Files) != 1 || m.Files[0].ID != "F1" || m.Files[0].Name != "shot.png" ||
		!strings.HasPrefix(m.Files[0].URL, "https://files.slack.com/") {
		t.Errorf("files = %+v, want the one that is not deleted", m.Files)
	}
}
