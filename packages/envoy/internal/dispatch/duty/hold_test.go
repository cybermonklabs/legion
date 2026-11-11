package duty_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// holdName is the name every hold test takes; each test runs on a database of its own.
const holdName = "dispatch-outbox"

// mustTakeHold takes holdName for holder on db and fails the test when it is refused. The hold is
// released when the test ends.
func mustTakeHold(t *testing.T, db duty.Holds, holder uuid.UUID, opts duty.HoldOptions) *duty.Hold {
	t.Helper()
	hold, err := duty.TryHold(context.Background(), db, holdName, holder, opts)
	if err != nil {
		t.Fatalf("take the hold: %v", err)
	}
	if hold == nil {
		t.Fatal("the hold was refused")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	return hold
}

// holdRow reads holdName's row: its holder, token and expiry, on the database's clock.
func holdRow(t *testing.T, db duty.Holds) (holder uuid.UUID, token string, expiresAt time.Time) {
	t.Helper()
	if err := db.QueryRow(context.Background(),
		`select holder, token, expires_at from duty_holds where name = $1`, holdName).Scan(&holder, &token, &expiresAt); err != nil {
		t.Fatalf("read the hold's row: %v", err)
	}
	return holder, token, expiresAt
}

// waitLost waits up to within for hold's Lost to close and fails the test when it does not.
func waitLost(t *testing.T, hold *duty.Hold, within time.Duration) {
	t.Helper()
	select {
	case <-hold.Lost():
	case <-time.After(within):
		t.Fatalf("Lost did not close within %s", within)
	}
}

// Two processes, each with a pool of its own: the second cannot take a hold the first holds live,
// and the first takes its own hold again.
//
// The TTL is a minute because this row needs the first hold live while the second tries, and
// nothing here waits for it to run out: a runner that descheduled the test for longer than a
// short TTL, with the renewals behind it, would read a lapse as a take-over.
func TestHoldRefusesASecondHolderWhileTheFirstHoldsItLive(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: time.Minute, Renew: 500 * time.Millisecond, Margin: time.Second}
	first := uuid.New()
	mustTakeHold(t, database.Pool, first, opts)

	refused, err := duty.TryHold(context.Background(), second.Pool, holdName, uuid.New(), opts)
	if err != nil {
		t.Fatalf("try the live hold from the second process: %v", err)
	}
	if refused != nil {
		t.Fatal("the second process took a hold the first holds live")
	}
	if holder, _, _ := holdRow(t, database.Pool); holder != first {
		t.Fatalf("the row names %s, want the first holder %s", holder, first)
	}

	mustTakeHold(t, second.Pool, first, opts)
}

// waitForLapse waits for holdName's hold to have lapsed on the database's clock - the clock the
// lapse is on, and the one a take-over is judged by (takeHold) - and fails the test when it has
// not within eventWait. A standby sleeps the remainder Expiry answered and tries then; this
// reads that the remainder has truly run out, so a host clock running a shade behind the
// database's cannot make the try that follows an early one.
func waitForLapse(t *testing.T, db duty.Holds) {
	t.Helper()
	for deadline := time.Now().Add(eventWait); ; time.Sleep(20 * time.Millisecond) {
		remaining, held, err := duty.Expiry(context.Background(), db, holdName)
		if err != nil || !held {
			t.Fatalf("read the hold's remainder: held %v, error %v", held, err)
		}
		if remaining <= 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hold still has %s to run %v after the remainder it reported ran out", remaining, eventWait)
		}
	}
}

