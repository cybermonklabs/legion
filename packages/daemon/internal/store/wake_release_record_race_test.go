package store

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// R3 and check l2's row (f) (dispatch://INFRA-3589): a wake taken after a release recorded its
// role as `released` and before the release ended names that release, so pod_resumes holds one
// resume, of that release. RecordPodRelease points at itself each such wake it can read, and a
// take reads the newest recorded release. Two transactions that run at once must not each miss
// the other: here the wake's take has read the releases and inserted its resume, and the
// release's record starts before the take commits. The test commits the take once the record
// waits for the take's lock on the release mark, or once the record returns.
//
// The idle rig's held-stop row (internal/daemon, AWakeDuringAHeldStopBringsThePodBackOnce) meets
// this order on a loaded pod: the daemon's record of the release and the wake's take of the
// implementer come within a few milliseconds of each other. Without the lock, the record would
// not read the take's resume, so the release would be recorded and its one resume would name
// release 0.
func TestAWakeTakenWhileItsReleaseIsRecordedNamesThatRelease(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	// The release is marked, and its stop has recorded the implementer as released.
	markRelease(t, store, 1, 1, 1, 0, claim.RoleImplementer)
	records := &PodRecords{store: store, project: "legion"}

	tx, err := store.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the wake's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeAgentMessage, at.Add(-time.Second), 0); err != nil || !taken {
		t.Fatalf("take the implementer's wake = %t, %v; want it taken", taken, err)
	}
	var take int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&take); err != nil {
		t.Fatalf("read the take's backend: %v", err)
	}

	recordCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type recorded struct {
		id  int64
		err error
	}
	done := make(chan recorded, 1)
	go func() {
		id, _, err := store.RecordPodRelease(recordCtx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
			Roles: []string{"implementer"}, At: at})
		done <- recorded{id, err}
	}()
	var rec recorded
	returned := false
	if !testwait.Holds(20*time.Second, 20*time.Millisecond, func() bool {
		select {
		case rec = <-done:
			returned = true
			return true
		default:
		}
		return blockedBy(t, store, take)
	}) {
		t.Fatal("control: the record neither waited for the take's lock on the release mark nor returned")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the wake's transaction: %v", err)
	}
	if !returned {
		rec = <-done
	}
	if rec.err != nil {
		t.Fatalf("record the release while the wake's transaction is open: %v", rec.err)
	}
	release := rec.id

	var resumes, named int64
	if err := store.Pool().QueryRow(ctx, `select count(*), count(*) filter (where release_id = $2)
		from pod_resumes where project = 'legion' and issue = $1`, rootIssue, release).Scan(&resumes, &named); err != nil {
		t.Fatalf("read the resumes: %v", err)
	}
	if resumes != 1 || named != 1 {
		var got int64
		_ = store.Pool().QueryRow(ctx, `select release_id from pod_resumes where project = 'legion' and issue = $1
			order by id limit 1`, rootIssue).Scan(&got)
		t.Errorf("pod_resumes holds %d resumes, %d of them naming release %d (the first names release %d); want one resume, of that release: the wake came after the release recorded the role and ends that release",
			resumes, named, release, got)
	}
}

