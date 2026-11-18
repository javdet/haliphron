package store

import (
	"fmt"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The merge rule for runs.extra_repo_results, which reports from several
// attempts of one run feed in any order.

func TestALaterReportWithoutAPullRequestKeepsTheExtraRepositorysEarlierOne(t *testing.T) {
	first := []runv1.ExtraRepoResult{{Path: "lib", RepoResult: runv1.RepoResult{
		Pushed: true, TargetBranch: "haliphron/run-x", CommitSHA: "aaa",
		PRURL: "https://github.com/acme/lib/pull/7", PRNumber: 7, PRAction: runv1.PRActionCreated,
	}}}
	// The retry resumed past the agent: fresh clone, nothing to push, no PR.
	retry := []runv1.ExtraRepoResult{{Path: "lib", RepoResult: runv1.RepoResult{
		TargetBranch: "haliphron/run-x", CommitSHA: "bbb",
	}}}

	got := MergeExtraRepoResults(MergeExtraRepoResults(nil, first), retry)
	if len(got) != 1 {
		t.Fatalf("merged into %d entries, want one per path: %+v", len(got), got)
	}
	r := got[0]
	if r.PRURL != first[0].PRURL || r.PRNumber != 7 || r.PRAction != runv1.PRActionCreated || !r.Pushed {
		t.Errorf("the earlier pull request was lost: %+v", r)
	}
	if r.CommitSHA != "bbb" {
		t.Errorf("commit %q, want the later report's", r.CommitSHA)
	}
}

func TestAnExtraRepositorySeenForTheFirstTimeIsAppended(t *testing.T) {
	stored := []runv1.ExtraRepoResult{{Path: "lib"}, {Path: "infra"}}
	incoming := []runv1.ExtraRepoResult{
		{Path: "docs", RepoResult: runv1.RepoResult{Pushed: true}},
		{Path: "", RepoResult: runv1.RepoResult{PRURL: "https://nowhere"}},
		{Path: "lib", RepoResult: runv1.RepoResult{PRAction: runv1.PRActionUpdated}},
	}
	got := MergeExtraRepoResults(stored, incoming)
	var paths []string
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	if fmt.Sprint(paths) != "[lib infra docs]" {
		t.Errorf("paths %v, want the stored order with the new one appended and the nameless one dropped", paths)
	}
	if got[0].PRAction != runv1.PRActionUpdated {
		t.Errorf("lib was not updated: %+v", got[0])
	}
	if stored[0].PRAction != "" {
		t.Error("the merge wrote into the slice it was given")
	}
}