// A holder that stops renewing, as a killed process does, lets the hold lapse on the database's
// clock, and a standby that arms one timer to the lapse through Expiry takes it on its first try
// after it: the take-over writes now() + TTL, so its own expiry minus the TTL is the moment it
// ran, and that moment is at or after the lapse.
//
// The take-over is read from below, against the lapse the dying holder's row named. The row
// allowed a second after the lapse as well until this round, which measured the poll loop it
// used to make the take with: a runner that descheduled the test between two tries moved the
// take without the hold moving at all (dispatch://INFRA-1075). What the standby owes is a take
// on the first try once the row has lapsed, and that is what the one try below reads - the try
// before the lapse is refused, and the try after it must take the name however late the runner
// lets it run.
func TestHoldTakesOverOnTheFirstTryAfterTheLapse(t *testing.T) {
	database := storetest.Open(t)
	dying := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 2 * time.Second, Renew: 300 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()

	first, err := duty.TryHold(ctx, dying.Pool, holdName, uuid.New(), opts)
	if err != nil || first == nil {
		t.Fatalf("take the hold on the first process: hold %v, error %v", first, err)
	}
	// Closing the first process's pool waits for its renewal in flight and fails every later one.
	dying.Pool.Close()
	_, _, lapse := holdRow(t, database.Pool)

	standby := uuid.New()
	if refused, err := duty.TryHold(ctx, database.Pool, holdName, standby, opts); err != nil || refused != nil {
		t.Fatalf("the standby's try before the lapse: hold %v, error %v; want neither", refused, err)
	}
	remaining, held, err := duty.Expiry(ctx, database.Pool, holdName)
	if err != nil || !held || remaining <= 0 {
		t.Fatalf("Expiry before the lapse = %s, held %v, error %v; want a positive remainder of a held name", remaining, held, err)
	}
	time.Sleep(remaining)
	waitForLapse(t, database.Pool)

	taken, err := duty.TryHold(ctx, database.Pool, holdName, standby, opts)
	if err != nil {
		t.Fatalf("the standby's try after the lapse: %v", err)
	}
	if taken == nil {
		t.Fatal("the standby's first try after the lapse was refused a hold nobody renews")
	}
	t.Cleanup(func() { _ = taken.Release(context.Background()) })

	holder, _, expiresAt := holdRow(t, database.Pool)
	if holder != standby {
		t.Fatalf("the row names %s, want the standby %s", holder, standby)
	}
	if after := expiresAt.Add(-opts.TTL).Sub(lapse); after < 0 {
		t.Fatalf("the standby took the hold %s before the lapse on the database's clock", -after)
	}
	select {
	case <-first.Lost():
	default:
		t.Error("the first holder had not reported its loss when the standby took the hold")
	}
}

// A holder whose renewals cannot land - its row locked by another transaction for longer than a
// renewal waits - reports the loss before the row lapses on the database's clock: the loss
// deadline is Margin ahead of the expiry the last confirmed renewal wrote.
//
// The moment of the loss is the hold's own (Deadline, which stands at the loss once Lost has
// closed), never the moment this goroutine was scheduled to see Lost closed: a runner that
// descheduled the test between the two would otherwise read its own delay as the hold's. What
// the comparison allows is the time between that moment and the read below, measured on this
// process's clock, as TestHoldExpiryMatchesTheDatabaseClock allows the time its two statements
// took: load can only lengthen it, so a loaded runner cannot fail the row, while a loss reported
// after the row's expiry still falls outside it.
func TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()
	holder := uuid.New()
	hold := mustTakeHold(t, database.Pool, holder, opts)

	locker, err := second.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(ctx, `select 1 from duty_holds where name = $1 for update`, holdName); err != nil {
		t.Fatalf("lock the hold's row: %v", err)
	}
	if waiting := storetest.WaitForLockWaiters(t, locker, 1, eventWait); waiting < 1 {
		t.Fatal("no renewal queued behind the locked row")
	}

	waitLost(t, hold, eventWait)
	lost := hold.Deadline()
	var (
		named uuid.UUID
		ahead int64
	)
	if err := locker.QueryRow(ctx, `
		select holder, (extract(epoch from expires_at - clock_timestamp()) * 1000000)::bigint
		from duty_holds where name = $1`, holdName).Scan(&named, &ahead); err != nil {
		t.Fatalf("read the row once Lost closed: %v", err)
	}
	since := time.Since(lost)
	margin := time.Duration(ahead) * time.Microsecond
	if margin+since <= 0 {
		t.Fatalf("the row had lapsed %s before the hold reported its loss on the database's clock, read %s after that loss",
			-(margin + since), since)
	}
	if named != holder {
		t.Fatalf("the row names %s, want the holder %s: the loss must come from the deadline, not a take-over", named, holder)
	}
	if !time.Now().After(hold.Deadline()) {
		t.Errorf("Lost is closed but the deadline %s is still ahead", time.Until(hold.Deadline()))
	}
	t.Logf("the hold reported its loss %s before the row's expiry on the database's clock, read %s after it", margin+since, since)
}

