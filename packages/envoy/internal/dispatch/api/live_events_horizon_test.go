package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// awaitRelayStart returns once a subscription has started relay's first generation and that start
// has read the log (eventStreamRelay.startsRead: firstRelayCursor's floor mark, newest id and
// gaps). An event committed after awaitRelayStart therefore commits after that read, and so stands
// above the cursor the start answers, which is what the tests of that cursor turn on; a sleep
// guessed at the read's length let an event commit first on a machine slower than the guess. Every
// caller holds the horizon back (a transaction open elsewhere on the instance, or an event id still
// open in this database), so the start is waiting on Horizon.Await by then and cannot finish until
// the caller lets it; the count reads the same if the start has finished since.
func awaitRelayStart(t *testing.T, relay *eventStreamRelay) {
	t.Helper()
	testwait.Until(t, "the relay's first start to read the log", func() (int, bool) {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		return relay.startsRead, relay.startsRead > 0
	})
}

// relayReads holds back the reads of a relay's generations and counts the ones under way, through
// the relay's own hook (eventStreamRelay.aroundRead): a test orders a reset of the event log
// against what the relay is doing, rather than against a sleep guessed at how long it takes.
type relayReads struct {
	mu sync.Mutex
	// held is open while reads are held back, and nil while they are let through.
	held     chan struct{}
	underWay int
}

// holdRelayReads installs relay's read hook, which must happen before any subscription starts a
// generation, and lets every read through until hold. The test's end lets them through again, so
// no read the server's shutdown waits for stays held.
func holdRelayReads(t *testing.T, relay *eventStreamRelay) *relayReads {
	t.Helper()
	reads := &relayReads{}
	relay.aroundRead = func() func() {
		reads.mu.Lock()
		for held := reads.held; held != nil; held = reads.held {
			reads.mu.Unlock()
			<-held
			reads.mu.Lock()
		}
		reads.underWay++
		reads.mu.Unlock()
		return func() {
			reads.mu.Lock()
			reads.underWay--
			reads.mu.Unlock()
		}
	}
	t.Cleanup(reads.release)
	return reads
}

// hold holds back every read that begins from now on, and returns once every read already under
// way has ended: from then until release, the relay reads nothing.
func (reads *relayReads) hold(t *testing.T) {
	t.Helper()
	reads.mu.Lock()
	reads.held = make(chan struct{})
	reads.mu.Unlock()
	testwait.Until(t, "the relay's reads under way to end", func() (int, bool) {
		reads.mu.Lock()
		defer reads.mu.Unlock()
		return reads.underWay, reads.underWay == 0
	})
}

// release lets the reads held back, and every read after them, through.
func (reads *relayReads) release() {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if reads.held != nil {
		close(reads.held)
		reads.held = nil
	}
}

