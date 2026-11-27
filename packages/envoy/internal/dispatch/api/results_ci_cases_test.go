package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every case the read of a check CI proves writes (CheckCI.Case, which check.state_changed
// publishes as ci_case) is one of contracts' closed lists: a case where the job ended at the head
// is one of CheckCICasesEnded, and one where it has not ended, or its end could not be read, is
// one of CheckCICasesNotEnded, which the Legion daemon waits on. A case Dispatch wrote outside
// both would be refused by the event's schema and by the daemon's decoder.
func TestEveryCICaseIsOneOfContractsCases(t *testing.T) {
	t.Parallel()
	for _, ended := range []string{ciConclusionSuccess, ciConclusionFailure, ciConclusionNeutral, ciConclusionSkipped, ciConclusionCancelled, ciCaseJobRecorded} {
		if !slices.Contains(contracts.CheckCICasesEnded, ended) {
			t.Errorf("the case %q names a job that ended, and contracts.CheckCICasesEnded %v does not hold it", ended, contracts.CheckCICasesEnded)
		}
	}
	for _, notEnded := range []string{ciCaseNotCompleted, ciCaseAbsent, ciCaseNoHead, ciCaseUnreadable, ciCaseReadFailed, ciCaseNotStarted} {
		if !slices.Contains(contracts.CheckCICasesNotEnded, notEnded) {
			t.Errorf("the case %q names a job that has not ended, and contracts.CheckCICasesNotEnded %v does not hold it", notEnded, contracts.CheckCICasesNotEnded)
		}
	}
}

// A run that concluded with a conclusion GitHub may write but the closed list does not name, or
// with none at all, is a fail, as any conclusion but success and the three that prove nothing is.
// Its case is failure, one of contracts.CheckCICasesEnded, so the state event that publishes it
// is one the schema and the daemon read; a conclusion the list names keeps its own case, which is
// the control.
func TestAConclusionTheListDoesNotNameIsTheCaseFailure(t *testing.T) {
	t.Parallel()
	const sha = "1111111111111111111111111111111111111111"
	check := model.CheckBlock{BlockID: "c1", IssueKey: "ACME-1", When: docs.CheckWhenBeforeMerge, CICheck: "go-checks"}
	head := delivery.NewestHead{HeadSHA: sha, Repo: "example/repo", Number: 1}
	for _, row := range []struct {
		conclusion, state, ciCase string
	}{
		{"a_conclusion_github_adds_later", model.CIStateFail, ciConclusionFailure},
		{"", model.CIStateFail, ciConclusionFailure},
		{"timed_out", model.CIStateFail, "timed_out"},
		{ciConclusionSuccess, model.CIStatePass, ciConclusionSuccess},
	} {
		ci, derived := ciAnswerOf(check, "go-checks", head,
			&ciRun{id: 71, name: "go-checks", status: ciRunCompleted, conclusion: row.conclusion}, false, lineWordsFor(nil, ""))
		if ci == nil || derived == nil || ci.State != row.state || ci.Case != row.ciCase || derived.Verdict != row.state {
			t.Errorf("a run that concluded %q reads %+v deriving %+v, want the state %s and the case %s", row.conclusion, ci, derived, row.state, row.ciCase)
		}
	}
}
