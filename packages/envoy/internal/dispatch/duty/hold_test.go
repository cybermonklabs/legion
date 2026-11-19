package duty_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
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

// waitLost waits through the shared wait for hold's Lost to close.
func waitLost(t *testing.T, hold *duty.Hold) {
	t.Helper()
	testwait.Receive(t, "the hold's Lost to close", hold.Lost())
}

// Two processes, each with a pool of its own: the second cannot take a hold the first holds live,
// and the first takes its own hold again.
func TestHoldRefusesASecondHolderWhileTheFirstHoldsItLive(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
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
// clock, and a standby takes it at its first try once it has: once Expiry reads no time left on
// the database's clock, one TryHold takes the name, so the take statement holds no grace past the
// lapse. The take-over writes now() + TTL, so its own expiry minus the TTL is the moment it ran,
// never before the lapse. How soon after the lapse a standby that arms a timer to it comes is the
// service level TestHoldTakesOverWithinASecondAfterTheLapse reads, in benchtime's timing lane.
func TestHoldTakesOverOnceItLapses(t *testing.T) {
	lapsing := newLapsingHold(t)
	ctx := context.Background()
	testwait.Until(t, "the hold to lapse on the database's clock (Expiry reading no time left)", func() (time.Duration, bool) {
		remaining, held, err := duty.Expiry(ctx, lapsing.database.Pool, holdName)
		if err != nil || !held {
			t.Fatalf("Expiry of the lapsing hold: held %v, error %v; want the name still held", held, err)
		}
		return remaining, remaining <= 0
	})
	taken, err := duty.TryHold(ctx, lapsing.database.Pool, holdName, lapsing.standby, lapsing.opts)
	if err != nil || taken == nil {
		t.Fatalf("the standby's first try once Expiry read the hold lapsed: hold %v, error %v; want the hold taken", taken, err)
	}
	if after := lapsing.takenBy(t, taken); after < 0 {
		t.Fatalf("the standby took the hold %s before the lapse on the database's clock", -after)
	}
}

// A standby that arms one timer to the lapse through Expiry takes the hold within a second of it,
// measured on the database's clock. A reading of time on a shared runner is its load as much as
// the product's work, so the row runs only in benchtime's timing lane (benchtime.Timing).
func TestHoldTakesOverWithinASecondAfterTheLapse(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	lapsing := newLapsingHold(t)
	ctx := context.Background()
	taken := testwait.Until(t, "the standby to take the hold once it lapsed", func() (*duty.Hold, bool) {
		hold, err := duty.TryHold(ctx, lapsing.database.Pool, holdName, lapsing.standby, lapsing.opts)
		if err != nil {
			t.Fatalf("the standby's try: %v", err)
		}
		return hold, hold != nil
	})
	if after := lapsing.takenBy(t, taken); after < 0 || after > time.Second {
		t.Fatalf("the standby took the hold %s after the lapse on the database's clock, want within [0, 1s]", after)
	}
}

// lapsingHold is a hold whose holder has stopped renewing, as a killed holder's does, and the
// standby that will try the name, before the hold has lapsed.
type lapsingHold struct {
	database *store.Store
	opts     duty.HoldOptions
	first    *duty.Hold
	// lapse is the expiry the first holder's take wrote, on the database's clock.
	lapse   time.Time
	standby uuid.UUID
}

// newLapsingHold takes the hold on a second process and closes that process's pool, so no
// renewal lands again, and checks that before the lapse the standby is refused and Expiry reads
// time left on a held name.
func newLapsingHold(t *testing.T) *lapsingHold {
	t.Helper()
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
	return &lapsingHold{database: database, opts: opts, first: first, lapse: lapse, standby: standby}
}

// takenBy checks that the row names the standby, whose hold taken is, and that the first holder
// had reported its loss, and answers how long after the lapse the take-over ran, on the
// database's clock.
func (l *lapsingHold) takenBy(t *testing.T, taken *duty.Hold) time.Duration {
	t.Helper()
	t.Cleanup(func() { _ = taken.Release(context.Background()) })
	holder, _, expiresAt := holdRow(t, l.database.Pool)
	if holder != l.standby {
		t.Fatalf("the row names %s, want the standby %s", holder, l.standby)
	}
	select {
	case <-l.first.Lost():
	default:
		t.Error("the first holder had not reported its loss when the standby took the hold")
	}
	return expiresAt.Add(-l.opts.TTL).Sub(l.lapse)
}

// A holder whose renewals cannot land - its row locked by another transaction for longer than a
// renewal waits - reports the loss at the deadline the take armed, which is TTL - Margin after
// the take was sent and so Margin before the expiry that take wrote on the database's clock. The
// hold runs on a fake clock here, so the deadline arrives only where the test moves it, and no
// renewal can confirm anything meanwhile: the row the loss is read against still names this
// holder and is still live.
func TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	// Held: the hold's loss deadline is measured against the row's expiry, which PostgreSQL dates
	// with clock_timestamp() on the machine's clock, so the hold's clock must run with it.
	fake := clocktest.Held()
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Clock: fake}
	ctx := context.Background()
	holder := uuid.New()
	before := fake.Now()
	hold := mustTakeHold(t, database.Pool, holder, opts)
	after := fake.Now()

	// The take's loss deadline is TTL - Margin from the moment it was sent, which is between the
	// two readings around it, and the timer it armed on the fake is that deadline, later by no
	// more than the take took: the fake runs with the machine's clock (Held) and reads its own now
	// again as it arms, still inside the take.
	deadline := hold.Deadline()
	if deadline.Before(before.Add(opts.TTL-opts.Margin)) || deadline.After(after.Add(opts.TTL-opts.Margin)) {
		t.Fatalf("the loss deadline is %s; want TTL - Margin (%s) after the take, which was sent between %s and %s",
			deadline, opts.TTL-opts.Margin, before, after)
	}
	armed := fake.AwaitArmed(t, "the hold's first renewal and its loss deadline", 2)
	loss := armed[len(armed)-1]
	if loss.Before(deadline) || loss.After(deadline.Add(after.Sub(before))) {
		t.Fatalf("the latest timer armed on the hold's clock is at %s; want its loss deadline %s, later by at most the %s the take took",
			loss, deadline, after.Sub(before))
	}

	locker, err := second.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(ctx, `select 1 from duty_holds where name = $1 for update`, holdName); err != nil {
		t.Fatalf("lock the hold's row: %v", err)
	}
	// One renewal, which finds the row locked and waits on it: it confirms nothing, so the loss
	// deadline stays where the take put it.
	fake.Advance(opts.Renew)
	storetest.WaitForLockWaiters(t, locker, 1)
	if moved := hold.Deadline(); !moved.Equal(deadline) {
		t.Fatalf("the loss deadline moved to %s while the renewal was still waiting on the row; want the take's %s", moved, deadline)
	}
	select {
	case <-hold.Lost():
		t.Fatal("Lost closed before the hold's clock reached the loss deadline")
	default:
	}

	// Only now does the hold's clock reach the deadline.
	fake.Set(loss)

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
	if deadline := hold.Deadline(); deadline.After(fake.Now()) {
		t.Errorf("Lost is closed but the deadline is %s ahead on the hold's clock", deadline.Sub(fake.Now()))
	}
	t.Logf("Lost closed %s before the row's expiry on the database's clock", margin)
}

