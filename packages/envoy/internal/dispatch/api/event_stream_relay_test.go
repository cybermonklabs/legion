package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// secondProcessHandler mounts a second API stack on the database the first one is on, as the
// second envoy-dispatch task of a deployment is: its own pool (storetest.SecondProcess), its own
// broker, its own document service and its own event stream relay. The two share Postgres and
// nothing else, which is the whole point -- an in-process fan-out cannot cross between them.
func secondProcessHandler(t *testing.T, database *store.Store) http.Handler {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown the second process's document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store:       second,
		Identity:    headerIdentity(second),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Docs:        documentService,
		Events:      broker,
		// Its sockets' event feeds are waited on as the first process's are (eventFeedFrame).
		HorizonStallAfter: horizontest.StallWarning,
		HorizonWarnEvery:  horizontest.StallWarning,
	})
	if err != nil {
		t.Fatalf("new API dependencies for the second process: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux
}

// TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed is what the live feed is for with two
// envoy-dispatch processes on one Postgres: a page whose socket landed on process B sees an event
// process A committed, on the subscription it already has. Nothing of A's reaches B but the row
// itself -- B's relay hears the commit on `listen dispatch_events` (migration 0107) and reads it
// back out of the log. The broker fan-out the feed used to read would have ended at A.
func TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed(t *testing.T) {
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	second := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(second.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(second.URL, "http")+"/ws/live", withBearer("agent-token")).servedBy(database).read()
	subscribeEvents(t, client, "events", nil)

	renameIssue(t, first, issue.Key, "Renamed on the other process")
	committed := eventHead(t, database)

	for id := int64(0); id != committed; {
		id = nextEventID(t, client, "events")
		if id > committed {
			t.Fatalf("the feed on the second process sent id %d before the event %d committed on the first", id, committed)
		}
	}
}

// newCountedRelay is a relay of its own on deps' database, on a lifetime the test ends and waits
// out, whose every page read is reported on reads, after it returns, with the id it read after.
func newCountedRelay(t *testing.T, deps Deps) (*eventStreamRelay, chan int64) {
	t.Helper()
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(directServer(deps), lifetime)
	relay.coalesce = time.Millisecond
	reads := make(chan int64, 1024)
	relay.readAfter = func(ctx context.Context, after, through int64) ([]model.Event, error) {
		page, err := relay.server.readEventsAfterID(ctx, everyReader, after, through, maxEventReplay)
		reads <- after
		return page, err
	}
	t.Cleanup(func() {
		stop()
		relay.wait()
	})
	return relay, reads
}

// subscribeRelay subscribes to the relay, cancelled when the test ends if not before.
func subscribeRelay(t *testing.T, relay *eventStreamRelay) (<-chan relayedEvent, func(), int64) {
	t.Helper()
	subscription, err := relay.subscribe(context.Background(), liveEventsBuffer)
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	t.Cleanup(subscription.cancel)
	return subscription.events, subscription.cancel, subscription.head
}

// relayReceipt is one receive from a relay subscription: the event, or open false once it ended.
type relayReceipt struct {
	event relayedEvent
	open  bool
}

// receiveRelayed is the subscription's next event. The relay publishes an event only once the
// event horizon of database's server has passed it, so the wait is the horizon's.
func receiveRelayed(t *testing.T, database *store.Store, subscription <-chan relayedEvent) relayedEvent {
	t.Helper()
	received := make(chan relayReceipt, 1)
	go func() {
		event, open := <-subscription
		received <- relayReceipt{event: event, open: open}
	}()
	receipt := horizontest.WaitInProcess(t, "the relay's next event", received, database)
	if !receipt.open {
		t.Fatal("the relay's subscription ended")
	}
	return receipt.event
}

// receiveRelayedThrough reads the subscription until the event last, failing on an id out of
// order or at or below after.
func receiveRelayedThrough(t *testing.T, database *store.Store, subscription <-chan relayedEvent, after, last int64) {
	t.Helper()
	for previous := after; previous != last; {
		id := receiveRelayed(t, database, subscription).Event.ID
		if id <= previous {
			t.Fatalf("the relay published %d after %d; it publishes each event once, in id order", id, previous)
		}
		previous = id
	}
}

// quietRelayReads drains every page read the relay has made and waits until it has made none for
// 300 ms: a generation's start, its LISTEN catch-up and the re-reads a gap above the horizon
// schedules all read on their own, so a count of the reads one commit costs is taken after them.
func quietRelayReads(t *testing.T, reads <-chan int64) {
	t.Helper()
	deadline := time.Now().Add(eventFeedWait)
	for quiet := time.Now(); time.Since(quiet) < 300*time.Millisecond; {
		if time.Now().After(deadline) {
			t.Fatalf("the relay was still reading the event log after %s", eventFeedWait)
		}
		select {
		case <-reads:
			quiet = time.Now()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// relayPublished is the id the relay's running generation has published through, and 0 while none
// runs.
func relayPublished(relay *eventStreamRelay) int64 {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.generation == nil {
		return 0
	}
	return relay.generation.cursor
}

// recordRelayDials has relay dial its listener connections on deps' pool, as it does by default,
// and records each one (listenerDials), so a test reads the relay's own connections and their
// backends alone. A predicate on a backend's last statement, `listen events`, stops naming the
// relay's connection once its listener has pinged it, one pglisten.DefaultListenIdle after its last
// round trip, which a wait behind a held event horizon outlasts: a count that wants none then reads
// none while the connection stands open, and one that wants it finds nothing to terminate.
func recordRelayDials(relay *eventStreamRelay, deps Deps) *listenerDials {
	dials := &listenerDials{}
	relay.dial = dials.wrap(deps.Store.Pool.Listener)
	return dials
}

// relayBackends is the pid of every backend still connected for a listener connection the relay
// dialed (recordRelayDials), read from pg_stat_activity, so a test can read it while a listener of
// the relay's may still be running: a connection's pid is fixed once its dial returns, where its
// other state is the listener's own.
func relayBackends(t *testing.T, database *store.Store, dials *listenerDials) []int32 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select pid from pg_stat_activity where pid = any($1) order by pid`, dials.pids())
	if err != nil {
		t.Fatalf("read the backends of the relay's listener connections: %v", err)
	}
	connected, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatalf("read the backends of the relay's listener connections: %v", err)
	}
	return connected
}

// waitRelayStopped waits for every goroutine of the relay's generations to return, and so for every
// listener connection it dialed to close (a listener's Run returns only once it has closed its
// connection), failing the test naming a connection still open, and then for each of those
// connections' backends to exit.
func waitRelayStopped(t *testing.T, database *store.Store, relay *eventStreamRelay, dials *listenerDials, when string) {
	t.Helper()
	waitReturned(t, "every goroutine of the relay to return "+when, &relay.running)
	for _, conn := range dials.dialed() {
		// Read after the wait above, which orders each listener's close before this read.
		if !conn.IsClosed() {
			t.Fatalf("the relay's listener connection to backend %d is still open %s", conn.PgConn().PID(), when)
		}
	}
	testwait.Eventually(t, "every backend of the relay's listener connections to exit "+when, func() bool {
		return len(relayBackends(t, database, dials)) == 0
	})
}

// endRelayGeneration ends the relay's running generation, which closes every subscription it
// holds, as a reset of the event log does (resetCursor).
func endRelayGeneration(relay *eventStreamRelay) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.generation != nil {
		relay.endLocked(relay.generation)
	}
}

// relaySubscribers is how many subscriptions the relay's running generation holds, and none while
// no generation runs.
func relaySubscribers(relay *eventStreamRelay) int {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.generation == nil {
		return 0
	}
	return relay.generation.stream.SubscriberCount()
}

// TestEventStreamRelayReadsNothingWithoutASubscriber: a process nobody streams from holds no
// LISTEN connection for the event log and reads none of it, before its first stream and after its
// last one closes. A relay that kept running after its last subscriber left would read the log
// after every commit until the process exited.
func TestEventStreamRelayReadsNothingWithoutASubscriber(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, reads := newCountedRelay(t, deps)
	dials := recordRelayDials(relay, deps)
	commit := func() {
		t.Helper()
		for range 3 {
			seedEvents(t, database, issue, 1, 0)
		}
	}
	idle := func(when string) {
		t.Helper()
		dialed := len(dials.dialed())
		commit()
		if count := len(reads); count != 0 {
			t.Fatalf("the relay read the event log %d times %s", count, when)
		}
		if got := len(dials.dialed()); got != dialed {
			t.Fatalf("the relay dialed %d listener connections %s, want none", got-dialed, when)
		}
		if backends := relayBackends(t, database, dials); len(backends) != 0 {
			t.Fatalf("the relay's listener connections hold backends %v %s, want none", backends, when)
		}
	}

	if dialed := len(dials.dialed()); dialed != 0 {
		t.Fatalf("the relay dialed %d listener connections before anyone subscribed, want none", dialed)
	}
	idle("before anyone subscribed")

	subscription, cancel, head := subscribeRelay(t, relay)
	waitListening(t, database, "the relay's listener holding a LISTEN", dials)
	last := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThrough(t, database, subscription, head, last)
	cancel()
	waitRelayStopped(t, database, relay, dials, "after its last subscriber left")
	for len(reads) > 0 {
		<-reads
	}

	idle("after its last subscriber left")
}

// TestEventStreamRelayReadsEachCommitOnceForEverySubscriber: one commit costs the process one read
// of the event log and one encoding of its frame, however many streams are open. Every subscriber
// receives the same frame bytes.
func TestEventStreamRelayReadsEachCommitOnceForEverySubscriber(t *testing.T) {
	const subscribers = 3
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, reads := newCountedRelay(t, deps)
	dials := recordRelayDials(relay, deps)
	subscriptions := make([]<-chan relayedEvent, subscribers)
	for index := range subscriptions {
		subscriptions[index], _, _ = subscribeRelay(t, relay)
	}
	// The generation's start, its catch-up after its LISTEN and any re-read a gap above the
	// horizon scheduled, none of which this commit paid for.
	waitListening(t, database, "the relay's listener holding a LISTEN", dials)
	quietRelayReads(t, reads)

	committed := seedEvents(t, database, issue, 1, 0)
	frames := make([][]byte, subscribers)
	for index, subscription := range subscriptions {
		event := receiveRelayed(t, database, subscription)
		if event.Event.ID != committed {
			t.Fatalf("subscriber %d received event %d, want the committed %d", index, event.Event.ID, committed)
		}
		frames[index] = event.Encoded
	}
	if count := len(reads); count != 1 {
		t.Fatalf("the relay read the event log %d times for one commit with %d subscribers, want once", count, subscribers)
	}
	for index := 1; index < subscribers; index++ {
		if &frames[index][0] != &frames[0][0] {
			t.Fatalf("subscriber %d received a frame encoded apart from subscriber 0's", index)
		}
	}
}

// TestEventStreamRelaySubscribeRacingTheLastCancelReceivesTheNextEvent: a subscription made while
// the last one is being cancelled either joins the running generation before it stops or starts a
// new one; either way it receives the next commit.
func TestEventStreamRelaySubscribeRacingTheLastCancelReceivesTheNextEvent(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, _ := newCountedRelay(t, deps)
	for range 20 {
		_, cancelFirst, _ := subscribeRelay(t, relay)
		cancelled := make(chan struct{})
		go func() {
			defer close(cancelled)
			cancelFirst()
		}()
		subscription, cancelSecond, head := subscribeRelay(t, relay)
		<-cancelled
		committed := seedEvents(t, database, issue, 1, 0)
		receiveRelayedThrough(t, database, subscription, head, committed)
		cancelSecond()
	}
}

// TestOutOfOrderCommitsOnAnotherProcessReachAnOpenSubscriptionInIDOrder: process A commits two
// events out of id order -- tx A appends on issue X (id a) and stays open while tx B appends on
// issue Y (id b > a) and commits -- and the page's socket is on process B. B's relay hears B's
// commit and must not publish b while a is open: a relay that read `id > cursor` and moved its
// cursor to b would never publish a, and the page would miss it for good. B's relay reads only up
// to its own process's horizon, which A's open transaction holds below a however far the two
// processes are apart, so the subscription on B is sent nothing past a while A is open, and a and
// then b once A commits.
func TestOutOfOrderCommitsOnAnotherProcessReachAnOpenSubscriptionInIDOrder(t *testing.T) {
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, first, "OTHER", "Other issue", "A spec")
	second := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(second.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(second.URL, "http")+"/ws/live", withBearer("agent-token")).servedBy(database).read()
	subscribeEvents(t, client, "events", nil)

	// Appended through a broker of process A's own: B learns of them only from the rows.
	low, high, commitLow := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	expectNoEventAbove(t, client, "events", low-1, time.Second, "on process B while process A's lower id is open")
	commitLow()
	expectEventsInOrder(t, client, "events", low, high)
}

// A gap held open above the safe horizon (an id still open below one that committed) has the relay
// re-read the event log every 10-250 ms until the gap closes, for as long as the transaction
// holding it stays open. That re-read asks only the horizon, on the horizon's own connections, and
// reads the shared pool once the horizon has moved past the generation's cursor, so a gap a long
// transaction holds open takes no connection a request needs. The reset check, a shared-pool
// query, runs on the reads a notification wakes.
func TestARelayGapRereadTakesNoSharedConnectionWhileTheHorizonStandsStill(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	// The relay reads through a pool of its own, so its acquires are its alone.
	deps.Store = storetest.SecondProcess(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	relay, reads := newCountedRelay(t, deps)
	dials := recordRelayDials(relay, deps)
	subscription, _, opened := subscribeRelay(t, relay)
	waitListening(t, database, "the relay's listener holding a LISTEN", dials)
	quietRelayReads(t, reads)

	low, high, commitLow := appendOutOfOrder(t, database, deps.Events, issue.Key, other.Key)
	// The higher id's commit notifies: that read checks for a reset, takes the horizon and leaves
	// the gap open, publishing nothing.
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d while the lower id %d was still open", event.Event.ID, low)
	case <-time.After(time.Second):
	}
	before := acquires(deps.Store)
	time.Sleep(time.Second)
	if got := acquires(deps.Store); got != before {
		t.Fatalf("the relay's gap re-reads took %d shared-pool connections while %d stayed open, want none", got-before, low)
	}
	if len(reads) != 0 {
		t.Fatalf("the relay read the event log %d times while the gap below %d stood open, want none", len(reads), high)
	}

	commitLow()
	receiveRelayedThrough(t, database, subscription, opened, high)
}

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped from every open page's feed.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, _ := newCountedRelay(t, deps)
	var down atomic.Bool
	down.Store(true)
	// The relay's own connections, so the test terminates the one it listens on by its backend's
	// pid (recordRelayDials says why not by its last statement).
	var dials listenerDials
	relay.dial = dials.wrap(func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the listener has no connection")
		}
		return database.Pool.Listener(ctx)
	})
	relay.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	subscription, _, opened := subscribeRelay(t, relay)

	// Committed while the relay has no connection: its notification reaches nobody.
	committed := seedEvents(t, database, issue, 1, 0)
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d with no listening connection", event.Event.ID)
	case <-time.After(100 * time.Millisecond):
	}
	down.Store(false)
	receiveRelayedThrough(t, database, subscription, opened, committed)

	// Every later LISTEN catches up the same way, from the cursor as it stands: a redial that
	// reset it would send every open page the whole log again.
	dialed := dials.dialed()
	if len(dialed) == 0 {
		t.Fatal("the relay published with no listener connection dialed")
	}
	listening := dialed[len(dialed)-1].PgConn().PID()
	var terminated bool
	if err := database.Pool.QueryRow(context.Background(), `select pg_terminate_backend($1)`, int32(listening)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the relay's LISTEN backend, pid %d: terminated %t (%v), want true", listening, terminated, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(dials.dialed()) <= len(dialed) {
		if time.Now().After(deadline) {
			t.Fatal("the relay did not redial after its connection was terminated")
		}
		time.Sleep(5 * time.Millisecond)
	}
	next := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThrough(t, database, subscription, committed, next)
}

// TestEventStreamRelayRecoversWhenTheEventLogIsReset: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the new rows'
// ids restart at 1, below the cursor the relay has published through, and `id > cursor` matches
// nothing. A relay that only ever published forward would fall silent there for good: every live
// update of every later scenario would be lost, with no error anywhere. The relay reads the reset
// through eventCursorCheck and ends its generation with every subscription it holds -- the
// stream's answer to a reader it can no longer serve in order, which ends each open events
// subscription with an ended frame (behind) and sends its page through a resubscribe
// (TestTheEventFeedResumesAfterTheEventLogIsResetBeneathTheReader) -- and the next subscription
// starts a generation on the new log.
func TestEventStreamRelayRecoversWhenTheEventLogIsReset(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, _ := newCountedRelay(t, deps)
	dials := recordRelayDials(relay, deps)
	subscription, _, opened := subscribeRelay(t, relay)

	// A cursor well above the ids the reset log will hand out.
	committed := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThrough(t, database, subscription, opened, committed)

	if _, err := database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	// TRUNCATE notifies nobody: it is the refill's own insert that fires migration 0107's trigger
	// and wakes the relay onto a log that restarts at id 1.
	refilled := seedEvents(t, database, issue, 1, 0)
	if refilled >= committed {
		t.Fatalf("event %d committed after the reset is not below the pre-reset %d: the reset reused no id, so this test proves nothing", refilled, committed)
	}

	// The subscription open across the reset ends without a frame of the new log, whose ids
	// stand for other events than the ones its reader was sent.
	for open := true; open; {
		select {
		case event, more := <-subscription:
			if !more {
				open = false
			} else {
				t.Fatalf("the relay published event %d into a subscription whose log was reset under it", event.Event.ID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the subscription open across the reset was never ended: its reader waits on a stream whose next frame can only lie")
		}
	}
	// The reset ended the generation: its LISTEN connection closes though its subscriber has not
	// cancelled.
	waitRelayStopped(t, database, relay, dials, "after the reset ended its generation")

	// And the next subscription starts on the new log.
	reopened, _, head := subscribeRelay(t, relay)
	if head != refilled {
		t.Fatalf("a subscription after the reset has head %d, want the new log's %d", head, refilled)
	}
	next := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThrough(t, database, reopened, head, next)
}

// TestEventLogResetClearsTheRelaysHorizon: a reset log (`truncate events restart identity`, what
// e2e/seed.ts does between browser scenarios) hands out ids from 1 again, far below the horizon the
// relay read the old log through. The relay forgets that horizon (Horizon.Reset) and takes its new
// cursor under one proven afresh, ends every subscription open across the reset as it always has,
// and serves the new log to the subscriptions made after it.
//
// The refill here commits out of id order on process A -- id a still open, id b > a committed --
// and the page's socket is on process B. Without Horizon.Reset, B's horizon stands at the old log's
// head, above every new id, so its relay takes the new log's b for its cursor at once while a is
// open, and a subscription made after that reset is never sent a when it commits, though it is sent
// the event after it. With the horizon forgotten, the relay's new cursor waits for a to end. A
// subscription made meanwhile finds the relay still on the old cursor, so it hands over only once
// the relay has reset, and is sent a, which committed after it subscribed, below that new cursor.
func TestEventLogResetClearsTheRelaysHorizon(t *testing.T) {
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, first, "OTHER", "Other issue", "A spec")
	second := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(second.Close)
	dial := func() *socketClient {
		t.Helper()
		return openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(second.URL, "http")+"/ws/live", withBearer("agent-token")).servedBy(database).read()
	}

	// B's relay publishes through the old log's head, so its cursor and horizon stand there.
	across := dial()
	subscribeEvents(t, across, "events", nil)
	renameIssue(t, first, issue.Key, "Renamed before the reset")
	head := eventHead(t, database)
	expectEventsInOrder(t, across, "events", head)

	if _, err := database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	low, high, commitLow := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	if high >= head {
		t.Fatalf("event %d committed after the reset is not below the pre-reset head %d: the reset reused no id, so this test proves nothing", high, head)
	}
	// Time for B's relay to hear the refill's commit and find the reset.
	time.Sleep(500 * time.Millisecond)
	// This subscription hands over only once the relay's reset, which waits for a, has run, so it
	// subscribes first and a commits once it has had time to reach the relay.
	waiting := dial()
	sendEventsSubscribe(t, waiting, "events", nil)
	time.Sleep(300 * time.Millisecond)
	commitLow()
	renameIssue(t, first, issue.Key, "Renamed after the reset")

	// The subscription open across the reset ends without an event of the new log, whose ids it has
	// already seen stand for other events.
	for ended := false; !ended; {
		switch frame := across.next(t, eventFeedWait, "the end of the subscription open across the reset").(type) {
		case LiveEventFrame:
			if id := mustEventFrameID(t, frame); id <= head {
				t.Fatalf("the subscription open across the reset was sent event %d of the new log", id)
			}
		case LiveEnded:
			if frame.Sub != "events" || frame.Reason != "behind" {
				t.Fatalf("ended = %+v, want the events subscription's, behind", frame)
			}
			ended = true
		default:
			t.Fatalf("frame = %+v, want the events subscription's end", frame)
		}
	}
	// The subscription made while a was open is sent it first.
	eventsStartID(t, waiting, "events")
	if id := mustEventFrameID(t, eventFeedEvent(t, waiting, "events")); id != low {
		t.Fatalf("the subscription made while %d was open was sent event %d first, want %d, which committed after it subscribed", low, id, low)
	}

	// And a subscription made now is served the new log.
	after := dial()
	subscribeEvents(t, after, "events", nil)
	renameIssue(t, first, issue.Key, "Renamed for the subscription made after the reset")
	expectEventsInOrder(t, after, "events", eventHead(t, database))
}

// mustEventFrameID is the decimal id an event frame names.
func mustEventFrameID(t *testing.T, frame LiveEventFrame) int64 {
	t.Helper()
	var event feedEvent
	if err := json.Unmarshal(frame.Event, &event); err != nil {
		t.Fatalf("decode the event of frame %+v: %v", frame, err)
	}
	return event.ID
}

// TestLiveAskEventFrameCarriesTheReadTimeFields: an ask payload cannot carry opened_event_id or
// referenced_by_count from the write that appended it, so both are attached at read time
// (attachAskEventFields). Sourcing the live feed through the same row read as the replay
// (readEventRows) and the same encoding (encodeLiveEvent) is what makes the frame a page receives
// live and the frame it receives when it resumes the same bytes; a hand-rolled live read would
// hand a page an ask without them, and only a resubscribe would repair it.
func TestLiveAskEventFrameCarriesTheReadTimeFields(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Ask fields", "A spec")
	since := eventHead(t, rig.database)
	live := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, live, "events", new(since))

	created := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which path?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	liveFrame, _ := eventOfType(t, live, "events", "ask.opened")

	// The same event, replayed by a subscription resuming from before it.
	replay := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, replay, "events", new(since))
	replayedFrame, _ := eventOfType(t, replay, "events", "ask.opened")

	if liveFrame.ID != replayedFrame.ID || !bytes.Equal(liveFrame.Event, replayedFrame.Event) {
		t.Fatalf("live ask.opened frame\n\t%s %s\ndiffers from the replayed one\n\t%s %s", liveFrame.ID, liveFrame.Event, replayedFrame.ID, replayedFrame.Event)
	}
	var event struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(liveFrame.Event, &event); err != nil {
		t.Fatalf("decode the live ask.opened event %s: %v", liveFrame.Event, err)
	}
	for _, field := range []string{"opened_event_id", "referenced_by_count"} {
		value, ok := event.Payload[field]
		if !ok || string(value) == "null" {
			t.Fatalf("live ask.opened payload = %s, want a read-time %s", liveFrame.Event, field)
		}
	}
}

// liveEventSeed is one event a test writes directly, oldest first.
type liveEventSeed struct {
	kind    string
	actor   string
	payload string
	at      time.Time
}

// seedLiveEvents writes each event on issue LIVE-1, oldest first, and returns the ids they took.
func seedLiveEvents(t *testing.T, database *store.Store, seeds []liveEventSeed) []int64 {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('LIVE', 'Live', 'alice') on conflict do nothing`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U') on conflict do nothing`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	ids := make([]int64, 0, len(seeds))
	for _, seed := range seeds {
		var id int64
		if err := database.Pool.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select 'LIVE-1', coalesce(max(seq), 0) + 1, $1, $2::jsonb, $3::jsonb, false, $4
			from events where issue_key = 'LIVE-1'
			returning id
		`, seed.kind, seed.actor, seed.payload, seed.at).Scan(&id); err != nil {
			t.Fatalf("insert %s event: %v", seed.kind, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveUser is the actor of the events a test writes.
const liveUser = `{"kind":"user","id":"alice"}`

// eventCursorCheck reads the newest event's id and the time of the event at the cursor; once the
// cursor's row is gone (a reset of the log), cursorAt is nil.
func TestEventCursorCheckReadsTheNewestEventAndTheCursorsRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	maxID, cursorAt, err := eventCursorCheck(ctx, database.Pool, 0)
	if err != nil {
		t.Fatalf("eventCursorCheck on an empty log: %v", err)
	}
	if maxID != 0 || cursorAt != nil {
		t.Fatalf("an empty log = %d, %v; want 0, nil", maxID, cursorAt)
	}

	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	maxID, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck: %v", err)
	}
	if maxID != ids[2] {
		t.Fatalf("newest event = %d, want %d", maxID, ids[2])
	}
	if cursorAt == nil || !cursorAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("the cursor's row time = %v, want %v", cursorAt, base.Add(time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `delete from events where id = $1`, ids[1]); err != nil {
		t.Fatalf("delete the cursor's row: %v", err)
	}
	maxID, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck once the cursor's row is gone: %v", err)
	}
	if maxID != ids[2] || cursorAt != nil {
		t.Fatalf("with the cursor's row gone = %d, %v; want %d, nil", maxID, cursorAt, ids[2])
	}

	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	reset := seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	maxID, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[2])
	if err != nil {
		t.Fatalf("eventCursorCheck after a reset: %v", err)
	}
	if maxID != reset[0] || maxID >= ids[2] || cursorAt != nil {
		t.Fatalf("after a reset = %d, %v; want the new newest id %d below the cursor %d, and nil", maxID, cursorAt, reset[0], ids[2])
	}
}

// cursorCheck is one answer of eventCursorCheck.
type cursorCheck struct {
	maxID    int64
	cursorAt *time.Time
}

func (c cursorCheck) equal(other cursorCheck) bool {
	if c.maxID != other.maxID {
		return false
	}
	if c.cursorAt == nil || other.cursorAt == nil {
		return c.cursorAt == nil && other.cursorAt == nil
	}
	return c.cursorAt.Equal(*other.cursorAt)
}

func (c cursorCheck) String() string {
	cursorAt := "no row"
	if c.cursorAt != nil {
		cursorAt = c.cursorAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("newest id %d, cursor's row %s", c.maxID, cursorAt)
}

// resetAfterFirstStatement is a queryer that resets the event log the moment the first statement
// read through it has answered, which is a reset committing in the middle of whatever read that
// statement began.
type resetAfterFirstStatement struct {
	queryer
	reset func()
	fired bool
}

func (q *resetAfterFirstStatement) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return scanThen{row: q.queryer.QueryRow(ctx, sql, args...), after: func() {
		if !q.fired {
			q.fired = true
			q.reset()
		}
	}}
}

// scanThen is a row that runs after once its Scan has answered.
type scanThen struct {
	row   pgx.Row
	after func()
}

func (r scanThen) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.after()
	return err
}

// A reset of the event log (e2e/seed.ts's `truncate events restart identity` and its refill) that
// commits while eventCursorCheck is reading must leave the answer describing one log, the old one
// or the new one. A mixed answer - the old log's newest id with no row at the cursor in the new
// one - is one the relay would take for a reset to the old log's head: it would move its cursor
// there, and the new log's lower ids would then read as a second reset that closed the
// subscription opened after the first (TestEventStreamRelayRecoversWhenTheEventLogIsReset).
func TestEventCursorCheckReadsOneLogWhenTheLogIsResetMidCheck(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	cursor := ids[1]
	check := func(q queryer) cursorCheck {
		t.Helper()
		maxID, cursorAt, err := eventCursorCheck(ctx, q, cursor)
		if err != nil {
			t.Fatalf("eventCursorCheck: %v", err)
		}
		return cursorCheck{maxID: maxID, cursorAt: cursorAt}
	}

	oldLog := check(database.Pool)
	resetting := &resetAfterFirstStatement{queryer: database.Pool, reset: func() {
		if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
			t.Fatalf("reset the event log: %v", err)
		}
		seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	}}
	got := check(resetting)
	if !resetting.fired {
		t.Fatal("the check read nothing through the queryer, so no reset landed inside it")
	}
	newLog := check(database.Pool)

	if !got.equal(oldLog) && !got.equal(newLog) {
		t.Fatalf("a reset during the check answered %v; want the old log's answer (%v) or the new log's (%v), not two logs in one answer",
			got, oldLog, newLog)
	}
}
