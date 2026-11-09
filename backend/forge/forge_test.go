package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

func TestEachForgeIsAskedForTheFileAtItsOwnAPIAddress(t *testing.T) {
	c := &Client{}
	for _, tc := range []struct {
		source, ref, want string
		provider          runv1.GitProvider
	}{
		{"playneta/claude-plugin", "",
			"https://api.github.com/repos/playneta/claude-plugin/contents/.claude-plugin/marketplace.json",
			runv1.GitProviderGitHub},
		{"https://github.com/playneta/claude-plugin.git", "v1.2",
			"https://api.github.com/repos/playneta/claude-plugin/contents/.claude-plugin/marketplace.json?ref=v1.2",
			runv1.GitProviderGitHub},
		{"https://github.example.com/org/repo", "feature/x",
			"https://github.example.com/api/v3/repos/org/repo/contents/.claude-plugin/marketplace.json?ref=feature%2Fx",
			runv1.GitProviderGitHub},
		{"https://gitlab.example.com/group/sub/repo.git", "",
			"https://gitlab.example.com/api/v4/projects/group%2Fsub%2Frepo/repository/files/.claude-plugin%2Fmarketplace.json/raw?ref=HEAD",
			runv1.GitProviderGitLab},
	} {
		got, provider, err := c.fileURL(tc.source, tc.ref, ".claude-plugin/marketplace.json")
		if err != nil {
			t.Errorf("%s: %v", tc.source, err)
			continue
		}
		if got != tc.want || provider != tc.provider {
			t.Errorf("%s at %q:\n got %s (%s)\nwant %s (%s)", tc.source, tc.ref, got, provider, tc.want, tc.provider)
		}
	}
}

func TestAHostThatIsNeitherForgeIsRefusedRatherThanGuessedAt(t *testing.T) {
	c := &Client{}
	for _, source := range []string{
		"https://git.example.com/org/repo",
		"https://bitbucket.org/org/repo",
	} {
		if _, _, err := c.fileURL(source, "", "x.json"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", source, err)
		}
	}
	for _, source := range []string{
		"https://user:pw@github.com/org/repo",
		"https://github.com/org/repo?x=1",
		"https://github.com/org/repo/tree/main",
		"https://github.com/",
	} {
		if _, _, err := c.fileURL(source, "", "x.json"); err == nil {
			t.Errorf("%s was accepted as a repository address", source)
		}
	}
}

func TestTheFileIsReadWithTheTokenAsABearerAndMissingFilesAreNotFound(t *testing.T) {
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		switch r.URL.Path {
		case "/repos/o/r/contents/.claude-plugin/marketplace.json":
			_, _ = w.Write([]byte(`{"name":"m"}`))
		case "/repos/o/private/contents/.claude-plugin/marketplace.json":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), GitHubAPI: srv.URL}

	body, err := c.ReadFile(context.Background(), "o/r", "", ".claude-plugin/marketplace.json", "tok")
	if err != nil || string(body) != `{"name":"m"}` {
		t.Fatalf("read %q, %v", body, err)
	}
	if gotAuth != "Bearer tok" || !strings.Contains(gotAccept, "raw") {
		t.Errorf("sent Authorization %q, Accept %q", gotAuth, gotAccept)
	}

	if _, err := c.ReadFile(context.Background(), "o/r", "", "nope.json", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing file reads as %v, want ErrNotFound", err)
	}
	if gotAuth != "" {
		t.Errorf("no token was given and %q was sent", gotAuth)
	}
	if _, err := c.ReadFile(context.Background(), "o/private", "", ".claude-plugin/marketplace.json", ""); !errors.Is(err, ErrDenied) {
		t.Errorf("a 403 reads as %v, want ErrDenied", err)
	}
}

func TestAFileLargerThanAManifestCouldBeIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", MaxFileBytes+1)))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), GitHubAPI: srv.URL}
	if _, err := c.ReadFile(context.Background(), "o/r", "", "big.json", ""); err == nil {
		t.Fatal("a file over the limit was returned")
	}
}
