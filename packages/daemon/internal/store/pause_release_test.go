package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// servingReleasedClaim is role's claim of the root issue as a release left it: suspended as
// `released`, serving the issue's generation 1, which is the claim a cancel resumes or, while the
// project is paused, records stopped by the pause.
func servingReleasedClaim(t *testing.T, st *Store, role claim.Role) {
	t.Helper()
	if err := st.PutClaim(context.Background(), supervise.Claim{
		Token: claim.Token("legion-legion-legion-208-" + string(role)), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: role, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatalf("seed the released %s: %v", role, err)
	}
}

// implementingWorker is the open phase's worker as workflow.OpenPhaseRole names it for the phases
// these tests use: the implementer while the issue implements, and nobody otherwise.
func implementingWorker(p phase.Phase) claim.Role {
	if p == phase.Implementing {
		return claim.RoleImplementer
	}
	return ""
}

// endPause ends pause at at as the operator, as the resume's transaction writes the end.
func endPause(t *testing.T, st *Store, pause int64, at time.Time) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(), `update project_pauses
		set ended_at = $2, ended_by_kind = 'operator', ended_by_session = '' where id = $1`, pause, at); err != nil {
		t.Fatalf("end pause %d: %v", pause, err)
	}
}

// Decision 12 of dispatch://INFRA-3759: the pause starts no release. The release's transaction
// hands decide the view, which carries the pause, and marks nothing however decide answers; a
// pod a pause holds reads as held in the same view. Once the pause ends, the same decision marks
// the release.
func TestAReleaseIsMarkedNothingWhileTheProjectIsPaused(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	pause, _ := pauseLegion(t, st, pausedAt)
	if _, err := holdPod(st, pause, rootIssue, pausedAt); err != nil {
		t.Fatalf("hold the pod: %v", err)
	}
	var seen pods.IdleView
	decide := func(view pods.IdleView) (IdleReleaseMark, error) {
		seen = view
		return IdleReleaseMark{Generation: 1, Role: claim.RoleArchitect, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"architect","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`)},
		}}, nil
	}

	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, decide)
	if err != nil || marked {
		t.Fatalf("ReleaseIdleIssue while paused = %t, %v; want nothing marked", marked, err)
	}
	if !seen.Paused || !seen.PauseHeld {
		t.Fatalf("the view decide read = paused %t, held %t; want both", seen.Paused, seen.PauseHeld)
	}
	if releaseMarked(t, st) || pauseCount(t, st, `select count(*) from outbox where issue = $1`, rootIssue) != 0 {
		t.Fatal("a release was marked or queued while the project is paused")
	}

	endPause(t, st, pause.ID, pausedAt.Add(time.Hour))
	if _, err := st.EndPausePodHold(ctx, "LEGION", rootIssue, PausePodHoldReady, pausedAt.Add(time.Hour)); err != nil {
		t.Fatalf("end the pod's hold: %v", err)
	}
	marked, err = st.ReleaseIdleIssue(ctx, "legion", rootIssue, decide)
	if err != nil || !marked || seen.Paused || seen.PauseHeld {
		t.Fatalf("ReleaseIdleIssue after the resume = %t, %v (paused %t, held %t); want the release marked",
			marked, err, seen.Paused, seen.PauseHeld)
	}
}

// Decision 12 and departure 4 of dispatch://INFRA-3759: a release whose save has not run when the
// pause begins is cancelled at its save, for the pause; a save that ran before the pause acts, and
// the release's role stop after the pause still acts, since only the save reads the pause.
func TestAReleaseSaveIsCancelledWhileTheProjectIsPaused(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	first, boundary, _ := releaseThroughTheStore(t, st)

	before, err := st.IssueReleaseSave(ctx, "legion", rootIssue, first)
	if err != nil || before.Act != ReleaseActs {
		t.Fatalf("the save before the pause = %s, %v; want it to act", before.Act, err)
	}
	pauseLegion(t, st, pausedAt)
	paused, err := st.IssueReleaseSave(ctx, "legion", rootIssue, first)
	if err != nil || paused.Act != ReleaseCancel || paused.Cause != ReleaseCausePaused {
		t.Fatalf("the save while paused = %s for %s, %v; want %s for %s", paused.Act, paused.Cause, err, ReleaseCancel, ReleaseCausePaused)
	}
	if stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary); err != nil || stop.Act != ReleaseActs {
		t.Fatalf("the role stop while paused = %s, %v; want it to act: a release that saved finishes", stop.Act, err)
	}
}