// unansweredRenewal is a hold's database that leaves one renewal, the one at index leave (0 the
// first), unanswered: it waits until its context ends, as a renewal sent to a database that
// answers nothing does. Every other statement, and every other renewal, reaches the database. It
// records the hold's clock as each renewal is sent, and closes left when the unanswered one
// arrives.
type unansweredRenewal struct {
	duty.Holds
	clock *clocktest.Fake
	leave int
	left  chan struct{}

	mu   sync.Mutex
	sent []time.Time
}

func (u *unansweredRenewal) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if !strings.HasPrefix(sql, "update duty_holds set expires_at = now() + make_interval") {
		return u.Holds.Exec(ctx, sql, arguments...)
	}
	u.mu.Lock()
	index := len(u.sent)
	u.sent = append(u.sent, u.clock.Now())
	u.mu.Unlock()
	if index == u.leave {
		close(u.left)
		<-ctx.Done()
		return pgconn.CommandTag{}, ctx.Err()
	}
	return u.Holds.Exec(ctx, sql, arguments...)
}

// renewals is the hold's clock at each renewal sent so far.
func (u *unansweredRenewal) renewals() []time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.sent)
}

// awaitArmedAt waits until fake has a timer armed at at.
func awaitArmedAt(t *testing.T, fake *clocktest.Fake, what string, at time.Time) {
	t.Helper()
	testwait.Until(t, what+" to be armed at "+at.Format(time.TimeOnly), func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, slices.ContainsFunc(armed, at.Equal)
	})
}