// The same two transactions, where the release does not finish: a cancel for another cause (a
// role stop that met a start of another role, say) deletes each wake taken under the mark and
// starts the role once (dropWakesUnderRelease), since the pod never went away. A take still open
// when the cancel runs must not leave its resume behind: that resume names no release that
// happened, and the wake's own start beside it would tell the role its pod was released.
func TestAWakeTakenWhileItsReleaseIsCancelledLeavesNoResume(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	// The release stops both roles of the pod and has recorded the implementer as released, as in
	// TestACancelAfterAWakeOfARoleTheReleaseRecordedStartsTheRoleOnceWithNoWake, where the take
	// commits before the cancel and the cancel drops it.
	rootClaim(t, store, claim.RoleImplementer, supervise.StateIdle, "")
	rootClaim(t, store, claim.RoleArchitect, supervise.StateIdle, "")
	boundary, _, _ := releaseTwoRolesThroughTheStore(t, store)
	stoppedByTheRelease(t, store, claim.RoleImplementer, boundary)
	records := &PodRecords{store: store, project: "legion"}

	tx, err := store.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the wake's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeAgentMessage, at, 0); err != nil || !taken {
		t.Fatalf("take the implementer's wake = %t, %v; want it taken", taken, err)
	}
	var take int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&take); err != nil {
		t.Fatalf("read the take's backend: %v", err)
	}

	cancelCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.CancelIssueRelease(cancelCtx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
		done <- err
	}()
	var cancelErr error
	returned := false
	if !testwait.Holds(20*time.Second, 20*time.Millisecond, func() bool {
		select {
		case cancelErr = <-done:
			returned = true
			return true
		default:
		}
		return blockedBy(t, store, take)
	}) {
		t.Fatal("control: the cancel neither waited for the take's lock on the release mark nor returned")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the wake's transaction: %v", err)
	}
	if !returned {
		cancelErr = <-done
	}
	if cancelErr != nil {
		t.Fatalf("cancel the release while the wake's transaction is open: %v", cancelErr)
	}

	var resumes int64
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes where project = 'legion' and issue = $1`,
		rootIssue).Scan(&resumes); err != nil {
		t.Fatalf("read the resumes: %v", err)
	}
	if resumes != 0 {
		t.Errorf("pod_resumes holds %d resumes after the cancel, want none: the release was cancelled, so the pod never went away and no released pod came back", resumes)
	}
}

// blockedBy is whether a backend of the database waits for a lock that backend holds
// (pg_blocking_pids): what the rows of this file wait for before they end that backend's
// transaction.
func blockedBy(t *testing.T, st *Store, backend int32) bool {
	t.Helper()
	var waiting bool
	err := st.Pool().QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity
		where $1 = any(pg_blocking_pids(pid)))`, backend).Scan(&waiting)
	return err == nil && waiting
}