// Decision 12 of dispatch://INFRA-3759: a cancel made while the project is paused queues no resume.
// It records each role the release had stopped as stopped by the pause, holds the pod whatever
// the pod's other roles do, and arms the backstop of the open phase's worker serving the issue's
// generation, as the stop transaction does while its pause stands; the architect gets none. A role
// a wake already owes a launch keeps that wake, owes the launch in its stop, and gets no backstop.
func TestACancelWhileThePausedProjectHoldsItRecordsTheStopsAndQueuesNoResume(t *testing.T) {
	for _, tc := range []struct {
		name     string
		woken    bool
		backstop bool
		live     bool
	}{
		{name: "no wake owes the implementer a launch", backstop: true},
		{name: "a wake taken while paused owes the implementer its launch", woken: true},
		{name: "another role of the pod is live", backstop: true, live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			openRunningTree(t, st)
			servingReleasedClaim(t, st, claim.RoleArchitect)
			servingReleasedClaim(t, st, claim.RoleImplementer)
			if tc.live {
				if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-208-tester", Project: "legion",
					Tree: rootIssue, Issue: rootIssue, Role: claim.RoleTester, TreeEpoch: 1, Generation: 1,
					ServingGeneration: 1, State: supervise.StateReady}); err != nil {
					t.Fatalf("seed the live tester: %v", err)
				}
			}
			markRelease(t, st, 18, 20, 22, 0, claim.RoleArchitect, claim.RoleImplementer)
			pause, _ := pauseLegion(t, st, pausedAt)
			if tc.woken {
				records := podRecords(t, st)
				inTx(t, st, func(tx pgx.Tx) error {
					if taken, err := records.TakeHeldPodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, pausedAt, pause.ID); err != nil || !taken {
						t.Fatalf("take the held wake = %t, %v", taken, err)
					}
					return nil
				})
			}
			at := pausedAt.Add(time.Minute)

			cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow,
				ReleaseCancelPause{At: at, Backstop: time.Hour, OpenPhaseRole: implementingWorker})
			if err != nil {
				t.Fatalf("cancel the release while paused: %v", err)
			}
			if cancelled.Pause != pause.ID || len(cancelled.Resumed) != 0 ||
				!slices.Equal(cancelled.Stopped, []claim.Role{claim.RoleArchitect, claim.RoleImplementer}) {
				t.Fatalf("the cancel = %+v; want pause %d, no role resumed and both stopped", cancelled, pause.ID)
			}
			if queued := startRows(t, st); len(queued) != 0 {
				t.Fatalf("starts queued by the cancel = %v, want none: the pause holds the roles", queued)
			}
			if releaseMarked(t, st) {
				t.Fatal("the cancel left the mark standing")
			}
			for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer} {
				owed := tc.woken && role == claim.RoleImplementer
				if n := pauseCount(t, st, `select count(*) from project_pause_stops where pause_id = $1 and role = $2
					and ended_as = 'stopped' and launch_owed = $3`, pause.ID, string(role), owed); n != 1 {
					t.Fatalf("%s's stops ended stopped owing a launch %t = %d, want 1", role, owed, n)
				}
			}
			if hold, held, err := st.PausePodHoldOf(ctx, "LEGION", rootIssue); err != nil || !held || hold.Pause != pause.ID {
				t.Fatalf("the pod's hold = %+v, %t, %v; want pause %d's", hold, held, err, pause.ID)
			}
			wantBackstops := 0
			if tc.backstop {
				wantBackstops = 1
			}
			if n := pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer'
				and not fired and due_at = $2`, rootIssue, at.Add(time.Hour)); n != wantBackstops {
				t.Fatalf("the implementer's backstops due an hour after the cancel = %d, want %d", n, wantBackstops)
			}
			if n := pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1 and role = 'architect'`, rootIssue); n != 0 {
				t.Fatalf("the architect's backstops = %d, want none: an architect's phase is its own", n)
			}
			if held, open := heldOpenResume(t, st, claim.RoleImplementer); open != tc.woken || (open && held != pause.ID) {
				t.Fatalf("the implementer's open resume = pause %d, open %t; want open %t, held by pause %d: the cancel leaves the wake that owes the launch",
					held, open, tc.woken, pause.ID)
			}
		})
	}
}

