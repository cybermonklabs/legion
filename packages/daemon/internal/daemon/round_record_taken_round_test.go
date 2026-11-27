package daemon

// The Legion reviewer's Medium 2 on #5285 at 4b123279: a launch's finish that takes a round's
// record ends that round. The round's replacement by issue records its act on the round's pod and
// waits for the pod's launch turn, which a launch holds for its whole try, so the call fails as a
// call that ran out of time does, and its record stands. The launch then replaces the pod with an
// act of its own on it, and its finish (tellLaunchTook) takes the round's record with its own, by
// the target both name. The round ends there, as the act that took its pod, with no notice of its
// own: the launch's notice is the one word of the act, and no later sweep writes on the issue that
// the pod went before the round was over.
//
// TestALaunchsFinishEndsTheRoundItTookWhereTheStoreCannotReadTheClaims is the Legion reviewer's
// finding on podreplace.go:355 at 18d81dad: the same finish while the store cannot read the
// project's claims still ends the round, since the issue's claims are read as their machines hold
// them, so no later walk writes on the issue that the pod went.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// launchOverAFailedRoundCall is the rig of a round whose replacement by issue failed as a call that
// ran out of time on the pod's launch turn does, its record standing, and the launch's record of its
// own replacement of the round's pod, whose read found launchReadCommit, with the pod gone: the
// launch replaced it and has yet to hand the daemon what it took, which the test does with the
// answered TakenPod.
func launchOverAFailedRoundCall(t *testing.T) (*podUpdateRig, runtime.TakenPod) {
	t.Helper()
	rig := podlessBreakingRound(t)
	rig.rt.FailReplaceIssuePod(context.DeadlineExceeded)
	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 1 {
		t.Fatalf("the replacement by issue was asked %+v, want once", asked)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records once the round's call failed, want the round's", records)
	}
	rig.rt.FailReplaceIssuePod(nil)

	// The launch's read through a launcher of the pod finds a commit; the launch records its own
	// replacement of the round's pod and replaces it.
	taken := movedFacetTaken(rig, "launch-pod-1", runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1}, nil)
	if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
		t.Fatalf("record the launch's act: %v", err)
	}
	rig.rt.ScriptIssuePod(podUpdateIssue, "")
	return rig, taken
}

// wantRoundEndedAsTheLaunchsAct waits for the implementer's round to end, sweeps roundSweeps times,
// and holds the rig to the launch's one workspace-taken notice, nothing written on the issue, and no
// record left.
func wantRoundEndedAsTheLaunchsAct(t *testing.T, rig *podUpdateRig) {
	t.Helper()
	rig.awaitSwept(claim.RoleImplementer, "its round to end as the launch's act", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	for range roundSweeps {
		rig.tick()
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the launch's act took the round's pod, and its notice is the one word of the act", messages)
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 1 {
		t.Fatalf("the architect was told %+v, want one workspace-taken notice, the launch's", told)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}

func TestALaunchsFinishThatTakesARoundsRecordEndsThatRound(t *testing.T) {
	t.Parallel()
	rig, taken := launchOverAFailedRoundCall(t)
	rig.s.tellLaunchTook(rig.ctx, taken)
	wantRoundEndedAsTheLaunchsAct(t, rig)
}

func TestALaunchsFinishEndsTheRoundItTookWhereTheStoreCannotReadTheClaims(t *testing.T) {
	t.Parallel()
	rig, taken := launchOverAFailedRoundCall(t)
	// The implementer's row holds turn outcomes the store cannot decode, so every read of the
	// project's claims from the store fails while the launch hands the daemon what it took.
	token := rig.tokens[claim.RoleImplementer]
	var outcomes string
	if err := rig.pool.QueryRow(rig.ctx, `select turn_outcomes::text from claims where token = $1`, string(token)).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.pool.Exec(rig.ctx, `update claims set turn_outcomes = '{"unreadable": true}'::jsonb where token = $1`, string(token)); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.s.supervisor.Claims(rig.ctx); err == nil {
		t.Fatal("the store read the project's claims over the unreadable row, so this case proves nothing")
	}
	rig.s.tellLaunchTook(rig.ctx, taken)
	if _, err := rig.pool.Exec(rig.ctx, `update claims set turn_outcomes = $2::jsonb where token = $1`, string(token), outcomes); err != nil {
		t.Fatal(err)
	}
	wantRoundEndedAsTheLaunchsAct(t, rig)
}
