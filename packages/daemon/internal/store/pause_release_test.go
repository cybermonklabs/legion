package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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

// Under a project pause, a release whose workspace check has not run
// when the pause begins is cancelled at its check, for the pause; a check that ran before the
// pause acts, and the release's role stop after the pause still acts, since only the check reads
// the pause.
func TestAReleaseCheckIsCancelledWhileTheProjectIsPaused(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	first, boundary, _ := releaseThroughTheStore(t, st, rootIssue)

	before, err := st.IssueReleaseCheck(ctx, "legion", rootIssue, first)
	if err != nil || before.Act != ReleaseActs {
		t.Fatalf("the check before the pause = %s, %v; want it to act", before.Act, err)
	}
	pauseLegion(t, st, pausedAt)
	paused, err := st.IssueReleaseCheck(ctx, "legion", rootIssue, first)
	if err != nil || paused.Act != ReleaseCancel || paused.Cause != ReleaseCausePaused {
		t.Fatalf("the check while paused = %s for %s, %v; want %s for %s", paused.Act, paused.Cause, err, ReleaseCancel, ReleaseCausePaused)
	}
	if stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary); err != nil || stop.Act != ReleaseActs {
		t.Fatalf("the role stop while paused = %s, %v; want it to act: a release whose check ran finishes", stop.Act, err)
	}
}

// otherIssue is an issue of another project whose pod this project's daemon runs: the operator's
// spawn (`legion claims spawn`) makes a claim of any issue key under the daemon's own project
// token, and such a claim is part of this project's pause as every other claim is. No production
// row reaches a release's check or its cancel for such an issue (IssueReleaseCheck,
// CancelIssueRelease): a release's rows are keyed by its issue, the release queues no row keyed by
// another project's issue (insertOutboxRow, claim.KeyOfProject) and so marks no release of it, and
// the daemon's outbox runs only the rows of its own project's keys. The tests below write the mark
// by hand (markReleaseOf) and hold only the reading of the pause by the project's token that the
// two make, which is the right reading and costs nothing.
const otherIssue = "OTHER-5"

// The rows of the hand-written mark of otherIssue's release: its first row and its boundary, the
// role stop.
const otherFirst, otherBoundary = 18, 20

// For the pod of another project's issue, the check of its
// release reads the pause of the project whose release it is, never the pause of the issue key's
// project, so it is cancelled while LEGION is paused.
func TestACheckOfAReleaseOfAnotherProjectsIssueIsCancelledWhileThisProjectIsPaused(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	markReleaseOf(t, st, otherIssue, otherFirst, otherBoundary, 22, 0)
	pauseLegion(t, st, pausedAt)

	checked, err := st.IssueReleaseCheck(ctx, "legion", otherIssue, otherFirst)
	if err != nil || checked.Act != ReleaseCancel || checked.Cause != ReleaseCausePaused {
		t.Fatalf("the check of %s's release while LEGION is paused = %s for %s, %v; want %s for %s: the pause deletes no pod",
			otherIssue, checked.Act, checked.Cause, err, ReleaseCancel, ReleaseCausePaused)
	}
}

