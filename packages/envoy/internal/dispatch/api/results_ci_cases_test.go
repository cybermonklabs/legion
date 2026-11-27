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
// publishes as ci_case) is one of contracts' closed lists, and every case of those lists is one
// Dispatch writes: a case where the job ended at the head is one of CheckCICasesEnded, and one
// where it has not ended, or its end could not be read, is one of CheckCICasesNotEnded, which the
// Legion daemon waits on. A case Dispatch wrote outside both would be refused by the event's
// schema and by the daemon's decoder; a case of a list Dispatch writes on the other side, or not
// at all, would leave the daemon waiting on a case Dispatch never writes, or sending work back on
// one Dispatch writes as unfinished.
//
// The two sides are not read the same way, because Dispatch does not spell them the same way. The
// cases of a job that has not ended are Dispatch's own, so its constants and that list are
// compared as sets, each way: a case added to one and left out of the other is what a one-way
// read misses. The cases of a job that ended are the conclusions GitHub writes, which the read
// passes through, and job_recorded, which is Dispatch's: each constant Dispatch spells is held by
// the list, and each case of the list that no constant spells is read back from a run that
// concluded with that word, so no member of either list is left unread here. A conclusion the
// list does not name is no case of its own, since it is written as failure
// (TestAConclusionTheListDoesNotNameIsTheCaseFailure below).
func TestEveryCICaseIsOneOfContractsCases(t *testing.T) {
	t.Parallel()
	spelledEnded := []string{ciConclusionSuccess, ciConclusionFailure, ciConclusionNeutral,
		ciConclusionSkipped, ciConclusionCancelled, ciCaseJobRecorded}
	for _, ended := range spelledEnded {
		if !slices.Contains(contracts.CheckCICasesEnded, ended) {
			t.Errorf("the case %q names a job that ended, and contracts.CheckCICasesEnded %v does not hold it", ended, contracts.CheckCICasesEnded)
		}
	}
	notEnded := slices.Sorted(slices.Values([]string{ciCaseNotCompleted, ciCaseAbsent, ciCaseNoHead,
		ciCaseUnreadable, ciCaseReadFailed, ciCaseNotStarted}))
	if held := slices.Sorted(slices.Values(contracts.CheckCICasesNotEnded)); !slices.Equal(notEnded, held) {
		t.Errorf("Dispatch spells %v for a job that has not ended at the head, or whose end it could not read, and contracts holds %v: the two name the same cases",
			notEnded, held)
	}
	// Each ended case no constant spells is GitHub's own conclusion, read back from a run that
	// concluded with it: the read writes that word as the case, so the list names no case Dispatch
	// never writes.
	check := model.CheckBlock{BlockID: "c1", IssueKey: "ACME-1", When: docs.CheckWhenBeforeMerge, CICheck: "go-checks"}
	head := delivery.NewestHead{HeadSHA: "2222222222222222222222222222222222222222", Repo: "example/repo", Number: 1}
	for _, conclusion := range contracts.CheckCICasesEnded {
		if slices.Contains(spelledEnded, conclusion) {
			continue
		}
		ci, derived := ciAnswerOf(check, "go-checks", head,
			&ciRun{id: 71, name: "go-checks", status: ciRunCompleted, conclusion: conclusion}, false, lineWordsFor(nil, ""))
		if ci == nil || derived == nil || ci.Case != conclusion {
			t.Errorf("a run that concluded %q, a case of contracts.CheckCICasesEnded no constant spells, reads %+v deriving %+v, want the case %s",
				conclusion, ci, derived, conclusion)
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
