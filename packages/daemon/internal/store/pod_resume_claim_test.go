package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// A role's open resume is the resume of the claim its wake brought back (pod_resumes.claim), so
// a read of the resume for another claim of the same role finds none, and a close for that other
// claim leaves it open. Two claims can hold one role: the agent of key a, `…-implementer`, works
// the tester's step, beside the tester's own claim of an earlier version of the plan. A paused
// cancel of a release reads each released claim's resume (stopReleasedForPause), and a read by
// role alone would mark the tester's own claim owed the launch the wake took for key a's agent. A
// resume the release before wrote names no claim, and is still each claim of its role's.
func TestAnOpenResumeIsTheClaimItsWakeBroughtBack(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	keyed, err := claim.NewToken("legion", rootIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	own, err := claim.NewToken("legion", rootIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, keyed, claim.RoleTester, pods.WakeDelivery, at, 0); err != nil || !taken {
		t.Fatalf("take key a's wake on the tester's step = %t, %v; want it taken", taken, err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, keyed); err != nil || !found {
		t.Fatalf("key a's open resume = %t, %v; want the one its wake took", found, err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, own); err != nil || found {
		t.Fatalf("the tester's own claim's open resume = %t, %v; want none: the wake brought key a's agent back", found, err)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		closed, err := CloseRoleOpenResumeTx(ctx, tx, "legion", rootIssue, claim.RoleTester, own, at)
		if err == nil && closed {
			t.Errorf("a close for the tester's own claim closed key a's resume")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, keyed); err != nil || !found {
		t.Fatalf("key a's open resume after the other claim's close = %t, %v; want it still open", found, err)
	}

	if _, err := st.Pool().Exec(ctx, `update pod_resumes set claim = '' where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, own); err != nil || !found {
		t.Fatalf("a resume the release before wrote, read for the tester's own claim = %t, %v; want it found: it names no claim", found, err)
	}
}

// The wake pacing's let-out of a launch moves the stuck clock of that claim's own resume and of no
// other claim's, so another claim of the same role is not hidden from the stuck rule by a launch
// that is not its own. The agent of key a, `…-implementer`, works the tester's step beside the
// tester's own claim; key a's wake took the tester's open resume. A let-out of the tester's own
// claim finds no resume of its own and moves nothing; key a's let-out moves its clock. A resume
// the release before wrote names no claim, so any claim of its role's let-out moves it.
func TestALetOutMovesTheStuckClockOfItsOwnClaimsResumeAlone(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	keyed, err := claim.NewToken("legion", rootIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	own, err := claim.NewToken("legion", rootIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, keyed, claim.RoleTester, pods.WakeDelivery, at, 0); err != nil || !taken {
		t.Fatalf("take key a's wake on the tester's step = %t, %v; want it taken", taken, err)
	}
	clock := func() time.Time {
		t.Helper()
		var clockAt time.Time
		if err := st.Pool().QueryRow(ctx, `select clock_at from pod_resumes where issue = $1 and role = 'tester'
			and first_turn_at is null and closed_at is null`, rootIssue).Scan(&clockAt); err != nil {
			t.Fatalf("read the tester's open resume: %v", err)
		}
		return clockAt.UTC()
	}

	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, own, at.Add(5*time.Minute)); err != nil || set {
		t.Errorf("the tester's own claim's let-out = %t, %v; want no resume of its own found", set, err)
	}
	if got := clock(); !got.Equal(at) {
		t.Errorf("key a's clock after the tester's own claim's let-out = %s, want %s: that launch is not key a's", got, at)
	}
	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, keyed, at.Add(5*time.Minute)); err != nil || !set {
		t.Errorf("key a's let-out = %t, %v; want its resume found", set, err)
	}
	if got := clock(); !got.Equal(at.Add(5 * time.Minute)) {
		t.Errorf("key a's clock after its own let-out = %s, want %s", got, at.Add(5*time.Minute))
	}

	if _, err := st.Pool().Exec(ctx, `update pod_resumes set claim = '' where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, own, at.Add(10*time.Minute)); err != nil || !set {
		t.Errorf("the tester's own claim's let-out over a resume that names no claim = %t, %v; want it found", set, err)
	}
	if got := clock(); !got.Equal(at.Add(10 * time.Minute)) {
		t.Errorf("the clock of a resume that names no claim = %s, want %s", got, at.Add(10*time.Minute))
	}
}

// A wait in the wake pacing's queue stops the stuck clock of that claim's own resume and of no
// other claim's (SetResumeQueued), so another claim of the same role is not hidden from the stuck
// warning by a wait that is not its own: the end of a wait starts the clock of its own claim's
// resume alone (SetResumeClock), so a clock a wait stopped for another claim would stay stopped
// until the next boot. The agent of key a, `…-implementer`, works the tester's step beside the
// tester's own claim, and each claim's wake took a resume of its own. A resume the release before
// wrote names no claim, so a wait of any claim of its role stops its clock and the end of any
// claim's wait starts it.
func TestAWaitInThePacingStopsTheStuckClockOfItsOwnClaimsResumeAlone(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	keyed, err := claim.NewToken("legion", rootIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	own, err := claim.NewToken("legion", rootIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, token := range []claim.Token{keyed, own} {
		if taken, err := st.TakePodWake(ctx, "legion", rootIssue, token, claim.RoleTester, pods.WakeAgentMessage, at, 0); err != nil || !taken {
			t.Fatalf("take the wake of %s on the tester's step = %t, %v; want it taken", token, taken, err)
		}
	}
	resume := func(token claim.Token) int64 {
		t.Helper()
		id, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, token)
		if err != nil || !found {
			t.Fatalf("the open resume of %s = %t, %v; want the one its wake took", token, found, err)
		}
		return id
	}
	keyedResume, ownResume := resume(keyed), resume(own)
	queued := func(id int64) bool {
		t.Helper()
		var stopped bool
		if err := st.Pool().QueryRow(ctx, `select queued from pod_resumes where id = $1`, id).Scan(&stopped); err != nil {
			t.Fatalf("read whether the clock of resume %d is stopped: %v", id, err)
		}
		return stopped
	}
	// stuck is the resumes the stuck warning reads an hour after the wakes, past every clock here.
	stuck := func() []int64 {
		t.Helper()
		resumes, err := st.StuckPodResumes(ctx, "legion", at.Add(time.Hour))
		if err != nil {
			t.Fatalf("the stuck read: %v", err)
		}
		var ids []int64
		for _, r := range resumes {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got, want := stuck(), []int64{keyedResume, ownResume}; !slices.Equal(got, want) {
		t.Fatalf("the stuck read before any wait = %v, want both claims' resumes %v", got, want)
	}

	if err := st.SetResumeQueued(ctx, "legion", rootIssue, claim.RoleTester, keyed); err != nil {
		t.Fatalf("stop key a's clock for its wait in the queue: %v", err)
	}
	if !queued(keyedResume) {
		t.Errorf("key a's resume after its wait began reads its clock running, want it stopped")
	}
	if queued(ownResume) {
		t.Errorf("the tester's own claim's resume reads its clock stopped by key a's wait, want it running: that wait is not its own")
	}
	if got, want := stuck(), []int64{ownResume}; !slices.Equal(got, want) {
		t.Errorf("the stuck read while key a waits = %v, want the tester's own claim's resume alone %v", got, want)
	}
	if _, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, keyed, at.Add(5*time.Minute)); err != nil {
		t.Fatalf("start key a's clock at its let-out: %v", err)
	}
	if got, want := stuck(), []int64{keyedResume, ownResume}; !slices.Equal(got, want) {
		t.Errorf("the stuck read after key a's let-out = %v, want both claims' resumes %v", got, want)
	}

	if _, err := st.Pool().Exec(ctx, `update pod_resumes set closed_at = now() where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	var legacy int64
	if err := st.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, clock_at)
		values (0, 'legion', $1, 'tester', $2, $3, $3) returning id`, rootIssue, pods.WakeChecks, at).Scan(&legacy); err != nil {
		t.Fatalf("seed a resume the release before wrote: %v", err)
	}
	if err := st.SetResumeQueued(ctx, "legion", rootIssue, claim.RoleTester, own); err != nil {
		t.Fatalf("stop the tester's own claim's clock for its wait in the queue: %v", err)
	}
	if !queued(legacy) || len(stuck()) != 0 {
		t.Errorf("a resume that names no claim after a wait of the tester's own claim reads stopped = %t, stuck %v; want it stopped and out of the read",
			queued(legacy), stuck())
	}
	if _, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, keyed, at.Add(5*time.Minute)); err != nil {
		t.Fatalf("start the clock at key a's let-out: %v", err)
	}
	if got, want := stuck(), []int64{legacy}; queued(legacy) || !slices.Equal(got, want) {
		t.Errorf("a resume that names no claim after key a's let-out reads stuck %v, want %v with its clock running", got, want)
	}
}

// A claim has one open resume, its own, so two claims of one role each take a wake: the agent of
// key a, `…-implementer`, on the tester's step, and the tester's own claim of an earlier version
// of the plan, both released. While key a's resume is open, a wake of the tester's own claim — a
// message to its session, a pause's owed launch of it — still takes a resume of its own; a second
// wake of key a's agent takes none. A resume the release before wrote names no claim and is each
// claim of its role's, so while one is open no claim of its role takes a wake, as before.
func TestTwoClaimsOfOneRoleEachTakeAWakeOfTheirOwn(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	keyed, err := claim.NewToken("legion", rootIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	own, err := claim.NewToken("legion", rootIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	take := func(token claim.Token) bool {
		t.Helper()
		taken, err := st.TakePodWake(ctx, "legion", rootIssue, token, claim.RoleTester, pods.WakeAgentMessage, at, 0)
		if err != nil {
			t.Fatalf("take the wake of %s: %v", token, err)
		}
		return taken
	}
	if !take(keyed) {
		t.Fatalf("the wake of key a's agent on the tester's step was refused, want it taken")
	}
	if !take(own) {
		t.Fatalf("the wake of the tester's own claim was refused while key a's resume is open, want a resume of its own")
	}
	if take(keyed) {
		t.Fatalf("a second wake of key a's agent was taken, want it refused: its own resume is open")
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, own); err != nil || !found {
		t.Fatalf("the tester's own claim's open resume = %t, %v; want the one its wake took", found, err)
	}

	if _, err := st.Pool().Exec(ctx, `update pod_resumes set closed_at = now() where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', $1, 'tester', $2, now())`, rootIssue, pods.WakeChecks); err != nil {
		t.Fatalf("seed a resume the release before wrote: %v", err)
	}
	if take(keyed) || take(own) {
		t.Fatalf("a wake of a claim of the tester was taken while a resume naming no claim of that role is open, want it refused")
	}
}

// The agent of a key takes the role of each step it works, so its role can change between its
// wake and its first turn. Its resume is its own whatever role the claim holds now: key a's agent,
// woken as the implementer, re-roled to the tester's step before its first turn, still finds,
// stamps and closes the resume its wake took.
func TestAReRoledClaimStillReachesTheResumeItsWakeTook(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	keyed, err := claim.NewToken("legion", rootIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, keyed, claim.RoleImplementer, pods.WakeAgentMessage, at, 0); err != nil || !taken {
		t.Fatalf("take key a's wake as the implementer = %t, %v; want it taken", taken, err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, claim.RoleTester, keyed); err != nil || !found {
		t.Fatalf("key a's open resume, read as the tester = %t, %v; want the one its wake took", found, err)
	}
	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, claim.RoleTester, keyed, at.Add(time.Minute)); err != nil || !set {
		t.Fatalf("key a's let-out as the tester = %t, %v; want its resume found", set, err)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		closed, err := CloseRoleOpenResumeTx(ctx, tx, "legion", rootIssue, claim.RoleTester, keyed, at)
		if err == nil && !closed {
			t.Errorf("a close of key a's resume as the tester closed nothing, want the one its wake took")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