// A cancel names the moment a pause's stops would carry, from its caller's clock: whether the
// project is paused is read inside the cancel's own transaction, so a cancel with no moment is
// refused, naming the field, before it reads anything, and the release stays marked.
func TestAReleaseCancelWithNoMomentIsRefused(t *testing.T) {
	st := migratedStore(t)
	openRunningTree(t, st)
	servingReleasedClaim(t, st, claim.RoleImplementer)
	markRelease(t, st, 18, 20, 22, 0, claim.RoleImplementer)
	pauseLegion(t, st, pausedAt)

	_, err := st.CancelIssueRelease(context.Background(), "legion", rootIssue, resumeRow, record.OutOfWorkflow,
		ReleaseCancelPause{Backstop: time.Hour, OpenPhaseRole: implementingWorker})
	if err == nil || !strings.Contains(err.Error(), "ReleaseCancelPause.At") {
		t.Fatalf("a cancel with no moment = %v, want it refused naming ReleaseCancelPause.At", err)
	}
	if !releaseMarked(t, st) || pauseCount(t, st, `select count(*) from project_pause_stops where ended_as = 'stopped'`) != 0 {
		t.Fatal("a cancel refused for want of its moment deleted the mark or recorded a stop")
	}
}

// heldOpenResume is the pause that holds role's open resume, and whether role has an open resume,
// read as the delivery wake reads it (OpenPodResume).
func heldOpenResume(t *testing.T, st *Store, role claim.Role) (int64, bool) {
	t.Helper()
	_, pause, open, err := st.OpenPodResume(context.Background(), "legion", rootIssue, role)
	if err != nil {
		t.Fatalf("read the open resume of %s: %v", role, err)
	}
	return pause, open
}

