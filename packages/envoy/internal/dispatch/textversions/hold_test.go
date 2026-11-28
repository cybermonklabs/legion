package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds runs the fill as two serving
// processes run it, each on a store of its own and on a fake clock of its own (Config.clock), which
// its hold runs on too, and writes events while both are woken by each: the hold and the cursor row
// name the first process alone, which writes every event's rows, while the second stands by. No
// release wakes the standby, so only its ticker of standbyRetry brings a try: the test moves the
// standby's clock to each tick, ten times, one for each event, and each try finds the first's hold
// live and is refused. The first's clock never moves, so its hold is never renewed: its expiry is
// planted a day ahead once it holds, so the hold stays live on the database's clock however long a
// loaded machine takes over the ten tries. Then the first dies with its hold held: its pool closes,
// so no renewal and no release of its reaches the database, and its context ends, and the lapse
// its TTL would bring is planted (expires_at = now()). The second takes the hold at its first tick
// after the lapse, on its clock, and writes the next events, and the cursor row's holder, once it
// names the second, never names the first again.
//
// The 60 s of the name is a fact of the configuration, read from the constants and on no clock: a
// dead holder's hold lapses holdOptions.TTL after its last renewal, and a standby takes a lapsed
// hold at its next tick, within standbyRetry. Each wait is on its fact, bounded by the test
// binary's deadline alone: each tick armed, the take, and the rows, which wait on the event horizon
// (textversionstest.AwaitRows).
func TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds(t *testing.T) {
	t.Parallel()
	if holdOptions.TTL+standbyRetry > time.Minute {
		t.Fatalf("holdOptions.TTL %v and standbyRetry %v: a standby may take a dead holder's hold more than 60 s after its death",
			holdOptions.TTL, standbyRetry)
	}
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFillWith(t, storetest.SecondProcess(t, database), nil, true, func(config *Config) { config.clock = clocktest.New(fakeStart) })
	firstHolder := awaitLiveHolder(t, database, "a fill to take the hold")
	plantHoldExpiry(t, database, firstHolder, 24*time.Hour)
	standbyClock := clocktest.New(fakeStart)
	second := startFillWith(t, storetest.SecondProcess(t, database), nil, true, func(config *Config) { config.clock = standbyClock })
	second.AwaitLine(t, standingByMessage)
	tick := fakeStart.Add(standbyRetry)
	standbyClock.AwaitArmedAt(t, "the standby's first tick, standbyRetry after it stood by", tick)

	var holders cursorHolders
	var ids []int64
	// Each event is written while both fills run, and then the standby tries for the hold at its
	// tick: its next tick armed says the try is done, and a try that took the live hold fails here.
	for range 10 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
		first.Wake()
		second.Wake()
		holders.sample(t, database)
		standbyClock.Set(tick)
		nextTick := tick.Add(standbyRetry)
		testwait.Until(t, fmt.Sprintf("the standby's try at its tick %s to be refused and arm its next tick", tick.Format(time.RFC3339)),
			func() (string, bool) {
				if live := liveHolder(t, database); live != firstHolder {
					t.Fatalf("while both fills run, the hold names %q after the standby's try at its tick %s; want the first's, %s",
						live, tick.Format(time.RFC3339), firstHolder)
				}
				armed := standbyClock.Armed()
				return fmt.Sprintf("the standby's clock holds %v", armed), slices.ContainsFunc(armed, nextTick.Equal)
			})
		tick = nextTick
	}
	textversionstest.AwaitRows(t, database, ids, func() { holders.sample(t, database) })
	if got := holders.read; len(got) != 1 || got[0] != firstHolder {
		t.Fatalf("while both fills ran the cursor row named %v; want the first's holder %s alone", got, firstHolder)
	}
	if second.Logged(t, batchMessage) {
		t.Fatalf("the standby wrote a batch while the first held the hold:\n%s", second.Log())
	}

	first.db.Pool.Close()
	first.Stop(t)
	// The dead holder's hold lapses as holdOptions.TTL passing with no renewal leaves it: its
	// expiry at the database's now(), its holder kept.
	plantHoldExpiry(t, database, firstHolder, 0)
	var next []int64
	for range 3 {
		next = append(next, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	second.Wake()
	// Nothing tells the standby that the hold lapsed, and its clock has not reached its next tick,
	// so it has not taken it: only its try at that tick, below, can.
	if live := liveHolder(t, database); live != "" || second.Logged(t, tookHoldMessage) {
		t.Fatalf("the standby took the lapsed hold before its tick: the hold names %q live:\n%s", live, second.Log())
	}
	standbyClock.Set(tick)
	var standby string
	testwait.Until(t, fmt.Sprintf("the standby's try at its first tick after the lapse, %s, to take the dead holder's hold", tick.Format(time.RFC3339)),
		func() (string, bool) {
			holders.sample(t, database)
			if standby = liveHolder(t, database); standby != "" && standby != firstHolder {
				return "the hold names " + standby, true
			}
			return fmt.Sprintf("%s; the standby's clock holds %v", holdRow(t, database), standbyClock.Armed()), false
		})
	// The standby took the hold at that tick's moment on its clock: the hold's loss deadline is
	// that moment plus holdOptions.TTL - holdOptions.Margin.
	standbyClock.AwaitArmedAt(t, "the loss deadline of the hold the standby took at its tick", tick.Add(holdOptions.TTL-holdOptions.Margin))
	// The standby's clock does not move again, so its hold is never renewed either: its expiry is
	// planted a day ahead, as the first's was, so the hold stays live while the standby writes.
	plantHoldExpiry(t, database, standby, 24*time.Hour)
	textversionstest.AwaitRows(t, database, next, func() {
		holders.sample(t, database)
		second.Wake()
	})
	holders.sample(t, database)
	got := holders.read
	if len(got) != 2 || got[0] != firstHolder || got[1] == firstHolder {
		t.Fatalf("the cursor row named %v in turn; want the first's holder %s, then the second's, and never the first's again",
			got, firstHolder)
	}
	if live := liveHolder(t, database); live != got[1] {
		t.Fatalf("the hold names %q; want the second's holder %s, which wrote the cursor row", live, got[1])
	}
}

// TestAStandbyTakesTheReleasedHoldAtOnce stops the holder as a serving process stops, by its
// context: the batch in flight commits or rolls back and the hold is released, which notifies
// the standby through its listener (duty.HoldReleasedChannel on a pglisten.Hub, as the server
// registers it). The standby runs on a fake clock the test never moves (Config.clock), so its
// ticker of standbyRetry never ticks, and its released wake is signalled by a notification naming
// the fill's hold alone (releasedWake), so the notification is the one thing that can make it take
// the hold: it takes the hold, and a pass writes the next event. Its hold runs on that clock too,
// so it is never renewed: its expiry is planted a day ahead once it holds (plantHoldExpiry), so no
// check after the take waits on the hold's TTL on the database's clock. Its events are woken only
// once it is seen to hold the hold, so a standby that needs an event's wake to take it fails. Each
// wait is on its fact, bounded by the test binary's deadline alone: a fixed span from the stop
// would include the pass's wait on the event horizon, which waits on the database's open event
// writes (textversionstest.AwaitRows), not on the standby.
func TestAStandbyTakesTheReleasedHoldAtOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, "a fill to take the hold")
	standbyStore := storetest.SecondProcess(t, database)
	released := releasedWake(t, standbyStore)
	standbyClock := clocktest.New(fakeStart)
	second := startFillWith(t, standbyStore, released, true, func(config *Config) { config.clock = standbyClock })
	second.AwaitLine(t, standingByMessage)
	// The holder writes an event before it stops, so what it releases is a hold it fills under.
	written := commitComment(t, database, broker, textversionstest.Issue).id
	first.Wake()
	textversionstest.AwaitRows(t, database, []int64{written}, nil)

	first.Stop(t)
	if !first.Logged(t, releasedHoldMessage) {
		t.Fatalf("the holder stopped without logging its release:\n%s", first.Log())
	}
	if live := liveHolder(t, database); live == firstHolder {
		t.Fatalf("the holder stopped and the hold still names it, %s", firstHolder)
	}
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing wakes the standby's events before its take, and its clock stands still: only the
	// release's notification can bring the take.
	standby := awaitLiveHolder(t, database, "the standby to take the released hold on the release's notification")
	// The standby's clock never moves, so its hold is never renewed: its expiry is planted a day
	// ahead, so the hold stays live while the standby writes.
	plantHoldExpiry(t, database, standby, 24*time.Hour)
	// The standby took the hold, and may have run its first pass under it, before the event above
	// was written.
	second.Wake()
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestAStandbyTakesAFreeHoldWithNoNotification gives the standby a released wake that is never
// signalled, as a standby whose listener is down has: the holder releases its hold and nothing
// tells the standby, which leaves the hold free until its next retry. The standby's ticker of
// standbyRetry runs on a fake clock (Config.clock), which the test moves to each tick once the
// ticker has armed it, as standbyRetry passing does, until a try at a tick takes the free hold.
// Its hold runs on that clock too, which the test does not move again, so it is never renewed: its
// expiry is planted a day ahead once it holds (plantHoldExpiry), so no check after the take waits
// on the hold's TTL on the database's clock. Nothing wakes the standby's events, so a standby that
// took the hold only once an event woke it would not pass; its first pass under the hold writes
// what was written while it stood by.
//
// Each wait is on its fact, bounded by the test binary's deadline alone: the hold coming free, the
// tick's try, and the row, which the pass writes only once the event horizon, which waits on the
// database's open event writes, has passed the event (textversionstest.AwaitRows). A fixed span
// from the holder's stop, the ticker's 10 s and 5 s more, read the instance's load as well as the
// standby (merge-group run 44963211024: `the events [1] had 0 rows within 14.996081355s; want 1`).
func TestAStandbyTakesAFreeHoldWithNoNotification(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, "a fill to take the hold")
	standbyClock := clocktest.New(fakeStart)
	second := startFillWith(t, storetest.SecondProcess(t, database), make(chan struct{}), true, func(config *Config) { config.clock = standbyClock })
	second.AwaitLine(t, standingByMessage)
	tick := fakeStart.Add(standbyRetry)
	standbyClock.AwaitArmedAt(t, "the standby's first tick, standbyRetry after it stood by", tick)

	first.Stop(t)
	// The holder released its hold as it stopped, or, where its release did not reach the
	// database, the hold lapses holdOptions.TTL after its last renewal: either way the stopped
	// holder's hold ends, and nothing tells the standby.
	testwait.Until(t, "the stopped holder's hold to end", func() (string, bool) {
		live := liveHolder(t, database)
		return live, live != firstHolder
	})
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing has told the standby that the hold is free, nothing wakes its events, and its clock
	// has not reached its tick, so it has not taken it: only a try at a tick, below, can.
	if live := liveHolder(t, database); live != "" || second.Logged(t, tookHoldMessage) {
		t.Fatalf("the standby took the hold before its tick, with no wake for the release: the hold names %q live:\n%s", live, second.Log())
	}

	// A try at a tick that found no free hold, or failed, arms the next tick standbyRetry later.
	var standby string
	for standby == "" {
		standbyClock.Set(tick)
		next := tick.Add(standbyRetry)
		testwait.Until(t, fmt.Sprintf("the standby's try at its tick %s to take the free hold, or to arm its next tick", tick.Format(time.RFC3339)),
			func() (string, bool) {
				if standby = liveHolder(t, database); standby != "" {
					return "the hold names " + standby, true
				}
				armed := standbyClock.Armed()
				return fmt.Sprintf("%s; the standby's clock holds %v", holdRow(t, database), armed), slices.Contains(armed, next)
			})
		tick = next
	}
	// The standby's clock does not move again, so its hold is never renewed: its expiry is planted
	// a day ahead, so the hold stays live while the standby writes.
	plantHoldExpiry(t, database, standby, 24*time.Hour)
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestTheFillWritesNothingUntilTheServerListens holds Serving open with events written and the
// fill woken: Run's goroutine waits at its gate on Serving (Config.awaitingServing), and the fill
// has written no row, no cursor row and no hold, then and after a span of 5 s (notServingWhy).
// Once Serving closes, as it does once the server listens and is admitted to the registry of live
// server processes, the rows come.
func TestTheFillWritesNothingUntilTheServerListens(t *testing.T) {
	// Not parallel: it holds a span of 5 s of real time with Serving open (notServingWhy), in which
	// a write the parallel tests' load delayed past the span would pass.
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	atGate := make(chan struct{})
	fill := startFillWith(t, storetest.SecondProcess(t, database), nil, false, func(config *Config) {
		config.awaitingServing = func() { close(atGate) }
	})
	var ids []int64
	for range 3 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	fill.Wake()

	testwait.Receive(t, "the fill's Run to reach its gate on Serving", atGate)
	requireNothingWritten(t, database, "at the fill's gate on Serving")
	testwait.Span(t, 5*time.Second, notServingWhy)
	requireNothingWritten(t, database, "5 s after the fill reached its gate on Serving")

	close(fill.serving)
	textversionstest.AwaitRows(t, database, ids, nil)
}

