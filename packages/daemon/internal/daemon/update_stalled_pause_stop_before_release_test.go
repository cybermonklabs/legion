package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// fields are "the state that the reader acts on that its event has"; a mark that names a
// suspension the claim never had is the change stating a fact wrongly.
//
// stoppedByAPause reads the pause's stop of a claim it owes no launch through
// store.StoppingPauseTx, which answers no pause once any release of the issue's pod is recorded
// after the stop. A release suspends only the live claims it stops, and it records them
// (store.PodRelease.Roles: "the roles whose claims the release suspended"); a claim the pause had
// suspended before it, with no ready since, is none of them. Here a project's pause stopped the
// tester while it held no task, owing it no launch. After the resume the implementer ran in the
// pod, answered the round, and the daemon's idle release then suspended it and released the pod,
// recording that it suspended the implementer alone. The operator suspended the reviewer, which
// has not answered. An hour past the bound the podless round is told: the notice names the
// tester `suspended_by: released`, an idle release that suspended the implementer and never the
// tester.
func TestAnUpdateStalledNoticeNamesThePauseForAClaimALaterReleaseDidNotSuspend(t *testing.T) {
	const idleRole = claim.RoleTester
	rig := newPodUpdateRig(t,
		podlessMember(idleRole, supervise.StateSuspended, supervise.SuspendedByReleased, true),
		podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByReleased, true),
		podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByOperator, false))

	heldAt, resumedAt := podUpdateStart.Add(-time.Hour), podUpdateStart.Add(-40*time.Minute)
	pauseID := openLegionPause(t, rig.pool, heldAt)
	holdLegionPod(t, rig.st, pauseID, podUpdateIssue, heldAt)
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
	if ended, err := rig.st.EndPausePodHold(rig.ctx, "LEGION", podUpdateIssue, store.PausePodHoldReady, resumedAt.Add(5*time.Minute)); err != nil || !ended {
		t.Fatalf("end the pod's hold at the implementer's ready = %t, %v; want the hold ended", ended, err)
	}
	releasedAt := podUpdateStart.Add(10 * time.Minute)
	if _, _, err := rig.st.RecordPodRelease(rig.ctx, store.PodRelease{Project: podUpdateProject, Issue: podUpdateIssue, Generation: 1,
		Roles: []string{string(claim.RoleImplementer)}, At: releasedAt}); err != nil {
		t.Fatalf("record the idle release of the pod: %v", err)
	}

	rig.clockAt(podUpdateBound.Add(time.Hour))
	for range 3 {
		rig.tick()
	}
	rig.settleAll()

	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsOperator || notices[0].Update == nil {
		t.Fatalf("the controller was told %+v, want one notice of the operator's hold of the round", notices)
	}
	rows := stalledRows(t, notices[0])
	if got := rows[claim.RoleImplementer].SuspendedBy; got != string(supervise.SuspendedByReleased) {
		t.Fatalf("the implementer the idle release suspended is suspended_by %q, want %q; this case proves nothing", got, supervise.SuspendedByReleased)
	}
	if got := rows[idleRole].SuspendedBy; got != texts.SuspendedByPause {
		t.Errorf("the update-stalled notice names the %s that pause %d stopped idle, owing it no launch, %+v: suspended_by %q, which the controller's prompt reads as "+
			"the daemon's idle release of the claim's pod; the one release recorded since (%s) suspended %s alone, and nothing readied the %s after the pause's stop, so the pause suspended it, want %q",
			idleRole, pauseID, rows[idleRole], got, releasedAt.Format(time.RFC3339), claim.RoleImplementer, idleRole, texts.SuspendedByPause)
	}
}
