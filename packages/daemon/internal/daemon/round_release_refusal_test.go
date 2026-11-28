package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A round's replacement that the runtime refuses with runtime.ErrProjectReleased was refused before
// any suspension: the record written before the call is forgotten, the round stays open, and no
// late round is told even past its bound. Both replacement paths — through a process (ReplacePod)
// and by issue (ReplaceIssuePod) — share replaceForRound and behave the same.
func TestARoundReplacementRefusedByProjectReleaseForgetsItsRecordAndTellsNoLateRound(t *testing.T) {
	t.Parallel()
	refused := fmt.Errorf("sandbox runtime: replace pod: %w", runtime.ErrProjectReleased)

	t.Run("through_a_process", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
		rig.rt.FailReplacePod(refused)
		rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)
		rig.clockAt(podUpdateBound)
		rig.awaitReplacement(1, "for the call the project release refused")

		if records := rig.replacementRecords(); records != 0 {
			t.Fatalf("the store holds %d pod_replacements record(s) after ErrProjectReleased, want none", records)
		}
		if notices := rig.notices(); len(notices) != 0 {
			t.Fatalf("the controller was told %+v of a replacement the project release refused, want no late-round notice", notices)
		}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			if c := rig.claimOf(role); c.UpdateRound != podUpdateRound {
				t.Fatalf("the %s records round %q after ErrProjectReleased, want the round still open", role, c.UpdateRound)
			}
		}
		if !strings.Contains(rig.logs.String(), "project release holds this update round's pod") {
			t.Errorf("the log does not record the project release refusal:\n%s", rig.logs.String())
		}
	})

	t.Run("by_issue", func(t *testing.T) {
		t.Parallel()
		rig := podlessBreakingRound(t)
		rig.rt.FailReplaceIssuePod(refused)
		rig.clockAt(podUpdateBound)
		rig.tick()

		if asked := rig.byIssueAsks(); len(asked) != 1 {
			t.Fatalf("the replacement by issue was asked %+v, want once", asked)
		}
		if records := rig.replacementRecords(); records != 0 {
			t.Fatalf("the store holds %d pod_replacements record(s) after ErrProjectReleased by issue, want none", records)
		}
		if notices := rig.notices(); len(notices) != 0 {
			t.Fatalf("the controller was told %+v of a by-issue replacement the project release refused, want no late-round notice", notices)
		}
		if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
			t.Fatalf("the implementer records round %q after ErrProjectReleased by issue, want the round still open", c.UpdateRound)
		}
		if !strings.Contains(rig.logs.String(), "project release holds this update round's pod") {
			t.Errorf("the log does not record the project release refusal:\n%s", rig.logs.String())
		}
	})
}

// A launch into a live pod an update round covers can see the project release set its mark while
// roundPodEndsAtLaunch reads the workspace through the pod's launcher. The hook then forgets the
// replacement record it wrote before marking anything for the sweep and refuses with
// runtime.ErrProjectReleased, so no record stands and the launch does not replace the pod.
func TestARoundPodEndsAtLaunchRefusedByProjectReleaseForgetsItsRecord(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	release := &projectRelease{}
	rig.s.plan.release = release
	rig.rt.OnWorkspaceWork(func() { release.mark() })
	rig.intoRoundPod(claim.RoleImplementer)

	token := rig.tokens[claim.RoleImplementer]
	_, err := rig.rt.Resume(rig.ctx, nil, runtime.SpawnSpec{Claim: token, Issue: podUpdateIssue, Role: claim.RoleImplementer})
	if !release.held() {
		t.Fatal("the workspace read did not run to set the project release mark")
	}
	if !errors.Is(err, runtime.ErrProjectReleased) {
		t.Fatalf("the launch whose RoundPodEnds read raced with the project release = %v, want runtime.ErrProjectReleased", err)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d pod_replacements record(s) after RoundPodEnds saw the project release, want none", records)
	}
	if got, marked := rig.s.launchReplaced(podUpdateIssue, podUpdateRound); marked {
		t.Fatalf("launchReplaced marked %+v after RoundPodEnds saw the project release, want none", got)
	}
}