// notServingWhy is why TestTheFillWritesNothingUntilTheServerListens holds a span with Serving
// open, after the state that proves what it can.
const notServingWhy = "the gate hook and the reads show Run's one goroutine waiting on Serving with nothing written, " +
	"but no state reports, no clock of the test drives and no later action is ordered against a write from a goroutine " +
	"Run starts before its gate, and synctest.Wait cannot hold the fill's real Postgres connections; " +
	"5 s is main's window, so a write within it fails here as it failed there"

// requireNothingWritten holds the fill to no keyword row, no cursor row and no hold, when names
// the moment.
func requireNothingWritten(t *testing.T, database *store.Store, when string) {
	t.Helper()
	var rows, cursors, holds int
	if err := database.Pool.QueryRow(t.Context(), `
		select (select count(*) from search_text_versions), (select count(*) from search_text_versions_fill),
		       (select count(*) from duty_holds where name = $1)
	`, fillHoldName).Scan(&rows, &cursors, &holds); err != nil {
		t.Fatalf("read the keyword rows, the cursor row and the hold: %v", err)
	}
	if rows != 0 || cursors != 0 || holds != 0 {
		t.Fatalf("%s, before the process serves, the fill wrote %d rows, %d cursor rows and %d holds; want none", when, rows, cursors, holds)
	}
}

