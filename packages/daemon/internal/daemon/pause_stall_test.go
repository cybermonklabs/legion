package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An agent a project's pause stopped is no stall: its turn ended at the stop, so the record of the
// last step its claim holds names no turn, however old that step is. The stall backstop's sweep
// leaves it alone — no relaunch, no report from its machine, and no notice to its owner.
//
// Two guards keep it so: the sweep posts the backstop only over a claim in StateWorking
// (backstopStalledTurns), and the machine's table ignores the backstop over a suspended claim
// (supervise's onStallBackstop rows). The negative control undoes both: the backstop then ends
// the stopped agent's "turn", brings its session back and tells its owner a worker-stalled notice.
func TestTheStallBackstopTellsNobodyOfAnAgentThePauseStopped(t *testing.T) {
	t.Parallel()
	rig := newBackstopRig(t)
	stopped := rig.claim(claim.RoleImplementer, "LEGION-218", backstopBound+time.Hour, true)
	stopped.State, stopped.SuspendedBy, stopped.Locator = supervise.StateSuspended, supervise.SuspendedByReleased, nil
	claims := rig.supervising(stopped)

	rig.sweep(claims)
	rig.settled(stopped)

	if told := rig.reported(); told != 0 {
		t.Fatalf("the machines' reports = %d, want none: an agent the pause stopped is in no turn, so no owner is told of a stall", told)
	}
	if notices := rig.notices(stopped.Issue); len(notices) != 0 {
		t.Fatalf("notices = %+v, want nobody told of a stall", notices)
	}
	if resumes := rig.resumes(stopped.Token); len(resumes) != 0 {
		t.Fatalf("resumes = %d, want the agent the pause stopped left alone", len(resumes))
	}
}

// A round over a pod a project's pause holds, whose one role the pause stopped (suspended as
// released, its process let go), is no stalled round: past its bound it tells nobody — neither
// the controller nor an architect — because the pause stopped the role on purpose and its resume
// or the role's ready ends the hold (podlessRound, roundClock.held).
//
// The negative control is podlessRound's hold check undone: the sweep tells the controller, past
// the bound, that no claim of the issue records a process in a pod.
func TestPodUpdateTellsNobodyOfARoundWhoseRoleThePauseStopped(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)

	pause := openLegionPause(t, rig.pool, podUpdateStart.Add(time.Minute))
	holdLegionPod(t, rig.st, pause, podUpdateIssue, podUpdateStart.Add(time.Minute))
	rig.handle(claim.RoleImplementer, supervise.RequestSuspend{
		Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByReleased, Reason: "the project is paused",
	})
	rig.awaitClaim(claim.RoleImplementer, "the pause's stop to let its process go", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil
	})

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round whose role the pause stopped", notices)
	}
	if rows := rig.rows(record.OutboxKindNotice); len(rows) != 0 {
		t.Fatalf("an architect was told %d notices of a round whose role the pause stopped, want none", len(rows))
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times over a pod the pause holds", len(calls))
	}
}
