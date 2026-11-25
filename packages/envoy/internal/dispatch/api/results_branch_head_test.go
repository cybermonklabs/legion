// results_branch_head_test.go holds the results read to rule two of R12 of the spec for a
// before_merge check: before a change lands, its CI job runs on a push of the change's branch, so
// a pass stands at the head of each branch that a CI job's result on the check's own issue names,
// beside the head of each pull request of that issue, and no result waits for main or for a pull
// request. The fixture is Task 5.1's (ci_job_results_test.go): issue ACME-1, its check c1 that
// the CI job go-checks proves, its ordinary check k1, and the job's run on a push of
// legion/ACME-1 at ciJobCommit.
package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// branchHeadKind is the kind of the basis the head of a pushed branch is, as the wire spells it:
// the test names the word a client reads, not the server's own constant.
const branchHeadKind = "branch_head"

// branchHeadBasis is the basis a before_merge check of the job's issue compares with for the head
// of branch, at commit.
func branchHeadBasis(branch, commit string) model.CheckBasis {
	return model.CheckBasis{Kind: branchHeadKind, Branch: branch, Commit: commit}
}

// comparedWithExactly fails the test unless check blockID of read compares with want, in that
// order, and names no reason for comparing with nothing.
func comparedWithExactly(t *testing.T, read issueResults, blockID, what string, want ...model.CheckBasis) model.CheckBlock {
	t.Helper()
	check := checkOf(t, read, blockID)
	if !slices.Equal(check.ComparedWith, want) || check.ComparedNothing != "" {
		t.Fatalf("%s: %s compared with %+v (nothing for %q), want %+v", what, blockID, check.ComparedWith, check.ComparedNothing, want)
	}
	return check
}

// passStandsFrom fails the test unless the check's pass_stands is answer, from roles.
func passStandsFrom(t *testing.T, check model.CheckBlock, what, answer string, roles ...string) {
	t.Helper()
	if check.PassStands.Answer != answer || !slices.Equal(check.PassStands.Roles, roles) {
		t.Fatalf("%s: %s answers pass_stands %+v, want %s from %v", what, check.BlockID, check.PassStands, answer, roles)
	}
}

// standsAtBasis fails the test unless the result stands at want, under the check's words now.
func standsAtBasis(t *testing.T, read issueResults, blockID, resultID, what string, want model.CheckBasis) {
	t.Helper()
	stands := standsAt(t, read, blockID, resultID, what, model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Kind != want.Kind || stands.Basis.Branch != want.Branch || stands.Basis.Commit != want.Commit {
		t.Fatalf("%s stands at %+v, want %+v", what, stands.Basis, want)
	}
}

// TestAJobsPassStandsAtTheHeadOfItsBranchWithNoPullRequest: the job passed c1 at the commit it
// tested, on a push of legion/ACME-1, and no pull request is open. GitHub answers that the
// branch's head is that commit, so c1 compares with that head and the pass stands there, from ci.
func TestAJobsPassStandsAtTheHeadOfItsBranchWithNoPullRequest(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	pass := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))

	read := readResults(t, rig.handler, rig.issue)
	head := branchHeadBasis(ciJobBranch, ciJobCommit)
	check := comparedWithExactly(t, read, "c1", "a branch a job pushed, with no pull request", head)
	standsAtBasis(t, read, "c1", pass.ID, "the job's pass at the head of its branch", head)
	passStandsFrom(t, check, "the job's pass at the head of its branch", model.PassStandsYes, "ci")
}

