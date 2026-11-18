package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Several repositories on one run, as admission sees them: one list in the
// request, the working repository and the extras in the spec.

const reposRunID = runv1.ULID("01JREPOS000000000000000001")

func reposRequest(t *testing.T, repos ...RepoRequest) SubmitRequest {
	t.Helper()
	req := SubmitRequest{Prompt: "change the API and its client"}
	if err := req.SetRepos(repos); err != nil {
		t.Fatalf("SetRepos: %v", err)
	}
	return req
}

func depth(n int32) *int32 { return &n }

func refusedOn(t *testing.T, err error, field, contains string) {
	t.Helper()
	var invalidErr *InvalidRequestError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("err = %v, want an InvalidRequestError on %s", err, field)
	}
	if invalidErr.Field != field {
		t.Errorf("refused on %q, want %q (%v)", invalidErr.Field, field, err)
	}
	if contains != "" && !strings.Contains(invalidErr.Detail, contains) {
		t.Errorf("the refusal %q does not say %q", invalidErr.Detail, contains)
	}
}

func TestTheFirstRepositoryIsTheWorkingRepositoryAndTheRestAreExtra(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app", BaseBranch: "develop"},
		RepoRequest{URL: "https://github.com/acme/lib.git"},
		RepoRequest{URL: "https://gitlab.example.com/ops/infra", BaseBranch: "release", Name: "infra", Depth: depth(0)},
	)
	spec, err := Render(reposRunID, req, nil, Defaults{Image: "img", Model: "m"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if spec.Repo.URL != "https://github.com/acme/app" || spec.Repo.BaseBranch != "develop" {
		t.Errorf("the working repository is %+v, want the first of the list", spec.Repo)
	}
	if spec.Repo.TargetBranch != TargetBranch(reposRunID) {
		t.Errorf("target branch %q, want the generated one", spec.Repo.TargetBranch)
	}
	want := []runv1.ExtraRepoSpec{
		{URL: "https://github.com/acme/lib.git", Provider: runv1.GitProviderGitHub, Path: "lib", CloneDepth: 1},
		{URL: "https://gitlab.example.com/ops/infra", Provider: runv1.GitProviderGitLab,
			BaseBranch: "release", Path: "infra", CloneDepth: 0},
	}
	if fmt.Sprint(spec.ExtraRepos) != fmt.Sprint(want) {
		t.Errorf("extra repositories\n got %+v\nwant %+v", spec.ExtraRepos, want)
	}
}

func TestASingleRepositoryInTheListIsTheSameRunAsRepo(t *testing.T) {
	listed := reposRequest(t, RepoRequest{URL: "https://github.com/acme/app", BaseBranch: "main"})
	single := SubmitRequest{Prompt: listed.Prompt, RepoURL: "https://github.com/acme/app", BaseBranch: "main"}

	a, err := Render(reposRunID, listed, nil, Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(reposRunID, single, nil, Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("repos with one entry renders\n%s\nand repo renders\n%s", ja, jb)
	}
	if len(a.ExtraRepos) != 0 {
		t.Errorf("one repository rendered extras: %+v", a.ExtraRepos)
	}
}

func TestARequestWithMoreRepositoriesThanTheLimitIsRefusedNamingTheLimit(t *testing.T) {
	repos := []RepoRequest{{URL: "https://github.com/acme/app"}}
	for i := 0; i < 3; i++ {
		repos = append(repos, RepoRequest{URL: fmt.Sprintf("https://github.com/acme/lib%d", i)})
	}
	_, err := Render(reposRunID, reposRequest(t, repos...), nil, Defaults{MaxRepos: 3})
	refusedOn(t, err, "repos", "4 repositories exceeds the limit of 3")
}

func TestWithoutASavedLimitTenRepositoriesAreAllowed(t *testing.T) {
	repos := []RepoRequest{{URL: "https://github.com/acme/app"}}
	for i := 0; i < 9; i++ {
		repos = append(repos, RepoRequest{URL: fmt.Sprintf("https://github.com/acme/lib%d", i)})
	}
	if _, err := Render(reposRunID, reposRequest(t, repos...), nil, Defaults{}); err != nil {
		t.Fatalf("ten repositories under the default limit: %v", err)
	}
	repos = append(repos, RepoRequest{URL: "https://github.com/acme/one-too-many"})
	_, err := Render(reposRunID, reposRequest(t, repos...), nil, Defaults{})
	refusedOn(t, err, "repos", fmt.Sprintf("limit of %d", DefaultMaxRepos))
}

func TestRepoAndReposTogetherAreRefused(t *testing.T) {
	req := SubmitRequest{Prompt: "x", RepoURL: "https://github.com/acme/app"}
	refusedOn(t, req.SetRepos([]RepoRequest{{URL: "https://github.com/acme/lib"}}), "repos", "not both")

	req = SubmitRequest{Prompt: "x", BaseBranch: "main"}
	refusedOn(t, req.SetRepos([]RepoRequest{{URL: "https://github.com/acme/lib"}}), "base_branch", "")

	req = SubmitRequest{Prompt: "x"}
	refusedOn(t, req.SetRepos([]RepoRequest{{URL: "https://github.com/acme/app", Name: "app"}}),
		"repos[0].name", "/workspace")
	refusedOn(t, req.SetRepos([]RepoRequest{{URL: "https://github.com/acme/app", Depth: depth(1)}}),
		"repos[0].depth", "")
}

func TestARepositoryURLCarryingCredentialsIsRefused(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "https://x-access-token:ghp_secret@github.com/acme/lib"},
	)
	_, err := Render(reposRunID, req, nil, Defaults{})
	refusedOn(t, err, "repos[1].url", "credentials")
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Errorf("the refusal repeats the credential: %v", err)
	}

	single := SubmitRequest{Prompt: "x", RepoURL: "https://user:pass@github.com/acme/app"}
	_, err = Render(reposRunID, single, nil, Defaults{})
	refusedOn(t, err, "repo", "credentials")
}

