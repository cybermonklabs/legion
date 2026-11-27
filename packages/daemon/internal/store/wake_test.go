package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releasedToken is the implementer claim of the released pod these tests read.
const releasedToken = claim.Token("legion-legion-legion-208-implementer")

// seedReleasedPod is one issue pod as a release leaves it: the running tree, and the
// implementer's claim suspended with the source `released`, its session kept.
func seedReleasedPod(t *testing.T, store *Store, session string) {
	t.Helper()
	openRunningTree(t, store)
	c := supervise.Claim{Token: releasedToken, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, Session: session,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased, PluginContract: 47}
	if err := store.PutClaim(context.Background(), c); err != nil {
		t.Fatalf("seed the released claim: %v", err)
	}
}

// inTx runs read in a transaction of the store's pool, as a fact's handler does.
func inTx(t *testing.T, store *Store, read func(pgx.Tx) error) {
	t.Helper()
	if err := Transact(context.Background(), store.pool, func(ctx context.Context, tx pgx.Tx) error {
		return read(tx)
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

// podRecords is project legion's pod tables as a caller outside the store reads and writes them.
func podRecords(t *testing.T, store *Store) *PodRecords {
	t.Helper()
	records, err := store.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	return records
}

// reopenStore is a daemon restart: a fresh pool and a fresh store over the same database, with
// nothing of the first one carried in memory. Only what was written survives it.
func reopenStore(t *testing.T, store *Store) *Store {
	t.Helper()
	reopened, err := Open(context.Background(), store.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	t.Cleanup(reopened.Close)
	return reopened
}

// A listener's report reads the claim it names by its claim token and by the session it kept,
// whatever the claim's state, since the report marks it in any of them. What the read says of the
// claim is what decides the wake: it is released until another path takes it over, and a
// suspension of any other source is somebody else's to resume.
func TestAListenersReportReadsItsClaimByTokenAndBySessionInAnyState(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	seedReleasedPod(t, store, "session-1")

	records := podRecords(t, store)
	inTx(t, store, func(tx pgx.Tx) error {
		byToken, found, err := records.ClaimByToken(ctx, tx, string(releasedToken))
		if err != nil || !found || !byToken.Released() {
			t.Fatalf("ClaimByToken = %+v, %t, %v; want the released claim", byToken, found, err)
		}
		if byToken.Issue != rootIssue || byToken.Role != claim.RoleImplementer || byToken.ServingGeneration != 1 {
			t.Fatalf("the claim read by token = %+v, want the implementer of %s at generation 1", byToken, rootIssue)
		}
		bySession, found, err := records.ClaimBySession(ctx, tx, "session-1")
		if err != nil || !found || bySession.Token != releasedToken || !bySession.Released() {
			t.Fatalf("ClaimBySession = %+v, %t, %v; want the same claim", bySession, found, err)
		}
		if _, found, err := records.ClaimBySession(ctx, tx, "another-daemons-session"); err != nil || found {
			t.Fatalf("a session of no claim of this project = %t, %v; want not found", found, err)
		}
		return nil
	})

	for _, source := range []supervise.SuspendSource{supervise.SuspendedByOperator, supervise.SuspendedByDaemon} {
		if _, err := store.Pool().Exec(ctx, `update claims set suspended_by = $2 where token = $1`,
			string(releasedToken), string(source)); err != nil {
			t.Fatalf("take the claim over: %v", err)
		}
		inTx(t, store, func(tx pgx.Tx) error {
			byToken, found, err := records.ClaimByToken(ctx, tx, string(releasedToken))
			if err != nil || !found || byToken.Released() || byToken.SuspendedBy != source {
				t.Fatalf("a claim suspended by %s reads %+v, %t, %v; want it found and not released", source, byToken, found, err)
			}
			bySession, found, err := records.ClaimBySession(ctx, tx, "session-1")
			if err != nil || !found || bySession.Released() {
				t.Fatalf("a claim suspended by %s reads by session %+v, %t, %v; want it found and not released", source, bySession, found, err)
			}
			return nil
		})
	}
}

// One open resume per claim is the whole of the dedupe: the second wake of a claim whose pod is
// already coming back writes nothing and answers false, and the wake of another claim of the same
// pod is taken, since a wake resumes one claim. A claim whose resume has its first turn stamped is
// free to be woken again, which is a pod that came back, ran and was released afresh.
func TestAClaimTakesOneWakeWhileItsPodIsComingBack(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	records := podRecords(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	inTx(t, store, func(tx pgx.Tx) error {
		first, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, releasedToken, pods.WakeChecks, at, 0)
		if err != nil || !first {
			t.Fatalf("the implementer's first wake = %t, %v; want it taken", first, err)
		}
		again, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, releasedToken, pods.WakeRoleMessage, at, 0)
		if err != nil || again {
			t.Fatalf("a second wake of the implementer = %t, %v; want it refused: its pod is coming back", again, err)
		}
		other, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleArchitect, "legion-legion-legion-208-architect", pods.WakeRoleMessage, at, 0)
		if err != nil || !other {
			t.Fatalf("the architect's wake = %t, %v; want it taken: a wake resumes one claim", other, err)
		}
		return nil
	})
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set first_turn_at = $1 where issue = $2 and role = 'implementer'`,
		at, rootIssue); err != nil {
		t.Fatalf("stamp the implementer's first turn: %v", err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		next, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleImplementer, releasedToken, pods.WakeChecks, at, 0)
		if err != nil || !next {
			t.Fatalf("a wake after the first turn = %t, %v; want it taken: that resume is closed", next, err)
		}
		return nil
	})
}

// Two wakes of one claim in transactions of their own — the workflow's, under the apply-fact
// lock, and the outbox's delivery wake, which runs in one of its own — take once. The index
// decides it inside each transaction, where a read could be overtaken between its answer and the
// insert, so the record holds one resume and the caller writes one start row.
func TestTwoWakesOfOneClaimInSeparateTransactionsTakeOnce(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	taken := 0
	for _, kind := range []string{pods.WakeChecks, pods.WakeDelivery} {
		got, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, kind, at, 0)
		if err != nil {
			t.Fatalf("take the %s wake: %v", kind, err)
		}
		if got {
			taken++
		}
	}
	if taken != 1 {
		t.Fatalf("wakes taken = %d, want 1: one open resume per claim", taken)
	}
	var resumes int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes where issue = $1 and role = 'implementer'`,
		rootIssue).Scan(&resumes); err != nil {
		t.Fatalf("count the resumes: %v", err)
	}
	if resumes != 1 {
		t.Fatalf("resumes recorded = %d, want 1", resumes)
	}
}

// R5: the backstop is a stored row with a due time, so it survives the daemon that armed it. A
// store opened again over the same database reads the same row due, and the conditional fire is
// what makes a backstop two passes read wake its pod once.
func TestABackstopSurvivesTheDaemonAndFiresOnce(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	armed := time.Now().UTC().Truncate(time.Millisecond)
	due := armed.Add(time.Hour)
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Claim: releasedToken, Role: claim.RoleImplementer, DueAt: due}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	// A release retried after a failure arms the one backstop, not two.
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Claim: releasedToken, Role: claim.RoleImplementer, DueAt: due.Add(time.Minute)}); err != nil {
		t.Fatalf("arm the backstop again: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", due.Add(-time.Minute)); err != nil || len(open) != 0 {
		t.Fatalf("backstops due before their time = %+v, %v; want none", open, err)
	}

	// The daemon stops and starts again: a new store over the same database, as a restart makes.
	restarted := reopenStore(t, store)
	due2, err := restarted.DuePodBackstops(ctx, "legion", due.Add(time.Minute))
	if err != nil {
		t.Fatalf("read the due backstops after the restart: %v", err)
	}
	if len(due2) != 1 || due2[0].Issue != rootIssue || due2[0].Claim != releasedToken || due2[0].Role != claim.RoleImplementer {
		t.Fatalf("due backstops after the restart = %+v, want the one armed before it", due2)
	}
	if !due2[0].DueAt.UTC().Equal(due) {
		t.Fatalf("the backstop is due at %s, want the time the first arming set, %s", due2[0].DueAt.UTC(), due)
	}

	records := podRecords(t, restarted)
	inTx(t, restarted, func(tx pgx.Tx) error {
		fired, err := records.FirePodBackstop(ctx, tx, due2[0].ID)
		if err != nil || !fired {
			t.Fatalf("the first fire = %t, %v; want it fired", fired, err)
		}
		again, err := records.FirePodBackstop(ctx, tx, due2[0].ID)
		if err != nil || again {
			t.Fatalf("the second fire = %t, %v; want it already fired", again, err)
		}
		return nil
	})
	if open, err := restarted.DuePodBackstops(ctx, "legion", due.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops due after the fire = %+v, %v; want none", open, err)
	}
}

