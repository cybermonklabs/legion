package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closedBefore is a root that an earlier release closed at its merge: done, with no mark of a close
// on its checks (issues.closed_on_checks_at).
const closedBefore = "LEGION-210"

// A fail recorded after an issue closed on its checks tells the controller once, in one
// closed-check-failed notice that names the issue, the check and the result, and reopens nothing:
// the issue stays done, and nothing else is queued for it. A pass after the fail tells nothing
// more. A fail on an issue that an earlier release closed, with no mark, tells nobody and moves
// nothing either.
func TestAFailAfterACloseOnChecksTellsTheControllerOnce(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	seedIssue(t, rig.pool, record.Issue{Key: closedBefore, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: closedBefore,
		Project: "LEGION", Title: "closed before", Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "W", LastDispatchSeq: 1})
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, record.CheckRecorded)
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)

	queued := issueOutboxKinds(t, rig, waitRoot)
	rig.fact(intake.DispatchResultChanged{Key: waitRoot, Seq: 20, ResultID: "result-9", Check: "c-after", Verdict: "fail"})
	rig.fact(intake.DispatchResultChanged{Key: waitRoot, Seq: 21, ResultID: "result-10", Check: "c-after", Verdict: "pass"})
	told := closedCheckFailedReasons(t, rig, waitRoot)
	if len(told) != 1 {
		t.Fatalf("closed-check-failed notices of %s = %q, want one for the fail after its close on its checks", waitRoot, told)
	}
	for _, name := range []string{waitRoot, "c-after", "result-9"} {
		if !strings.Contains(told[0], name) {
			t.Fatalf("the notice %q does not name %s", told[0], name)
		}
	}
	rig.stands(waitRoot, phase.Done, "done")
	if got, want := issueOutboxKinds(t, rig, waitRoot), append(slices.Clone(queued), "controller_notice"); !slices.Equal(got, want) {
		t.Fatalf("%s's outbox after the fail = %v, want %v: the controller's notice and nothing else", waitRoot, got, want)
	}

	queued = issueOutboxKinds(t, rig, closedBefore)
	rig.fact(intake.DispatchResultChanged{Key: closedBefore, Seq: 5, ResultID: "result-11", Check: "c-after", Verdict: "fail"})
	if told := closedCheckFailedReasons(t, rig, closedBefore); len(told) != 0 {
		t.Fatalf("closed-check-failed notices of %s = %q, want none: an earlier release closed it, not its checks", closedBefore, told)
	}
	rig.stands(closedBefore, phase.Done, "done")
	if got := issueOutboxKinds(t, rig, closedBefore); !slices.Equal(got, queued) {
		t.Fatalf("%s's outbox after the fail = %v, want it as it stood, %v", closedBefore, got, queued)
	}
}

// issueOutboxKinds is the kind of each outbox row of issue, in the order queued.
func issueOutboxKinds(t *testing.T, rig *waitRig, issue string) []string {
	t.Helper()
	return outboxStrings(t, rig.pool, "select kind from outbox where issue = $1 order by id", issue)
}

// closedCheckFailedReasons is the reason of each closed-check-failed controller notice of issue.
func closedCheckFailedReasons(t *testing.T, rig *waitRig, issue string) []string {
	t.Helper()
	return outboxStrings(t, rig.pool, "select payload->>'reason' from outbox where kind = 'controller_notice' and payload->>'kind' = '"+
		string(record.ClosedCheckFailedNotice)+"' and issue = $1 order by id", issue)
}