// TestAJobsPassStopsStandingWhenItsBranchMoves: a push moves the branch past the commit the job
// tested. c1 then compares with the new head, the job's pass is at another commit, and no pass of
// c1 stands.
func TestAJobsPassStopsStandingWhenItsBranchMoves(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	pass := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	standsAtBasis(t, readResults(t, rig.handler, rig.issue), "c1", pass.ID,
		"the job's pass at the head of its branch", branchHeadBasis(ciJobBranch, ciJobCommit))

	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobOtherCommit})
	read := readResults(t, rig.handler, rig.issue)
	check := comparedWithExactly(t, read, "c1", "a branch pushed past the job's commit", branchHeadBasis(ciJobBranch, ciJobOtherCommit))
	standsAt(t, read, "c1", pass.ID, "the job's pass once its branch moved", model.CommitOtherCommit, false)
	passStandsFrom(t, check, "the job's pass once its branch moved", model.PassStandsNo)
}

// TestAJobsPassOnADeletedBranchStandsAtAPullRequestHead: the branch's head is one basis, until a
// pull request opens at that head, whose head names the commit already, so the branch adds none.
// The pull request merges and the branch is deleted: GitHub answers 404 for it, which adds no
// basis and is no unknown, and the job's pass stands at the merged pull request's final head.
// Where that final head is a later commit, the deleted branch leaves the pass at another commit,
// not unknown.
func TestAJobsPassOnADeletedBranchStandsAtAPullRequestHead(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	pass := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	comparedWithExactly(t, readResults(t, rig.handler, rig.issue), "c1", "a branch with no pull request",
		branchHeadBasis(ciJobBranch, ciJobCommit))

	number := 70
	pull := model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: ciJobCommit, PullRequest: &number}
	for _, step := range []struct {
		what  string
		state string
	}{
		{"a pull request open at the branch's head", "open"},
		{"the pull request merged and its branch deleted", "merged"},
	} {
		writeHead(t, rig.database, ciJobRepository, number, ciJobCommit, step.state, rig.issue)
		if step.state == "merged" {
			rig.github.deleteBranch(ciJobBranch)
		}
		read := readResults(t, rig.handler, rig.issue)
		check := checkOf(t, read, "c1")
		if len(check.ComparedWith) != 1 || check.ComparedWith[0].Kind != model.CheckBasisPullRequest ||
			check.ComparedWith[0].Commit != ciJobCommit || check.ComparedWith[0].PullRequest == nil || *check.ComparedWith[0].PullRequest != number {
			t.Fatalf("%s: c1 compared with %+v, want the head of pull request #%d at %s alone", step.what, check.ComparedWith, number, ciJobCommit)
		}
		standsAtBasis(t, read, "c1", pass.ID, "the job's pass, "+step.what, pull)
		passStandsFrom(t, check, step.what, model.PassStandsYes, "ci")
	}

	writeHead(t, rig.database, ciJobRepository, number, ciJobHead, "merged", rig.issue)
	read := readResults(t, rig.handler, rig.issue)
	check := checkOf(t, read, "c1")
	if len(check.ComparedWith) != 1 || check.ComparedWith[0].Commit != ciJobHead {
		t.Fatalf("a merge past the job's commit: c1 compared with %+v, want the merged head %s alone", check.ComparedWith, ciJobHead)
	}
	standsAt(t, read, "c1", pass.ID, "the job's pass on a deleted branch, merged past", model.CommitOtherCommit, false)
	passStandsFrom(t, check, "the job's pass on a deleted branch, merged past", model.PassStandsNo)
}

