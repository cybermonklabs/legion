package classify

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// keptRunIDs is the run id runs keep for each check name, which is what a fence decides of a name
// apart from the conclusion its run came to.
func keptRunIDs(runs []record.AttemptRun) map[string]int64 {
	kept := make(map[string]int64, len(runs))
	for _, run := range runs {
		kept[run.Name] = run.ID
	}
	return kept
}

// withoutConclusions is runs with the conclusion of each left out, so two fences compare by the
// runs they keep: which run, of which head, for which name.
func withoutConclusions(runs []record.AttemptRun) []record.AttemptRun {
	bare := make([]record.AttemptRun, 0, len(runs))
	for _, run := range runs {
		run.Conclusion = ""
		bare = append(bare, run)
	}
	return bare
}

// A run GitHub concluded skipped decides everything a run that succeeded decides
// (dispatch://INFRA-4135 R2): every reader that decides from a run's conclusion takes one row, and
// each row is decided once with its run succeeded and once with it skipped, and must come to the
// same answer. The fence keeps each name's newest run whole whichever of the two it concluded, a
// skipped run makes its name neither failing nor cancelled, it is a run on the head it was reported
// on, and a required check whose run skipped is green, so a confirmed conflict is answered as for a
// check that passed, as GitHub's branch protection takes a skipped required check as passing. The
// value of the run's own conclusion is the one thing the two may differ in, so the rows that return
// runs compare them without it. Where the reader reads a run's conclusion at all, a failed run must
// decide otherwise, which shows the row puts the run where the reader looks; EffectiveOutcome reads
// only the lists, and HeadRuns only the head each run was reported on. A reader of the conclusion
// added later takes a row here.
func TestASkippedRunDecidesAsASucceededOne(t *testing.T) {
	const head, prior = "h2", "h1"
	type fenceView struct {
		Applied     bool
		Failing     []string
		Cancelled   []string
		CheckedHead string
		Generation  int64
		Kept        map[string]int64
	}
	type appliedView struct {
		Stands, Applied bool
		PR              record.PullRequest
	}
	type headView struct {
		Concluded bool
		HeadRuns  map[string]int64
	}
	type greenView struct {
		Green []string
		Why   record.ConflictWhy
	}
	for _, tc := range []struct {
		reader string
		// reads is whether the reader reads a run's conclusion at all.
		reads  bool
		decide func(conclusion record.RunConclusion) any
	}{
		// mergeAttemptSets, through ApplySettlement: the fence holds an older failed run of ci, which
		// the settlement's run of ci replaces, and a newer run of lint, which stands over the
		// settlement's older report of lint failing.
		{"fence", true, func(conclusion record.RunConclusion) any {
			pr := record.PullRequest{HeadSHA: head, CheckedHead: head, Generation: 1, Snapshot: "h2-1",
				Failing: []string{"ci"}, Cancelled: []string{},
				CheckRuns: []record.AttemptRun{
					{Name: "ci", ID: 1, Head: head, Conclusion: record.RunFailed},
					{Name: "lint", ID: 7, Head: head, Conclusion: conclusion},
				}}
			got, applied := ApplySettlement(pr, SettlementCandidate{Head: head, Generation: 2, Snapshot: "h2-2",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2, Conclusion: conclusion}, {Name: "lint", ID: 6}},
				Failing:   []string{"lint"}})
			return fenceView{Applied: applied, Failing: got.Failing, Cancelled: got.Cancelled, CheckedHead: got.CheckedHead,
				Generation: got.Generation, Kept: keptRunIDs(got.CheckRuns)}
		}},
		// The lists name ci failing and lint cancelled, and the fence's newest run of each concluded.
		{"concludedBy", true, func(conclusion record.RunConclusion) any {
			return concludedBy([]record.AttemptRun{
				{Name: "ci", ID: 2, Head: head, Conclusion: conclusion},
				{Name: "lint", ID: 3, Head: head, Conclusion: conclusion},
			}, CiOutcome{Failing: []string{"ci"}, Cancelled: []string{"lint"}})
		}},
		// The pull request keeps ci failing and e2e cancelled, and the settlement reports a new run
		// of ci and nothing of e2e.
		{"EffectiveOutcome", false, func(conclusion record.RunConclusion) any {
			pr := record.PullRequest{HeadSHA: head, CheckedHead: head, Generation: 1, Snapshot: "h2-1",
				Failing: []string{"ci"}, Cancelled: []string{"e2e"},
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1, Head: head, Conclusion: record.RunFailed}}}
			return EffectiveOutcome(pr, SettlementCandidate{Head: head, Generation: 2, Snapshot: "h2-2",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2, Conclusion: conclusion}}})
		}},
		// SettlementFor and ApplySettlement, the whole record: a newer settlement of the checked head
		// reports ci again and names lint cancelled with no run of it, so the fence's run of lint
		// decides it, and it names a newer success status of deploy.
		{"ApplySettlement", true, func(conclusion record.RunConclusion) any {
			pr := record.PullRequest{HeadSHA: head, CheckedHead: head, Generation: 1, Snapshot: "h2-1",
				Failing: []string{}, Cancelled: []string{},
				CheckRuns: []record.AttemptRun{
					{Name: "ci", ID: 2, Head: head, Conclusion: conclusion},
					{Name: "lint", ID: 3, Head: head, Conclusion: conclusion},
				},
				Statuses: []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 1, head)}}
			candidate := SettlementCandidate{Head: head, Generation: 2, Snapshot: "h2-2",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2, Conclusion: conclusion}}, Cancelled: []string{"lint"},
				Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2)}}
			stood, stands := SettlementFor(pr, candidate)
			got, applied := ApplySettlement(stood, candidate)
			got.CheckRuns = withoutConclusions(got.CheckRuns)
			return appliedView{Stands: stands, Applied: applied, PR: got}
		}},
		// HeadRuns and ChecksConcludedOnHead: the run under test is the head's only run, beside a run
		// of the head before it.
		{"ChecksConcludedOnHead", false, func(conclusion record.RunConclusion) any {
			pr := record.PullRequest{HeadSHA: head, CheckedHead: head, Failing: []string{},
				CheckRuns: []record.AttemptRun{
					{Name: "ci", ID: 2, Head: head, Conclusion: conclusion},
					{Name: "lint", ID: 1, Head: prior, Conclusion: record.RunSucceeded},
				}}
			return headView{Concluded: ChecksConcludedOnHead(pr), HeadRuns: keptRunIDs(HeadRuns(pr))}
		}},
		// GreenContexts, read by ConflictNeedsMerge: the base requires ci, whose head run is the run
		// under test, beside pr-title's run that succeeded and a success status of deploy.
		{"GreenContexts", true, func(conclusion record.RunConclusion) any {
			pr := record.PullRequest{HeadSHA: head, CheckedHead: head, Failing: []string{},
				CheckRuns: []record.AttemptRun{
					{Name: "ci", ID: 2, Head: head, Conclusion: conclusion},
					{Name: "pr-title", ID: 3, Head: head, Conclusion: record.RunSucceeded},
				},
				Statuses: []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 1, head)}}
			green := GreenContexts(pr)
			rules := &record.BaseRules{Required: []string{"ci", "deploy", "pr-title"}}
			return greenView{Green: green,
				Why: ConflictNeedsMerge(record.Issue{Key: "LEGION-208", Phase: phase.Testing}, rules, green)}
		}},
	} {
		t.Run(tc.reader, func(t *testing.T) {
			succeeded, skipped := tc.decide(record.RunSucceeded), tc.decide(record.RunSkipped)
			if !reflect.DeepEqual(skipped, succeeded) {
				t.Fatalf("%s decides %+v for a skipped run and %+v for a succeeded one, want the same", tc.reader, skipped, succeeded)
			}
			if !tc.reads {
				return
			}
			if failed := tc.decide(record.RunFailed); reflect.DeepEqual(failed, succeeded) {
				t.Fatalf("%s decides %+v for a failed run as for a succeeded one: the row does not reach the conclusion it reads", tc.reader, failed)
			}
		})
	}
}