// Decision 14 of dispatch://INFRA-3759: the stuck warning's read and the stranded read measure a
// resume from its stuck clock (clock_at), never from the wake's own moment, and pass over every
// resume of a project paused now. Decision 15: the stranded read keeps a resume a pause held,
// which pairs with no release, until a release that suspended a role is recorded after it.
func TestTheStuckAndStrandedReadsMeasureTheStuckClockAndPassOverAPausedProject(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	woke := pausedAt.Add(-2 * time.Hour)
	recordRelease(t, st, woke.Add(-time.Hour))
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, claim.RoleArchitect, pods.WakeDelivery, woke); err != nil || !taken {
		t.Fatalf("take the architect's wake = %t, %v", taken, err)
	}
	clock := woke.Add(30 * time.Minute)
	if _, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleArchitect, clock); err != nil {
		t.Fatalf("start the architect's stuck clock at its let-out: %v", err)
	}
	reads := func(before time.Time) (stuck, unfinished []PodResume) {
		t.Helper()
		stuck, err := st.StuckPodResumes(ctx, "legion", before)
		if err != nil {
			t.Fatalf("the stuck read: %v", err)
		}
		unfinished, err = unfinishedResumes(ctx, st, "legion", before)
		if err != nil {
			t.Fatalf("the stranded read: %v", err)
		}
		return stuck, unfinished
	}

	// Ten minutes after the wake, but before the clock: the wake waited its turn and is not stuck.
	if stuck, unfinished := reads(woke.Add(10 * time.Minute)); len(stuck) != 0 || len(unfinished) != 0 {
		t.Fatalf("before the stuck clock the reads = %+v and %+v, want neither", stuck, unfinished)
	}
	stuck, unfinished := reads(clock)
	if len(stuck) != 1 || len(unfinished) != 1 || !stuck[0].ClockAt.Equal(clock) || !unfinished[0].ClockAt.Equal(clock) {
		t.Fatalf("at the stuck clock the reads = %+v and %+v, want the architect's resume with its clock", stuck, unfinished)
	}

	pause, _ := pauseLegion(t, st, pausedAt)
	if stuck, unfinished := reads(pausedAt); len(stuck) != 0 || len(unfinished) != 0 {
		t.Fatalf("while paused the reads = %+v and %+v, want none: the pause holds every launch", stuck, unfinished)
	}
	records := podRecords(t, st)
	inTx(t, st, func(tx pgx.Tx) error {
		if taken, err := records.TakeHeldPodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, pausedAt, pause.ID); err != nil || !taken {
			t.Fatalf("take the implementer's held wake = %t, %v", taken, err)
		}
		return nil
	})
	endPause(t, st, pause.ID, pausedAt.Add(time.Minute))

	_, unfinished = reads(pausedAt.Add(time.Hour))
	var held []PodResume
	for _, resume := range unfinished {
		if resume.Pause == pause.ID {
			held = append(held, resume)
		}
	}
	if len(unfinished) != 2 || len(held) != 1 || held[0].Release != 0 || held[0].Role != string(claim.RoleImplementer) {
		t.Fatalf("after the resume the stranded read = %+v, want the architect's and the implementer's held resume", unfinished)
	}

	recordRelease(t, st, pausedAt.Add(2*time.Hour))
	_, unfinished = reads(pausedAt.Add(3 * time.Hour))
	for _, resume := range unfinished {
		if resume.Pause != 0 {
			t.Fatalf("after a later release the stranded read keeps the held resume %+v; that release let its pod go", resume)
		}
	}
}

