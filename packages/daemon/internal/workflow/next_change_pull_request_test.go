package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A next change opens its own pull request, and the record holds it as GitHub's opened writes it:
// no CI settlement of it yet, so its row's settlement generation (record.PullRequest.Generation) is
// 0, which names no issue generation. A read of the change's generation in which each check passes
// then ends nothing: a change that opened its own pull request holds the close until that pull
// request merges. The project runs no production check, so the open change is the one term that
// holds the close, and an end there would close the issue with the change's work unmerged.
func TestAChangeWhosePullRequestIsOpenDoesNotEndWhenItsChecksPass(t *testing.T) {
	t.Parallel()
	const opened = 44
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	rig.standIn(waitRoot, phase.Implementing, "in_progress")
	rig.fact(intake.PullRequestOpened{Repo: waitRepo, Number: opened, Branch: claim.IssueBranch(waitRoot), Base: "main", HeadSHA: headTwo,
		UpdatedAt: rig.now})
	if pr := recordedPR(t, rig.pool, waitRoot); pr.Number != opened || pr.State != record.PullRequestOpen || pr.Generation != 0 {
		t.Fatalf("%s's pull request after GitHub's opened is #%d %s at settlement generation %d, want #%d open at 0, as the opened records it",
			waitRoot, pr.Number, pr.State, pr.Generation, opened)
	}

	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))

	got := rig.issue(waitRoot)
	if ended := rig.stored(waitRoot).Ended; ended != nil || got.NextChange != changeGeneration || got.Phase != phase.Implementing {
		t.Fatalf("%s's change %d ended with its pull request #%d open: it stands in %s with change %d open and its wait's end %+v, want the change still in implementing",
			waitRoot, changeGeneration, opened, got.Phase, got.NextChange, ended)
	}
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("done status writes of %s = %d, want none while its change's pull request is open", waitRoot, got)
	}
}
