package duty_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
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

// waitLost waits for the hold to report its loss.
func waitLost(t *testing.T, hold *duty.Hold) {
	t.Helper()
	testwait.Receive(t, "the hold's Lost to close", hold.Lost())
}

// Two processes, each with a pool of its own: the second cannot take a hold the first holds live,
// and the first takes its own hold again. The TTL is testwait.PastTheRun: the first hold must be
// live while the second tries, and under a TTL of seconds a loaded machine that starved its
// renewals would let it lapse, and the second would take it.
func TestHoldRefusesASecondHolderWhileTheFirstHoldsItLive(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second}
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

// A holder that stops renewing, as a killed process does, lets the hold lapse on the database's
// clock, and a standby that arms one timer to the lapse through Expiry takes it within a second of
// it, measured on that clock: the take-over writes now() + TTL, so its own expiry minus the TTL is
// the moment it ran.
func TestHoldTakesOverWithinASecondAfterTheLapse(t *testing.T) {
	// Not parallel: it holds the standby's take-over to within a second of the lapse on the
	// database's clock, and its try before the lapse to inside the 2 s TTL; the parallel tests'
	// load could push either past its bound.
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
	var taken *duty.Hold
	for give := time.Now().Add(3 * time.Second); taken == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(give) {
			t.Fatal("the standby did not take the hold within 3 s of its expiry")
		}
		if taken, err = duty.TryHold(ctx, database.Pool, holdName, standby, opts); err != nil {
			t.Fatalf("the standby's try: %v", err)
		}
	}
	t.Cleanup(func() { _ = taken.Release(context.Background()) })

	holder, _, expiresAt := holdRow(t, database.Pool)
	if holder != standby {
		t.Fatalf("the row names %s, want the standby %s", holder, standby)
	}
	after := expiresAt.Add(-opts.TTL).Sub(lapse)
	if after < 0 || after > time.Second {
		t.Fatalf("the standby took the hold %s after the lapse on the database's clock, want within [0, 1s]", after)
	}
	select {
	case <-first.Lost():
	default:
		t.Error("the first holder had not reported its loss when the standby took the hold")
	}
}

// The loss deadline is at least Margin before the row expires on the database's clock.
// With a renewal blocked on the row, the test moves the hold's clock to that deadline.
// Lost stays open until the deadline and closes while the row is still live.
func TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	ctx := testwait.Context(t)
	holder := uuid.New()
	var sent time.Time
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&sent); err != nil {
		t.Fatalf("read the database's clock before taking the hold: %v", err)
	}
	fake := clocktest.New(sent)
	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second, Clock: fake}
	hold := mustTakeHold(t, database.Pool, holder, opts)
	deadline := hold.Deadline()
	if want := sent.Add(opts.TTL - opts.Margin); !deadline.Equal(want) {
		t.Errorf("Deadline() after the take = %s, want send time + TTL - Margin = %s", deadline, want)
	}
	_, _, expiresAt := holdRow(t, database.Pool)
	if latest := expiresAt.Add(-opts.Margin); deadline.After(latest) {
		t.Errorf("Deadline() after the take = %s, want at or before database expiry - Margin = %s", deadline, latest)
	}
	armed := fake.AwaitArmed(t, "the first renewal and the loss deadline", 2)
	if !armed[0].Equal(sent.Add(opts.Renew)) || !armed[len(armed)-1].Equal(deadline) {
		t.Fatalf("timers armed at %v, want first renewal at %s and latest timer at loss deadline %s",
			armed, sent.Add(opts.Renew), deadline)
	}

	locker, err := second.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(ctx, `select 1 from duty_holds where name = $1 for update`, holdName); err != nil {
		t.Fatalf("lock the hold's row: %v", err)
	}
	fake.Advance(opts.Renew)
	storetest.WaitForStatementBehind(t, database, locker, duty.RenewHold)
	select {
	case <-hold.Lost():
		t.Fatal("Lost closed before the loss deadline while the renewal was blocked")
	default:
	}
	if got := hold.Deadline(); !got.Equal(deadline) {
		t.Fatalf("Deadline() while the renewal waits = %s, want unchanged at %s", got, deadline)
	}

	fake.Set(deadline)
	waitLost(t, hold)
	var (
		lapsed bool
		named  uuid.UUID
		ahead  int64
	)
	if err := locker.QueryRow(ctx, `
		select clock_timestamp() >= expires_at, holder, (extract(epoch from expires_at - clock_timestamp()) * 1000000)::bigint
		from duty_holds where name = $1`, holdName).Scan(&lapsed, &named, &ahead); err != nil {
		t.Fatalf("read the row once Lost closed: %v", err)
	}
	margin := time.Duration(ahead) * time.Microsecond
	if lapsed {
		t.Fatalf("Lost closed %s after the row lapsed on the database's clock", -margin)
	}
	if named != holder {
		t.Fatalf("the row names %s, want the holder %s: the loss must come from the deadline, not a take-over", named, holder)
	}
	if got := hold.Deadline(); got.After(fake.Now()) {
		t.Errorf("Lost is closed but the deadline %s is after the hold's clock at %s", got, fake.Now())
	}
	t.Logf("Lost closed %s before the row's expiry on the database's clock", margin)
}