// holdEventWriterTransactionID opens a transaction in Dispatch's own database and takes a
// transaction id, as every write of the event log holds one from before it draws its id
// (events.Broker.Append). While it is open, no mark taken now is older than every transaction id
// held in this database, so the relay's first start reads the log's ids from the boot mark's floor
// (firstRelayFloor), and no horizon is proven either. It rolls back when release runs or when the
// test ends.
func holdEventWriterTransactionID(t *testing.T, database *store.Store) (release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that holds an event writer's id: %v", err)
	}
	var xid string
	if err := tx.QueryRow(t.Context(), `select pg_current_xact_id()::text`).Scan(&xid); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("take a transaction id in Dispatch's own database: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// liveHorizonHeartbeat is the heartbeat of these tests' servers: a socket that waits on the horizon
// sends one every second (awaitHeartbeat reads it as the sign the socket is open and owed nothing).
// Their sockets set no deadline for the test client's pong (liveNoPongDeadline): a socket waits two
// heartbeats for its page's pong in production (serveLiveSocket), which these tests leave as it is,
// but the test client answers each ping on a goroutine a loaded machine can put off past any number
// of heartbeats, and the socket then ends with no close frame (1006) for the machine's speed
// (dispatch://INFRA-3904). What the pong deadline guarded for these rows, a read loop that keeps
// reading while the horizon is held, the stays-alive row checks by a frame the read loop must
// answer (awaitReadLoopAnswers).
const liveHorizonHeartbeat = time.Second

// nextSocketArrival is client's next frame, or the end of its socket, waited for until the run's
// deadline (testwait.Receive): it gives up 30 s before the test binary's deadline, naming what, not
// at a span a loaded machine could outlast.
func nextSocketArrival(t *testing.T, client *socketClient, what string) socketArrival {
	t.Helper()
	next := make(chan socketArrival, 1)
	go func() {
		select {
		case data := <-client.frames:
			next <- socketArrival{data: data}
		case <-client.ended:
			// The reader queues every message before it ends.
			select {
			case data := <-client.frames:
				next <- socketArrival{data: data}
			default:
				next <- socketArrival{ended: true, err: client.err}
			}
		}
	}()
	return testwait.Receive(t, what, next)
}

// awaitHeartbeat waits for client's next frame, which must be a heartbeat: the socket is open and
// alive, and nothing of the subscription has arrived, since one still waiting for its relay
// subscription is owed nothing yet, its events_start included. The heartbeat is the server's own
// work, so the wait for it is the run's (nextSocketArrival).
func awaitHeartbeat(t *testing.T, client *socketClient, while string) {
	t.Helper()
	arrived := nextSocketArrival(t, client, "a heartbeat "+while)
	if arrived.ended {
		t.Fatalf("the socket ended (%v) waiting for a heartbeat %s", arrived.err, while)
	}
	if _, beat := decodeSocketFrame(t, arrived.data).(LiveHeartbeat); !beat {
		t.Fatalf("the socket sent %.200s %s, want nothing but its heartbeat", arrived.data, while)
	}
}

// awaitReadLoopAnswers sends client's socket a subscribe of a view kind no socket serves, which its
// read loop refuses itself, at once (INVALID_FRAME), and waits for that refusal, letting heartbeats
// through: the read loop is reading. A socket whose read loop a subscription's start held, waiting
// on the horizon, reads no pong either, and loses its page once the hold outlasts two heartbeats;
// it sends no refusal until the hold ends, and the wait for it, the run's (nextSocketArrival),
// fails by name. Any other frame fails at once: nothing else is owed while the horizon is held.
func awaitReadLoopAnswers(t *testing.T, client *socketClient, while string) {
	t.Helper()
	const probe = "read-loop-probe"
	client.sendText(t, []byte(`{"type":"subscribe","sub":"`+probe+`","view":{"kind":"forecast"}}`))
	for {
		arrived := nextSocketArrival(t, client, "the read loop's refusal of a view no socket serves "+while)
		if arrived.ended {
			t.Fatalf("the socket ended (%v) waiting for its read loop's refusal %s", arrived.err, while)
		}
		switch frame := decodeSocketFrame(t, arrived.data).(type) {
		case LiveHeartbeat:
			continue
		case LiveError:
			if frame.Sub != probe || frame.Code != "INVALID_FRAME" {
				t.Fatalf("the socket sent %.200s %s, want the read loop's INVALID_FRAME for %s", arrived.data, while, probe)
			}
			return
		default:
			t.Fatalf("the socket sent %.200s %s, want the read loop's INVALID_FRAME for %s", arrived.data, while, probe)
		}
	}
}

// expectNoEventAbove fails the test when sub is sent an event above floor within hold, or its
// socket ends; sub's events at or below floor, and the socket's heartbeats, are let through.
func expectNoEventAbove(t *testing.T, client *socketClient, sub string, floor int64, hold time.Duration, while string) {
	t.Helper()
	deadline := time.After(hold)
	for {
		select {
		case data := <-client.frames:
			switch frame := decodeSocketFrame(t, data).(type) {
			case LiveHeartbeat:
			case LiveEventFrame:
				if id := mustEventFrameID(t, frame); frame.Sub != sub || id > floor {
					t.Fatalf("%s was sent event %d %s, want nothing above %d", sub, id, while, floor)
				}
			default:
				t.Fatalf("%s was sent %+v %s, want nothing above event %d", sub, frame, while, floor)
			}
		case <-client.ended:
			t.Fatalf("the socket ended (%v) %s", client.err, while)
		case <-deadline:
			return
		}
	}
}

// expectEventsInOrder reads sub's events until every id in want has arrived, in want's order, and
// fails on an id at or below one already sent (a repeat, or ids out of order), on an id above the
// next wanted one before it (the wanted one skipped), and on a subscription that ends first or
// whose next event does not come before the horizon wait ends (eventFeedFrame: for every caller in
// this file, each of which calls horizontest.UntilTheDeadline, that is the drain before the run's
// deadline or a hold of this database past OwnHoldBound; callers in other files keep the wait's
// bounds). Ids want does not name are let through: a document settling beside the test's own writes
// appends events of its own.
func expectEventsInOrder(t *testing.T, client *socketClient, sub string, want ...int64) {
	t.Helper()
	var last int64
	for next := 0; next < len(want); {
		id := mustEventFrameID(t, eventFeedEvent(t, client, sub))
		switch {
		case id <= last:
			t.Fatalf("%s was sent event %d after %d: a repeat, or ids out of order", sub, id, last)
		case id == want[next]:
			next++
		case id > want[next]:
			t.Fatalf("%s was sent event %d before event %d, which it skipped", sub, id, want[next])
		}
		last = id
	}
}

// TestAColdSubscriptionIsSentAnIdStillOpenWhenItSubscribed: a cold subscription asks for no
// backlog, but it is owed every event that commits after it began, and a lower id still open when
// it subscribed is one of them. tx A appends on issue X (id a) and stays open, tx B appends on
// issue Y (id b > a) and commits, and the subscription starts. Started at the log's newest id (b),
// the live loop drops a when A commits, for good, since a is at or below that start. Started at the
// relay's published cursor, which never passes an id still open, it is sent a and then b.
func TestAColdSubscriptionIsSentAnIdStillOpenWhenItSubscribed(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{liveNoPongDeadline: true}, nil)
	first := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	second := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")
	// This subscription starts the relay before A opens: a relay starting while A is open waits for
	// A to end before it answers its first subscriber.
	subscribeEvents(t, rig.dial(t, withBearer("agent-token")), "starter", nil)

	low, high, commitLow := appendOutOfOrder(t, rig.database, rig.served.deps.Events, first.Key, second.Key)
	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "cold", nil); start >= low {
		t.Fatalf("a cold subscription starts after event %d, at or above the %d still open when it subscribed", start, low)
	}
	// Long enough for the relay to read b's commit while A is still open.
	expectNoEventAbove(t, client, "cold", low-1, time.Second, "while the lower id is still open")

	commitLow()
	expectEventsInOrder(t, client, "cold", low, high)
}

