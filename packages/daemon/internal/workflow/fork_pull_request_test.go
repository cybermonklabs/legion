package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A pull request whose head is a branch of another repository than the one it is opened against -
// a fork's - is never an issue's, whatever its branch is named. The record holds no row of a fork's
// pull request on the branch a task of the plan holds, legion/LEGION-208.impl, nor on
// legion/LEGION-208 of an issue with no plan, and none of an opened whose source named no
// repository for the head, which no source can show is the issue's. The same open from a branch of
// the repository itself is recorded (the control).
//
// The fault: the recorder reads the branch alone, so the fork's pull request is recorded as the
// task's, and the merge then acts on it. The control: the recorder without its rule of the head's
// repository.
func TestAPullRequestFromAForkIsNeverRecorded(t *testing.T) {
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	headIn := func(opened intake.PullRequestOpened, repo string) intake.PullRequestOpened {
		opened.HeadRepo = repo
		return opened
	}
	for _, tc := range []struct {
		name, branch string
		plan         bool
	}{
		{name: "on the branch a task holds", branch: "legion/LEGION-208.impl", plan: true},
		{name: "on the issue's branch of an issue with no plan", branch: "legion/LEGION-208"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := tasksRoot(t, pool)
			if tc.plan {
				seedTasks(t, pool, root.Key, root.Generation,
					planTask{id: "design", steps: []string{"tester:design"}, agents: []claim.Agent{"test-design"}, state: record.TaskDone, place: atStep(1)},
					planTask{id: "impl", steps: []string{"implementer:impl"}, agents: []claim.Agent{"impl-impl"}, needs: []string{"design"},
						state: record.TaskRunning, place: atStep(1)})
				seedTaskBranch(t, pool, root.Key, "design", "legion/LEGION-208")
				seedTaskBranch(t, pool, root.Key, "impl", "legion/LEGION-208.impl")
			}
			for number, headRepo := range map[int]string{9: "someone/legion", 10: ""} {
				mustSpend(t, fmt.Sprintf("the open of #%d", number), applyFacts(t, pool, engine)(fmt.Sprintf("opened-%d", number),
					headIn(openedOn(number, tc.branch, at, false), headRepo)))
				if rows := pullRequestRows(t, pool, number); rows != 0 {
					t.Fatalf("the record holds %d rows of #%d on %s, its head in %q; want none: its head is no branch of sjawhar/legion", rows, number, tc.branch, headRepo)
				}
			}
			mustSpend(t, "the open of #2", applyFacts(t, pool, engine)("opened-2", headIn(openedOn(2, tc.branch, at, false), "Sjawhar/Legion")))
			if rows := pullRequestRows(t, pool, 2); rows != 1 {
				t.Fatalf("the record holds %d rows of #2 on %s, its head in Sjawhar/Legion; want its one row: its head is a branch of sjawhar/legion", rows, tc.branch)
			}
		})
	}
}