// TestACaughtUpFillSendsNoStatement runs the fill, and its hold, on a fake clock (Config.clock),
// on a store whose every statement a tracer sees, the hold's renewals counted apart from the rest.
// Once the fill has written the events, it waits for its next pass with no timer, no wake standing
// and no statement since (Config.waiting), and the fake, which has not moved, holds only its hold's
// first renewal and its loss deadline. The test then moves the fake through four renewal
// intervals, 20 s of its time, each move to the renewal armed: each brings one renewal and no other
// statement, and arms the next renewal and the loss deadline holdOptions.TTL - holdOptions.Margin
// past the renewal's moment, so every timer the fill and its hold run on the fake has had 20 s to
// fire. A renewal that left the deadline where it was would let the hold lapse 13 s in, and the
// fill release it and take it again. Then, with the fake standing still, the fill sends no
// statement at all, renewals included, over a span of 19 s (caughtUpWhy).
func TestACaughtUpFillSendsNoStatement(t *testing.T) {
	// Not parallel: it holds a span of 19 s of real time over every statement of the fill
	// (caughtUpWhy), in which a statement the parallel tests' load delayed past the span would
	// pass.
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	tracer := &renewalsApart{}
	fake := clocktest.New(fakeStart)
	// waited is the fill's last report that it waits for its next pass: the wait due then, whether
	// a wake it had not taken yet stood on its events channel, which ends that wait at once, and
	// how many statements other than the renewals it had sent by then.
	var waited struct {
		sync.Mutex
		seen, woken bool
		due         time.Duration
		others      int64
	}
	fill := startFillWith(t, storetest.TracedStore(t, database, tracer), nil, true, func(config *Config) {
		config.clock = fake
		// config is startFillWith's Config, whose Events the fill's goroutine sets before it runs
		// Run on it, and the hook runs on that goroutine.
		config.waiting = func(due time.Duration) {
			waited.Lock()
			defer waited.Unlock()
			waited.seen, waited.woken, waited.due, waited.others = true, len(config.Events) > 0, due, tracer.others.Count()
		}
	})
	awaitLiveHolder(t, database, "a fill to take the hold")
	var ids []int64
	for range 3 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	fill.Wake()
	textversionstest.AwaitRows(t, database, ids, nil)

	// The state: the fill's last report, with no statement after it and no wake standing, is of a
	// wait for its next pass that nothing the test does ends. The rows' statements were sent before
	// the read above returned, and the test's one wake before that, so that report came after the
	// pass that wrote them, and a timer due then is a re-read at rest. A wake standing at the report
	// starts a pass at once, whose first statements, its read of the horizon, go in a pipeline the
	// tracer does not count (storetest.Traced), so the count alone cannot tell that report from a
	// rest.
	testwait.Until(t, "the fill to wait for its next pass with no wake standing and no statement since it began to wait", func() (string, bool) {
		waited.Lock()
		seen, woken, due, others := waited.seen, waited.woken, waited.due, waited.others
		waited.Unlock()
		switch since := tracer.others.Count() - others; {
		case !seen:
			return "the fill has not waited for a pass", false
		case since != 0:
			return fmt.Sprintf("%d statements since the fill last began to wait", since), false
		case woken:
			return "a wake stood on the fill's events channel as it last began to wait", false
		case due != 0:
			t.Fatalf("the fill wrote the events and waits for its next pass with a timer of %v; want none for a fill caught up with nothing written:\n%s",
				due, fill.Log())
		}
		return "the fill waits for its next pass with no timer and no wake", true
	})
	// The fake has not moved since the fill took its hold: it holds the hold's first renewal,
	// holdOptions.Renew after the take, and its loss deadline, holdOptions.TTL - holdOptions.Margin
	// after it, and no other timer.
	renewal := fakeStart.Add(holdOptions.Renew)
	armed := fake.AwaitArmedAt(t, "the hold's first renewal", renewal)
	if want := []time.Time{renewal, fakeStart.Add(holdOptions.TTL - holdOptions.Margin)}; !slices.EqualFunc(armed, want, time.Time.Equal) {
		t.Fatalf("the fill rests with %v armed on its clock; want only its hold's first renewal and loss deadline, %v", armed, want)
	}

	// The later action: each move to the renewal armed brings one renewal and no other statement,
	// and arms the next renewal and the loss deadline from the renewal's moment.
	renewed, others := tracer.renewals.Load(), tracer.others.Count()
	for range 4 {
		fake.Set(renewal)
		renewed++
		want := []time.Time{renewal.Add(holdOptions.Renew), renewal.Add(holdOptions.TTL - holdOptions.Margin)}
		testwait.Until(t, fmt.Sprintf("the hold's renewal at %s, and its next renewal and its loss deadline armed from it", renewal.Format(time.RFC3339)),
			func() (string, bool) {
				n, armed := tracer.renewals.Load(), fake.Armed()
				if n > renewed {
					t.Fatalf("the move to the hold's renewal at %s brought %d renewals; want one", renewal.Format(time.RFC3339), n-renewed+1)
				}
				return fmt.Sprintf("%d renewals, %v armed on the fill's clock; want %d, and %v", n, armed, renewed, want),
					n == renewed && slices.EqualFunc(armed, want, time.Time.Equal)
			})
		if made := tracer.others.Count() - others; made != 0 {
			t.Fatalf("by the hold's renewal at %s the fill sent %d statements but its renewals; want none:\n%s", renewal.Format(time.RFC3339), made, fill.Log())
		}
		renewal = want[0]
	}

	storetest.RequireNoStatementsBecause(t, &tracer.all, 19*time.Second, "a fill caught up with nothing written, on a clock standing still", caughtUpWhy)
}

