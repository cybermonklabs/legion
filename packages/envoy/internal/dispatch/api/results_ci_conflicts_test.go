package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ciEvidenceAt is c1, a check CI proves, holding two results at one commit, the head of the open
// pull request #1 and the check's one subject: the result its CI job recorded with verdict
// recorded, built by the record route's own builder (ciJobCheckResult), and the result Dispatch
// derives from the go-checks run that concluded conclusion there (ciAnswerOf). Both stand at
// the subject under the check's words now.
func ciEvidenceAt(recorded, conclusion string) (model.CheckBlock, comparison) {
	const sha = "1111111111111111111111111111111111111111"
	basis := model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: sha}
	check := model.CheckBlock{BlockID: "c1", IssueKey: "ACME-1", When: docs.CheckWhenBeforeMerge, SHA256: "current-digest", CICheck: "go-checks", NegativeControl: "A server that does not build."}
	ci, derived := ciAnswerOf(check, "go-checks", delivery.NewestHead{HeadSHA: sha, Repo: "example/repo", Number: 1},
		&ciRun{id: 71, name: "go-checks", status: ciRunCompleted, conclusion: conclusion}, false, lineWordsFor(nil, ""))
	project := "ACME"
	actor := model.Actor{Kind: "session", ID: "github-actions:run-1:attempt-1:check-71", Project: &project}
	run := oauthwire.Run{
		CIRun: oauthwire.CIRun{Repository: "example/repo", Ref: "refs/heads/legion/ACME-1", SHA: sha, CheckRunID: "71", RunID: "1", RunAttempt: "1", EventName: "push"},
		ID:    1, Attempt: 1, CheckRun: 71,
	}
	resolved := resolvedCheck{
		checkBlockRef:   checkBlockRef{IssueKey: "ACME-1", BlockID: "c1", Ref: "dispatch://ACME-1/artifact/verification-md#b-c1"},
		checkResolution: checkResolution{indexedCheck: indexedCheck{SHA256: check.SHA256}, Attributes: docs.CheckAttributesOfBlock(check)},
	}
	job := ciJobCheckResult(checkResultInput{Check: "c1", Commit: sha, Verdict: recorded}, resolved, actor,
		githubapp.CheckRun{ID: 71, Name: "go-checks", HeadSHA: sha}, run, "ACME-1")
	moved := false
	for _, result := range []*model.CheckResult{&job, derived} {
		result.CheckWordsMoved = &moved
		result.CommitStands = &model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: &basis}
	}
	check.CI, check.Results = ci, []model.CheckResult{job, *derived}
	return check, comparison{bases: []model.CheckBasis{basis}, subjects: []headSubject{{basis: basis, repo: "example/repo"}}}
}

// TestEveryCIResultAtOneCommitJoinsTheConflictTest: R7, on the gate's own rule (passStandsOf). The
// result a check's CI job recorded and the one Dispatch derives from the check run are both CI's
// evidence of the check at one commit, so a fail of one and a pass of the other, in each order,
// read unknown (pass_and_fail). The controls: the two read as two results the job recorded still
// read unknown, and a recorded pass beside a derived pass reads yes. The results read derives no
// result at a head where the job recorded one of its own (jobResultHeads), so there the two meet
// only where the issue's repository could not be resolved.
func TestEveryCIResultAtOneCommitJoinsTheConflictTest(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, recorded, conclusion string
		asRecorded                 bool
		answer                     string
	}{
		{"the control: two results the job recorded", "fail", ciConclusionSuccess, true, model.PassStandsUnknown},
		{"the control: a recorded pass beside a derived pass", "pass", ciConclusionSuccess, false, model.PassStandsYes},
		{"a recorded fail beside a derived pass", "fail", ciConclusionSuccess, false, model.PassStandsUnknown},
		{"a recorded pass beside a derived fail", "pass", "failure", false, model.PassStandsUnknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			check, compared := ciEvidenceAt(row.recorded, row.conclusion)
			if row.asRecorded {
				check.Results[1].Branch = "legion/ACME-1"
			}
			got := passStandsOf(check, compared)
			if got.Answer != row.answer || (row.answer == model.PassStandsUnknown && got.Reason != model.PassStandsReasonPassAndFail) {
				t.Fatalf("pass_stands = %+v, want %s", got, row.answer)
			}
		})
	}
}
