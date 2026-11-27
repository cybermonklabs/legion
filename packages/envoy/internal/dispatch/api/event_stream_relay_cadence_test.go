package api

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The cadence a relay generation reads a gap on (eventGapReread), step by step: a gap only the
// read's own observation holds is re-read at once; a held one waits for the next notification,
// read without the coalescing window, or the fallback, which doubles while the horizon stands
// still and drops the eager read until it moves; an immediate re-read is never asked twice
// without the horizon moving between; and no gap schedules nothing.
func TestEventGapRereadCadence(t *testing.T) {
	t.Parallel()
	var gaps eventGapReread
	defer gaps.stop()
	for step, want := range []struct {
		safe     int64
		gap      events.Gap
		now      bool
		fallback time.Duration
		eager    bool
	}{
		{safe: 5, gap: events.GapUnproven, now: true, eager: true},
		{safe: 5, gap: events.GapHeld, fallback: eventRelayGapMin, eager: true},
		{safe: 5, gap: events.GapHeld, fallback: 2 * eventRelayGapMin},
		{safe: 5, gap: events.GapUnproven, fallback: 4 * eventRelayGapMin},
		{safe: 8, gap: events.GapHeld, fallback: eventRelayGapMin, eager: true},
		{safe: 9, gap: events.GapUnproven, now: true, eager: true},
		{safe: 9, gap: events.GapNone},
	} {
		gaps.schedule(want.safe, want.gap)
		due := gaps.due()
		var fallback time.Duration
		if gaps.timer != nil {
			fallback = gaps.wait
		}
		now := due != nil && due == gapRereadNow
		if now != want.now || fallback != want.fallback || gaps.eager != want.eager || (want.gap == events.GapNone && due != nil) {
			t.Fatalf("step %d (safe %d, gap %d): now %v, fallback %s, eager %v; want %v, %s, %v",
				step+1, want.safe, want.gap, now, fallback, gaps.eager, want.now, want.fallback, want.eager)
		}
	}
}

// cadenceXid is the base of the transaction ids a cadenceHorizonDB script hands out: far above
// any the test Postgres has assigned, so a scripted read of the event writers proves every real
// observation before it, and a real read proves a scripted one only when it finds no event writer
// open at all.
const cadenceXid = uint64(1) << 40

// A relay with nothing to publish observes nothing; an event's notification wakes one read, whose
// observation sees the event but does not prove itself, and the relay observes again at once
// rather than after a delay: with no older event writer open, that second observation proves the
// first and the event is published. The script's observations never prove themselves
// (cadenceRow), so this holds the relay to the cadence whatever the test Postgres was doing. The
// fallback timer is an hour here, so only the immediate second observation can publish it, and the
// relay is idle again once it has.
func TestEventStreamRelayProvesItsOwnObservationAtOnce(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	relay, horizon := newCadenceRelay(t, deps, database, time.Millisecond)
	subscription, _, _ := subscribeRelay(t, relay)
	horizon.settle(t)
	if observed := horizon.observedDuring(600 * time.Millisecond); observed != 0 {
		t.Fatalf("an idle relay took %d observations in 600 ms, want none", observed)
	}

	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 1},     // sees the event: unproven
		cadenceObservation{writer: cadenceXid + 2, xid: cadenceXid + 2}, // no older writer is open: proves it
	)
	id := commitCadenceEvent(t, database)
	requireRelayPublishes(t, subscription, id)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 2 scripted observations", 2-left)
	}
	if observed := horizon.observedDuring(600 * time.Millisecond); observed != 0 {
		t.Fatalf("the relay took %d observations in 600 ms after it published, want none", observed)
	}
}