// TestAnEventsSubscriptionStaysAliveWhileTheHorizonIsHeld: the first subscription on a fresh
// process starts the relay, whose first cursor waits for a horizon proven after the call, and so
// for every transaction that holds a transaction id anywhere on the Postgres instance: another
// database's job, another package's test. A subscription's events_start waits with it -- it names
// the point the relay hands over at -- but the page's socket is its own: it stays open and
// heartbeats while that wait lasts, and its read loop keeps reading, so its page's pongs keep it,
// as does one made meanwhile, and no page is dropped for a transaction it has nothing to do with.
// Once the holder ends, each is sent its events_start, whose replay is complete for a since of the
// log as it stands, and the event committed during the hold.
func TestAnEventsSubscriptionStaysAliveWhileTheHorizonIsHeld(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	since := eventHead(t, rig.database)

	release := holdClusterTransactionID(t)
	first := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, first, "events", &since)
	awaitHeartbeat(t, first, "while the horizon is held")
	second := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, second, "events", &since)
	awaitHeartbeat(t, second, "while the first subscription waits on the held horizon")
	awaitReadLoopAnswers(t, first, "while the horizon is held")
	awaitReadLoopAnswers(t, second, "while the horizon is held")

	renameIssue(t, rig.handler, issue.Key, "Renamed while the horizon is held")
	committed := eventHead(t, rig.database)
	release()

	for name, client := range map[string]*socketClient{"the first": first, "the one made while it waited": second} {
		start := eventsStart(t, client, "events")
		if start.Since != strconv.FormatInt(since, 10) || !start.ReplayComplete {
			t.Errorf("%s subscription's events_start = %+v, want a complete replay from %d", name, start, since)
			continue
		}
		expectEventsInOrder(t, client, "events", committed)
	}
}

