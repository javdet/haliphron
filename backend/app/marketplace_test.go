package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/forge"
	"github.com/automagicops/haliphron/backend/run"
)

type fakeRepoFiles struct {
	files map[string]string // path -> body
	err   error
	token string
	reads []string
}

func (f *fakeRepoFiles) ReadFile(_ context.Context, _, _, path, token string) ([]byte, error) {
	f.reads = append(f.reads, path)
	f.token = token
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.files[path]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return []byte(body), nil
}

type fixedToken string

func (t fixedToken) MintToken(context.Context, string, runv1.GitProvider, time.Duration) (string, error) {
	return string(t), nil
}

func TestAMarketplaceReportsTheNameItGivesItselfAndThePluginsItOffers(t *testing.T) {
	files := &fakeRepoFiles{files: map[string]string{
		".claude-plugin/marketplace.json": `{
			"name": "playneta",
			"metadata": {"description": "Playneta's plugins"},
			"plugins": [
				{"name": "infra-coder", "description": "Writes Terraform", "version": "1.2.0"},
				{"name": "reviewer", "version": 3},
				{"name": "has space"},
				{"name": "infra-coder"},
				{"description": "no name"}
			]
		}`,
	}}
	s := New(Options{Git: fixedToken("tok"), RepoFiles: files})

	got, err := s.InspectMarketplace(context.Background(), " playneta/claude-plugin ", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Name != "playneta" || got.Description != "Playneta's plugins" ||
		got.ManifestPath != ".claude-plugin/marketplace.json" || got.URL != "playneta/claude-plugin" {
		t.Fatalf("inspected as %+v", got)
	}
	if files.token != "tok" {
		t.Errorf("read with token %q; a private catalogue is read with the token a run clones it with", files.token)
	}
	want := []MarketplacePlugin{
		{Name: "infra-coder", Description: "Writes Terraform", Version: "1.2.0"},
		{Name: "reviewer"},
	}
	if len(got.Plugins) != len(want) {
		t.Fatalf("plugins %+v, want %+v: a duplicate or an unusable name is left out, "+
			"a number for a version costs only the version", got.Plugins, want)
	}
	for i := range want {
		if got.Plugins[i] != want[i] {
			t.Errorf("plugin %d is %+v, want %+v", i, got.Plugins[i], want[i])
		}
	}
}

func TestACodexCatalogueIsFoundWhereCodexKeepsIt(t *testing.T) {
	files := &fakeRepoFiles{files: map[string]string{
		".agents/plugins/marketplace.json": `{"name":"tools","plugins":[{"name":"lint"}]}`,
	}}
	s := New(Options{RepoFiles: files})
	got, err := s.InspectMarketplace(context.Background(), "https://github.com/o/tools", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Name != "tools" || got.ManifestPath != ".agents/plugins/marketplace.json" {
		t.Fatalf("inspected as %+v after reading %v", got, files.reads)
	}
}

func TestAnUnreadableCatalogueIsAProblemAndNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		files        *fakeRepoFiles
		want         string
	}{
		{"missing", "o/r", &fakeRepoFiles{}, "may not be a marketplace"},
		{"denied", "o/r", &fakeRepoFiles{err: forge.ErrDenied}, "git token"},
		{"not json", "o/r", &fakeRepoFiles{files: map[string]string{
			".claude-plugin/marketplace.json": `[1,2]`}}, "not a JSON object"},
		{"unusable name", "o/r", &fakeRepoFiles{files: map[string]string{
			".claude-plugin/marketplace.json": `{"name":"has space"}`}}, "cannot name"},
		{"no name", "o/r", &fakeRepoFiles{files: map[string]string{
			".claude-plugin/marketplace.json": `{"plugins":[]}`}}, "no name"},
		{"other forge", "https://git.example.com/o/r", &fakeRepoFiles{}, "GitHub and GitLab"},
	} {
		s := New(Options{RepoFiles: tc.files})
		got, err := s.InspectMarketplace(context.Background(), tc.source, "")
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Found || got.Name != "" || !strings.Contains(got.Problem, tc.want) {
			t.Errorf("%s: %+v, want not found with a problem mentioning %q", tc.name, got, tc.want)
		}
	}
}

func TestASourceARoleCouldNotSaveIsRefusedBeforeAnyForgeIsAsked(t *testing.T) {
	files := &fakeRepoFiles{}
	s := New(Options{RepoFiles: files})
	for _, tc := range []struct{ url, ref, field string }{
		{"git@github.com:o/r.git", "", "url"},
		{"http://github.com/o/r", "", "url"},
		{"", "", "url"},
		{"o/r", "../x", "ref"},
	} {
		_, err := s.InspectMarketplace(context.Background(), tc.url, tc.ref)
		var invalid *run.InvalidRequestError
		if !errors.As(err, &invalid) || invalid.Field != tc.field {
			t.Errorf("%q at %q: %v, want an invalid %s", tc.url, tc.ref, err, tc.field)
		}
	}
	if len(files.reads) != 0 {
		t.Errorf("the forge was asked for %v", files.reads)
	}
}