// Decision 13 of dispatch://INFRA-3759: a tree's cleanup deletes every pod of the tree, so the
// transaction that confirms it ends the pause's hold of each, as deleted. An operator's tree of
// another project's issue, whose claim the daemon runs and whose issue it keeps no record of, is
// cleaned up the same way: its pod's hold is found through the tree's claims. A record that
// another project's daemon keeps on the same database, filing an issue under that tree, files no
// pod of this project's there: the hold of this daemon's own spawn of that issue, in a tree of its
// own, stands.
func TestATreesCleanupEndsThePauseHoldsOfItsPods(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openLingeringTree(t, st)
	pause, _ := pauseLegion(t, st, pausedAt)
	if _, err := holdPod(st, pause, rootIssue, pausedAt); err != nil {
		t.Fatalf("hold the pod: %v", err)
	}
	lifecycle, reserved, err := st.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve the cleanup = %+v, %t, %v", lifecycle, reserved, err)
	}
	if err := st.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, &countingReleaser{}); err != nil {
		t.Fatalf("clean the tree up: %v", err)
	}
	if _, held, err := st.PausePodHoldOf(ctx, "LEGION", rootIssue); err != nil || held {
		t.Fatalf("after the cleanup a hold stands = %t, %v; want it ended", held, err)
	}
	if n := pauseCount(t, st, `select count(*) from project_pause_pod_holds where issue = $1 and ended_why = 'deleted'`, rootIssue); n != 1 {
		t.Fatalf("holds ended as deleted = %d, want 1", n)
	}

	const other = "OTHER-5"
	if _, err := st.OpenTreeLifecycle(ctx, "legion", other, treelifecycle.AuthorityOperator); err != nil {
		t.Fatalf("open the operator's tree %s: %v", other, err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-other-5-architect", Project: "legion", Tree: other, Issue: other,
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateRetired}); err != nil {
		t.Fatalf("seed the retired architect of %s: %v", other, err)
	}
	if _, err := holdPod(st, pause, other, pausedAt); err != nil {
		t.Fatalf("hold the pod of %s: %v", other, err)
	}
	const recorded = "OTHER-9"
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $2, 'OTHER', 'child', 'planning', 1, 'in_progress', 'A', 1)`, recorded, other); err != nil {
		t.Fatalf("record %s as OTHER's issue under the tree %s: %v", recorded, other, err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-other-9-architect", Project: "legion", Tree: recorded, Issue: recorded,
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateRetired}); err != nil {
		t.Fatalf("seed the retired architect of %s: %v", recorded, err)
	}
	if _, err := holdPod(st, pause, recorded, pausedAt); err != nil {
		t.Fatalf("hold the pod of %s: %v", recorded, err)
	}
	operatorTree, reserved, err := st.ReserveOperatorTreeCleanup(ctx, "legion", other)
	if err != nil || !reserved {
		t.Fatalf("reserve the cleanup of %s = %+v, %t, %v", other, operatorTree, reserved, err)
	}
	if err := st.CleanupReservedTree(ctx, "legion", other, operatorTree.Epoch, &countingReleaser{}); err != nil {
		t.Fatalf("clean the tree %s up: %v", other, err)
	}
	if _, held, err := st.PausePodHoldOf(ctx, "LEGION", other); err != nil || held {
		t.Fatalf("after the cleanup of %s its hold stands = %t, %v; want it ended", other, held, err)
	}
	if _, held, err := st.PausePodHoldOf(ctx, "LEGION", recorded); err != nil || !held {
		t.Fatalf("after the cleanup of %s the hold of %s = %t, %v; want it standing: OTHER's record of %s files no pod of this project under %s",
			other, recorded, held, err, recorded, other)
	}
}

// Decision 13 of dispatch://INFRA-3759: a hold ends at a role's ready only where a role of the pod
// is live, read under the issue's lock; a ready read after the role stopped again ends nothing.
// The pod of a claim of another project's issue, held under this project's pause, ends the same
// way: the hold is read by its project, not by its issue key's prefix.
func TestAPodHoldEndsAtAReadyOnlyWhereARoleIsLive(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	servingReleasedClaim(t, st, claim.RoleImplementer)
	pause, _ := pauseLegion(t, st, pausedAt)
	if _, err := holdPod(st, pause, rootIssue, pausedAt); err != nil {
		t.Fatalf("hold the pod: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", rootIssue, PausePodHoldReady, pausedAt.Add(time.Minute)); err != nil || ended {
		t.Fatalf("the end with no live role = %t, %v; want nothing ended", ended, err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateReady}); err != nil {
		t.Fatalf("make the implementer ready: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", rootIssue, PausePodHoldReady, pausedAt.Add(2*time.Minute)); err != nil || !ended {
		t.Fatalf("the end at the implementer's ready = %t, %v; want the hold ended", ended, err)
	}
	if n := pauseCount(t, st, `select count(*) from project_pause_pod_holds where issue = $1 and ended_why = 'ready'`, rootIssue); n != 1 {
		t.Fatalf("holds ended at a ready = %d, want 1", n)
	}

	if _, err := holdPod(st, pause, "OTHER-5", pausedAt); err != nil {
		t.Fatalf("hold the pod of OTHER-5: %v", err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-other-5-architect", Project: "legion", Tree: "OTHER-5",
		Issue: "OTHER-5", Role: claim.RoleArchitect, Generation: 1, State: supervise.StateReady}); err != nil {
		t.Fatalf("make the architect of OTHER-5 ready: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", "OTHER-5", PausePodHoldReady, pausedAt.Add(3*time.Minute)); err != nil || !ended {
		t.Fatalf("the end at the ready of OTHER-5's architect = %t, %v; want the hold LEGION's pause put on its pod ended", ended, err)
	}
}
