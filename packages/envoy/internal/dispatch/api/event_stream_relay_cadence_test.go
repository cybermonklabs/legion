package api

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
// relay's timers run on a fake clock of its own that the test moves, and its fallback is an hour,
// so only an immediate second observation can publish the event: a re-read after a delay waits on
// a timer the fake never reaches. Before the event and once it is published the relay is at rest:
// it has no read due (requireCadenceRelayAtRest).
func TestEventStreamRelayProvesItsOwnObservationAtOnce(t *testing.T) {
	t.Parallel()
	const coalesce = time.Millisecond
	_, database, deps := newTestServer(t, testServerOptions{})
	relay, fake, horizon, reads := newCadenceRelay(t, deps, database, coalesce)
	subscription, _, _ := subscribeRelay(t, relay)
	requireCadenceRelayAtRest(t, relay, fake, settleCadenceRelay(t, relay, fake, coalesce, reads), "an idle relay")

	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 1},     // sees the event: unproven
		cadenceObservation{writer: cadenceXid + 2, xid: cadenceXid + 2}, // no older writer is open: proves it
	)
	id := commitCadenceEvent(t, database)
	if first := nextCadenceRead(t, fake, coalesce, reads, "the read the event's notification wakes"); first.gap != events.GapUnproven {
		t.Fatalf("the notification's read left gap %d, want %d: its observation sees the event and does not prove itself", first.gap, events.GapUnproven)
	}
	second := testwait.Receive(t, "the relay's second observation, at once, with its clock standing still", reads)
	requireRelayPublishes(t, subscription, id)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 2 scripted observations", 2-left)
	}
	requireCadenceRelayAtRest(t, relay, fake, second, "the relay once it published")
}

// A gap an older event writer holds open is closed by that writer's commit notification: the
// read it wakes skips the coalescing window, since the events it publishes have waited already.
// Here a write holds the lower id open while the higher one commits; the relay's read of the
// higher one, and the immediate observation after it, find the write still open (the oldest event
// writer their reads find is the write's id), so the gap is held. The write's commit notifies,
// and the read it wakes proves both and publishes the two events in id order. The relay's timers
// run on a fake clock of its own that the test moves, its coalescing window is two seconds and its
// fallback an hour: the read the commit wakes must publish both events without arming the window.
// The scripted proof applies only to an observation taken once the lower id is committed, so the
// script never claims the write ended before it did.
func TestEventStreamRelayClosesAHeldGapOnTheHoldersCommitNotification(t *testing.T) {
	t.Parallel()
	const coalesce = 2 * time.Second
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	// No write of the setup is still to notify once its writes are committed: only an insert into
	// events notifies the relay (events_notify, 0107), the two creates committed theirs before
	// they answered, and the one write each leaves running past its answer, its project's search
	// term statistics, inserts no event. The relay then LISTENs after every notification the setup
	// sent, so none of them reaches it.
	awaitNoTransactionIDHolder(t, database, "TEST", "OTHER")
	relay, fake, horizon, reads := newCadenceRelay(t, deps, database, coalesce)
	subscription, _, _ := subscribeRelay(t, relay)
	settleCadenceRelay(t, relay, fake, coalesce, reads)

	var low atomic.Int64
	committed := func() bool {
		var visible bool
		id := low.Load()
		return id != 0 && database.Pool.QueryRow(context.Background(), `select exists(select 1 from events where id = $1)`, id).Scan(&visible) == nil && visible
	}
	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 1},                      // the write (cadenceXid) is open: unproven
		cadenceObservation{writer: cadenceXid, xid: cadenceXid + 2},                      // it is still open: held
		cadenceObservation{writer: cadenceXid + 3, xid: cadenceXid + 3, when: committed}, // it has committed: proves both
	)
	lowID, high, commitLow := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	low.Store(lowID)
	nextCadenceRead(t, fake, coalesce, reads, "the read the higher id's commit wakes")
	held := testwait.Receive(t, "the relay's second observation, at once, with its clock standing still", reads)
	if held.gap != events.GapHeld {
		t.Fatalf("the second observation left gap %d, want %d: the write holding id %d still runs", held.gap, events.GapHeld, lowID)
	}
	// The relay publishes within a read, before it reports the read, and has no read due now, so
	// nothing it publishes can arrive later: the subscription is read as it stands.
	requireCadenceRelayAtRest(t, relay, fake, held, "the relay holding the gap")
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d while the write holding %d is open", event.Event.ID, lowID)
	default:
	}

	commitLow()
	window := fake.Now().Add(coalesce)
	if testwait.Until(t, "the relay to publish both events, or to arm its coalescing window", func() (string, bool) {
		switch {
		case len(subscription) >= 2:
			return "published", true
		case slices.ContainsFunc(fake.Armed(), window.Equal):
			return "window armed", true
		}
		return fmt.Sprintf("%d published, timers armed for %v", len(subscription), fake.Armed()), false
	}) != "published" {
		t.Fatalf("the read the write's commit notification woke waits the %s coalescing window; want it read at once (eventGapReread.eager)", coalesce)
	}
	requireRelayPublishes(t, subscription, lowID, high)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 3 scripted observations", 3-left)
	}
}

// relayRead is one read of a cadence relay's generation as run reports it (afterRead): the
// horizon it read through and the gap it left open.
type relayRead struct {
	safe int64
	gap  events.Gap
}