// TestAnEventsSubscriptionOnARelayStillOnAReplacedLogWaitsForItsReset: after the log is reset
// (e2e/seed.ts truncates it between browser scenarios) the relay keeps the old log's cursor until
// its reset check has run, on the next read a notification or a subscriber wakes, so a
// subscription can take its relay subscription in between, on a published cursor above the new
// log's newest id. Served from there it would drop the new log's events up to that id, and then the
// relay's reset would end it, sending the page through a resubscribe and the whole refresh a gap
// owes. So it waits for that reset and subscribes again, with its socket open and heartbeating
// meanwhile, while the next generation's first cursor waits for a horizon proven after the reset: a
// cold subscription starts then, and one resuming from an id of the new log is replayed every event
// after it. Either way the new log's events reach the page on the subscription it already has.
//
// The new log can also have climbed past the old cursor by then, as the browser suite's next
// scenario does while it seeds: the cursor then stands at or below the new log's newest id, on an
// id whose row carries another created_at than the relay read. Started there, the subscription
// would be sent its events_start and then be ended by the relay's reset, which the page answers by
// subscribing again on the same socket. It waits for that reset too.
func TestAnEventsSubscriptionOnARelayStillOnAReplacedLogWaitsForItsReset(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	for _, subscription := range []struct {
		name      string
		since     *int64
		climbPast bool
	}{
		{name: "cold", since: nil},
		{name: "resuming from the new log's start", since: new(int64(0))},
		{name: "cold, the new log past the old cursor", since: nil, climbPast: true},
	} {
		t.Run(subscription.name, func(t *testing.T) { replacedLogConnection(t, subscription.since, subscription.climbPast) })
	}
}

func replacedLogConnection(t *testing.T, since *int64, climbPast bool) {
	t.Helper()
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	reads := holdRelayReads(t, rig.served.eventRelay)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	// The first subscription starts the relay; once it has been sent an event, the relay's cursor
	// stands on the old log's newest id and its start-up read is behind it.
	first := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, first, "events", nil)
	renameIssue(t, rig.handler, issue.Key, "Renamed before the reset")
	old := eventHead(t, rig.database)
	expectEventsInOrder(t, first, "events", old)

	// The relay's reset check is the first thing each of its reads does, a read of the log that
	// waits on nothing, so any read landing after the truncate below resets the relay at once: one
	// for a notification it has not answered yet (its LISTEN's, or the rename's when that came
	// during a read), or the one the refill's own commit wakes. Either would reset it before the
	// subscription this test is about finds it on the replaced log, and the test would prove
	// nothing. So every read is held back from here, once those under way have ended, until that
	// subscription has taken its relay subscription and been sent its heartbeat, the relay still
	// on the replaced log.
	reads.hold(t)

	// Once the reads go on and the relay resets, the next generation's first cursor waits on this
	// held horizon, the subscription's socket heartbeating meanwhile, until release.
	release := holdClusterTransactionID(t)
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed after the reset")
	afterReset := eventHead(t, rig.database)
	for i := 0; climbPast && afterReset <= old; i++ {
		renameIssue(t, rig.handler, issue.Key, fmt.Sprintf("Renamed past the old cursor %d", i))
		afterReset = eventHead(t, rig.database)
	}
	if !climbPast && afterReset >= old {
		t.Fatalf("the new log's head %d is not below the old log's %d: the relay's cursor would not stand above it, so this test proves nothing", afterReset, old)
	}

	second := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, second, "events", since)
	testwait.Until(t, "the subscription to take its relay subscription on the generation still on the replaced log", func() (int, bool) {
		subscribers := relaySubscribers(rig.served.eventRelay)
		return subscribers, subscribers == 2
	})
	awaitHeartbeat(t, second, "while the relay still stood on the replaced log")
	reads.release()
	release()
	if start := eventsStartID(t, second, "events"); start > afterReset {
		t.Fatalf("the subscription starts after event %d, above the new log's newest id %d: it was served from the replaced log's cursor", start, afterReset)
	}
	if since != nil {
		expectEventsInOrder(t, second, "events", afterReset)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed once the relay has reset")
	expectEventsInOrder(t, second, "events", eventHead(t, rig.database))
}