// caughtUpWhy is why TestACaughtUpFillSendsNoStatement holds a span with the fill's clock standing
// still, after the state and the later action that prove what they can.
const caughtUpWhy = "the waiting hook and the fake's armed set show the fill between passes with no timer but its hold's renewal and loss deadline, " +
	"and the four renewals on the fake give every timer of the fill and its hold on Config.clock 20 s to fire, " +
	"but no state reports, no clock of the test drives and no later action is ordered against a statement from a timer left on the wall clock " +
	"or from work no state reports, and synctest.Wait cannot hold the fill's real Postgres connections; " +
	"19 s is main's 9 s of quiet and the 10 s after it: main fails a statement that recurs with a period under 19 s, " +
	"one under 9 s never giving the quiet and one from 9 s to 19 s landing in the 10 s after it, so a shorter span would pass one main fails"

// renewalsApart counts the statements the store it traces sends (storetest.TracedStore): every
// one in all, and apart, the hold's renewals (duty's renewHold) in renewals and every other
// statement in others.
type renewalsApart struct {
	all, others storetest.StatementCounter
	renewals    atomic.Int64
}

func (r *renewalsApart) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = r.all.TraceQueryStart(ctx, conn, data)
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "update duty_holds set expires_at = now() + make_interval") {
		r.renewals.Add(1)
		return ctx
	}
	return r.others.TraceQueryStart(ctx, conn, data)
}