// newCadenceRelay is a relay on a server of its own whose horizon observes through a
// cadenceHorizonDB, with coalesce as its coalescing window and an hour for its fallback timer,
// both on the fake clock it returns (eventStreamRelay.clock). reads receives each read the relay's
// generation makes, once that read's re-read is scheduled (eventStreamRelay.afterRead).
func newCadenceRelay(t *testing.T, deps Deps, database *store.Store, coalesce time.Duration) (*eventStreamRelay, *clocktest.Fake, *cadenceHorizonDB, <-chan relayRead) {
	t.Helper()
	horizon := &cadenceHorizonDB{HorizonDB: database.Pool}
	served := directServer(deps)
	served.horizonOnce.Do(func() { served.eventHorizon = events.NewHorizon(horizon) })
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(served, lifetime)
	fake := clocktest.New(time.Now())
	relay.coalesce, relay.gapFallback, relay.clock = coalesce, time.Hour, fake
	reads := make(chan relayRead, 64)
	relay.afterRead = func(safe int64, gap events.Gap) {
		select {
		case reads <- relayRead{safe: safe, gap: gap}:
		case <-lifetime.Done():
		}
	}
	t.Cleanup(func() {
		stop()
		testwait.Returns(t, "the cadence relay's generation goroutines (its listener and its run) to return once its lifetime ended", relay.wait)
	})
	return relay, fake, horizon, reads
}

// wakePending reports whether a notification waits for the relay's running generation (its wake
// signal is set), which the generation answers with a read next.
func wakePending(relay *eventStreamRelay) bool {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.generation != nil && len(relay.generation.wake) > 0
}

// nextCadenceRead takes the relay's next read, moving fake past the coalescing window when the
// relay arms one before it: a notification's read waits that window unless the read before it
// found a gap held. A try with no read reads the fake's moment and the moments armed on it.
func nextCadenceRead(t *testing.T, fake *clocktest.Fake, coalesce time.Duration, reads <-chan relayRead, what string) relayRead {
	t.Helper()
	var taken relayRead
	testwait.Until(t, what, func() (string, bool) {
		select {
		case taken = <-reads:
			return fmt.Sprintf("%+v", taken), true
		default:
		}
		armed := fake.Armed()
		if window := fake.Now().Add(coalesce); slices.ContainsFunc(armed, window.Equal) {
			fake.Set(window)
			return fmt.Sprintf("no read; moved the fake to the coalescing window at %s", window.Format(time.RFC3339Nano)), false
		}
		return fmt.Sprintf("no read; the fake at %s holds %v armed", fake.Now().Format(time.RFC3339Nano), armed), false
	})
	return taken
}

// settleCadenceRelay takes the reads of the relay's start, moving fake past each coalescing window
// it arms, until a read leaves no immediate re-read and no notification is pending, and returns
// that read. The relay's start and the catch-up its listener's LISTEN wakes are over then.
func settleCadenceRelay(t *testing.T, relay *eventStreamRelay, fake *clocktest.Fake, coalesce time.Duration, reads <-chan relayRead) relayRead {
	t.Helper()
	var last relayRead
	taken := 0
	testwait.Until(t, "the relay to come to rest after its start", func() (string, bool) {
		select {
		case last = <-reads:
			taken++
			rest := last.gap != events.GapUnproven && !wakePending(relay) && len(reads) == 0
			return fmt.Sprintf("%d reads, the last leaving gap %d", taken, last.gap), rest
		default:
		}
		if window := fake.Now().Add(coalesce); slices.ContainsFunc(fake.Armed(), window.Equal) {
			fake.Set(window)
		}
		return fmt.Sprintf("%d reads, waiting for the next", taken), false
	})
	return last
}

// requireCadenceRelayAtRest fails the test when the relay has a read due after last, the read it
// reported last: an immediate re-read (last left a gap unproven), a notification pending, or a
// timer armed on fake short of its hour's fallback. Its generation reads only when one of those
// comes due (eventStreamRelay.run), so a relay with none observes nothing until a notification.
func requireCadenceRelayAtRest(t *testing.T, relay *eventStreamRelay, fake *clocktest.Fake, last relayRead, what string) {
	t.Helper()
	if last.gap == events.GapUnproven {
		t.Fatalf("%s re-reads at once: its last read left gap %d", what, last.gap)
	}
	if wakePending(relay) {
		t.Fatalf("%s has a notification pending", what)
	}
	for _, at := range fake.Armed() {
		if at.Sub(fake.Now()) < time.Hour {
			t.Fatalf("%s has a timer armed for %s, %s from now, want none before its hour's fallback", what, at, at.Sub(fake.Now()))
		}
	}
}

// cadenceHorizonDB observes through the real pool, so the relay starts and reads as it does in
// production. Once armed, each observation that sees an id at or above from has its transaction id
// and the oldest event writer its read found replaced by the next scripted observation's, so the
// test decides which observation proves which.
type cadenceHorizonDB struct {
	events.HorizonDB

	mu     sync.Mutex
	from   int64
	script []cadenceObservation
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

func (db *cadenceHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
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
	row.results.scripted = &next
	*dest[2].(*string) = strconv.FormatUint(next.xid, 10)
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

// requireRelayPublishes requires the relay to publish exactly ids, in order.
func requireRelayPublishes(t *testing.T, subscription <-chan relayedEvent, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if event := testwait.Receive(t, fmt.Sprintf("the relay to publish event %d (of %v)", id, ids), subscription); event.Event.ID != id {
			t.Fatalf("the relay published event %d, want %d (of %v); 0 is its subscription's end", event.Event.ID, id, ids)
		}
	}
}