// A renewal that draws no answer is given up at its own bound, Renew after it was sent, and the
// renewal after it is sent at once, as the ticker before the hold's clock sent the tick that fell
// while the renewal waited, not one Renew later. With the delivery intake's hold (TTL 15 s, Renew
// 5 s, Margin 2 s) a renewal confirmed at 5 s leaves the loss deadline at 18 s; the one sent at
// 10 s draws no answer and is given up at 15 s, and only a renewal sent then is confirmed before
// 18 s: one sent at 20 s would find the hold already lost. The hold runs on a fake clock, so each
// renewal is sent where the row moves it, and the clock stands at 15 s while the next one goes out.
func TestAHoldRenewsAgainAtOnceAfterARenewalDrawsNoAnswer(t *testing.T) {
	database := storetest.Open(t)
	fake := clocktest.New(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	db := &unansweredRenewal{Holds: database.Pool, clock: fake, leave: 1, left: make(chan struct{})}
	opts := duty.HoldOptions{TTL: 15 * time.Second, Renew: 5 * time.Second, Margin: 2 * time.Second, Clock: fake}
	began := fake.Now()
	at := func(d time.Duration) time.Time { return began.Add(d) }
	hold := mustTakeHold(t, db, uuid.New(), opts)
	awaitDeadline := func(what string, want time.Time) {
		t.Helper()
		testwait.Until(t, what, func() (time.Time, bool) {
			deadline := hold.Deadline()
			return deadline, deadline.Equal(want)
		})
	}

	awaitArmedAt(t, fake, "the first renewal", at(opts.Renew))
	fake.Set(at(opts.Renew))
	awaitDeadline("the first renewal to move the loss deadline to 18 s", at(opts.Renew+opts.TTL-opts.Margin))

	awaitArmedAt(t, fake, "the second renewal", at(2*opts.Renew))
	fake.Set(at(2 * opts.Renew))
	testwait.Receive(t, "the second renewal, the one left unanswered, to reach the database", db.left)
	awaitArmedAt(t, fake, "the unanswered renewal's own bound, Renew after it was sent", at(3*opts.Renew))
	fake.Set(at(3 * opts.Renew))

	awaitDeadline("the renewal after the unanswered one to move the loss deadline to 28 s", at(3*opts.Renew+opts.TTL-opts.Margin))
	if sent := db.renewals(); len(sent) != 3 || !sent[2].Equal(at(3*opts.Renew)) {
		t.Fatalf("renewals sent at %v on the hold's clock; want three, the third at %s, when the unanswered one was given up",
			sent, at(3*opts.Renew))
	}
	select {
	case <-hold.Lost():
		t.Fatal("the hold reported itself lost; want it kept by the renewal sent once the unanswered one was given up")
	default:
	}
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
// notified - but keeps the row's token, so the next holder reads what this one recorded.
func TestHoldLapseFreesAtOnceAndKeepsTheToken(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.pending"}
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

// A renewal that affects no row - the row taken over by another holder after a lapse - closes Lost
// at once, long before the loss deadline, and the holder's Release then leaves the new holder's
// row alone.
func TestHoldZeroRowRenewalClosesLost(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	fake := clocktest.New(time.Now())
	opts := duty.HoldOptions{TTL: 10 * time.Second, Renew: 200 * time.Millisecond, Margin: time.Second, Clock: fake}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)
	loss := hold.Deadline()
	fake.AwaitArmed(t, "the hold's first renewal and its loss deadline", 2)

	other := uuid.New()
	if _, err := second.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() + interval '10 seconds' where name = $1`,
		holdName, other); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	// The fake brings the one renewal that finds the row another holder's, and nothing else: it
	// never reaches the loss deadline, so a Lost that closes here is that renewal's.
	fake.Advance(opts.Renew)
	waitLost(t, hold)
	if now := fake.Now(); !now.Before(loss) {
		t.Fatalf("the hold's clock stands at %s, at or past the loss deadline %s: Lost may have closed at the deadline rather than on the zero-row renewal", now, loss)
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
	// Both ends of the window are read from the database's clock, the one Expiry answers on;
	// nothing here is measured on this process's.
	direct := expiresAt.Sub(after)
	window := after.Sub(before)
	if gap := remaining - direct; gap < 0 || gap > window {
		t.Fatalf("Expiry = %s and expires_at - now() = %s, %s apart, while the database's clock moved %s between the two statements: want a gap in [0, %s]",
			remaining, direct, gap, window, window)
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
	// the whole window. The window's two ends are the database's clock, as the remainder is.
	window := databaseNow(t, database).Sub(wrote)
	if remaining > -2*time.Second || remaining < -2*time.Second-window {
		t.Fatalf("Expiry of a row lapsed 2 s ago = %s, while the database's clock moved %s between the write and the read after it: want a remainder in [%s, -2s]",
			remaining, window, -2*time.Second-window)
	}
}

// HoldToken reads the token a hold's row records and whether a holder holds it live: no row is ""
// and not held, a live hold is its token and held, and a lapsed row keeps its token, not held, so
// a standby reads what the lapsed holder recorded before TryHold records its own.
func TestHoldTokenReadsTheRowsTokenAndWhetherItIsLive(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if token, held, err := duty.HoldToken(ctx, database.Pool, holdName); err != nil || held || token != "" {
		t.Fatalf("HoldToken with no row = %q, held %v, error %v; want \"\", not held", token, held, err)
	}

	opts := duty.HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.live"}
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