// A gap an older event writer holds open is closed by that writer's commit notification: the
// read it wakes skips the coalescing window, since the events it publishes have waited already.
// Here a write holds the lower id open while the higher one commits; the relay's read of the
// higher one, and the immediate observation after it, find the write still open (the oldest event
// writer their reads find is the write's id), so the gap is held. The write's commit notifies, and
// the read it wakes proves both and publishes the two events in id order. The coalescing window is
// two seconds and the fallback timer an hour, so only a read the notification woke at once can
// take its observation less than two seconds after the commit: that moment is when the relay's
// read, its observation back, asks the script whether the write has committed (committed), and
// the clock starts once the commit has returned. The scripted proof applies only to an
// observation taken once the lower id is committed, so the script never claims the write ended
// before it did.
func TestEventStreamRelayClosesAHeldGapOnTheHoldersCommitNotification(t *testing.T) {
	t.Parallel()
	const coalesce = 2 * time.Second
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	waitForQuietEventLog(t, database, 500*time.Millisecond)
	relay, horizon := newCadenceRelay(t, deps, database, coalesce)
	subscription, _, _ := subscribeRelay(t, relay)
	// The listener's first LISTEN wakes one catch-up read, a coalescing window later; once it has
	// run no wake is pending, so the next read is the one the higher id's commit wakes. settle
	// waits for that read's observation and then for the relay to fall quiet.
	horizon.settle(t)

	var low atomic.Int64
	// lowRead is when the relay's read, its observation back, found the lower id committed: the
	// third scripted observation's, written before that observation is reported on taken.
	var lowRead time.Time
	committed := func() bool {
		asked := time.Now()
		var visible bool
		id := low.Load()
		if id != 0 && database.Pool.QueryRow(context.Background(), `select exists(select 1 from events where id = $1)`, id).Scan(&visible) == nil && visible {
			lowRead = asked
			return true
		}
		return false
	}
	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 1},                      // the write (cadenceXid) is open: unproven
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 2},                      // it is still open: held
		cadenceObservation{writer: cadenceXid + 3, xid: cadenceXid + 3, when: committed}, // it has committed: proves both
	)
	lowID, high, commitLow, _ := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	low.Store(lowID)
	horizon.awaitTaken(t, 2)
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d while the write holding %d is open", event.Event.ID, lowID)
	case <-time.After(200 * time.Millisecond):
	}

	commitLow()
	commitReturned := time.Now()
	horizon.awaitTaken(t, 3)
	if took := lowRead.Sub(commitReturned); took >= coalesce {
		t.Fatalf("the relay's read proving the lower id took its observation %v after the commit returned, at or past the %v coalescing window: the read the commit's notification woke waited the window (eventGapReread.eager)",
			took, coalesce)
	}
	requireRelayPublishes(t, subscription, lowID, high)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 3 scripted observations", 3-left)
	}
}

// waitForQuietEventLog waits until the event log's head has stood still for quiet, so no write
// the test's setup made is still to notify. Its one bound is the test binary's deadline
// (awaitStill).
func waitForQuietEventLog(t *testing.T, database *store.Store, quiet time.Duration) {
	t.Helper()
	awaitStill(t, "the event log's head", quiet, func() int64 { return eventHead(t, database) })
}

// newCadenceRelay is a relay on a server of its own whose horizon observes through a
// cadenceHorizonDB, with coalesce as its coalescing window and an hour for its fallback timer.
func newCadenceRelay(t *testing.T, deps Deps, database *store.Store, coalesce time.Duration) (*eventStreamRelay, *cadenceHorizonDB) {
	t.Helper()
	horizon := &cadenceHorizonDB{HorizonDB: database.Pool, taken: make(chan int, 16)}
	horizon.dialed.Store(-1)
	served := directServer(deps)
	served.horizonOnce.Do(func() { served.eventHorizon = events.NewHorizon(horizon) })
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(served, lifetime)
	relay.coalesce, relay.gapFallback = coalesce, time.Hour
	// The listener dials only once the generation's first cursor is read, so every observation
	// after its first dial is the catch-up read's its LISTEN wakes, or a later read's (settle).
	relay.dial = func(ctx context.Context) (*pgx.Conn, error) {
		horizon.dialed.CompareAndSwap(-1, horizon.observations.Load())
		return deps.Store.Pool.Listener(ctx)
	}
	t.Cleanup(func() {
		stop()
		relay.wait()
	})
	return relay, horizon
}

// cadenceHorizonDB observes through the real pool, so the relay starts and reads as it does in
// production, and counts every observation. Once armed, each observation that sees an id at or
// above from has its transaction id and the oldest event writer its read found replaced by the
// next scripted observation's, so the test decides which observation proves which; taken
// receives each scripted observation's position, from 1, as it is taken.
type cadenceHorizonDB struct {
	events.HorizonDB
	observations atomic.Int64
	taken        chan int
	// dialed is observations when the relay first dialed its listener, and -1 before (settle).
	dialed atomic.Int64

	mu       sync.Mutex
	from     int64
	script   []cadenceObservation
	position int
}