// TestAColdSubscriptionThatStartsTheRelayIsSentAnEventCommittedWhileItStarts: the first
// subscription of a process starts the relay, whose first cursor is that subscription's start
// point. Here a horizon is already proven (by a since read of the references graph), so the start
// reads the log's gaps only above it, and an event committed before the subscribe but not yet
// proven makes the start wait for every transaction open on the instance. The first cursor is the
// log's newest id as the start read it, so an event that commits during that wait stands above it
// and reaches the subscription; a cursor taken under the horizon proven after the wait would cover
// it instead.
func TestAColdSubscriptionThatStartsTheRelayIsSentAnEventCommittedWhileItStarts(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	// A since read answers a mention only once the horizon is proven past its event, so the
	// mention's answer is the proof this test starts from.
	other := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")
	if response := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+other.Key+"/messages", map[string]any{
		"body": "See dispatch://" + issue.Key, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create the mentioning message: status=%d body=%s", response.Code, response.Body.String())
	}
	awaitSinceRead(t, rig.database, "since 0 ?to= dispatch://"+issue.Key+" for mentions, wanting the message's mention",
		func() model.GraphReferences {
			return graphEdges(t, rig.handler, url.Values{"to": {"dispatch://" + issue.Key}, "since": {"0"}, "kind": {"mentions"}})
		},
		func(read model.GraphReferences) bool { return len(read.Edges) == 1 })

	release := holdClusterTransactionID(t)
	// An event committed before the subscribe but above the proven horizon, which the hold keeps
	// from being proven: a first cursor awaited after it must wait for the hold to end.
	renameIssue(t, rig.handler, issue.Key, "Renamed before the subscribe")

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// TestTheFirstColdSubscriptionOfAFreshProcessIsSentAnEventCommittedWhileTheRelayStarts: the same
// start as the test above, on a process where no reader has proven a horizon yet, which is every
// process at boot. The delivery hub proves none at boot: it runs nothing until its first subscriber
// (newDeliveryHub). A page's first subscription carries no since and runs no refresh of its own, so
// an event that commits after the page's own reads and while the relay's first start waits on a
// transaction open elsewhere on the instance must still reach it, or the page never shows it.
func TestTheFirstColdSubscriptionOfAFreshProcessIsSentAnEventCommittedWhileTheRelayStarts(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	release := holdClusterTransactionID(t)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// TestTheFirstColdSubscriptionIsSentAnEventCommittedWhileAWriteFromBeforeBootHoldsTheStart: the
// same start, held this time by a write of Dispatch's own database that took its transaction id
// before the boot mark (a restart under load). The first start waits for a mark that counts, which
// none does until that write ends, and its floor is then the newest such mark, taken after the
// write: an event that committed after the subscription began and before the write ended stands
// at or below that floor, and so below the cursor the subscription starts at, and must still reach
// the subscription.
func TestTheFirstColdSubscriptionIsSentAnEventCommittedWhileAWriteFromBeforeBootHoldsTheStart(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	var release func()
	// persistence runs on the test's database before register takes the boot mark.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true, persistence: func(database *store.Store) docs.VersionedStore {
		release = holdEventWriterTransactionID(t, database)
		return nil
	}}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// The whole-log rows count how many rows of events the relay's first start read, by a sequential
// scan of the table or through its primary key, whichever plan Postgres chose: the table statistics'
// count after the start, less the count before it. A backend adds what it read to those counters
// only when it flushes its statistics: at most once a second while it idles, and when it exits,
// before it gives up its process slot (the statistics' shutdown hook runs ahead of the slot's
// release). The rows used to sleep 1.5 s and give each backend's exit 5 s, unchecked, so a backend
// the machine was slow to run left its reads uncounted, and a start that read the whole log passed
// (dispatch://INFRA-3904).

// eventsTuplesBefore is the count before the start: what the statistics hold now, read on a
// connection of its own. It waits for no flush and ends no backend, so the server's pooled
// connections stay alive. A read the test's own requests made before it and no backend has flushed
// yet is counted by eventsTuplesAfter instead: that only adds, and none of those requests reads the
// whole log (ANALYZE counts no rows read), so it cannot pass a start that did.
func eventsTuplesBefore(t *testing.T, database *store.Store) int64 {
	t.Helper()
	conn := statisticsConn(t, database)
	return eventsTuplesCounted(t, conn)
}

// eventsTuplesAfter is the count after the start, every backend's reads included. It ends every
// other client backend of this database and waits until none of them holds a process slot or a
// pg_stat_activity row any more: pg_terminate_backend answers false for a pid no backend holds, and
// a backend whose slot is gone has run its statistics' shutdown hook. A backend inside a writing
// transaction is ended too, so none of its reads goes uncounted; each caller releases its own
// writer before the count. It reads the counters on a connection of its own, which has read no
// events.
//
// Every pooled connection to those backends is dead from then on, and the servers' shutdown still
// uses their pools. So database must come from pingingStore, whose pools, and every pool opened
// from their configuration (the server's separate pools, a second process's), ping each connection
// before handing it out and dial again past a dead one; it refuses one that does not.
func eventsTuplesAfter(t *testing.T, database *store.Store) int64 {
	t.Helper()
	if database.Pool.Config().ShouldPing == nil {
		t.Fatal("eventsTuplesAfter ends the database's backends, so its store must come from pingingStore, whose pools hand out no connection those ends left dead")
	}
	conn := statisticsConn(t, database)
	ctx, cancel := untilDrain(t)
	defer cancel()
	var others []int32
	if err := conn.QueryRow(ctx, `
		select coalesce(array_agg(pid), '{}') from pg_stat_activity
		where datname = current_database() and backend_type = 'client backend' and pid <> pg_backend_pid()
	`).Scan(&others); err != nil {
		t.Fatalf("list the other connections of the database: %v", err)
	}
	testwait.Until(t, "every other client backend of the database to exit, which flushes its statistics", func() (int, bool) {
		// pg_terminate_backend signals each pid a backend still holds, and answers true for it;
		// pg_stat_activity still lists one whose signal failed for another reason.
		var left int
		if err := conn.QueryRow(ctx, `
			select count(*) from unnest($1::int[]) as p
			where pg_terminate_backend(p) or p in (select pid from pg_stat_activity)
		`, others).Scan(&left); err != nil {
			t.Fatalf("end the other connections so they flush their statistics: %v", err)
		}
		return left, left == 0
	})
	return eventsTuplesCounted(t, conn)
}

// pingingStore is a store on a database of the test's own whose pool pings every connection before
// handing it out, where pgxpool pings only one idle more than a second (pgxpool.Config.ShouldPing's
// default), and destroys one that does not answer and takes or dials another. The pools opened from
// its configuration (store.Pool's separate pools, storetest.SecondProcess) inherit the rule. A
// test whose count ends the database's backends (eventsTuplesAfter) builds its servers on it, so
// what they do after the count, their shutdown's settlement reads included, gets only live
// connections.
func pingingStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Reopened(t, storetest.Open(t), func(config *pgxpool.Config) {
		config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	})
}

