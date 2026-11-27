package workflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownClosed is GitHub's close of LEGION-208's sjawhar/legion#42 with no merge.
var ownClosed = intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-a", UpdatedAt: linkedAt.Add(2 * time.Minute)}

// todaysUnmerged is the pr-closed-unmerged reason the daemon gives today for #42 of LEGION-208,
// closed with no merge while the issue stood in at.
func todaysUnmerged(at phase.Phase) string {
	return "pull request sjawhar/legion#42 closed without merging while LEGION-208 was in " + string(at)
}

// A pull request closed with no merge while another of its issue's set is open tells the architect
// in a notice of its own kind and moves nothing (dispatch://INFRA-5910 D5). LEGION-208's task a,
// at its review, has its own sjawhar/legion#42 on legion/LEGION-208, and the issue links
// acme/deploy#6990, which stands on the same branch name in a second repository. #6990 closes with
// no merge: one pr-closed-others-open names #42 as still open, no pr-closed-unmerged is sent, a
// stays at its review, and #42 is still in the set. #42 is then reviewed, a passes its retro, and
// #42 merges: the issue closes, with #6990 still closed, which holds nothing.
//
// The faults it catches: the close handler that sends pr-closed-unmerged for any close, under which
// this close finds that notice; and the record keyed by issue and branch, under which the closed
// pull request's row replaces the open one's of the same branch name.
func TestAPullRequestClosedUnmergedWhileAnotherIsOpenKeepsTheOtherInTheSet(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := noCheckEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	linkDeploy(t, pool, apply, root.Key, "", 2, "legion/LEGION-208")
	mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
	if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
		t.Fatalf("the close of %s sent pr-closed-unmerged %+v, want none: #42 is still open", linkedPR, told)
	}
	if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#42") ||
		!strings.Contains(told[0].Reason, linkedPR) {
		t.Fatalf("the close of %s sent pr-closed-others-open %+v, want one that names it closed and sjawhar/legion#42 still open", linkedPR, told)
	}
	stillOpen(t, pool, root.Key, phase.Reviewing, "the close of "+linkedPR)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
	if set := pullRequestSetOf(t, pool, root.Key); !slices.Contains(set, "sjawhar/legion#42") {
		t.Fatalf("after the close of %s the set of %s is %v, want sjawhar/legion#42 in it", linkedPR, root.Key, set)
	}

	approvesReviewOf(t, pool, engine, root.Key)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
	taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
	stillOpen(t, pool, root.Key, phase.Merging, "a's retro")
	mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
	closedDone(t, pool, root.Key, "the merge of #42")
	if pr := pullRequestOf(t, pool, deployLink.Repo, deployLink.Number); pr == nil || pr.State != record.PullRequestClosed {
		t.Fatalf("after the close of %s it is %+v, want it still closed", root.Key, pr)
	}
}

// The last open pull request of an issue's set closed with no merge, after another merged, makes
// the daemon judge the close again (dispatch://INFRA-5910 D5, decisions 9 and 12). LEGION-208 is in
// merging with its task a at its merge, a's own sjawhar/legion#42, and the link of acme/deploy#6990:
//   - #42 merges, which the open #6990 holds the close of; #6990 then closes with no merge: the
//     issue closes at that close, with no pr-closed-unmerged;
//   - #6990 merges first, and a's own #42 then closes with no merge: a's merge is not met, so the
//     issue stays in merging and the architect gets the pr-closed-unmerged of today, whose words are
//     then true.
//
// The faults it catches: the close handler left as it was, which never judges the close, under
// which the issue stays in merging after #6990 closes; and a close that ends the issue at any last
// close, a verdict of its own, under which the second row closes it.
func TestTheLastOpenPullRequestClosedUnmergedAfterAnotherMergedJudgesTheCloseAgain(t *testing.T) {
	t.Parallel()
	merging := func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Merging)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
		apply := applyFacts(t, pool, noCheckEngine())
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		return pool, apply, root
	}

	t.Run("a's #42 merged, then the linked one closed", func(t *testing.T) {
		pool, apply, root := merging(t)
		mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
		mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
		closedDone(t, pool, root.Key, "the close of "+linkedPR)
		for _, kind := range []record.NoticeKind{"pr-closed-unmerged", "pr-closed-others-open"} {
			if told := noticesOf(t, pool, root.Key, kind); len(told) != 0 {
				t.Fatalf("the close of %s sent %s %+v, want none: it closed the issue", linkedPR, kind, told)
			}
		}
	})

	t.Run("the linked one merged, then a's own #42 closed", func(t *testing.T) {
		pool, apply, root := merging(t)
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		mustSpend(t, "the close of #42", apply("closed-42", ownClosed))
		stillOpen(t, pool, root.Key, phase.Merging, "the close of #42")
		if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 1 || told[0].Reason != todaysUnmerged(phase.Merging) {
			t.Fatalf("the close of #42 sent pr-closed-unmerged %+v, want one: %q", told, todaysUnmerged(phase.Merging))
		}
		if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 0 {
			t.Fatalf("the close of #42 sent pr-closed-others-open %+v, want none: no pull request of the set is open", told)
		}
	})
}

// The last open pull request of an issue's set closed with no merge, while none merged, tells the
// architect as the daemon does today (dispatch://INFRA-5910 D5): LEGION-208, in reviewing with no
// plan rows, has its #42. Each row is a record of its own: #42 alone closes; and acme/deploy#6990,
// which the issue links, closes first (pr-closed-others-open, naming #42) and then #42 closes. The
// close of #42 sends today's one pr-closed-unmerged and moves nothing.
//
// The fault it catches is a close that judges the issue closed, or tells nothing, where nothing
// merged.
func TestTheLastOpenPullRequestClosedUnmergedWithNoneMergedTellsTheArchitectAsToday(t *testing.T) {
	t.Parallel()
	for _, linked := range []bool{false, true} {
		name := "#42 alone"
		if linked {
			name = "after the linked pull request closed"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			root := stepRoot(t, pool, phase.Reviewing)
			apply := applyFacts(t, pool, noCheckEngine())
			if linked {
				linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
				mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
				if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#42") {
					t.Fatalf("the close of %s sent pr-closed-others-open %+v, want one that names sjawhar/legion#42", linkedPR, told)
				}
			}
			mustSpend(t, "the close of #42", apply("closed-42", ownClosed))
			if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 1 || told[0].Reason != todaysUnmerged(phase.Reviewing) {
				t.Fatalf("the close of #42 sent pr-closed-unmerged %+v, want one: %q", told, todaysUnmerged(phase.Reviewing))
			}
			stillOpen(t, pool, root.Key, phase.Reviewing, "the close of #42")
		})
	}
}