// A path that takes a released claim over forgets the pod's wake marks and its open backstops,
// so no later backstop acts on it.
func TestTakingAReleasedPodOverCancelsItsBackstops(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Claim: releasedToken, Role: claim.RoleImplementer, DueAt: at}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	if err := store.CancelPodBackstopsOf(ctx, "legion", rootIssue, "", ""); err != nil {
		t.Fatalf("cancel the pod's backstops: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the take-over = %+v, %v; want none", open, err)
	}
}

// R7: the daemon releases nothing unless it has read a `listener_ready` report within the window.
// A daemon that has read none, and one whose newest report is older than the window, both answer
// no; one inside the window answers yes.
func TestTheListenerIsLiveOnlyWithAReportInsideTheWindow(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	const window = 15 * time.Minute
	records := podRecords(t, store)

	live, _, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || found {
		t.Fatalf("with no report at all: live = %t, found = %t, %v; want neither", live, found, err)
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-16*time.Minute))
	})
	live, at, found, err := store.ListenerLive(ctx, "legion", now, window)
	if err != nil || live || !found {
		t.Fatalf("with a sixteen-minute-old report: live = %t, found = %t, %v; want found but not live", live, found, err)
	}
	if !at.Equal(now.Add(-16 * time.Minute)) {
		t.Fatalf("the report is stamped %s, want %s", at, now.Add(-16*time.Minute))
	}
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-5*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("with a five-minute-old report: live = %t, %v; want live", live, err)
	}
	// A report that arrives out of order never moves the stamp backwards: the reports are the
	// listener's own clock, and one delivered late must not make a live listener look dead.
	inTx(t, store, func(tx pgx.Tx) error {
		return records.RecordListenerReady(ctx, tx, now.Add(-20*time.Minute))
	})
	live, _, _, err = store.ListenerLive(ctx, "legion", now, window)
	if err != nil || !live {
		t.Fatalf("after an out-of-order older report: live = %t, %v; want it still live", live, err)
	}
}