// statisticsConn is a connection of the test's own to database, closed when the test ends.
func statisticsConn(t *testing.T, database *store.Store) *pgx.Conn {
	t.Helper()
	ctx, cancel := untilDrain(t)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect to read the events table's statistics: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// eventsTuplesCounted reads the statistics' count of rows of events read, on conn.
func eventsTuplesCounted(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	ctx, cancel := untilDrain(t)
	defer cancel()
	var read int64
	if err := conn.QueryRow(ctx, `
		select coalesce((select seq_tup_read from pg_stat_user_tables where relname = 'events'), 0)
			+ coalesce((select idx_tup_read from pg_stat_user_indexes where indexrelname = 'events_pkey'), 0)
	`).Scan(&read); err != nil {
		t.Fatalf("read the events table's statistics: %v", err)
	}
	return read
}

// untilDrain is t's context, ended 30 s before the test binary's deadline (the margin testwait and
// horizontest give up at), so a statement that stops answering fails the test by name rather than
// holding it to the package's timeout.
func untilDrain(t *testing.T) (context.Context, context.CancelFunc) {
	if deadline, ok := t.Deadline(); ok {
		return context.WithDeadline(t.Context(), deadline.Add(-horizontest.Drain))
	}
	return context.WithCancel(t.Context())
}

// TestTheRelaysFirstStartDoesNotReadTheWholeEventLog: the relay's first start runs on every process
// at boot (nothing proves a horizon before the first subscription) and holds that subscription's
// events_start, and every live event, until it ends. Its cost must not grow with the log: on
// production's millions of events a read of every id from 0 keeps the first subscription and every
// resuming one's events waiting for seconds after each restart (5,000,000 ids: 3.8 s on a 2-vCPU
// pod, against 0.015 s before this change). The start may read the newest events and the ones still
// open, not the log's whole history: here it may read at most a tenth of a log of 200,000
// committed, published events, measured by the rows of events Postgres counts as read, by a
// sequential scan or through the primary key, while the first cold subscription starts.
func TestTheRelaysFirstStartDoesNotReadTheWholeEventLog(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	const logSize = 200_000
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true, store: pingingStore(t)}, nil)
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	before := eventsTuplesBefore(t, rig.database)
	subscribeEvents(t, rig.dial(t, withBearer("agent-token")), "events", nil)
	if read := eventsTuplesAfter(t, rig.database) - before; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events, want at most %d: its cost grows with the whole log", read, logSize, logSize/10)
	}
}

