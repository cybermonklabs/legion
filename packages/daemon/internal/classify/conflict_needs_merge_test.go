package classify

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A confirmed conflict is acted on only when the issue's next step needs a head that merges, and
// the first of R0's reasons that holds is the one the act records (dispatch://INFRA-4374 R0): the
// merge is next in merging, rules the daemon could not read tell it nothing, a base that requires
// a current head holds this one whatever its checks say, and a check the base requires that has
// not passed on the head never will while GitHub runs no checks on it. Where the merge comes next
// before merging (MergeComesNext) - retro, or accepting where no step of the task follows the
// acceptance, which on the default steps is every accepting, or held from one of them - nothing is
// acted on, whatever the rules: the conflict waits for merging, where the merger merges the base
// forward (cybermonklabs/legion#4979). On a task whose acceptance a step follows, accepting is
// decided as testing and reviewing are, and its retro waits for merging as every retro does. A
// required context is green by a check run on the head that succeeded, or by a success status the
// record keeps for the head once a settlement stands for it; a failed run, a run or a status of
// another head, and a status kept while no settlement stands for the head are not green. Each
// reason the decision answers is one a merge forward's ask can be queued with, to the role it goes
// to, so no act can name a reason its message cannot state.
func TestConflictNeedsMergeAnswersTheFirstReasonThatHolds(t *testing.T) {
	const head, other = "h2", "h1"
	issueIn := func(at phase.Phase) record.Issue { return record.Issue{Key: "LEGION-208", Phase: at} }
	heldFrom := func(at phase.Phase) record.Issue {
		return record.Issue{Key: "LEGION-208", Phase: phase.Held, Hold: &record.Hold{From: at}}
	}
	// The line of `implementer acceptance:owner reviewer`, whose acceptance the reviewer step follows.
	reviewedAfter := []phase.Phase{phase.Implementing, phase.Accepting, phase.Reviewing, phase.Retro, phase.Merging}
	settled := func(runs []record.AttemptRun, statuses ...record.CommitStatus) record.PullRequest {
		return record.PullRequest{HeadSHA: head, CheckedHead: head, Failing: []string{}, CheckRuns: runs, Statuses: statuses}
	}
	passed := func(name, on string) record.AttemptRun {
		return record.AttemptRun{Name: name, ID: 1, Head: on, Conclusion: record.RunSucceeded}
	}
	failed := record.AttemptRun{Name: "ci", ID: 2, Head: head, Conclusion: record.RunFailed}
	green := settled([]record.AttemptRun{passed("ci", head), passed("pr-title", head)})
	requires := func(contexts ...string) *record.BaseRules { return &record.BaseRules{Required: contexts} }
	strict := &record.BaseRules{Required: []string{"ci"}, Strict: true}
	// A status of ci kept for the head, but recorded while the pull request's checked head is
	// another: no settlement stands for the head, so the status says nothing of it.
	unsettled := record.PullRequest{HeadSHA: head, CheckedHead: other, Failing: []string{},
		Statuses: []record.CommitStatus{keptStatus("ci", record.CommitStatusSuccess, 1, head)}}
	for _, tc := range []struct {
		name  string
		issue record.Issue
		line  []phase.Phase
		pr    record.PullRequest
		rules *record.BaseRules
		want  record.ConflictWhy
	}{
		{"accepting, before the rules are read", issueIn(phase.Accepting), phase.Order, green, nil, ""},
		{"accepting, a required check with no run", issueIn(phase.Accepting), phase.Order, settled(nil), requires("ci"), ""},
		{"retro, every check green", issueIn(phase.Retro), phase.Order, green, requires("ci"), ""},
		{"retro under a strict ruleset", issueIn(phase.Retro), phase.Order, green, strict, ""},
		{"held from accepting", heldFrom(phase.Accepting), phase.Order, green, requires("ci"), ""},
		{"merging under a strict ruleset", issueIn(phase.Merging), phase.Order, green, strict, record.ConflictMergeNext},
		{"merging, every check green", issueIn(phase.Merging), phase.Order, green, requires("ci"), record.ConflictMergeNext},
		{"held from merging", heldFrom(phase.Merging), phase.Order, green, requires("ci"), record.ConflictMergeNext},
		{"accepting a step follows, every check green", issueIn(phase.Accepting), reviewedAfter, green, requires("ci"), ""},
		{"accepting a step follows, before the rules are read", issueIn(phase.Accepting), reviewedAfter, green, nil, record.ConflictRulesUnread},
		{"accepting a step follows, a required check with no run", issueIn(phase.Accepting), reviewedAfter, settled(nil), requires("ci"), record.ConflictCheckNotGreen},
		{"held from accepting a step follows, a required check with no run", heldFrom(phase.Accepting), reviewedAfter, settled(nil), requires("ci"), record.ConflictCheckNotGreen},
		{"accepting on a line that holds no acceptance, before the rules are read", issueIn(phase.Accepting),
			[]phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}, green, nil, record.ConflictRulesUnread},
		{"retro of a task whose acceptance a step follows, under a strict ruleset", issueIn(phase.Retro), reviewedAfter, green, strict, ""},
		{"merging of a task whose acceptance a step follows", issueIn(phase.Merging), reviewedAfter, green, requires("ci"), record.ConflictMergeNext},
		{"testing, no rules read", issueIn(phase.Testing), phase.Order, green, nil, record.ConflictRulesUnread},
		{"reviewing, the read failed under what would be a strict ruleset", issueIn(phase.Reviewing), phase.Order, green,
			&record.BaseRules{Required: []string{"ci"}, Strict: true, Unread: "HTTP 502"}, record.ConflictRulesUnread},
		{"implementing, a strict ruleset with every check green", issueIn(phase.Implementing), phase.Order, green, strict, record.ConflictRequiresCurrent},
		{"testing, a required check with no run", issueIn(phase.Testing), phase.Order, settled(nil), requires("ci"), record.ConflictCheckNotGreen},
		{"testing, a required check whose run failed", issueIn(phase.Testing), phase.Order, settled([]record.AttemptRun{failed}), requires("ci"), record.ConflictCheckNotGreen},
		{"testing, one of two required checks green", issueIn(phase.Testing), phase.Order, settled([]record.AttemptRun{passed("ci", head)}), requires("ci", "pr-title"), record.ConflictCheckNotGreen},
		{"testing, a required check that passed on another head", issueIn(phase.Testing), phase.Order, settled([]record.AttemptRun{passed("ci", other)}), requires("ci"), record.ConflictCheckNotGreen},
		{"testing, a required status kept for another head", issueIn(phase.Testing), phase.Order,
			settled(nil, keptStatus("ci", record.CommitStatusSuccess, 1, other)), requires("ci"), record.ConflictCheckNotGreen},
		{"testing, a required status that failed", issueIn(phase.Testing), phase.Order,
			settled(nil, keptStatus("ci", record.CommitStatusFailure, 1, head)), requires("ci"), record.ConflictCheckNotGreen},
		{"testing, a success status while no settlement stands for the head", issueIn(phase.Testing), phase.Order, unsettled, requires("ci"), record.ConflictCheckNotGreen},
		{"testing, every required check green by a run", issueIn(phase.Testing), phase.Order, green, requires("ci", "pr-title"), ""},
		{"reviewing, a required check green by a success status of the settled head", issueIn(phase.Reviewing), phase.Order,
			settled([]record.AttemptRun{passed("pr-title", head)}, keptStatus("ci", record.CommitStatusSuccess, 1, head)), requires("ci", "pr-title"), ""},
		{"implementing, a plan with no rulesets: nothing is required", issueIn(phase.Implementing), phase.Order, settled(nil), &record.BaseRules{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ConflictNeedsMerge(tc.issue, tc.line, tc.rules, GreenContexts(tc.pr))
			if got != tc.want {
				t.Fatalf("ConflictNeedsMerge = %q, want %q", got, tc.want)
			}
			if got == "" {
				return
			}
			at := record.StandsAt(tc.issue)
			ask := record.MergeForwardMessage{Role: record.MergeForwardAsker(at), Step: record.MergeForwardAsk, Head: head, Base: "main",
				Phase: at, PullRequest: 42, Why: got}
			if _, err := record.NewOutboxRow(tc.issue.Key, ask, time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("the merge forward's ask with why %q cannot be queued: %v", got, err)
			}
		})
	}
}