// TakePodWake is the outbox's wake: it records the resume against the pod's newest release and
// cancels the open backstops, once, and the resume is what makes it once.
func TestTakingAWakeRecordsItAgainstTheNewestReleaseOnce(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	release, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{"implementer"}, At: at.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("record the release: %v", err)
	}
	if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
		Claim: releasedToken, Role: claim.RoleImplementer, DueAt: at}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}
	taken, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, pods.WakeDelivery, at, 0)
	if err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the wake taken", taken, err)
	}
	again, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, pods.WakeDelivery, at, 0)
	if err != nil || again {
		t.Fatalf("a second delivery wake = %t, %v; want it refused: the role's resume is still open", again, err)
	}
	records, err := store.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	if len(records.Resumes) != 1 || records.Resumes[0].Release != release || records.Resumes[0].Kind != pods.WakeDelivery {
		t.Fatalf("resumes = %+v, want the one delivery wake of release %d", records.Resumes, release)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("backstops after the wake = %+v, %v; want none: the pod is coming back", open, err)
	}
	// The resume is the one a pod-ready and a first-turn observation stamp, and it is no longer
	// open once its first turn is in.
	id, pause, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer, releasedToken)
	if err != nil || !found || id != records.Resumes[0].ID || pause != 0 {
		t.Fatalf("OpenPodResume = %d, pause %d, %t, %v; want the resume the wake recorded, %d, which no pause held",
			id, pause, found, err, records.Resumes[0].ID)
	}
	if err := store.MarkPodResumeFirstTurn(ctx, id, at.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the first turn: %v", err)
	}
	if _, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer, releasedToken); err != nil || found {
		t.Fatalf("OpenPodResume after the first turn = %t, %v; want none", found, err)
	}
}