// Window 1 of round 9's class search (dispatch://INFRA-3589): the workflow's wake closes a
// resume whose pod went away (ClosePodGoneResume) and then takes its own (TakePodWake), in one
// transaction (workflow's wakeReleased). A release recorded between the close's read of the
// newest release and the take's lock on the mark is missed by the close and read by the take.
// The take then finds the old resume still open and takes nothing, so the wake is lost while the
// role's pod is gone. Here the record of a release of the pod no claim owns, which closes no
// resume, holds the mark while the wake runs. The close must wait for the record and read its
// release: the old resume is closed, and the wake takes its own.
func TestAWakeWhileANewerReleaseIsRecordedClosesTheOldResumeAndTakesItsOwn(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	// An idle release took the pod and recorded the implementer. A wake of the implementer came
	// after it, and its resume stands open: the pod it waits for never came back.
	idle, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("record the idle release: %v", err)
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, pods.WakeDelivery,
		at.Add(-time.Hour), 0); err != nil || !taken {
		t.Fatalf("take the implementer's first wake = %t, %v; want it taken", taken, err)
	}
	old, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer)
	if err != nil || !found {
		t.Fatalf("the implementer's open resume = %t, %v; want the one its first wake recorded", found, err)
	}
	// The second rule then releases the pod no claim can use. Its mark names no role, so it closes
	// no resume.
	marked, err := store.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true, Rows: []IdleReleaseRow{{
			Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`),
			Boundary: true}}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release of the pod no claim owns marked", marked, err)
	}

	// The teardown's record of that release has locked the mark and written the release's row, and
	// has not committed: the first two statements of RecordPodRelease. The rest of it changes
	// nothing here, since the mark names no role and the pod has no hold.
	rec, err := store.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the record's transaction: %v", err)
	}
	defer func() { _ = rec.Rollback(ctx) }()
	if _, err := rec.Exec(ctx, `select 1 from issue_releases where project = 'legion' and issue = $1 for update`,
		rootIssue); err != nil {
		t.Fatalf("lock the release mark: %v", err)
	}
	var newer int64
	if err := rec.QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
		values ('legion', $1, 1, '', $2, 0) returning id`, rootIssue, at.Add(-time.Minute)).Scan(&newer); err != nil {
		t.Fatalf("write the newer release: %v", err)
	}
	var holder int32
	if err := rec.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatalf("read the record's backend: %v", err)
	}

	// The implementer's backstop fires while the record is open.
	records := podRecords(t, store)
	wakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type woken struct {
		newest int64
		closed bool
		taken  bool
		err    error
	}
	done := make(chan woken, 1)
	go func() {
		var w woken
		w.err = Transact(wakeCtx, store.Pool(), func(wakeCtx context.Context, tx pgx.Tx) error {
			var err error
			if w.newest, w.closed, err = records.ClosePodGoneResume(wakeCtx, tx, rootIssue, claim.RoleImplementer); err != nil {
				return err
			}
			w.taken, err = records.TakePodWake(wakeCtx, tx, rootIssue, claim.RoleImplementer, pods.WakeBackstop, at, 0)
			return err
		})
		done <- w
	}()
	if !testwait.Holds(20*time.Second, 20*time.Millisecond, func() bool { return blockedBy(t, store, holder) }) {
		t.Fatal("the wake never waited for the record's lock on the release mark")
	}
	if err := rec.Commit(ctx); err != nil {
		t.Fatalf("commit the record: %v", err)
	}
	w := <-done
	if w.err != nil {
		t.Fatalf("the wake: %v", w.err)
	}

	if !w.closed || w.newest != newer || !w.taken {
		t.Errorf("the wake closed a resume = %t under release %d and took its own wake = %t; want the old resume closed under release %d and the wake taken: that release was recorded before the take",
			w.closed, w.newest, w.taken, newer)
	}
	var oldClosed bool
	if err := store.Pool().QueryRow(ctx, `select closed_at is not null from pod_resumes where id = $1`,
		old).Scan(&oldClosed); err != nil {
		t.Fatalf("read the old resume: %v", err)
	}
	if !oldClosed {
		t.Errorf("the old resume %d is still open; want it closed: its pod went away with release %d", old, newer)
	}
	// The wake's own resume ends the idle release, the newest that suspended a role
	// (LatestPodRelease), and was taken after the newer release.
	var open, id, release, seen int64
	if err := store.Pool().QueryRow(ctx, `select count(*), coalesce(max(id), 0), coalesce(max(release_id), 0),
		coalesce(max(seen_release), 0) from pod_resumes where project = 'legion' and issue = $1
		and role = 'implementer' and first_turn_at is null and closed_at is null`, rootIssue).Scan(&open, &id, &release, &seen); err != nil {
		t.Fatalf("read the open resumes: %v", err)
	}
	if open != 1 || id == old || release != idle || seen != newer {
		t.Errorf("open resumes of the implementer = %d (the newest is %d, of release %d, seen release %d); want one, the wake's own and not the old %d, of release %d and seen release %d",
			open, id, release, seen, old, idle, newer)
	}
}