// The inversion of the test above: a pause of the issue key's project is not this project's, so
// the check of the same release acts while only OTHER is paused and LEGION runs.
func TestACheckOfAReleaseOfAnotherProjectsIssueActsWhileOnlyThatProjectIsPaused(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	markReleaseOf(t, st, otherIssue, otherFirst, otherBoundary, 22, 0)
	pauseProject(t, st, "OTHER", pausedAt)

	checked, err := st.IssueReleaseCheck(ctx, "legion", otherIssue, otherFirst)
	if err != nil || checked.Act != ReleaseActs {
		t.Fatalf("the check of %s's release while only OTHER is paused = %s for %s, %v; want it to act: LEGION runs",
			otherIssue, checked.Act, checked.Cause, err)
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
					if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, pausedAt, pause.ID); err != nil || !taken {
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

// Decision 12 of dispatch://INFRA-3759, for the pod of another project's issue: a cancel of its
// release reads the pause of the project whose release it is, never the pause of the issue key's
// project, so while LEGION is paused it records the roles the release stopped as stopped by
// LEGION's pause and queues no resume. The cancel reads the issue's record by its key
// (resumeStanding), so the test seeds the record that OTHER's daemon keeps of the issue on the
// same database.
func TestACancelOfAReleaseOfAnotherProjectsIssueReadsThisProjectsPause(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'OTHER', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, otherIssue); err != nil {
		t.Fatalf("record %s as OTHER's issue: %v", otherIssue, err)
	}
	markReleaseOf(t, st, otherIssue, otherFirst, otherBoundary, 22, 0)
	roles := []claim.Role{claim.RoleArchitect, claim.RoleImplementer}
	for _, role := range roles {
		token, err := claim.NewToken("legion", otherIssue, role)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: otherIssue, Issue: otherIssue,
			Role: role, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
			SuspendedBy: supervise.SuspendedByReleased}); err != nil {
			t.Fatalf("seed the released %s of %s: %v", role, otherIssue, err)
		}
		if err := st.ReleaseSuspendedClaim(ctx, "legion", otherIssue, role, otherBoundary); err != nil {
			t.Fatalf("record the %s's suspension: %v", role, err)
		}
	}
	pause, _ := pauseLegion(t, st, pausedAt)
	resume := func(role claim.Role, generation uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{Kind: "supervise", Payload: []byte(fmt.Sprintf(
			`{"op":"start","tree":%q,"role":%q,"generation":%d}`, otherIssue, role, generation))}, nil
	}

	cancelled, err := st.CancelIssueRelease(ctx, "legion", otherIssue, resume, record.OutOfWorkflow,
		ReleaseCancelPause{At: pausedAt.Add(time.Minute), Backstop: time.Hour, OpenPhaseRole: implementingWorker})
	if err != nil {
		t.Fatalf("cancel the release of %s while LEGION is paused: %v", otherIssue, err)
	}
	if cancelled.Pause != pause.ID || len(cancelled.Resumed) != 0 || !slices.Equal(cancelled.Stopped, roles) {
		t.Fatalf("the cancel of %s's release while LEGION is paused = %+v; want LEGION's pause %d, no role resumed and %v stopped",
			otherIssue, cancelled, pause.ID, roles)
	}
	starts := pauseCount(t, st, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start'`, otherIssue)
	stops := pauseCount(t, st, `select count(*) from project_pause_stops where pause_id = $1 and issue = $2
		and ended_as = 'stopped'`, pause.ID, otherIssue)
	if starts != 0 || stops != len(roles) {
		t.Fatalf("after the cancel %d starts are queued and pause %d records %d stops of %s; want none queued and %d stopped",
			starts, pause.ID, stops, otherIssue, len(roles))
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
// dispatch://INFRA-5682 R1: both reads pass over a resume whose clock a wait in the wake pacing's
// queue stopped, however long ago its wake was, until the end of that wait or the next daemon's
// boot starts the clock.
func TestTheStuckAndStrandedReadsMeasureTheStuckClockAndPassOverAPausedProject(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	woke := pausedAt.Add(-2 * time.Hour)
	recordRelease(t, st, woke.Add(-time.Hour))
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, "legion-legion-legion-208-architect",
		pods.WakeDelivery, woke, 0); err != nil || !taken {
		t.Fatalf("take the architect's wake = %t, %v", taken, err)
	}
	clock := woke.Add(30 * time.Minute)
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

	queue := func() {
		t.Helper()
		if err := st.SetResumeQueued(ctx, "legion", rootIssue, claim.RoleArchitect); err != nil {
			t.Fatalf("stop the architect's stuck clock for its wait in the queue: %v", err)
		}
		if stuck, unfinished := reads(woke.Add(24 * time.Hour)); len(stuck) != 0 || len(unfinished) != 0 {
			t.Fatalf("while the wake is queued the reads = %+v and %+v, want neither, a day after the wake", stuck, unfinished)
		}
	}
	queue()
	boot := woke.Add(20 * time.Minute)
	if started, err := st.StartQueuedResumeClocks(ctx, "legion", boot); err != nil || started != 1 {
		t.Fatalf("the boot started %d stopped clocks, %v; want the architect's", started, err)
	}
	if stuck, unfinished := reads(boot); len(stuck) != 1 || len(unfinished) != 1 || !stuck[0].ClockAt.Equal(boot) {
		t.Fatalf("after the boot the reads = %+v and %+v, want the architect's resume with its clock at the boot", stuck, unfinished)
	}
	queue()
	if _, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleArchitect, clock); err != nil {
		t.Fatalf("start the architect's stuck clock at its let-out: %v", err)
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
		if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, pausedAt, pause.ID); err != nil || !taken {
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

// Decision 13 of dispatch://INFRA-3759: the resume ends the hold of a pod in which a role is live,
// and a claim with no process recorded that is queued or launching is no live role. The held rows
// are claimable from the resume's commit, before the resume ends the holds, so a held start the
// outbox runs then, or an operator's spawn the resume's rule parks in the wake pacing, leaves its
// claim launching with no locator while no role of the pod runs, and a claim nothing has launched
// yet is queued with none: the hold stays. A launch whose process is recorded is coming up in the
// pod, and the resume ends the hold for it.
func TestTheResumeEndsNoPodHoldForAClaimWithNoProcessYet(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	pause, _ := pauseLegion(t, st, pausedAt)
	if _, err := holdPod(st, pause, rootIssue, pausedAt); err != nil {
		t.Fatalf("hold the pod: %v", err)
	}
	resumedAt := pausedAt.Add(time.Hour)
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-208-tester", Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleTester, TreeEpoch: 1, Generation: 1, State: supervise.StateQueued}); err != nil {
		t.Fatalf("queue the tester: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", rootIssue, PausePodHoldResumed, resumedAt); err != nil || ended {
		t.Fatalf("the resume's end with the tester queued and no process recorded = %t, %v; want nothing ended", ended, err)
	}
	const token = claim.Token("legion-legion-legion-208-implementer")
	waiting := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleImplementer,
		TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateLaunching}
	if err := st.PutClaim(ctx, waiting); err != nil {
		t.Fatalf("park the implementer's launch: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", rootIssue, PausePodHoldResumed, resumedAt); err != nil || ended {
		t.Fatalf("the resume's end with the implementer launching and no process recorded = %t, %v; want nothing ended", ended, err)
	}
	waiting.Generation = 2
	waiting.Locator = &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: "pod-uid-2/2",
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-208", PodUID: "pod-uid-2", Container: "roles", Generation: 2}}
	if err := st.PutClaim(ctx, waiting); err != nil {
		t.Fatalf("record the implementer's launch in the pod: %v", err)
	}
	if ended, err := st.EndLivePausePodHold(ctx, "legion", rootIssue, PausePodHoldResumed, resumedAt); err != nil || !ended {
		t.Fatalf("the resume's end with the implementer's process coming up in the pod = %t, %v; want the hold ended", ended, err)
	}
}

// Decision 12 of dispatch://INFRA-3759, and the let-out of decision 14: the sweep's read of the
// holds carries with each hold the latest let-out of a launch its own pause held for a role of the
// pod, the greatest stuck clock of the issue's resumes that pause held, and reads a hold that ended
// before since where that let-out came after it. A resume no pause held moves nothing.
//
// A hold whose pause still owes a launch of the pod is read whatever since: the resume of that
// launch is open (no first turn, not closed, no ready), and its stuck clock is still the moment of
// the project's resume until the wake pacing lets the launch out. The third pod's hold is that
// hold. A round that opened after the hold ended reads it, so the round keeps the pod for the owed
// launch (internal/daemon's podHolds.owes). Each launch the other two pods' holds owed has taken
// its first turn, so those holds are read by their let-outs alone. Without the read of the open
// resume, the third pod's hold is missing from every read since a moment after the resume.
//
// dispatch://INFRA-5682 R1: while a launch the pause owes waits in the wake pacing's queue, its
// resume's clock is stopped and it has no let-out yet, so the hold reads its let-out as the read's
// own now, and a round's bound counts the hold on to it. Only an open resume waits: a clock left
// stopped on a resume whose launch took its first turn says nothing.
func TestTheSweepsReadOfTheHoldsCarriesTheLatestLetOutOfTheirPause(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)
	const other, owed = "LEGION-300", "LEGION-301"
	resumedAt := pausedAt.Add(time.Hour)
	for _, issue := range []string{rootIssue, other, owed} {
		if _, err := holdPod(st, pause, issue, pausedAt); err != nil {
			t.Fatalf("hold the pod of %s: %v", issue, err)
		}
		if _, err := st.EndPausePodHold(ctx, "LEGION", issue, PausePodHoldResumed, resumedAt); err != nil {
			t.Fatalf("end the hold of %s at the resume: %v", issue, err)
		}
	}
	for _, resume := range []struct {
		issue   string
		role    claim.Role
		letOut  time.Duration
		byPause bool
		// launched is whether the launch took its first turn a minute after its let-out
		// (MarkPodResumeFirstTurn), which ends the resume's wait.
		launched bool
	}{
		{rootIssue, claim.RoleImplementer, 3 * time.Minute, true, true},
		{rootIssue, claim.RoleArchitect, 10 * time.Minute, true, true},
		{rootIssue, claim.RoleTester, 20 * time.Minute, false, false},
		{other, claim.RoleImplementer, 5 * time.Minute, true, true},
		{owed, claim.RoleImplementer, 0, true, false},
	} {
		var held *int64
		kind := "delivery"
		if resume.byPause {
			held, kind = &pause.ID, pods.WakePause
		}
		var id int64
		if err := st.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, clock_at, pause_id)
			values (0, 'legion', $1, $2, $3, $4, $5, $6) returning id`, resume.issue, string(resume.role), kind, pausedAt.Add(time.Minute),
			resumedAt.Add(resume.letOut), held).Scan(&id); err != nil {
			t.Fatalf("record the resume of %s's %s: %v", resume.issue, resume.role, err)
		}
		if resume.launched {
			if err := st.MarkPodResumeFirstTurn(ctx, id, resumedAt.Add(resume.letOut+time.Minute)); err != nil {
				t.Fatalf("stamp the first turn of %s's %s: %v", resume.issue, resume.role, err)
			}
		}
	}
	now := resumedAt.Add(30 * time.Minute)
	read := func(since time.Duration, want map[string]time.Duration) {
		t.Helper()
		holds, err := st.PausePodHoldsSince(ctx, "LEGION", resumedAt.Add(since), now)
		if err != nil {
			t.Fatalf("read the holds since the resume %+v: %v", since, err)
		}
		got := map[string]time.Duration{}
		for _, hold := range holds {
			if hold.Pause != pause.ID || !hold.EndedAt.Equal(resumedAt) || hold.End != PausePodHoldResumed {
				t.Fatalf("a hold read since the resume %+v = %+v, want pause %d's, ended at the resume", since, hold, pause.ID)
			}
			got[hold.Issue] = hold.LetOut.Sub(resumedAt)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("the let-outs of the holds read since the resume %+v = %v, want %v", since, got, want)
		}
	}
	read(-time.Minute, map[string]time.Duration{rootIssue: 10 * time.Minute, other: 5 * time.Minute, owed: 0})
	read(7*time.Minute, map[string]time.Duration{rootIssue: 10 * time.Minute, owed: 0})
	read(10*time.Minute, map[string]time.Duration{owed: 0})

	if err := st.SetResumeQueued(ctx, "legion", owed, claim.RoleImplementer); err != nil {
		t.Fatalf("stop the clock of %s's owed launch for its wait in the queue: %v", owed, err)
	}
	if _, err := st.Pool().Exec(ctx, `update pod_resumes set queued = true where issue = $1`, other); err != nil {
		t.Fatalf("leave a stopped clock on the launched resume of %s: %v", other, err)
	}
	read(-time.Minute, map[string]time.Duration{rootIssue: 10 * time.Minute, other: 5 * time.Minute, owed: now.Sub(resumedAt)})
	read(10*time.Minute, map[string]time.Duration{owed: now.Sub(resumedAt)})
}

// The architect-stalled arms count no time before the let-out of the launch a pause held for an
// architect (PauseLetOutsTx): the stuck clock of its resume of that pause, which the project's
// resume starts and the launch's let-out moves. While that launch waits in the wake pacing's
// queue it has no let-out yet (dispatch://INFRA-5682 R1), so the read answers the reader's own
// now for it, however long ago the project resumed, and the let-out's moment once it has one.
func TestALaunchWaitingInThePacingReadsAsLetOutAtTheReadersNow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)
	resumedAt := pausedAt.Add(time.Hour)
	if _, err := st.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, clock_at, pause_id)
		values (0, 'legion', $1, 'architect', $2, $3, $4, $5)`, rootIssue, pods.WakePause, pausedAt.Add(time.Minute), resumedAt, pause.ID); err != nil {
		t.Fatalf("record the architect's resume the pause held: %v", err)
	}
	now := resumedAt.Add(40 * time.Minute)
	want := func(what string, want time.Time) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) error {
			letOuts, err := PauseLetOutsTx(ctx, tx, "legion", pause.ID, claim.RoleArchitect, now)
			if err != nil {
				return err
			}
			if got, held := letOuts[rootIssue]; !held || len(letOuts) != 1 || !got.Equal(want) {
				t.Fatalf("%s the let-outs read %v, want the architect's of %s at %s", what, letOuts, rootIssue, want)
			}
			return nil
		})
	}
	want("before its launch reaches the pacing", resumedAt)
	if err := st.SetResumeQueued(ctx, "legion", rootIssue, claim.RoleArchitect); err != nil {
		t.Fatalf("stop the architect's stuck clock for its wait in the queue: %v", err)
	}
	want("while its launch waits in the queue", now)
	letOut := resumedAt.Add(25 * time.Minute)
	if _, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleArchitect, letOut); err != nil {
		t.Fatalf("start the architect's stuck clock at its let-out: %v", err)
	}
	want("once the pacing has let its launch out", letOut)
}