func (r *renewalsApart) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// fillProcess is the fill one serving process runs (Run, through textversionstest.StartFill), on a
// store of the test's own, so two of them take turns on the hold through the database as two
// processes do. Its serving is the process's, open until the test closes it where startFill did
// not.
type fillProcess struct {
	*textversionstest.Fill
	db      *store.Store
	serving chan struct{}
}

// startFill runs the fill on db, woken for a released hold by released, with the process serving
// already when serving is set, and stops it once the test ends. A standby of it retries on its
// ticker of standbyRetry, on the wall clock.
func startFill(t *testing.T, db *store.Store, released <-chan struct{}, serving bool) *fillProcess {
	t.Helper()
	return startFillWith(t, db, released, serving, func(*Config) {})
}

// startFillWith is startFill with configure's change to the Config the fill runs with: the clock
// its standby's ticker, its hold and its waits run on (Config.clock), or a hook that reports its
// state to the test.
func startFillWith(t *testing.T, db *store.Store, released <-chan struct{}, serving bool, configure func(*Config)) *fillProcess {
	t.Helper()
	process := &fillProcess{db: db, serving: make(chan struct{})}
	if serving {
		close(process.serving)
	}
	config := Config{
		Leases:   storetest.Leases(t, db.Pool),
		Horizon:  events.NewHorizon(db.Pool),
		Latency:  sessionanswers.NewLatency(time.Now),
		Serving:  process.serving,
		Released: released,
	}
	configure(&config)
	process.Fill = textversionstest.StartFill(t, func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error {
		config.Events, config.Logger = wake, logger
		return Run(ctx, config)
	})
	return process
}