func TestAnExtraRepositoryNeedsAnHTTPURL(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "git@github.com:acme/lib.git"},
	)
	_, err := Render(reposRunID, req, nil, Defaults{})
	refusedOn(t, err, "repos[1].url", "")
}

func TestDuplicateExtraRepositoriesAreRefused(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "https://GitHub.com/acme/app.git/"},
	)
	_, err := Render(reposRunID, req, nil, Defaults{})
	refusedOn(t, err, "repos[1].url", "repos[0]")

	req = reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "https://github.com/acme/lib", Name: "shared"},
		RepoRequest{URL: "https://github.com/acme/other", Name: "shared"},
	)
	_, err = Render(reposRunID, req, nil, Defaults{})
	refusedOn(t, err, "repos[2].name", "repos[1]")
}

func TestAnExtraRepositoryNameMustBeADirectoryName(t *testing.T) {
	for _, name := range []string{"..", ".hidden", "a/b", "with space", strings.Repeat("x", 65)} {
		req := reposRequest(t,
			RepoRequest{URL: "https://github.com/acme/app"},
			RepoRequest{URL: "https://github.com/acme/lib", Name: name},
		)
		_, err := Render(reposRunID, req, nil, Defaults{})
		refusedOn(t, err, "repos[1].name", "")
	}
}

func TestExtraRepositoryNamesDefaultToTheURLBasenameAndAreMadeUnique(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "https://github.com/acme/lib.git"},
		RepoRequest{URL: "https://gitlab.example.com/other/lib"},
		RepoRequest{URL: "https://github.com/third/lib/"},
		// An explicit name takes precedence over a default that would collide.
		RepoRequest{URL: "https://github.com/acme/tools", Name: "lib-2"},
	)
	spec, err := Render(reposRunID, req, nil, Defaults{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got []string
	for _, e := range spec.ExtraRepos {
		got = append(got, e.Path)
	}
	if want := "lib lib-3 lib-4 lib-2"; strings.Join(got, " ") != want {
		t.Errorf("paths %q, want %q", strings.Join(got, " "), want)
	}
	for _, path := range got {
		if err := runv1.ExtraRepoPath(path); err != nil {
			t.Errorf("a generated name is one the pod refuses: %v", err)
		}
	}
}

func TestExtraReposOfRendersTheSameRepositoriesAgain(t *testing.T) {
	req := reposRequest(t,
		RepoRequest{URL: "https://github.com/acme/app"},
		RepoRequest{URL: "https://github.com/acme/lib"},
		RepoRequest{URL: "https://github.com/acme/infra", BaseBranch: "release", Depth: depth(0)},
	)
	first, err := Render(reposRunID, req, nil, Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	again := SubmitRequest{Prompt: "continue", RepoURL: first.Repo.URL, ExtraRepos: ExtraReposOf(first.ExtraRepos)}
	second, err := Render("01JREPOS000000000000000002", again, nil, Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first.ExtraRepos) != fmt.Sprint(second.ExtraRepos) {
		t.Errorf("a continuation works in\n%+v\nnot\n%+v", second.ExtraRepos, first.ExtraRepos)
	}
}