// TestABranchHeadGitHubCannotReadIsUnknown: GitHub answers 502 for the branch's head, or 403, which
// it also sends for a rate limit, so Dispatch cannot say whether the job's commit is that head.
// The job's pass is unknown, and no pass of c1 is a no. Once a pull request is open at another
// commit, its head is the one subject of the issue's checks and no branch head is read: the job's
// pass of c1 and a tester's pass of k1 at the job's commit stand at another commit, and neither
// check reads a pass.
func TestABranchHeadGitHubCannotReadIsUnknown(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadGateway, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rig := newCIJobRig(t)
			rig.github.setBranch(ciJobBranch, ciJobBranchHead{status: status})
			pass := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))

			read := readResults(t, rig.handler, rig.issue)
			comparedNothing(t, read, "c1", model.ReasonReadFailed, model.PassStandsUnknown)
			if stands := standsAt(t, read, "c1", pass.ID, "the job's pass at an unread branch head", model.CommitUnknown, false); stands.Reason != model.ReasonReadFailed {
				t.Fatalf("the job's pass at an unread branch head is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
			}

			writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
			tester := jobRecorded(t, "the tester's pass of k1", rig.post(t, rig.issue, rig.sessionToken(),
				resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})))
			read = readResults(t, rig.handler, rig.issue)
			for _, result := range []struct{ check, id, what string }{
				{"c1", pass.ID, "the job's pass beside a pull request at another commit"},
				{"k1", tester.ID, "the tester's pass at the job's commit"},
			} {
				standsAt(t, read, result.check, result.id, result.what, model.CommitOtherCommit, false)
				passStandsFrom(t, checkOf(t, read, result.check), result.what, model.PassStandsNo)
			}
		})
	}
}