// Release frees the name at once, so a standby takes it on its next try, and wakes every session
// listening on HoldReleasedChannel with the name. The TTL is testwait.PastTheRun, so the hold is
// live until Release however long a loaded machine starves its renewals, and the second process's
// take at the end takes only a row Release deleted.
func TestHoldReleaseFreesAtOnceAndNotifiesTheName(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second}
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

	notice, err := listener.WaitForNotification(testwait.Context(t))
	if err != nil {
		t.Fatalf("wait for the release notification: %v", err)
	}
	if notice.Channel != duty.HoldReleasedChannel || notice.Payload != holdName {
		t.Fatalf("notification on %q with %q, want %q with %q", notice.Channel, notice.Payload, duty.HoldReleasedChannel, holdName)
	}

	mustTakeHold(t, second.Pool, uuid.New(), opts)
}

// Lapse ends the hold as Release does - Lost closes, a standby takes the name at once and is
// notified - but keeps the row's token, so the next holder reads what this one recorded. The TTL
// is testwait.PastTheRun, so the row is live until Lapse ends it, as in
// TestHoldReleaseFreesAtOnceAndNotifiesTheName.
func TestHoldLapseFreesAtOnceAndKeepsTheToken(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.pending"}
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
	if err := hold.Lapse(ctx); err != nil {
		t.Fatalf("lapse: %v", err)
	}
	select {
	case <-hold.Lost():
	default:
		t.Error("Lost is open after Lapse")
	}
	if token, held, err := duty.HoldToken(ctx, database.Pool, holdName); err != nil || held || token != "deliver.pending" {
		t.Fatalf("HoldToken after Lapse = %q, held %v, error %v; want %q, not held", token, held, err, "deliver.pending")
	}

	notice, err := listener.WaitForNotification(testwait.Context(t))
	if err != nil {
		t.Fatalf("wait for the lapse notification: %v", err)
	}
	if notice.Channel != duty.HoldReleasedChannel || notice.Payload != holdName {
		t.Fatalf("notification on %q with %q, want %q with %q", notice.Channel, notice.Payload, duty.HoldReleasedChannel, holdName)
	}

	mustTakeHold(t, second.Pool, uuid.New(), opts)
}

