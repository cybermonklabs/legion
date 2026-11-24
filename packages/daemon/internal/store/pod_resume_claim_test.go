package store

import (
	"context"
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
// role alone would mark the tester's own claim owed the launch the wake took for key a's agent.
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
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, keyed); err != nil || !found {
		t.Fatalf("key a's open resume = %t, %v; want the one its wake took", found, err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, own); err != nil || found {
		t.Fatalf("the tester's own claim's open resume = %t, %v; want none: the wake brought key a's agent back", found, err)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		closed, err := CloseRoleOpenResumeTx(ctx, tx, "legion", rootIssue, own, at)
		if err == nil && closed {
			t.Errorf("a close for the tester's own claim closed key a's resume")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, keyed); err != nil || !found {
		t.Fatalf("key a's open resume after the other claim's close = %t, %v; want it still open", found, err)
	}
}

// The wake pacing's let-out of a launch moves the stuck clock of that claim's own resume and of no
// other claim's, so another claim of the same role is not hidden from the stuck rule by a launch
// that is not its own. The agent of key a, `…-implementer`, works the tester's step beside the
// tester's own claim; key a's wake took the tester's open resume. A let-out of the tester's own
// claim finds no resume of its own and moves nothing; key a's let-out moves its clock.
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

	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, own, at.Add(5*time.Minute)); err != nil || set {
		t.Errorf("the tester's own claim's let-out = %t, %v; want no resume of its own found", set, err)
	}
	if got := clock(); !got.Equal(at) {
		t.Errorf("key a's clock after the tester's own claim's let-out = %s, want %s: that launch is not key a's", got, at)
	}
	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, keyed, at.Add(5*time.Minute)); err != nil || !set {
		t.Errorf("key a's let-out = %t, %v; want its resume found", set, err)
	}
	if got := clock(); !got.Equal(at.Add(5 * time.Minute)) {
		t.Errorf("key a's clock after its own let-out = %s, want %s", got, at.Add(5*time.Minute))
	}
}

// A claim has one open resume, its own, so two claims of one role each take a wake: the agent of
// key a, `…-implementer`, on the tester's step, and the tester's own claim of an earlier version
// of the plan, both released. While key a's resume is open, a wake of the tester's own claim — a
// message to its session, a pause's owed launch of it — still takes a resume of its own; a second
// wake of key a's agent takes none.
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
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, own); err != nil || !found {
		t.Fatalf("the tester's own claim's open resume = %t, %v; want the one its wake took", found, err)
	}
}

// The agent of a key takes the role of each step it works, so its role can change between its
// wake and its first turn. Its resume is its own whatever role the claim holds now: key a's agent,
// woken as the implementer, re-roled to the tester's step before its first turn, still finds,
// stamps and closes the resume its wake took, by its token alone.
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
	if _, _, found, err := st.OpenPodResume(ctx, "legion", rootIssue, keyed); err != nil || !found {
		t.Fatalf("key a's open resume after its re-role = %t, %v; want the one its wake took", found, err)
	}
	if set, err := st.SetResumeClock(ctx, "legion", rootIssue, keyed, at.Add(time.Minute)); err != nil || !set {
		t.Fatalf("key a's let-out after its re-role = %t, %v; want its resume found", set, err)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		closed, err := CloseRoleOpenResumeTx(ctx, tx, "legion", rootIssue, keyed, at)
		if err == nil && !closed {
			t.Errorf("a close of key a's resume after its re-role closed nothing, want the one its wake took")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
