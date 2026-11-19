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
// any the test Postgres has assigned, so a scripted snapshot proves every real observation before
// it and no real snapshot proves a scripted one.
const cadenceXid = uint64(1) << 40

// A relay with nothing to publish observes nothing; an event's notification wakes one read, whose
// observation sees the event but does not prove itself, and the relay observes again at once
// rather than after a delay: with no older transaction id running, that second observation proves
// the first and the event is published. The script's snapshots never prove themselves (cadenceRow),
// so this holds the relay to the cadence whatever the test Postgres was doing. The relay's timers
// run on a fake clock of its own that the test moves, and its fallback is an hour, so only an
// immediate second observation can publish the event: a re-read after a delay waits on a timer
// the fake never reaches. Before the event and once it is published the relay is at rest: it has
// no read due (requireCadenceRelayAtRest).
func TestEventStreamRelayProvesItsOwnObservationAtOnce(t *testing.T) {
	const coalesce = time.Millisecond
	_, database, deps := newTestServer(t, testServerOptions{})
	relay, fake, horizon, reads := newCadenceRelay(t, deps, database, coalesce)
	subscription, _, _ := subscribeRelay(t, relay)
	requireCadenceRelayAtRest(t, relay, fake, settleCadenceRelay(t, relay, fake, coalesce, reads), "an idle relay")

	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 1},     // sees the event: unproven
		cadenceObservation{xmin: cadenceXid + 2, xid: cadenceXid + 2}, // nothing older runs: proves it
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

// A gap an older transaction holds open is closed by that transaction's commit notification: the
// read it wakes skips the coalescing window, since the events it publishes have waited already.
// Here a write holds the lower id open while the higher one commits; the relay's read of the
// higher one, and the immediate observation after it, find the write still running (their
// snapshots' xmin is the write's id), so the gap is held. The write's commit notifies, and the
// read it wakes proves both and publishes the two events in id order. The relay's timers run on a
// fake clock of its own that the test moves, its coalescing window is two seconds and its fallback
// an hour: the read the commit wakes must publish both events without arming the window. The
// scripted proof applies only to an observation taken once the lower id is committed, so the
// script never claims the write ended before it did.
func TestEventStreamRelayClosesAHeldGapOnTheHoldersCommitNotification(t *testing.T) {
	const coalesce = 2 * time.Second
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	waitForQuietEventLog(t, database, 500*time.Millisecond)
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
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 1},                      // the write (cadenceXid) runs: unproven
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 2},                      // it still runs: held
		cadenceObservation{xmin: cadenceXid + 3, xid: cadenceXid + 3, when: committed}, // it has committed: proves both
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

// waitForQuietEventLog waits until the event log's head has stood still for quiet, so no write
// the test's setup made is still to notify. The quiet spell is what the row needs, not a bound;
// the wait has none of its own (testwait.Until).
func waitForQuietEventLog(t *testing.T, database *store.Store, quiet time.Duration) {
	t.Helper()
	head, since := eventHead(t, database), time.Now()
	testwait.Until(t, "the event log's head to stand still for "+quiet.String(), func() (int64, bool) {
		if now := eventHead(t, database); now != head {
			head, since = now, time.Now()
		}
		return head, time.Since(since) >= quiet
	})
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
		relay.wait()
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
// found a gap held.
func nextCadenceRead(t *testing.T, fake *clocktest.Fake, coalesce time.Duration, reads <-chan relayRead, what string) relayRead {
	t.Helper()
	return testwait.Until(t, what, func() (relayRead, bool) {
		select {
		case read := <-reads:
			return read, true
		default:
		}
		if window := fake.Now().Add(coalesce); slices.ContainsFunc(fake.Armed(), window.Equal) {
			fake.Set(window)
		}
		return relayRead{}, false
	})
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
// production. Once armed, each observation that sees an id at or above from has its snapshot and
// transaction id replaced by the next scripted observation, so the test decides which
// observation proves which, whatever else holds a transaction id on the shared test Postgres.
type cadenceHorizonDB struct {
	events.HorizonDB

	mu     sync.Mutex
	from   int64
	script []cadenceObservation
}

// cadenceObservation is one scripted snapshot xmin and transaction id. when, if set, holds it back
// until it answers true: an observation taken before then keeps its real values.
type cadenceObservation struct {
	xmin, xid uint64
	when      func() bool
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
	return cadenceResults{BatchResults: db.HorizonDB.SendBatch(ctx, batch), db: db}
}

// cadenceResults is an observation's results with its one row rewritten (cadenceRow).
type cadenceResults struct {
	pgx.BatchResults
	db *cadenceHorizonDB
}

func (r cadenceResults) QueryRow() pgx.Row {
	return cadenceRow{Row: r.BatchResults.QueryRow(), db: r.db}
}

// cadenceRow scans an observation's A and its created_at, its snapshot's xmin and xmax, whether
// that snapshot's in-progress list is empty and X, and rewrites the snapshot and X with the next
// scripted observation when the script is armed and A is at or above its from. A scripted
// observation never proves itself: its snapshot keeps an id in progress and its xmax is below X,
// so the script alone decides which observation proves which, whatever the test Postgres is doing.
type cadenceRow struct {
	pgx.Row
	db *cadenceHorizonDB
}

func (row cadenceRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	db := row.db
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
	*dest[2].(*string) = strconv.FormatUint(next.xmin, 10)
	*dest[3].(*string) = strconv.FormatUint(next.xid-1, 10)
	*dest[4].(*bool) = false
	*dest[5].(*string) = strconv.FormatUint(next.xid, 10)
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