// releasedWake runs a listener hub on db's listener pool, as the server runs its own, and answers
// a wake signalled by each notification on duty.HoldReleasedChannel that names the fill's hold,
// once the hub listens. The server's own wake (pglisten.Hub.Wake) is signalled after each LISTEN
// too, so that a standby catches up on a release sent while it had no connection; this one is
// signalled by a notification alone, so a standby that takes the hold on it took it on the
// release's notification.
func releasedWake(t *testing.T, db *store.Store) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "textversions test", Dial: db.Pool.Listener}
	wake := make(chan struct{}, 1)
	hub.Handle(duty.HoldReleasedChannel, func(payload string) {
		if payload != fillHoldName {
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	listening := make(chan struct{})
	var once sync.Once
	hub.OnConnected(func() { once.Do(func() { close(listening) }) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the standby's listener hub to return", done)
	})
	testwait.Receive(t, "the standby's listener hub to listen", listening)
	return wake
}

// liveHolder is the holder duty_holds names live for the fill's hold, as text, or "" for none.
func liveHolder(t *testing.T, database *store.Store) string {
	t.Helper()
	var holder string
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select holder::text from duty_holds where name = $1 and expires_at > now()), '')
	`, fillHoldName).Scan(&holder); err != nil {
		t.Fatalf("read the fill's hold: %v", err)
	}
	return holder
}

// awaitLiveHolder waits, through the shared wait, until the fill's hold names a live holder, and
// answers it; what names what the test waits for. A read that finds none reads what duty_holds
// holds for the hold (holdRow).
func awaitLiveHolder(t *testing.T, database *store.Store, what string) string {
	t.Helper()
	var holder string
	testwait.Until(t, what, func() (string, bool) {
		if holder = liveHolder(t, database); holder != "" {
			return "the hold names " + holder, true
		}
		return holdRow(t, database), false
	})
	return holder
}

// holdRow says what duty_holds holds for the fill's hold: the holder its row names and how far
// the row's expiry stands from the database's now() (below zero once it has lapsed), or that the
// hold has no row.
func holdRow(t *testing.T, database *store.Store) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select format('duty_holds names holder %s, expiring %s after now()', holder, expires_at - now())
			from duty_holds where name = $1), format('duty_holds has no row for %s', $1::text))
	`, fillHoldName).Scan(&row); err != nil {
		t.Fatalf("read the fill's hold row: %v", err)
	}
	return row
}