// cadenceObservation is one scripted transaction id and the oldest event writer's transaction id
// its read finds. when, if set, holds it back until it answers true: an observation taken before
// then keeps its real values.
type cadenceObservation struct {
	writer, xid uint64
	when        func() bool
}

func (db *cadenceHorizonDB) arm(from int64, script ...cadenceObservation) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.from, db.script = from, script
}

// left is how many scripted observations are still to be taken.
func (db *cadenceHorizonDB) left() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.script)
}

// settle waits for the catch-up read the relay's LISTEN wakes to observe, and then until the
// relay has taken no observation for 300 ms: its start and that catch-up are over. Its one bound
// is the test binary's deadline (testwait.Until, awaitStill).
func (db *cadenceHorizonDB) settle(t *testing.T) {
	t.Helper()
	testwait.Until(t, "the catch-up read the relay's LISTEN wakes to observe (the observations taken)", func() (int64, bool) {
		dialed, observed := db.dialed.Load(), db.observations.Load()
		return observed, dialed >= 0 && observed > dialed
	})
	awaitStill(t, "the relay's observations", 300*time.Millisecond, db.observations.Load)
}

// observedDuring is how many observations were taken during the next d.
func (db *cadenceHorizonDB) observedDuring(d time.Duration) int64 {
	before := db.observations.Load()
	time.Sleep(d)
	return db.observations.Load() - before
}

// awaitTaken waits for the scripted observation at position to be taken. Its one bound is the
// test binary's deadline (testwait.Receive).
func (db *cadenceHorizonDB) awaitTaken(t *testing.T, position int) {
	t.Helper()
	for {
		if taken := testwait.Receive(t, fmt.Sprintf("scripted observation %d to be taken", position), db.taken); taken >= position {
			return
		}
	}
}

func (db *cadenceHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	db.observations.Add(1)
	return &cadenceResults{BatchResults: db.HorizonDB.SendBatch(ctx, batch), db: db}
}

// cadenceResults is an observation's results with its two rows rewritten (cadenceRow): the mark's,
// read first, and then the event writers'. scripted is the scripted observation the mark's row
// took, which the writers' row answers.
type cadenceResults struct {
	pgx.BatchResults
	db       *cadenceHorizonDB
	rows     int
	scripted *cadenceObservation
}

func (r *cadenceResults) QueryRow() pgx.Row {
	r.rows++
	return cadenceRow{Row: r.BatchResults.QueryRow(), results: r, writers: r.rows == 2}
}

// cadenceRow scans one row of an observation. The mark's row (A, its created_at and X) has X
// rewritten with the next scripted observation's when the script is armed and A is at or above
// its from; the event writers' row then answers that observation's writer. A scripted
// observation never proves itself: its writer is at or below its X, so the script alone decides
// which observation proves which, whatever the test Postgres is doing.
type cadenceRow struct {
	pgx.Row
	results *cadenceResults
	writers bool
}

func (row cadenceRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	if row.writers {
		if scripted := row.results.scripted; scripted != nil {
			writer := strconv.FormatUint(scripted.writer, 10)
			*dest[0].(**string) = &writer
		}
		return nil
	}
	db := row.results.db
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.from == 0 || len(db.script) == 0 || *dest[0].(*int64) < db.from {
		return nil
	}
	next := db.script[0]
	if next.when != nil && !next.when() {
		return nil
	}
	db.script = db.script[1:]
	db.position++
	row.results.scripted = &next
	*dest[2].(*string) = strconv.FormatUint(next.xid, 10)
	db.taken <- db.position
	return nil
}

// commitCadenceEvent commits one ownerless event in a transaction of its own, taking its
// transaction id first as Broker.Append does, and returns its id. Its insert notifies the relay.
func commitCadenceEvent(t *testing.T, database *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var id int64
	if _, err := tx.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the event's transaction id: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'relay.cadence', '{"kind":"user","id":"alice"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		t.Fatalf("insert the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	return id
}

// requireRelayPublishes requires the relay to publish exactly ids, in order. Each wait's one
// bound is the test binary's deadline (testwait.Receive).
func requireRelayPublishes(t *testing.T, subscription <-chan relayedEvent, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if event := testwait.Receive(t, fmt.Sprintf("the relay to publish event %d (of %v)", id, ids), subscription); event.Event.ID != id {
			t.Fatalf("the relay published event %d, want %d (of %v); 0 is its subscription's end", event.Event.ID, id, ids)
		}
	}
}
