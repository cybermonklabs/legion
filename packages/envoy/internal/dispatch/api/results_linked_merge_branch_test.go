package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An `after_merge` check is compared with the branch the work merged into and with the default
// branch, and where Dispatch knows of no merge, with the default branch alone. A pull request an
// issue links by its URL is that issue's in any repository (R19 of the spec of several open pull
// requests), so the attribution writes the issue's key on a merged row of another repository
// (attributionHeldFor). That row's base branch is a branch of that repository, and only a result
// whose commit that repository holds is walked there (linkedBranchBases). The issue's own
// repository holds no merge of the issue, so its check is compared with its default branch alone,
// and a branch of its own that has the linked branch's name says nothing about the work.
//
// ACME's repository is acme/widgets. Its linked pull request acme/tools#10 merged into
// integration/x of acme/tools, and its row carries the issue's key, as the attribution writes it.
// acme/widgets has a branch integration/x too, which carries ownCommit and which no work of the
// issue merged into. A result at ownCommit stands at another commit, and the check names
// acme/widgets' main alone. A result at linkedMerge, the merge commit of acme/tools#10, stands at
// integration/x of acme/tools.
func TestALinkedPullRequestsMergeBranchIsWalkedOnlyInItsOwnRepository(t *testing.T) {
	t.Parallel()
	const (
		// ownCommit is a commit of acme/widgets that its branch integration/x carries and its
		// main does not; linkedMerge is the commit acme/tools#10 merged into integration/x of
		// acme/tools as, which acme/widgets does not hold.
		ownCommit   = "8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d"
		linkedMerge = "8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e"
	)
	linked := &linkedGitHub{
		held: map[string][]string{
			"acme/widgets": {ownCommit},
			"acme/tools":   {linkedMerge},
		},
		pulls: map[string]string{"acme/tools#10": "merged"},
		branches: map[string][]string{
			"acme/widgets:integration/x": {ownCommit},
			"acme/tools:integration/x":   {linkedMerge},
		},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELM", "acme/widgets", "Merged into the second repository's integration branch").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", afterMergeAndDefaultChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/10")
	// The merge as the attribution holds it once it judged the row: a row of acme/tools that
	// carries the issue's key, since the issue links the pull request by its URL.
	writeHeadOnBase(t, database, "acme/tools", 10, newHeadCommit, "merged", issue, "integration/x")

	own := recordStored(t, handler, issue, "k4", ownCommit)
	onLinked := recordStored(t, handler, issue, "k4", linkedMerge)
	if repo := storedCommitRepo(t, database, onLinked); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the result at the linked merge commit stored commit_repo %v, want acme/tools", repo)
	}

	read := readResults(t, handler, issue)
	if compared := checkOf(t, read, "k4").ComparedWith; len(compared) != 1 || compared[0].Branch != "main" ||
		compared[0].Repo != "" {
		t.Errorf("the after_merge check compared with %+v, want its own repository's main alone: "+
			"acme/widgets holds no merge of the issue", compared)
	}
	standsAt(t, read, "k4", own.ID,
		"an after_merge result at a commit only acme/widgets' own integration/x carries, a branch no work of the issue merged into",
		model.CommitOtherCommit, false)
	stands := standsAt(t, read, "k4", onLinked.ID,
		"an after_merge result at the merge commit of the linked pull request", model.CommitAtBasis, false)
	if basis := stands.Basis; basis == nil || basis.Kind != model.CheckBasisBranch || basis.Branch != "integration/x" ||
		basis.Repo != "acme/tools" {
		t.Errorf("the result at the linked merge commit stands at %+v, want the branch integration/x of acme/tools", basis)
	}
}