// plantHoldExpiry sets the expiry of the fill's hold, while its row names holder, to the
// database's now() plus left: zero plants the lapse holdOptions.TTL passing with no renewal
// leaves, and a day keeps live the hold of a fill whose clock the test does not move to its
// renewals.
func plantHoldExpiry(t *testing.T, database *store.Store, holder string, left time.Duration) {
	t.Helper()
	tag, err := database.Pool.Exec(t.Context(),
		`update duty_holds set expires_at = now() + make_interval(secs => $3) where name = $1 and holder::text = $2`,
		fillHoldName, holder, left.Seconds())
	if err != nil {
		t.Fatalf("plant the fill's hold's expiry %v after now() for holder %s: %v", left, holder, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("plant the fill's hold's expiry %v after now(): %s, not holder %s", left, holdRow(t, database), holder)
	}
}

// cursorHolders is the cursor row's holder as the test read it.
type cursorHolders struct {
	// read is the holders read, each change once, in order: a holder that came back is read twice.
	read []string
}

// sample reads the cursor row's holder and records it when it differs from the last one read. A
// missing row, or one that names no holder, records nothing.
func (h *cursorHolders) sample(t *testing.T, database *store.Store) {
	t.Helper()
	var holder string
	err := database.Pool.QueryRow(t.Context(),
		`select coalesce(holder::text, '') from search_text_versions_fill where id = 1`).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return
	case err != nil:
		t.Fatalf("read the cursor row's holder: %v", err)
	}
	if holder != "" && (len(h.read) == 0 || h.read[len(h.read)-1] != holder) {
		h.read = append(h.read, holder)
	}
}