// TestTheRelaysFirstStartDoesNotReadTheWholeEventLogWhileAWriterFromBeforeBootIsOpen: the same
// bound when a write transaction in Dispatch's database took its transaction id before the
// server's boot mark and is still open when the first subscription starts. That is a restart
// under load: write transactions hold their ids for seconds, and pages resubscribe within a
// second of the new process coming up. The start has to wait for that writer anyway, since the
// safe horizon cannot pass it (Horizon.Await), and a mark taken once it has ended bounds the read
// as well as the boot mark would have, so the writer is no reason to read every id from 0. On
// 5,000,000 ids with writers holding their ids for 3 s, that read made the first subscription wait
// 6.0 s after a restart, against 3.1 s when the boot mark held.
func TestTheRelaysFirstStartDoesNotReadTheWholeEventLogWhileAWriterFromBeforeBootIsOpen(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	const logSize = 200_000
	var release func()
	// persistence runs on the test's database before register takes the boot mark: the writer's
	// transaction id is older than the mark, as a write open across a restart's boot is.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true, store: pingingStore(t), persistence: func(database *store.Store) docs.VersionedStore {
		release = holdEventWriterTransactionID(t, database)
		return nil
	}}, nil)
	testwait.Receive(t, "the server's boot mark", rig.served.bootMark)
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	before := eventsTuplesBefore(t, rig.database)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	// The writer ends a second into the start, as a write holding its id for seconds does.
	time.Sleep(time.Second)
	release()
	eventsStartID(t, client, "events")
	if read := eventsTuplesAfter(t, rig.database) - before; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events while a writer from before the boot mark was open, want at most %d: it read the whole log", read, logSize, logSize/10)
	}
}

// TestTheFirstColdSubscriptionIsSentAnEventThatDrewItsIdBeforeIt: an event can draw its id before
// the first subscription and commit after it, below an id that had already committed. The relay's
// first start must not start above it: it reads the log's gaps when it starts, and once every id is
// final starts below the lowest one that has filled, so that event reaches the subscription, first.
func TestTheFirstColdSubscriptionIsSentAnEventThatDrewItsIdBeforeIt(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")

	low, _, commitLow := appendOutOfOrder(t, rig.database, rig.served.deps.Events, issue.Key, other.Key)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	// The start's gap read finds low missing and then waits on it: low's own transaction holds a
	// transaction id in this database, so no horizon is proven while it is open.
	awaitRelayStart(t, rig.served.eventRelay)
	commitLow()

	eventsStartID(t, client, "events")
	if id := mustEventFrameID(t, eventFeedEvent(t, client, "events")); id != low {
		t.Fatalf("the first cold subscription was sent event %d first, want %d, which committed after it subscribed", id, low)
	}
}