// Window 2 of round 9's class search (dispatch://INFRA-3589): the outbox's delivery wake resumes
// its claim in process, with no `start` row to wait for the release's teardown. A release's stop
// suspends the claim as `released` first, and records the role on the mark after it, in a
// transaction of its own. A delivery wake that read the role on the mark in that gap would find
// it not recorded, take the wake and resume the role in the pod the teardown is about to take. So
// the delivery wake takes nothing while any mark of the issue stands, and once the teardown has
// cleared the mark it takes its wake against the newest release recorded (LatestPodRelease): the
// release the teardown recorded, or the release before it where the teardown gave up on its record
// (internal/daemon's recordGivenUp, SettleUnrecordedRelease).
func TestADeliveryWakeTakesNothingWhileAReleaseMarkStands(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	rootClaim(t, store, claim.RoleImplementer, supervise.StateIdle, "")
	rootClaim(t, store, claim.RoleArchitect, supervise.StateIdle, "")
	boundary, stop, _ := releaseTwoRolesThroughTheStore(t, store)
	// The implementer's stop has suspended its claim as `released`, and has not recorded the role
	// on the mark yet.
	releasedClaim(t, store, claim.RoleImplementer)
	resumes := func() int64 {
		t.Helper()
		var n int64
		if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes where project = 'legion' and issue = $1`,
			rootIssue).Scan(&n); err != nil {
			t.Fatalf("count the resumes: %v", err)
		}
		return n
	}

	wake, err := store.TakeDeliveryWake(ctx, "legion", rootIssue, releasedToken, 1, at, 0)
	if err != nil || wake.Taken || !wake.InRelease {
		t.Fatalf("the delivery wake before the stop records the role = %+v, %v; want nothing taken and the release named: the teardown is about to take the pod",
			wake, err)
	}
	if n := resumes(); n != 0 {
		t.Fatalf("pod_resumes holds %d resumes after the delivery wake in the release, want none", n)
	}

	// The stops record their roles. The mark still stands, and the wake still takes nothing.
	stoppedByTheRelease(t, store, claim.RoleImplementer, boundary)
	stoppedByTheRelease(t, store, claim.RoleArchitect, stop)
	wake, err = store.TakeDeliveryWake(ctx, "legion", rootIssue, releasedToken, 1, at, 0)
	if err != nil || wake.Taken || !wake.InRelease {
		t.Fatalf("the delivery wake after the stops record their roles = %+v, %v; want nothing taken and the release named",
			wake, err)
	}
	if n := resumes(); n != 0 {
		t.Fatalf("pod_resumes holds %d resumes after the delivery wake in the release, want none", n)
	}

	// The teardown records the release and clears the mark. The next attempt takes the wake
	// against that release.
	release, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer", "architect"}, At: at.Add(time.Minute)})
	if err != nil {
		t.Fatalf("record the release: %v", err)
	}
	if err := store.ClearIssueRelease(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("clear the release's mark: %v", err)
	}
	wake, err = store.TakeDeliveryWake(ctx, "legion", rootIssue, releasedToken, 1, at.Add(2*time.Minute), 0)
	if err != nil || !wake.Taken || wake.InRelease || wake.StartQueued {
		t.Fatalf("the delivery wake once the teardown finished = %+v, %v; want the wake taken", wake, err)
	}
	if n := resumes(); n != 1 {
		t.Fatalf("pod_resumes holds %d resumes after the wake, want the wake's one", n)
	}
	var resumed, seen int64
	var kind string
	if err := store.Pool().QueryRow(ctx, `select release_id, seen_release, kind from pod_resumes
		where project = 'legion' and issue = $1 and role = 'implementer'`, rootIssue).Scan(&resumed, &seen, &kind); err != nil {
		t.Fatalf("read the wake's resume: %v", err)
	}
	if resumed != release || seen != release || kind != pods.WakeDelivery {
		t.Errorf("the wake's resume is a %s wake of release %d, seen release %d; want a %s wake of release %d, seen release %d",
			kind, resumed, seen, pods.WakeDelivery, release, release)
	}
}

// A release's record of a wake that a project's pause holds (main's c7e1cbd5 in this pull
// request). A pause starts no release, and a release whose workspace check ran before the pause
// runs to its end (IssueReleaseCheck, decision 12 of dispatch://INFRA-3759). A wake of a role
// that release recorded can then come while the project is paused, and it is a held wake of the
// pause (PauseOfWake, decision 15), which pairs with no release
// (pod_resumes_held_wake_pairs_with_no_release). The release's record points that wake at itself
// as it points every wake taken under its mark, and keeps its release 0: its seen release becomes
// the release, and its time and its stuck clock become the release's time. A record that set the
// release on it would fail the check, and the teardown with it.
func TestARecordedReleaseKeepsAHeldWakeTakenUnderItsMarkPairedWithNoRelease(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	markRelease(t, store, 1, 1, 1, 0, claim.RoleImplementer)
	pause, _ := pauseLegion(t, store, at.Add(-time.Minute))
	records := podRecords(t, store)
	inTx(t, store, func(tx pgx.Tx) error {
		held, stopping, err := records.PauseOfWake(ctx, tx, rootIssue, releasedToken, 0)
		if err != nil || held != pause.ID || stopping != 0 {
			t.Fatalf("the pause of the implementer's wake = held %d, stopping %d, %v; want held by pause %d and stopped by none",
				held, stopping, err, pause.ID)
		}
		if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, at.Add(-30*time.Second), held); err != nil || !taken {
			t.Fatalf("take the held wake = %t, %v; want it taken", taken, err)
		}
		return nil
	})

	release, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at})
	if err != nil {
		t.Fatalf("record the release under the held wake: %v", err)
	}
	var resumed, seen, held int64
	var when, clock time.Time
	if err := store.Pool().QueryRow(ctx, `select release_id, seen_release, coalesce(pause_id, 0), at, clock_at from pod_resumes
		where project = 'legion' and issue = $1 and role = 'implementer'`, rootIssue).Scan(&resumed, &seen, &held, &when, &clock); err != nil {
		t.Fatalf("read the held wake's resume: %v", err)
	}
	if resumed != 0 || seen != release || held != pause.ID || !when.Equal(at) || !clock.Equal(at) {
		t.Errorf("the held wake's resume = release %d, seen release %d, pause %d, time %s, stuck clock %s; want release 0, seen release %d, pause %d and the release's time %s for both",
			resumed, seen, held, when.UTC().Format(time.RFC3339Nano), clock.UTC().Format(time.RFC3339Nano),
			release, pause.ID, at.Format(time.RFC3339Nano))
	}
}

// A release's record of a held wake's start (dispatch://INFRA-3589, R3). A wake of a role that a
// project's pause stopped, taken while a release that recorded the role on its mark still stands,
// reads that pause as what stopped the role (PauseOfWake). So its start carries the pause's
// words, which say that the pod was kept. The record is the moment the release takes that pod, so
// it rewrites the task of that start through the workflow's rule (PodRelease.WakeTask), told the
// wake kind the start carries. Only that task changes: the start keeps the rest of its payload, and
// the wake stays held by its pause at release 0. A start below the release's last row, a phase's
// start, and the start of a role whose one wake came before the mark (below its wake_row) are no
// such start, and keep their task. The store does not import the workflow, so the rule here is a
// stand-in, as in TestACancelWhileThePausedProjectRetellsTheStartOfTheWakeItKeeps. The daemon's
// TestARecordedReleaseTellsAHeldWakeThatTheReleaseTookItsPod holds the workflow's own words.
func TestARecordedReleaseRetellsTheStartOfAHeldWakeTakenUnderItsMark(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	reviewer, err := claim.NewToken("legion", rootIssue, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	// A wake of the reviewer before the mark: the mark's wake_row is its resume's id.
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, reviewer, pods.WakeChecks, at.Add(-time.Hour), 0); err != nil || !taken {
		t.Fatalf("take the reviewer's wake before the mark = %t, %v; want it taken", taken, err)
	}
	before, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleReviewer)
	if err != nil || !found {
		t.Fatalf("the reviewer's open resume = %t, %v; want the one its wake recorded", found, err)
	}
	queueWakeStart(t, store, claim.RoleImplementer, pods.WakeChecks)
	last := int64(pauseCount(t, store, `select coalesce(max(id), 0)::int from outbox where issue = $1`, rootIssue))
	markRelease(t, store, last, last, last, 0, claim.RoleImplementer, claim.RoleReviewer)
	if _, err := store.Pool().Exec(ctx, `update issue_releases set wake_row = $2 where project = 'legion' and issue = $1`,
		rootIssue, before); err != nil {
		t.Fatalf("set the mark's newest wake: %v", err)
	}
	pause, _ := pauseLegion(t, store, at.Add(-time.Minute))
	records := podRecords(t, store)
	inTx(t, store, func(tx pgx.Tx) error {
		held, _, err := records.PauseOfWake(ctx, tx, rootIssue, releasedToken, 0)
		if err != nil || held != pause.ID {
			t.Fatalf("the pause of the implementer's wake = %d, %v; want pause %d", held, err, pause.ID)
		}
		if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, at.Add(-30*time.Second), held); err != nil || !taken {
			t.Fatalf("take the held wake = %t, %v; want it taken", taken, err)
		}
		return nil
	})
	// The held wake's start, as the workflow's wake queues it (wakeRow), in the pause's words.
	const paused = "the pause's words for the settlement"
	wake, err := record.NewOutboxRow(rootIssue, record.SuperviseRequest{Op: "start", Tree: rootIssue, Role: claim.RoleImplementer,
		Generation: 1, Task: paused, WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build", Pause: pause.ID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the held wake's start: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rootIssue, string(wake.Payload)); err != nil {
		t.Fatalf("queue the held wake's start: %v", err)
	}
	queueStart(t, store, claim.RoleImplementer, "implementing")
	queueWakeStart(t, store, claim.RoleReviewer, pods.WakeChecks)
	retell := func(kind, detail, task string) string {
		return "the release's " + kind + " wake of " + detail + ": " + task
	}

	release, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer", "reviewer"}, At: at, WakeTask: retell})
	if err != nil {
		t.Fatalf("record the release under the held wake: %v", err)
	}
	rows, err := store.Pool().Query(ctx, `select payload->>'role', coalesce(payload->>'phase', ''), coalesce(payload->>'task', ''),
		coalesce(payload->>'wakeKind', ''), coalesce(payload->>'pause', '') from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []string
	for rows.Next() {
		var role, named, task, kind, held string
		if err := rows.Scan(&role, &named, &task, &kind, &held); err != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, role+"|"+named+"|"+task+"|"+kind+"|"+held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	want := []string{
		"implementer||the wake's task|" + pods.WakeChecks + "|",
		"implementer||the release's " + pods.WakeChecks + " wake of head1 failed build: " + paused + "|" + pods.WakeChecks + "|" + strconv.FormatInt(pause.ID, 10),
		"implementer|implementing|the phase's task||",
		"reviewer||the wake's task|" + pods.WakeChecks + "|",
	}
	if !slices.Equal(starts, want) {
		t.Fatalf("queued starts after the record = %q, want %q: only the held wake's start is told again", starts, want)
	}
	var resumed, seen, held int64
	if err := store.Pool().QueryRow(ctx, `select release_id, seen_release, coalesce(pause_id, 0) from pod_resumes
		where project = 'legion' and issue = $1 and role = 'implementer'`, rootIssue).Scan(&resumed, &seen, &held); err != nil {
		t.Fatalf("read the held wake's resume: %v", err)
	}
	if resumed != 0 || seen != release || held != pause.ID {
		t.Errorf("the held wake's resume = release %d, seen release %d, pause %d; want release 0, seen release %d, pause %d",
			resumed, seen, held, release, pause.ID)
	}
}

// A release's record and a project's resume at the same moment (the reviewer's read-ahead of
// dispatch://INFRA-3589, finding 1). A wake that the pause holds was taken under the release's
// mark, and its start carries the pause's words. The record writes the held wake's resume and
// then retells its start (RecordPodRelease). The resume writes the starts the pause holds
// (ResumeRequest.MarkHeld) and then the project's open resumes (SetProjectResumeClocksTx). Those
// are the same two rows in the other order. Interleaved, each waits for a row the other wrote,
// and Postgres aborts one of the two as a deadlock. An aborted record writes no release, and the
// start keeps the pause's words. The record locks the pause's row after the mark and before it
// writes anything, and the resume locks that row first, so one of the two waits for the other.
//
// A barrier holds each transaction inside its first lock. The record waits in its retell, which
// runs after it wrote the resume. The resume waits in MarkHeld, which runs while it holds the
// pause's row. Each waits until the other reaches its own barrier, or until a transaction waits
// for a lock. Where both reach their barriers, the resume marks the start before the record
// rewrites it, which is the order that deadlocks. Both must commit, and the start must end in the
// release's words.
func TestARecordedReleaseAndAResumeOfThePauseAtOnceBothCommit(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	// A phase's start the pod was given before the mark, so the mark's rows have an id to name.
	queueStart(t, store, claim.RoleImplementer, "implementing")
	last := int64(pauseCount(t, store, `select coalesce(max(id), 0)::int from outbox where issue = $1`, rootIssue))
	markRelease(t, store, last, last, last, 0, claim.RoleImplementer)
	pause, _ := pauseLegion(t, store, at.Add(-time.Minute))
	records := podRecords(t, store)
	inTx(t, store, func(tx pgx.Tx) error {
		held, _, err := records.PauseOfWake(ctx, tx, rootIssue, releasedToken, 0)
		if err != nil || held != pause.ID {
			t.Fatalf("the pause of the implementer's wake = %d, %v; want pause %d", held, err, pause.ID)
		}
		if taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, pods.WakeChecks, at.Add(-30*time.Second), held); err != nil || !taken {
			t.Fatalf("take the held wake = %t, %v; want it taken", taken, err)
		}
		return nil
	})
	const paused = "the pause's words for the settlement"
	start := queueSupervise(t, store, record.SuperviseRequest{Op: "start", Tree: rootIssue, Role: claim.RoleImplementer,
		Generation: 1, Task: paused, WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build", Pause: pause.ID})

	recordIn, resumeIn, resumeMarked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var recordOnce, resumeOnce, markedOnce sync.Once
	// meet holds a transaction at its barrier until arrived is closed, or until a transaction of
	// this test's database waits for a lock that another holds.
	meet := func(arrived <-chan struct{}, what string) {
		if !testwait.Holds(20*time.Second, 20*time.Millisecond, func() bool {
			select {
			case <-arrived:
				return true
			default:
			}
			var waiting bool
			err := store.Pool().QueryRow(ctx, `select exists (select 1 from pg_stat_activity
				where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&waiting)
			return err == nil && waiting
		}) {
			t.Errorf("the barrier waited 20s for %s, and no transaction waited for a lock", what)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	recorded, resumed := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := store.RecordPodRelease(runCtx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
			Roles: []string{"implementer"}, At: at, WakeTask: func(kind, _, task string) string {
				recordOnce.Do(func() { close(recordIn) })
				meet(resumeIn, "the resume to hold the pause's row")
				meet(resumeMarked, "the resume to mark the rows the pause holds")
				return "the release's " + kind + " wake: " + task
			}})
		recorded <- err
	}()
	go func() {
		_, err := store.Resume(runCtx, ResumeRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator}, At: time.Now().UTC(),
			MarkHeld: func(ctx context.Context, tx pgx.Tx, held Pause) error {
				resumeOnce.Do(func() { close(resumeIn) })
				meet(recordIn, "the record to write the held wake's resume")
				defer markedOnce.Do(func() { close(resumeMarked) })
				_, err := record.NewStore().MarkPauseHeldRows(ctx, tx, "LEGION", held.ID, time.Now().UTC())
				return err
			}})
		resumed <- err
	}()
	recordErr, resumeErr := <-recorded, <-resumed
	if recordErr != nil || resumeErr != nil {
		t.Fatalf("the release's record = %v and the project's resume = %v; want both committed: they write the held wake's resume and its start, and the pause's row orders them",
			recordErr, resumeErr)
	}

	var task string
	var releases int
	if err := store.Pool().QueryRow(ctx, `select coalesce(payload->>'task', ''),
		(select count(*) from pod_releases where project = 'legion' and issue = $2)::int from outbox where id = $1`,
		start, rootIssue).Scan(&task, &releases); err != nil {
		t.Fatalf("read the held wake's start: %v", err)
	}
	if want := "the release's " + pods.WakeChecks + " wake: " + paused; task != want || releases != 1 {
		t.Errorf("the held wake's start says %q, and %d releases are recorded; want %q and the one release: the release took the pod",
			task, releases, want)
	}
}
