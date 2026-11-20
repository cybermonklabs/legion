// results_branch_issue_test.go holds POST and GET /api/v1/issues/{key}/results to the rule that a
// CI job's result counts for an issue only when the branch whose push its run ran on belongs to
// that issue (delivery.BranchOfIssue): the head branch of one of the issue's pull requests, or a
// branch whose name gives it to the issue by Dispatch's own attribution rule.
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// jobTokenOn is a results token of the job go-checks for a run at sha on a push of ref, in check
// run checkRun, which GitHub names go-checks at sha.
func jobTokenOn(rig *ciJobRig, sha, ref string, checkRun int64) string {
	rig.github.setCheckRun(checkRun, ciJobName, sha)
	return rig.jobToken(func(answer *brokertoken.Introspection) {
		answer.CI.SHA = sha
		answer.CI.Ref = ref
		answer.CI.WorkflowRef = ciJobRepository + "/.github/workflows/ci-results.yaml@" + ref
		answer.CI.CheckRunID = strconv.FormatInt(checkRun, 10)
	})
}

// jobResultAt is the job's result of c1 at sha.
func jobResultAt(sha, verdict string) map[string]any {
	body := jobResultOf("c1", verdict)
	body["commit"] = sha
	return body
}

// anotherIssue files a second issue of the rig's project, whose Verification document is the
// rig's own, so it holds a check c1 that the job go-checks proves too.
func anotherIssue(t *testing.T, rig *ciJobRig) string {
	t.Helper()
	filed := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": ciJobProject, "title": "Another change", "spec": "Another spec\n", "force": true,
	})
	if filed.Code != http.StatusCreated {
		t.Fatalf("create a second issue: status=%d body=%s", filed.Code, filed.Body.String())
	}
	other := decodeBody[model.Issue](t, filed).Key
	uploadDocument(t, rig.handler, "/api/v1/issues/"+other, "verification.md", ciJobVerification)
	return other
}

// refusedForTheBranch fails the test unless response is 403 BRANCH_NOT_THE_ISSUES naming the
// branch and the issue.
func refusedForTheBranch(t *testing.T, response interface {
	Result() *http.Response
}, body, branch, issue string) {
	t.Helper()
	code := response.Result().StatusCode
	if code != http.StatusForbidden || !strings.Contains(body, "BRANCH_NOT_THE_ISSUES") ||
		!strings.Contains(body, branch) || !strings.Contains(body, issue) {
		t.Fatalf("status=%d body=%s, want 403 BRANCH_NOT_THE_ISSUES naming %s and %s", code, body, branch, issue)
	}
}

// TestAJobOnAnotherIssuesBranchRecordsNothingThere: the job ran on a push of legion/ACME-1, the
// branch of ACME-1's change, and writes its pass of c1 on ACME-2's route. The write is refused
// with 403 BRANCH_NOT_THE_ISSUES, nothing is stored, and ACME-2's c1 compares with no head of
// legion/ACME-1 and reads no pass. The same token's write on ACME-1 is the control: it is stored,
// and a pass stands there.
func TestAJobOnAnotherIssuesBranchRecordsNothingThere(t *testing.T) {
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	other := anotherIssue(t, rig)

	response := rig.post(t, other, rig.jobToken(nil), jobResultOf("c1", "pass"))
	refusedForTheBranch(t, response, response.Body.String(), ciJobBranch, other)
	check := checkOf(t, readResults(t, rig.handler, other), "c1")
	for _, result := range check.Results {
		if result.Branch != "" {
			t.Errorf("%s's c1 holds the job's result %s on branch %s", other, result.ID, result.Branch)
		}
	}
	for _, basis := range check.ComparedWith {
		if basis.Branch == ciJobBranch {
			t.Errorf("%s's c1 compares with the head of %s, the branch of %s's change", other, ciJobBranch, rig.issue)
		}
	}
	if check.PassStands.Answer == model.PassStandsYes {
		t.Fatalf("%s's c1 reads a pass standing from a job of %s's branch: %+v", other, rig.issue, check.PassStands)
	}

	jobRecorded(t, "the same job's pass on its own issue", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	if own := checkOf(t, readResults(t, rig.handler, rig.issue), "c1"); own.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the job's pass on %s, whose branch it pushed, reads %+v, want a pass standing", rig.issue, own.PassStands)
	}
}

// TestAPullRequestsHeadBranchBelongsToItsIssue: a branch whose name holds no issue key belongs to
// the issue whose pull request has it as its head branch. The job on legion/work, the head branch
// of ACME-1's pull request #70, records its result on ACME-1, and the same token is refused on
// ACME-2, which no pull request on legion/work is attributed to.
func TestAPullRequestsHeadBranchBelongsToItsIssue(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobCommit, "open", rig.issue)
	other := anotherIssue(t, rig)
	token := jobTokenOn(rig, ciJobCommit, "refs/heads/legion/work", 98)

	response := rig.post(t, other, token, jobResultAt(ciJobCommit, "pass"))
	refusedForTheBranch(t, response, response.Body.String(), "legion/work", other)
	jobRecorded(t, "the job's pass on the issue whose pull request's branch it pushed",
		rig.post(t, rig.issue, token, jobResultAt(ciJobCommit, "pass")))
}

// TestAStoredRowOfABranchOfNoIssueAddsNoBasis: the read compares a check only with the heads of
// the branches that belong to its issue, whatever a stored row names. The job's row on
// legion/ACME-1 adds that branch's head (the control); the same row moved to legion/ACME-404, a
// branch whose name gives it to no issue and that no pull request has as its head, adds none.
func TestAStoredRowOfABranchOfNoIssueAddsNoBasis(t *testing.T) {
	rig := newCIJobRig(t)
	const stray = "legion/ACME-404"
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setBranch(stray, ciJobBranchHead{sha: ciJobCommit})
	recorded := jobRecorded(t, "the job's pass", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	if check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1"); check.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the control: the job's pass on its own branch reads %+v, want a pass standing", check.PassStands)
	}

	if _, err := rig.database.Pool.Exec(context.Background(),
		`update ci_check_results set branch = $2 where id = $1::uuid`, recorded.ID, stray); err != nil {
		t.Fatal(err)
	}
	check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	for _, basis := range check.ComparedWith {
		if basis.Branch == stray {
			t.Errorf("c1 compares with the head of %s, a branch of no issue", stray)
		}
	}
	if check.PassStands.Answer == model.PassStandsYes {
		t.Fatalf("a row on %s, a branch of no issue, makes a pass stand: %+v", stray, check.PassStands)
	}
}
