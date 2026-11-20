// results_pass_and_fail_heads_test.go holds the reason of a `before_merge` check whose heads hold
// a pass and a fail at one of them: the check names pass_and_fail whichever head comes first, in
// the route's answer and in the gate's rule alone. These tests read names of the rule's own code,
// so they live apart from results_pass_and_fail_test.go, whose test of R4 builds at a commit before
// the rule and fails there on its own assertion.
package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The heads of TestTheCheckReasonIsTheSameWhateverTheOrderOfItsHeads: reasonOrderBoth holds a
// pass and a fail of k1, and reasonOrderOther the one result each case gives it.
const (
	reasonOrderBoth  = "f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7"
	reasonOrderOther = "f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8"
)

// learnedInOrder sets when Dispatch learned the head of each of numbers, pull requests of repo,
// so delivery.HeadsAttributedTo lists them in the order given: the first was learned last. The
// times are fixed ones, a minute apart, and no clock's.
func learnedInOrder(t *testing.T, database *store.Store, repo string, numbers ...int) {
	t.Helper()
	newest := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for index, number := range numbers {
		at := newest.Add(-time.Duration(index) * time.Minute)
		if _, err := database.Pool.Exec(context.Background(),
			`update pull_request_heads set event_at = $3, checked_at = $3 where repo = $1 and number = $2`,
			repo, number, at); err != nil {
			t.Fatalf("set when the head of %s#%d was learned: %v", repo, number, err)
		}
	}
}

// TestTheCheckReasonIsTheSameWhateverTheOrderOfItsHeads: k1's pass_stands names the reason
// pass_and_fail where its answer is unknown because a head holds a pass and a fail, and no reason
// where its answer is no, whichever of its two heads Dispatch learned last and so lists first. #70
// is open at a head holding a pass and a fail, and #71 at a head holding the one result of each
// case. Each case reads k1 with the heads in both orders, and first checks that the read lists
// them in the order it set.
func TestTheCheckReasonIsTheSameWhateverTheOrderOfItsHeads(t *testing.T) {
	for _, each := range []struct {
		name, verdict, answer, reason string
	}{
		{"beside a head that holds a fail", "fail", "no", ""},
		{"beside a head that holds a pass", "pass", "unknown", "pass_and_fail"},
	} {
		t.Run(each.name, func(t *testing.T) {
			for _, order := range [][2]int{{70, 71}, {71, 70}} {
				rig := passAndFailRig{newCIJobRig(t)}
				writeHead(t, rig.database, ciJobRepository, 70, reasonOrderBoth, "open", rig.issue)
				writeHead(t, rig.database, ciJobRepository, 71, reasonOrderOther, "open", rig.issue)
				learnedInOrder(t, rig.database, ciJobRepository, order[0], order[1])
				eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", reasonOrderBoth)
				eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", reasonOrderBoth)
				eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", each.verdict, reasonOrderOther)
				stands := rig.read(t, "").PassStands
				first := map[int]string{70: reasonOrderBoth, 71: reasonOrderOther}[order[0]]
				if len(stands.Subjects) != 2 || stands.Subjects[0].Basis.Commit != first {
					t.Fatalf("with #%d learned last, k1's subjects are %+v, want the two open heads, #%d's first", order[0], stands.Subjects, order[0])
				}
				if stands.Answer != each.answer || stands.Reason != each.reason {
					t.Fatalf("with #%d learned last, k1 reads %q (reason %q), want %q (reason %q)",
						order[0], stands.Answer, stands.Reason, each.answer, each.reason)
				}
			}
		})
	}
}

// TestTheReasonOfAnUnknownCheckNamesAPassAndAFailAtAnyOfItsHeads: the gate's rule alone
// (passStandsOf), for the case a route read cannot set up at one head alone: a check CI proves
// whose read failed at one head (read_failed) and whose other head holds a pass and a fail. The
// check reads unknown for pass_and_fail with either head first.
func TestTheReasonOfAnUnknownCheckNamesAPassAndAFailAtAnyOfItsHeads(t *testing.T) {
	const unread, both = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
	moved := false
	at := func(id, verdict string, minute int) model.CheckResult {
		return model.CheckResult{ID: id, Verdict: verdict, CommitSHA: both, Role: "ci",
			CommitStands: &model.ResultCommitStands{Answer: model.CommitAtBasis}, CheckWordsMoved: &moved,
			CreatedAt: time.Date(2026, 11, 19, 0, minute, 0, 0, time.UTC)}
	}
	check := model.CheckBlock{BlockID: "c1", When: docs.CheckWhenBeforeMerge,
		Results: []model.CheckResult{at("fail", "fail", 2), at("pass", "pass", 1)}}
	const repo = "acme/widgets"
	seventy, seventyOne := 70, 71
	unreadHead := headSubject{basis: model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: unread, PullRequest: &seventy}, repo: repo}
	bothHead := headSubject{basis: model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: both, PullRequest: &seventyOne}, repo: repo}
	ci := map[ciHeadKey]*model.CheckCI{{repo: repo, commit: unread}: {Run: "go-checks", State: model.CIStateUnread}}
	for _, order := range [][]headSubject{{unreadHead, bothHead}, {bothHead, unreadHead}} {
		stands := passStandsOf(check, comparison{bases: []model.CheckBasis{order[0].basis, order[1].basis}, subjects: order, ci: ci})
		if len(stands.Subjects) != 2 || stands.Subjects[0].Basis.Commit != order[0].basis.Commit || stands.Subjects[0].Reason == stands.Subjects[1].Reason {
			t.Fatalf("with the head at %s first, the subjects are %+v, want that head first of two, one read_failed and one pass_and_fail", order[0].basis.Commit, stands.Subjects)
		}
		if stands.Answer != model.PassStandsUnknown || stands.Reason != model.PassStandsReasonPassAndFail {
			t.Fatalf("with the head at %s first, c1 reads %q (reason %q), want unknown for pass_and_fail: the head at %s holds a pass and a fail",
				order[0].basis.Commit, stands.Answer, stands.Reason, both)
		}
	}
}