// The test advances the hold's clock to the first renewal after another holder takes the row.
// The renewal affects no row, logs on the holder's logger, and closes Lost before the old deadline.
// Deadline moves back to the loss time, and Release leaves the new holder's row alone.
func TestHoldZeroRowRenewalClosesLost(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	var logs bytes.Buffer
	fake := clocktest.New(tickDue)
	opts := duty.HoldOptions{TTL: 10 * time.Second, Renew: 200 * time.Millisecond, Margin: time.Second,
		Clock: fake, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)
	deadline := hold.Deadline()
	fake.AwaitArmed(t, "the first renewal and the loss deadline", 2)

	other := uuid.New()
	if _, err := second.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() + interval '10 seconds' where name = $1`,
		holdName, other); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	fake.Advance(opts.Renew)
	waitLost(t, hold)
	if !fake.Now().Before(deadline) {
		t.Fatal("Lost closed at the deadline rather than on the zero-row renewal")
	}
	if got := hold.Deadline(); !got.Equal(fake.Now()) {
		t.Errorf("Deadline() after the zero-row renewal = %s, want the loss time %s", got, fake.Now())
	}
	// The renewal writes its line before it closes Lost, and writes nothing after, so the buffer
	// is read once Lost has closed.
	if line := `msg="hold lost: its row names another holder or none" hold=` + holdName; !strings.Contains(logs.String(), line) {
		t.Fatalf("the holder's logger holds %q, want the renewal's line %q", logs.String(), line)
	}

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release after the loss: %v", err)
	}
	if holder, _, _ := holdRow(t, database.Pool); holder != other {
		t.Fatalf("the row names %s after the lost holder's Release, want the new holder %s", holder, other)
	}
}

// unansweredRenewal records when each renewal reaches the wrapper on the hold's clock.
// The second renewal stays unanswered until its context ends. All other statements reach the database.
type unansweredRenewal struct {
	duty.Holds
	clock *clocktest.Fake
	left  chan struct{}

	mu   sync.Mutex
	sent []time.Time
}

func (u *unansweredRenewal) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql == duty.RenewHold {
		u.mu.Lock()
		u.sent = append(u.sent, u.clock.Now())
		second := len(u.sent) == 2
		u.mu.Unlock()
		if second {
			close(u.left)
			<-ctx.Done()
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	return u.Holds.Exec(ctx, sql, args...)
}

// requireRenewals checks the send time and count of the renewals so far.
func (u *unansweredRenewal) requireRenewals(t *testing.T, want ...time.Time) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if !slices.EqualFunc(u.sent, want, time.Time.Equal) {
		t.Fatalf("renewals sent at %v on the hold's clock, want %v", u.sent, want)
	}
}

// The fake clock brings the first renewal at 5 s, which moves the loss deadline to 18 s.
// The second renewal at 10 s gets no answer and times out at 15 s.
// The third renewal goes out at 15 s and moves the deadline to 28 s without closing Lost.
// Waiting another interval would send it at 20 s, after the hold was lost.
func TestAHoldRenewsAgainAtOnceAfterARenewalDrawsNoAnswer(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	fake := clocktest.New(tickDue)
	db := &unansweredRenewal{Holds: database.Pool, clock: fake, left: make(chan struct{})}
	opts := duty.HoldOptions{TTL: 15 * time.Second, Renew: 5 * time.Second, Margin: 2 * time.Second, Clock: fake}
	hold := mustTakeHold(t, db, uuid.New(), opts)

	first := tickDue.Add(opts.Renew)
	second := first.Add(opts.Renew)
	third := second.Add(opts.Renew)
	awaitDeadline := func(what string, want time.Time) {
		t.Helper()
		fake.AwaitArmedAt(t, what, want)
		if got := hold.Deadline(); !got.Equal(want) {
			t.Fatalf("Deadline() = %s, want %s", got, want)
		}
	}

	fake.AwaitArmedAt(t, "the first renewal", first)
	fake.Set(first)
	awaitDeadline("the first renewal's loss deadline at 18 s", first.Add(opts.TTL-opts.Margin))

	fake.AwaitArmedAt(t, "the second renewal", second)
	fake.Set(second)
	testwait.Receive(t, "the second renewal to reach the wrapper and draw no answer", db.left)
	fake.AwaitArmedAt(t, "the unanswered renewal's bound at 15 s", third)
	fake.Set(third)

	awaitDeadline("the third renewal's loss deadline at 28 s", third.Add(opts.TTL-opts.Margin))
	db.requireRenewals(t, first, second, third)
	select {
	case <-hold.Lost():
		t.Fatal("the hold reported itself lost despite the renewal at 15 s")
	default:
	}
}

// The token the take records reads back, SetToken replaces it, and SetToken on a row another holder
// has taken changes nothing and reports the loss. The TTL is testwait.PastTheRun, so the hold is
// live for the first SetToken however long a loaded machine starves its renewals.
func TestHoldTokenReadsBack(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.first"}
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
//
// The hold runs on a clocktest.Fake that nothing moves, so no renewal runs and expires_at stays
// where the take wrote it between Expiry and the direct read, however far apart load puts them.
func TestHoldExpiryMatchesTheDatabaseClock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Clock: clocktest.New(tickDue)}

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

// HoldToken reads the token a hold's row records and whether a holder holds it live: no row is ""
// and not held, a live hold is its token and held, and a lapsed row keeps its token, not held, so a
// standby reads what the lapsed holder recorded before TryHold records its own. The live hold's TTL
// is testwait.PastTheRun, so it still reads as live however long a loaded machine starves its
// renewals.
func TestHoldTokenReadsTheRowsTokenAndWhetherItIsLive(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	if token, held, err := duty.HoldToken(ctx, database.Pool, holdName); err != nil || held || token != "" {
		t.Fatalf("HoldToken with no row = %q, held %v, error %v; want \"\", not held", token, held, err)
	}

	opts := duty.HoldOptions{TTL: testwait.PastTheRun, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.live"}
	mustTakeHold(t, database.Pool, uuid.New(), opts)
	if token, held, err := duty.HoldToken(ctx, database.Pool, holdName); err != nil || !held || token != "deliver.live" {
		t.Fatalf("HoldToken of a live hold = %q, held %v, error %v; want %q, held", token, held, err, "deliver.live")
	}

	if _, err := database.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() - interval '1 second' where name = $1`,
		holdName, uuid.New()); err != nil {
		t.Fatalf("rewrite the row as lapsed: %v", err)
	}
	if token, held, err := duty.HoldToken(ctx, database.Pool, holdName); err != nil || held || token != "deliver.live" {
		t.Fatalf("HoldToken of a lapsed row = %q, held %v, error %v; want %q, not held", token, held, err, "deliver.live")
	}
}

// Options under which no renewal could be confirmed before the loss deadline are refused before
// any statement is sent.
func TestHoldRefusesOptionsNoRenewalCouldBeConfirmedUnder(t *testing.T) {
	t.Parallel()
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
