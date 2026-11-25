package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's update-stalled notice listed each claim with its state alone (daemon/podupdate.go:
// waitingOf at d46ec4e0: "Each is marked with whether it has already answered this round") and said
// nothing of who suspended a claim. The change marks a suspended claim with who suspended it, and
// roles/controller-root.md reads the marks: "A suspended claim names who suspended it
// (`suspended_by`): ... `released` is the daemon's idle release of the claim's pod". Its own
// pauseStopped says why a claim a project's pause stopped must not be named so: "Named released,
// such a claim would read as one an idle release suspended, which none did". INFRA-3686's R2: the
// fields are "the state that the reader acts on that its event has"; a field the reader's prompt
// explains wrongly is the change stating a fact wrongly.
//
// pauseStopped marks `pause` only where a hold of the pod still owes the claim's role a launch
// (podHolds.owedLaunch), and a pause owes a launch only to a role it stopped holding a task or
// meeting a launch (supervise.PauseStop.LaunchOwed). A role the pause stopped idle is owed none.
// Here a project's pause stopped the implementer, owed a launch, and the tester, which had answered
// the round and stood idle, owed none; the store records both stops (project_pause_stops), and no
// release of the issue's pod since. An hour past the bound the podless round is told: the notice
// names the tester `suspended_by: released`, the idle release that never was.
func TestAnUpdateStalledNoticeNamesNoClaimAPauseStoppedIdleAnIdleRelease(t *testing.T) {
	const idleRole = claim.RoleTester
	rig := newPodUpdateRig(t, releasedUnasked(pauseStoppedRole),
		podlessMember(idleRole, supervise.StateSuspended, supervise.SuspendedByReleased, true))

	heldAt, resumedAt := podUpdateStart.Add(-20*time.Minute), podUpdateStart.Add(-time.Minute)
	pauseID := openLegionPause(t, rig.pool, heldAt)
	holdLegionPod(t, rig.st, pauseID, podUpdateIssue, heldAt)
	recordPodWake(t, rig.st, store.PodResume{Project: podUpdateProject, Issue: podUpdateIssue, Role: string(pauseStoppedRole),
		Kind: pods.WakePause, At: heldAt, Pause: pauseID})
	if err := store.Transact(rig.ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.LockIssueForPause(ctx, tx, podUpdateIssue); err != nil {
			return err
		}
		_, err := store.RecordPauseStopTx(ctx, tx, store.PauseStopRecord{
			Pause: store.Pause{ID: pauseID, Project: "LEGION"},
			Stop: store.PauseStopClaim{Claim: rig.tokens[idleRole], Tree: podUpdateIssue, Issue: podUpdateIssue,
				Role: idleRole, Generation: 1},
			At: heldAt.Add(time.Minute), LaunchOwed: false, HoldPod: true,
		})
		return err
	}); err != nil {
		t.Fatalf("record the pause's stop of the idle %s: %v", idleRole, err)
	}
	if _, err := rig.st.Resume(rig.ctx, store.ResumeRequest{Project: "LEGION",
		By: store.PauseActor{Kind: store.PauseByOperator}, At: resumedAt}); err != nil {
		t.Fatalf("resume LEGION: %v", err)
	}
	if ended, err := rig.st.EndPausePodHold(rig.ctx, "LEGION", podUpdateIssue, store.PausePodHoldResumed, resumedAt); err != nil || !ended {
		t.Fatalf("end the pod's hold at the resume = %t, %v; want the hold ended", ended, err)
	}

	rig.clockAt(podUpdateBound.Add(time.Hour))
	for range 3 {
		rig.tick()
	}
	rig.settleAll()

	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsNoPod || notices[0].Update == nil {
		t.Fatalf("the controller was told %+v, want one no-pod notice of the round the pause's claim holds", notices)
	}
	rows := stalledRows(t, notices[0])
	if got := rows[pauseStoppedRole].SuspendedBy; got != texts.SuspendedByPause {
		t.Fatalf("the %s the pause stopped owing a launch is suspended_by %q, want %q", pauseStoppedRole, got, texts.SuspendedByPause)
	}
	stoppedBy, err := rig.st.StoppedByAPause(rig.ctx, podUpdateProject, podUpdateIssue, rig.tokens[idleRole])
	if err != nil {
		t.Fatalf("read the pause that stopped the %s: %v", idleRole, err)
	}
	if stoppedBy != pauseID {
		t.Fatalf("the store says pause %d stopped the %s since the pod's last release, want pause %d", stoppedBy, idleRole, pauseID)
	}

	const releasedIsIdleRelease = "`released` is the daemon's idle release of the claim's pod"
	if rows[idleRole].SuspendedBy == string(supervise.SuspendedByReleased) && strings.Contains(controllerRootWords(t), releasedIsIdleRelease) {
		t.Errorf("the update-stalled notice (holds: %s) names the %s that pause %d stopped idle, owing it no launch, %+v, and the controller's prompt says %q; "+
			"no idle release suspended it: the store records the pause's stop and no release of the pod since",
			notices[0].Holds, idleRole, pauseID, rows[idleRole], releasedIsIdleRelease)
	}
}