// A wake taken while a release still stands, after that release recorded the role as `released`,
// ends that release, and the take reads the release before it, since this one has no row yet. The
// release's own record points that wake at it: its release and the release it saw become this
// release, and its time and its stuck clock move to this release's time, when the pod starts to
// come back. Nothing else is pointed: not a wake of a role the release never recorded, not a wake
// whose role has taken its first turn, and not a wake taken before the mark.
func TestARecordedReleasePointsTheWakeTakenUnderItsMarkAtItself(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	earlier, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{"implementer"}, At: at.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("record the earlier release: %v", err)
	}
	take := func(role claim.Role, when time.Time) int64 {
		t.Helper()
		token, err := claim.NewToken("legion", rootIssue, role)
		if err != nil {
			t.Fatal(err)
		}
		if taken, err := store.TakePodWake(ctx, "legion", rootIssue, token, role, pods.WakeAgentMessage, when, 0); err != nil || !taken {
			t.Fatalf("take the %s's wake = %t, %v; want it taken", role, taken, err)
		}
		id, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, role, token)
		if err != nil || !found {
			t.Fatalf("the %s's open resume = %t, %v; want the one its wake recorded", role, found, err)
		}
		return id
	}
	// A wake of the reviewer before the mark: the mark's wake_row is its id. The mark is written
	// by hand so that resume stays open, which the mark of a release that suspends a role closes.
	before := take(claim.RoleReviewer, at.Add(-time.Hour))
	markRelease(t, store, 1, 1, 1, 0, claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester)
	if _, err := store.Pool().Exec(ctx, `update issue_releases set wake_row = $2 where project = 'legion' and issue = $1`,
		rootIssue, before); err != nil {
		t.Fatalf("set the mark's newest wake: %v", err)
	}
	taken := at.Add(-10 * time.Minute)
	woken := take(claim.RoleImplementer, taken)
	outside := take(claim.RoleArchitect, taken)
	turned := take(claim.RoleTester, taken)
	if err := store.MarkPodResumeFirstTurn(ctx, turned, taken.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the tester's first turn: %v", err)
	}

	release, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer", "reviewer", "tester"}, At: at})
	if err != nil {
		t.Fatalf("record the release: %v", err)
	}

	for _, want := range []struct {
		what          string
		id            int64
		release, seen int64
		at            time.Time
	}{
		{what: "the wake taken under the mark", id: woken, release: release, seen: release, at: at},
		{what: "the wake of a role the release never recorded", id: outside, release: earlier, seen: earlier, at: taken},
		{what: "the wake whose role took its first turn", id: turned, release: earlier, seen: earlier, at: taken},
		{what: "the wake taken before the mark", id: before, release: earlier, seen: earlier, at: at.Add(-time.Hour)},
	} {
		var release, seen int64
		var resumed, clock time.Time
		if err := store.Pool().QueryRow(ctx, `select release_id, seen_release, at, clock_at from pod_resumes where id = $1`,
			want.id).Scan(&release, &seen, &resumed, &clock); err != nil {
			t.Fatalf("read %s: %v", want.what, err)
		}
		if release != want.release || seen != want.seen || !resumed.Equal(want.at) || !clock.Equal(want.at) {
			t.Errorf("%s has release %d, seen release %d, time %s and stuck clock %s; want %d, %d, and %s for both",
				want.what, release, seen, resumed.UTC().Format(time.RFC3339Nano), clock.UTC().Format(time.RFC3339Nano),
				want.release, want.seen, want.at.Format(time.RFC3339Nano))
		}
	}
}

