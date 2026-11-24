package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// A task none of whose steps implements gets its pull request from Legion, as the implement App,
// once its join leaves its branch with a commit its base lacks: e, `tester` alone, needs d and is
// the task that merges, so its pull request is against the default branch. The default branch moves
// after d's work, so GitHub's compare of e's branch reads diverged, with ahead_by above 0, and the
// pull request is opened all the same: head legion/LEGION-208.e, base main, the title naming the
// issue and the task and the body holding the line `Dispatch: LEGION-208`. A row that runs again
// opens none. A task with no implementation step whose branch holds no commit its base lacks gets
// none.
//
// The fault it catches is the sink of a plan with no implementation step left with no pull request
// once main moved, so the issue never merges (its controls: the open only where the compare's
// status is ahead, and the executor without the open).
func TestASinkWithNoImplementationStepGetsItsPullRequestFromLegion(t *testing.T) {
	t.Parallel()
	t.Run("a branch ahead of its base", func(t *testing.T) {
		rig := newTaskBranchRig(t, plannedTask{id: "d"}, plannedTask{id: "e", needs: []string{"d"}, roles: []string{"tester"}})
		rig.github.Commit(t, rig.branch(t, "d"), map[string]string{"d.txt": "d\n"}, "d")
		rig.github.Commit(t, "main", map[string]string{"main.txt": "main moved\n"}, "main moved")

		rig.run(t, "e")

		pulls := rig.github.PullRequests()
		if len(pulls) != 1 {
			t.Fatalf("the pull requests GitHub holds = %+v, want the one Legion opens for task e", pulls)
		}
		pull := pulls[0]
		if pull.Head != "legion/LEGION-208.e" || pull.Base != "main" || pull.Title != "LEGION-208: e" ||
			!strings.Contains("\n"+pull.Body+"\n", "\nDispatch: LEGION-208\n") || pull.OpenedWith != "implement-app-token-for-acme" {
			t.Fatalf("the pull request Legion opened = %+v, want head legion/LEGION-208.e, base main, the title LEGION-208: e, "+
				"the line Dispatch: LEGION-208 in its body, opened with the implement App's token", pull)
		}

		rig.run(t, "e")
		if again := rig.github.PullRequests(); len(again) != 1 {
			t.Fatalf("the pull requests GitHub holds after the row ran again = %+v, want the one", again)
		}
	})

	t.Run("a branch with nothing its base lacks", func(t *testing.T) {
		rig := newTaskBranchRig(t, plannedTask{id: "d"}, plannedTask{id: "e", needs: []string{"d"}, roles: []string{"tester"}})
		fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), "git", "update-ref", "refs/heads/"+rig.branch(t, "d"), rig.github.Head(t, "main"))

		rig.run(t, "e")

		if pulls := rig.github.PullRequests(); len(pulls) != 0 {
			t.Fatalf("the pull requests GitHub holds = %+v, want none: e's branch holds nothing main lacks", pulls)
		}
	})
}