// TestTwoChecksOfOneIssueCompareWithOneListOfHeads: c2 is a second check the job proves, with no
// result of its own. The job ran on pushes of two branches of the change, legion/ACME-1 and then
// topic/ACME-1-retry, and recorded c1's result on each. c2 compares with the heads of both, as c1
// does, in name order whichever result is newer, and GitHub is asked for each head once per read.
func TestTwoChecksOfOneIssueCompareWithOneListOfHeads(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	twoProved := ciJobVerification + "\n" + ciCheckBlock("c2", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`)
	if written := uploadVerification(t, rig.handler, rig.issue, twoProved); written.Code != http.StatusCreated {
		t.Fatalf("write c1, k1 and c2: status=%d body=%s", written.Code, written.Body.String())
	}
	const retryRef, retryBranch = "refs/heads/topic/ACME-1-retry", "topic/ACME-1-retry"
	rig.github.setCheckRun(98, ciJobName, ciJobOtherCommit)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setBranch(retryBranch, ciJobBranchHead{sha: ciJobOtherCommit})
	jobRecorded(t, "the job's pass of c1 on "+ciJobBranch, rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	retry := rig.jobToken(func(answer *ciphertoken.Introspection) {
		answer.CI.SHA, answer.CI.CheckRunID = ciJobOtherCommit, "98"
		answer.CI.Ref, answer.CI.WorkflowRef = retryRef, ciJobRepository+"/.github/workflows/ci-results.yaml@"+retryRef
	})
	body := jobResultOf("c1", "pass")
	body["commit"] = ciJobOtherCommit
	jobRecorded(t, "the job's pass of c1 on "+retryBranch, rig.post(t, rig.issue, retry, body))

	read := readResults(t, rig.handler, rig.issue)
	heads := []model.CheckBasis{branchHeadBasis(ciJobBranch, ciJobCommit), branchHeadBasis(retryBranch, ciJobOtherCommit)}
	comparedWithExactly(t, read, "c1", "the check holding the job's results", heads...)
	second := comparedWithExactly(t, read, "c2", "the check holding no result", heads...)
	passStandsFrom(t, second, "the check holding no result", model.PassStandsNo)
	for _, branch := range []string{ciJobBranch, retryBranch} {
		if asked := rig.github.branchReadsOf(branch); asked != 1 {
			t.Fatalf("one read asked GitHub for the head of %s %d times, want once", branch, asked)
		}
	}
}

// TestARolesPassAtABranchHeadStandsThere: the job's result on c1 names its branch, so every
// before_merge check of the issue compares with that branch's head. A tester's pass of k1, an
// ordinary check, at that head stands there.
func TestARolesPassAtABranchHeadStandsThere(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	tester := jobRecorded(t, "the tester's pass of k1", rig.post(t, rig.issue, rig.sessionToken(),
		resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})))

	read := readResults(t, rig.handler, rig.issue)
	head := branchHeadBasis(ciJobBranch, ciJobCommit)
	check := comparedWithExactly(t, read, "k1", "an ordinary check of the job's issue", head)
	standsAtBasis(t, read, "k1", tester.ID, "the tester's pass at the branch's head", head)
	passStandsFrom(t, check, "the tester's pass at the branch's head", model.PassStandsYes, "tester")
}

// TestAChildsCIResultIsNoBranchOfItsParent: a CI job's result recorded on a child issue against
// its parent's c1 names the child's branch, which is no branch of the parent's change, as a
// child's pull request is no head of its parent (TestChildsResultAgainstAnAncestorsCheckStands
// AtNoBasisAtTheChildsHead). The parent's c1 compares with the head of the parent's own branch
// alone, GitHub is never asked for the child's, and the child's result stands at another commit
// on the parent's tab.
func TestAChildsCIResultIsNoBranchOfItsParent(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	filed := rig.asPerson(t, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": ciJobProject, "title": "Child", "parent": rig.issue, "force": true})
	if filed.Code != http.StatusCreated {
		t.Fatalf("create a child of %s: status=%d body=%s", rig.issue, filed.Code, filed.Body.String())
	}
	child := decodeBody[model.Issue](t, filed).Key
	childRef, childBranch := "refs/heads/legion/"+child, "legion/"+child
	rig.github.setCheckRun(98, ciJobName, ciJobOtherCommit)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setBranch(childBranch, ciJobBranchHead{sha: ciJobOtherCommit})

	childToken := rig.jobToken(func(answer *ciphertoken.Introspection) {
		answer.CI.SHA, answer.CI.CheckRunID = ciJobOtherCommit, "98"
		answer.CI.Ref, answer.CI.WorkflowRef = childRef, ciJobRepository+"/.github/workflows/ci-results.yaml@"+childRef
	})
	body := jobResultOf("c1", "pass")
	body["commit"] = ciJobOtherCommit
	fromChild := jobRecorded(t, "the child's job's pass of its parent's c1", rig.post(t, child, childToken, body))
	if fromChild.Branch != childBranch || fromChild.IssueKey != child || fromChild.CheckIssueKey != rig.issue {
		t.Fatalf("the child's job's result reads branch %q on %s against %s, want %s on %s against %s",
			fromChild.Branch, fromChild.IssueKey, fromChild.CheckIssueKey, childBranch, child, rig.issue)
	}
	fromParent := jobRecorded(t, "the parent's job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))

	read := readResults(t, rig.handler, rig.issue)
	head := branchHeadBasis(ciJobBranch, ciJobCommit)
	comparedWithExactly(t, read, "c1", "the parent's check", head)
	standsAtBasis(t, read, "c1", fromParent.ID, "the parent's own job's pass", head)
	standsAt(t, read, "c1", fromChild.ID, "the child's job's pass on the parent's tab", model.CommitOtherCommit, false)
	if asked := rig.github.branchReadsOf(childBranch); asked != 0 {
		t.Fatalf("the parent's read asked GitHub for the head of the child's branch %s %d times, want never", childBranch, asked)
	}
}

// TestABranchOfARepositoryGitHubCannotSeeIsUnknownNotGone: GitHub answers 404 for the branch and
// 404 for the repository too, which is a repository the App cannot see and not a deleted branch.
// c1 compares with nothing for repository_unreadable, and the job's pass and c1's pass_stands are
// unknown. A guard: it passes before and after rule two compares with a branch's head.
func TestABranchOfARepositoryGitHubCannotSeeIsUnknownNotGone(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	rig.github.hideRepository()
	pass := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))

	read := readResults(t, rig.handler, rig.issue)
	comparedNothing(t, read, "c1", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
	if stands := standsAt(t, read, "c1", pass.ID, "the job's pass in a repository the App cannot see", model.CommitUnknown, false); stands.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("the job's pass in a repository the App cannot see is unknown for %q, want %q", stands.Reason, model.ReasonRepositoryUnreadable)
	}
}