// TestTheFirstColdSubscriptionSkipsAnIdThatRolledBack: the relay's first start reads the gaps among
// the log's ids, since an event that commits after the first subscription can stand in one, and
// starts below the lowest gap that has filled once every id is final. An id whose transaction
// rolled back leaves a gap that never fills, and must move the cursor nowhere: taken for a late
// event, it would send the subscription every event above it, the backlog a cold subscription asked
// not to get.
//
// The gap must be one the start reads, so a write of this database holds a transaction id from
// before the start's own mark, which is what has the start read every id above the boot mark's
// floor rather than only above the log's newest id (firstRelayFloor). With no write open, no event
// id can still be open, the floor stands at the newest id and the start reads no gap at all.
func TestTheFirstColdSubscriptionSkipsAnIdThatRolledBack(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	// An id drawn and never written, as a rolled-back write leaves one, then an event above it.
	if _, err := rig.database.Pool.Exec(t.Context(), `select nextval('events_id_seq')`); err != nil {
		t.Fatalf("draw an event id: %v", err)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed above the gap")
	backlog := eventHead(t, rig.database)
	release := holdEventWriterTransactionID(t, rig.database)

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	release()
	if start := eventsStartID(t, client, "events"); start != backlog {
		t.Fatalf("the first cold subscription starts after event %d, want the log's newest id %d: the gap that rolled back moved its cursor", start, backlog)
	}

	renameIssue(t, rig.handler, issue.Key, "Renamed after the subscribe")
	live := eventHead(t, rig.database)
	for id := int64(0); id != live; {
		id = nextEventID(t, client, "events")
		if id <= backlog {
			t.Fatalf("the cold subscription was sent event %d, backlog at or below the head %d it started at, above an id that rolled back", id, backlog)
		}
	}
}

// TestShutdownEndsAnEventsSubscriptionWaitingOnTheHorizon: a live socket ends when the server's
// lifetime does (1012, as a restart does), and the shutdown waits for the hub and the relay. A
// socket whose events subscription is still waiting for the first horizon, held back by a
// transaction open elsewhere on the instance, ends then too, instead of holding the shutdown until
// that transaction ends. The transaction is released only once the test has ended, so a shutdown
// that waited for it would never close the socket while the test waits: the wait for the close is
// the run's (testwait.Receive), and fails by name, where a span of its own would also fail a correct
// shutdown that a loaded machine got to late.
func TestShutdownEndsAnEventsSubscriptionWaitingOnTheHorizon(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, liveNoPongDeadline: true}, nil)
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	since := eventHead(t, rig.database)

	holdClusterTransactionID(t)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", &since)
	awaitHeartbeat(t, client, "while the horizon is held")

	rig.stop()
	testwait.Receive(t, "the end of the socket of a subscription waiting on the horizon, once the server shuts down", client.ended)
	var closed *gws.CloseError
	if !errors.As(client.err, &closed) || closed.Code != gws.CloseServiceRestart {
		t.Fatalf("the socket of a subscription waiting on the horizon ended with %v, want a close with 1012", client.err)
	}
}

// holdEventInsert opens a transaction that takes its transaction id and inserts one event, as
// events.Broker.Append does, and holds it open: an event insert in flight, holding the lowest id
// not yet final. release commits it; it is rolled back when the test ends otherwise. It answers
// the event's id.
func holdEventInsert(t *testing.T, database *store.Store) (id int64, release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the event insert: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `select pg_current_xact_id()`); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("take the event insert's transaction id: %v", err)
	}
	if err := tx.QueryRow(t.Context(), `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'test.open', '{"kind":"system","id":"test"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("insert the open event: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if err := tx.Commit(context.Background()); err != nil {
				t.Errorf("commit the open event: %v", err)
			}
		})
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return id, release
}

// TestTheRelaysFirstStartReadsAboveThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen: a
// deploy under load restarts the process while an event insert is open, so no mark the new process
// takes counts until that insert ends, and the first start cannot bound its read of the log by
// one. Every event at or below the horizon the process before it proved is final, whichever process
// proved it, so the start reads only above the horizon that process persisted (event_horizon),
// not the whole log, and is still sent the open event and what committed while it waited, in id
// order. Here the log holds 200,000 events and the start may read at most a tenth of them.
func TestTheRelaysFirstStartReadsAboveThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen(t *testing.T) {
	horizontest.UntilTheDeadline(t)
	const logSize = 200_000
	first, database, _ := newTestServer(t, testServerOptions{store: pingingStore(t)})
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	if _, err := database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	// The process before proves and persists a horizon past the fill, as its relay or hub does. The
	// proof is instance-wide, so the wait for it keys on the horizon's own account of itself.
	before := events.NewHorizon(database.Pool)
	before.PersistProven()
	horizontest.AwaitPersisted(t, before, eventHead(t, database), database)

	// An event insert that opened before the next process boots, and outlives its first start.
	open, release := holdEventInsert(t, database)
	read := eventsTuplesBefore(t, database)
	handler, served := secondProcess(t, database)
	served.liveNoPongDeadline = true
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", withBearer("agent-token")).servedBy(database).read()
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, served.eventRelay)
	renameIssue(t, first, issue.Key, "Renamed while the insert is open")
	committed := eventHead(t, database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", open, committed)
	server.Close()
	if read = eventsTuplesAfter(t, database) - read; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events while an event insert from before boot was open, want at most %d: it read the whole log, not above the persisted horizon", read, logSize, logSize/10)
	}
}