// Release frees the name at once, so a standby takes it on its next try, and wakes every session
// listening on HoldReleasedChannel with the name.
func TestHoldReleaseFreesAtOnceAndNotifiesTheName(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()

	listener, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("open the listening connection: %v", err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(ctx, `listen `+duty.HoldReleasedChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}

	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-hold.Lost():
	default:
		t.Error("Lost is open after Release")
	}
	if _, held, err := duty.Expiry(ctx, database.Pool, holdName); err != nil || held {
		t.Fatalf("Expiry after Release: held %v, error %v; want no row", held, err)
	}

	waiting, cancel := context.WithTimeout(ctx, eventWait)
	defer cancel()
	notice, err := listener.WaitForNotification(waiting)
	if err != nil {
		t.Fatalf("wait for the release notification: %v", err)
	}
	if notice.Channel != duty.HoldReleasedChannel || notice.Payload != holdName {
		t.Fatalf("notification on %q with %q, want %q with %q", notice.Channel, notice.Payload, duty.HoldReleasedChannel, holdName)
	}

	mustTakeHold(t, second.Pool, uuid.New(), opts)
}

// A renewal that affects no row - the row taken over by another holder after a lapse - closes Lost
// at once, long before the loss deadline, and the holder's Release then leaves the new holder's
// row alone.
func TestHoldZeroRowRenewalClosesLost(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 10 * time.Second, Renew: 200 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)

	other := uuid.New()
	if _, err := second.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() + interval '10 seconds' where name = $1`,
		holdName, other); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	// The hold pulls its deadline back to the moment it loses the name (lose), so where the loss
	// came from is read from that record rather than from the clock this goroutine reads once it
	// is scheduled to see Lost closed: a runner that descheduled the test past the deadline would
	// otherwise fail a row whose loss landed seconds before it.
	deadline := hold.Deadline()
	waitLost(t, hold, eventWait)
	if !hold.Deadline().Before(deadline) {
		t.Fatal("Lost closed at the deadline rather than on the zero-row renewal")
	}

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release after the loss: %v", err)
	}
	if holder, _, _ := holdRow(t, database.Pool); holder != other {
		t.Fatalf("the row names %s after the lost holder's Release, want the new holder %s", holder, other)
	}
}

// The token the take records reads back, SetToken replaces it, and SetToken on a row another
// holder has taken changes nothing and reports the loss.
func TestHoldTokenReadsBack(t *testing.T) {
	database := storetest.Open(t)
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.first"}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)

	if _, token, _ := holdRow(t, database.Pool); token != "deliver.first" {
		t.Fatalf("token = %q, want the take's %q", token, "deliver.first")
	}
	if err := hold.SetToken(ctx, "deliver.second"); err != nil {
		t.Fatalf("set the token: %v", err)
	}
	if _, token, _ := holdRow(t, database.Pool); token != "deliver.second" {
		t.Fatalf("token = %q, want %q", token, "deliver.second")
	}

	if _, err := database.Pool.Exec(ctx, `update duty_holds set holder = $2 where name = $1`, holdName, uuid.New()); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	if err := hold.SetToken(ctx, "deliver.third"); !errors.Is(err, duty.ErrHoldLost) {
		t.Fatalf("SetToken on another holder's row = %v, want ErrHoldLost", err)
	}
	if _, token, _ := holdRow(t, database.Pool); token != "deliver.second" {
		t.Fatalf("token = %q after a refused SetToken, want %q", token, "deliver.second")
	}
	select {
	case <-hold.Lost():
	default:
		t.Error("Lost is open after SetToken found another holder's row")
	}
}

// Expiry reads expires_at - now() on the database's clock: held for a live row, held with a
// negative remainder for a lapsed one, and not held with no row.
//
// Both halves used to allow a fixed 100 ms between the statement Expiry runs and the statement
// the test compares it with, and a loaded runner spends more than that between two round trips:
// the run that red INFRA-1039 read -2.176685 s, which is 176 ms of inter-statement time and not a
// clock that disagrees. So each comparison is bracketed by the database's own clock instead, and
// what it allows is the time those statements took, read from that same clock. Load can only
// lengthen it, so a slow runner cannot fail the test, while a reading wrong by more than the time
// those two statements took still falls outside the bracket: the now() plus or minus one second
// mutants of this issue's plan (INFRA-985, task T7) are caught.
func TestHoldExpiryMatchesTheDatabaseClock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}

	if _, held, err := duty.Expiry(ctx, database.Pool, holdName); err != nil || held {
		t.Fatalf("Expiry with no row: held %v, error %v; want not held", held, err)
	}

	mustTakeHold(t, database.Pool, uuid.New(), opts)
	before := databaseNow(t, database)
	remaining, held, err := duty.Expiry(ctx, database.Pool, holdName)
	if err != nil || !held {
		t.Fatalf("Expiry of a live hold: held %v, error %v", held, err)
	}
	var expiresAt, after time.Time
	if err := database.Pool.QueryRow(ctx, `select expires_at, now() from duty_holds where name = $1`, holdName).Scan(&expiresAt, &after); err != nil {
		t.Fatalf("read the expiry directly: %v", err)
	}
	// Expiry's own now() was taken somewhere between these two, so its answer is the direct one
	// plus whatever of the window passed first: never less, and never more than the whole window.
	direct := expiresAt.Sub(after)
	elapsed := after.Sub(before)
	if gap := remaining - direct; gap < 0 || gap > elapsed {
		t.Fatalf("Expiry = %s and expires_at - now() = %s, %s apart, while the database's clock moved %s between the two statements: want a gap in [0, %s]",
			remaining, direct, gap, elapsed, elapsed)
	}

	lapsedExpiry(t, database)
}

// databaseNow is the database's own clock now, the one every bound in this test is read on.
func databaseNow(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(context.Background(), `select now()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// lapsedExpiry rewrites holdName's row to a holder that lapsed two seconds ago, as a killed
// holder's row reads, and checks Expiry reports it held with the negative remainder.
func lapsedExpiry(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	// The write's own now() is what its expires_at is two seconds behind, so it is the only clock
	// the remainder below can be measured from.
	var wrote time.Time
	if err := database.Pool.QueryRow(ctx, `update duty_holds set holder = $2, expires_at = now() - interval '2 seconds' where name = $1 returning now()`,
		holdName, uuid.New()).Scan(&wrote); err != nil {
		t.Fatalf("rewrite the row as lapsed: %v", err)
	}
	remaining, held, err := duty.Expiry(ctx, database.Pool, holdName)
	if err != nil || !held {
		t.Fatalf("Expiry of a lapsed row: held %v, error %v; want held", held, err)
	}
	// Expiry ran after the write and before this read, so its answer is -2 s less whatever of the
	// window had passed: exactly -2 s if it ran at the write's instant, and never below -2 s less
	// the whole window.
	elapsed := databaseNow(t, database).Sub(wrote)
	if remaining > -2*time.Second || remaining < -2*time.Second-elapsed {
		t.Fatalf("Expiry of a row lapsed 2 s ago = %s, while the database's clock moved %s between the write and the read after it: want a remainder in [%s, -2s]",
			remaining, elapsed, -2*time.Second-elapsed)
	}
}

// Options under which no renewal could be confirmed before the loss deadline are refused before
// any statement is sent.
func TestHoldRefusesOptionsNoRenewalCouldBeConfirmedUnder(t *testing.T) {
	for _, opts := range []duty.HoldOptions{
		{TTL: 6 * time.Second, Renew: 4 * time.Second, Margin: 2 * time.Second},
		{TTL: 2 * time.Second, Renew: time.Second, Margin: 2 * time.Second},
		{TTL: 6 * time.Second, Renew: 0, Margin: 2 * time.Second},
		{TTL: 6 * time.Second, Renew: time.Second, Margin: -time.Second},
	} {
		if hold, err := duty.TryHold(context.Background(), nil, holdName, uuid.New(), opts); err == nil || hold != nil {
			t.Errorf("TryHold with %+v = hold %v, error %v; want a refusal", opts, hold, err)
		}
	}
}