// Decision 12 of dispatch://INFRA-3759, as review rounds 3 and 4 of dispatch://INFRA-4109 set it:
// the sweep's read names, for each hold, every role of the pod that the hold's own pause still
// owes a launch (PausePodHold.Owed). A round keeps the pod for a role only while the pause still
// owes that role a launch, so the daemon reads it role by role and never from the latest let-out
// of all the roles.
//
// Both pods were held by the pause and their holds ended at the resume. On the first pod, the
// pause still owes the implementer and the tester a launch: each has a resume of that pause that
// no first turn stamped, nothing closed and no ready stamped, and each is named, in order. The
// architect became ready (MarkPodResumeReady) and its resume stays open, as a role the daemon
// suspended before its first turn leaves it. The reviewer took its first turn, and the merger's
// resume was closed. None of those three is owed, and neither is the planner, whose open resume is
// of a later pause. The second pod has the same four resumes and no other: its hold is read where
// it ended or its pause let a launch out after since, and is missing from a read past both,
// whatever its open, ready resume. The latest let-out still counts every resume of the pause
// (PausePodHold.LetOut).
func TestTheSweepsReadOfTheHoldsNamesEachRoleItsPauseStillOwesALaunch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)
	const nothingOwed = "LEGION-300"
	resumedAt := pausedAt.Add(time.Hour)
	for _, issue := range []string{rootIssue, nothingOwed} {
		if _, err := holdPod(st, pause, issue, pausedAt); err != nil {
			t.Fatalf("hold the pod of %s: %v", issue, err)
		}
		if _, err := st.EndPausePodHold(ctx, "LEGION", issue, PausePodHoldResumed, resumedAt); err != nil {
			t.Fatalf("end the hold of %s at the resume: %v", issue, err)
		}
	}
	endPause(t, st, pause.ID, resumedAt)
	later, _ := pauseLegion(t, st, resumedAt.Add(time.Minute))
	endPause(t, st, later.ID, resumedAt.Add(2*time.Minute))

	// resume writes the resume of issue's role that held held, with its stuck clock letOut after
	// the resume, and answers its id.
	resume := func(issue string, role claim.Role, held Pause, letOut time.Duration) int64 {
		t.Helper()
		var id int64
		if err := st.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, clock_at, pause_id)
			values (0, 'legion', $1, $2, $3, $4, $5, $6) returning id`, issue, string(role), pods.WakePause, pausedAt.Add(time.Minute),
			resumedAt.Add(letOut), held.ID).Scan(&id); err != nil {
			t.Fatalf("record the resume of %s's %s: %v", issue, role, err)
		}
		return id
	}
	for _, issue := range []string{rootIssue, nothingOwed} {
		if err := st.MarkPodResumeReady(ctx, resume(issue, claim.RoleArchitect, pause, 5*time.Minute), resumedAt.Add(6*time.Minute)); err != nil {
			t.Fatalf("stamp the ready of %s's architect: %v", issue, err)
		}
		if err := st.MarkPodResumeFirstTurn(ctx, resume(issue, claim.RoleReviewer, pause, time.Minute), resumedAt.Add(2*time.Minute)); err != nil {
			t.Fatalf("stamp the first turn of %s's reviewer: %v", issue, err)
		}
		resume(issue, claim.RoleMerger, pause, 2*time.Minute)
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := CloseRoleOpenResumeTx(ctx, tx, "legion", issue, claim.RoleMerger, resumedAt.Add(3*time.Minute))
			return err
		}); err != nil {
			t.Fatalf("close the resume of %s's merger: %v", issue, err)
		}
		resume(issue, claim.RolePlanner, later, 4*time.Minute)
	}
	resume(rootIssue, claim.RoleImplementer, pause, 3*time.Minute)
	resume(rootIssue, claim.RoleTester, pause, 0)

	owed := fmt.Sprint([]claim.Role{claim.RoleImplementer, claim.RoleTester})
	for _, tc := range []struct {
		since time.Duration
		// want is each hold the read returns, by issue, with the roles its pause owes a launch.
		want map[string]string
	}{
		{since: -time.Minute, want: map[string]string{rootIssue: owed, nothingOwed: fmt.Sprint([]claim.Role(nil))}},
		{since: 10 * time.Minute, want: map[string]string{rootIssue: owed}},
	} {
		holds, err := st.PausePodHoldsSince(ctx, "LEGION", resumedAt.Add(tc.since), resumedAt.Add(time.Hour))
		if err != nil {
			t.Fatalf("read the holds since the resume %+v: %v", tc.since, err)
		}
		got := map[string]string{}
		for _, hold := range holds {
			if hold.Pause != pause.ID || !hold.LetOut.Equal(resumedAt.Add(5*time.Minute)) {
				t.Fatalf("a hold read since the resume %+v = %+v, want pause %d's, with the latest let-out of its resumes %s",
					tc.since, hold, pause.ID, resumedAt.Add(5*time.Minute))
			}
			got[hold.Issue] = fmt.Sprint(hold.Owed)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("the roles the holds owe a launch, read since the resume %+v = %v, want %v", tc.since, got, tc.want)
		}
	}
}