// A release closes the resumes of the pod it stops, and closing is its own column: the role can
// be woken again afterwards, the records report no first turn for the closed row, and the stranded
// and stuck reports pass it over, since a wake of a pod that is gone is nothing either warns
// about. Writing first_turn_at to close a row, as this branch first did, reports a turn the daemon
// never saw: the release records serve that field and the plugin measures each wake's wait by it.
func TestClosingAResumeWritesNoFirstTurnAndFreesTheRole(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{"implementer"}, At: at.Add(-time.Hour)}); err != nil {
		t.Fatalf("record the release: %v", err)
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, pods.WakeDelivery, at, 0); err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the wake taken", taken, err)
	}

	inTx(t, store, func(tx pgx.Tx) error {
		return CloseOpenResumes(ctx, tx, "legion", rootIssue)
	})

	if _, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, claim.RoleImplementer, releasedToken); err != nil || found {
		t.Fatalf("OpenPodResume after the close = %t, %v; want none", found, err)
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, pods.WakeBackstop, at.Add(time.Hour), 0); err != nil || !taken {
		t.Fatalf("the wake after the close = %t, %v; want it taken: the closed resume holds nothing back", taken, err)
	}
	records, err := store.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	if len(records.Resumes) != 2 || !records.Resumes[0].FirstTurnAt.IsZero() {
		t.Fatalf("resumes = %+v, want the closed one reported with no first turn", records.Resumes)
	}
	stuck, err := store.StuckPodResumes(ctx, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the stuck resumes: %v", err)
	}
	stranded, err := unfinishedResumes(ctx, store, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the unfinished resumes: %v", err)
	}
	for _, read := range []struct {
		what    string
		resumes []PodResume
	}{{"stuck", stuck}, {"stranded", stranded}} {
		for _, r := range read.resumes {
			if r.ID == records.Resumes[0].ID {
				t.Errorf("the %s report lists the closed resume %d, want it passed over", read.what, r.ID)
			}
		}
	}
}

// R5: a wake resumes one role, so it cancels that role's backstop and no other's. A released
// tester waiting on a deploy run has nothing but its own backstop to bring it back, and a pod
// woken for its architect may be held again — by an unanswered question to a person, by
// background work — before it ever reaches the tester's work.
func TestAWakeCancelsTheWokenRolesBackstopAlone(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if err := store.ArmPodBackstop(ctx, PodWakeBackstop{Project: "legion", Issue: rootIssue,
			Claim: claim.Token("legion-legion-legion-208-" + string(role)), Role: role, DueAt: at.Add(time.Hour)}); err != nil {
			t.Fatalf("arm the %s backstop: %v", role, err)
		}
	}
	if taken, err := store.TakePodWake(ctx, "legion", rootIssue, releasedToken, claim.RoleImplementer, pods.WakeRoleMessage, at, 0); err != nil || !taken {
		t.Fatalf("TakePodWake = %t, %v; want the implementer's wake taken", taken, err)
	}

	open, err := store.DuePodBackstops(ctx, "legion", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("read the open backstops: %v", err)
	}
	if len(open) != 1 || open[0].Role != claim.RoleTester {
		t.Fatalf("open backstops after the implementer's wake = %+v, want the tester's alone", open)
	}

	// A path that takes the whole released pod over leaves none of them: nothing of that pod is
	// a wake's any more.
	if err := store.CancelPodBackstopsOf(ctx, "legion", rootIssue, "", ""); err != nil {
		t.Fatalf("cancel the pod's backstops: %v", err)
	}
	if open, err := store.DuePodBackstops(ctx, "legion", at.Add(2*time.Hour)); err != nil || len(open) != 0 {
		t.Fatalf("open backstops after the take-over = %+v, %v; want none", open, err)
	}
}
