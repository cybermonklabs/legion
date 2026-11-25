package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// secondProcess mounts a second API stack on the database the first one is on, as the second
// envoy-dispatch task of a deployment is: its own pool (storetest.SecondProcess), its own broker,
// its own document service and its own event stream relay. The two share Postgres and nothing
// else, which is the whole point -- an in-process fan-out cannot cross between them. It answers
// the stack's handler and the server Register would mount, whose relay a test reads.
func secondProcess(t *testing.T, database *store.Store) (http.Handler, *server) {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := docstest.Shutdown(t, context.Background(), documentService); err != nil {
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
		// Its horizon warns as the first process's does (newRegisteredTestServer), to the test's
		// own keeper of its database's warnings.
		HorizonStallAfter: horizontest.StallWarning,
		HorizonWarnEvery:  horizontest.StallWarning,
		HorizonLogger:     horizontest.Logger(horizontest.DatabaseOf(second)),
	})
	if err != nil {
		t.Fatalf("new API dependencies for the second process: %v", err)
	}
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return mux, served
}

// TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed is what the live feed is for with two
// envoy-dispatch processes on one Postgres: a page whose socket landed on process B sees an event
// process A committed, on the subscription it already has. Nothing of A's reaches B but the row
// itself -- B's relay hears the commit on `listen dispatch_events` (migration 0107) and reads it
// back out of the log. The broker fan-out the feed used to read would have ended at A.
func TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed(t *testing.T) {
	t.Parallel()
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	handler, _ := secondProcess(t, database)
	second := httptest.NewServer(handler)
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
		page, err := relay.server.readEventsAfterID(ctx, model.Actor{}, after, through, maxEventReplay)
		reads <- after
		return page, err
	}
	t.Cleanup(func() {
		stop()
		testwait.Returns(t, "the counted relay's generation goroutines (its listener and its run) to return once its lifetime ended", relay.wait)
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

// receiveRelayed is the subscription's next event. The relay publishes up to the safe horizon, so
// the wait is the horizon's (horizontest.WaitInProcess), which waits out any hold to the shared
// give-up and then names the holder, one in database, the test's own, among them.
func receiveRelayed(t *testing.T, subscription <-chan relayedEvent, database *store.Store) relayedEvent {
	t.Helper()
	return receiveRelayedOn(t, nil, subscription, database)
}

// receiveRelayedOn is receiveRelayed for a relay whose timers run on fake (eventStreamRelay.clock),
// or on the real clock where fake is nil. While it waits it moves fake to the first deadline the
// relay has armed, as time passing would, so a coalescing window, a read's retry or a held gap's
// fallback re-read comes due.
func receiveRelayedOn(t *testing.T, fake *clocktest.Fake, subscription <-chan relayedEvent, database *store.Store) relayedEvent {
	t.Helper()
	var event relayedEvent
	if fake == nil {
		event = horizontest.WaitInProcess(t, "the relay's next event", subscription, database)
	} else {
		horizontest.PollInProcess(t, "the relay's next event", database, func() bool {
			select {
			case event = <-subscription:
				return true
			default:
			}
			fake.SetWhen(func(_ time.Time, armed []time.Time) (time.Time, bool) {
				if len(armed) == 0 {
					return time.Time{}, false
				}
				return armed[0], true
			})
			return false
		})
	}
	if event.Event.ID == 0 {
		t.Fatal("the relay's subscription ended")
	}
	return event
}

// receiveRelayedThrough reads the subscription until the event last, failing on an id out of
// order or at or below after.
func receiveRelayedThrough(t *testing.T, subscription <-chan relayedEvent, database *store.Store, after, last int64) {
	t.Helper()
	receiveRelayedThroughOn(t, nil, subscription, database, after, last)
}

// receiveRelayedThroughOn is receiveRelayedThrough for a relay whose timers run on fake, which it
// moves as receiveRelayedOn does; nil is the real clock.
func receiveRelayedThroughOn(t *testing.T, fake *clocktest.Fake, subscription <-chan relayedEvent, database *store.Store, after, last int64) {
	t.Helper()
	for previous := after; previous != last; {
		id := receiveRelayedOn(t, fake, subscription, database).Event.ID
		if id <= previous {
			t.Fatalf("the relay published %d after %d; it publishes each event once, in id order", id, previous)
		}
		previous = id
	}
}

// quietRelayReads waits until the relay has published through the event log's head - its
// generation's start, its catch-up after the LISTEN and any re-read a gap above the horizon
// scheduled are over by then - and drains the page reads they made, so a count of the reads one
// commit costs counts that commit's alone. The relay publishes up to the safe horizon, so the wait
// is the horizon's (horizontest.PollInProcess), which fails at once on a hold of the test's own.
func quietRelayReads(t *testing.T, relay *eventStreamRelay, database *store.Store, reads <-chan int64) {
	t.Helper()
	head := eventHead(t, database)
	horizontest.PollInProcess(t, fmt.Sprintf("the relay to have published through event %d, the log's head", head), database, func() bool {
		return relayPublished(relay) >= head
	})
	for len(reads) > 0 {
		<-reads
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
// connections' backends to exit. The backends' wait reads pg_stat_activity; its one bound is the
// test binary's deadline (testwait.Until), and its failure names the backends still connected.
func waitRelayStopped(t *testing.T, database *store.Store, relay *eventStreamRelay, dials *listenerDials, when string) {
	t.Helper()
	waitReturned(t, "every goroutine of the relay to return "+when, &relay.running)
	for _, conn := range dials.dialed() {
		// Read after the wait above, which orders each listener's close before this read.
		if !conn.IsClosed() {
			t.Fatalf("the relay's listener connection to backend %d is still open %s", conn.PgConn().PID(), when)
		}
	}
	testwait.Until(t, "every backend of the relay's listener connections to exit "+when+" (the backends still connected)", func() ([]int32, bool) {
		connected := relayBackends(t, database, dials)
		return connected, len(connected) == 0
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
	t.Parallel()
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
	receiveRelayedThrough(t, subscription, database, head, last)
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
	t.Parallel()
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
	quietRelayReads(t, relay, database, reads)

	committed := seedEvents(t, database, issue, 1, 0)
	frames := make([][]byte, subscribers)
	for index, subscription := range subscriptions {
		event := receiveRelayed(t, subscription, database)
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
	t.Parallel()
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
		testwait.Receive(t, "the first subscription's cancel to return", cancelled)
		committed := seedEvents(t, database, issue, 1, 0)
		receiveRelayedThrough(t, subscription, database, head, committed)
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
	t.Parallel()
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, first, "OTHER", "Other issue", "A spec")
	handler, processB := secondProcess(t, database)
	held := reportRelayReads(processB.eventRelay)
	second := httptest.NewServer(handler)
	t.Cleanup(second.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(second.URL, "http")+"/ws/live", withBearer("agent-token")).servedBy(database).read()
	subscribeEvents(t, client, "events", nil)

	// Appended through a broker of process A's own: B learns of them only from the rows.
	drainRelayReads(held)
	low, high, commitLow := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	awaitRelayHeldRead(t, processB.eventRelay, held, low)
	expectNoEventAbove(t, client, "events", low-1, "on process B while process A's lower id is open")
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
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	// The relay reads through a pool of its own, so its acquires are its alone.
	deps.Store = storetest.SecondProcess(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	relay, reads := newCountedRelay(t, deps)
	held := reportRelayReads(relay)
	dials := recordRelayDials(relay, deps)
	subscription, _, opened := subscribeRelay(t, relay)
	waitListening(t, database, "the relay's listener holding a LISTEN", dials)
	quietRelayReads(t, relay, database, reads)

	drainRelayReads(held)
	low, high, commitLow := appendOutOfOrder(t, database, deps.Events, issue.Key, other.Key)
	// The higher id's commit notifies: that read checks for a reset on the shared pool, takes the
	// horizon and leaves the gap open, publishing nothing. The relay reports each read once it has
	// published (afterRead), so the subscription holds whatever that read published once it has
	// reported. Its reset check is the one shared connection the gap may cost, so the count starts
	// after it, and covers the gap re-reads the relay makes next while the lower id stays open, each
	// reported in its turn.
	awaitRelayHeldRead(t, relay, held, low)
	select {
	case event := <-subscription:
		t.Fatalf("the relay's subscription was sent event %d while the gap below %d stood open", event.Event.ID, high)
	default:
	}
	before := acquires(deps.Store)
	const gapRereads = 3
	for n := range gapRereads {
		// A try that takes no re-read with a gap reads how many reads this wait took, the last of
		// them, and how far the relay has published.
		taken := 0
		var last relayRead
		testwait.Until(t, fmt.Sprintf("the relay's gap re-read %d of %d while the lower id stays open", n+1, gapRereads), func() (string, bool) {
			select {
			case read := <-held:
				taken++
				last = read
				return fmt.Sprintf("read %+v", read), read.gap != events.GapNone
			default:
				return fmt.Sprintf("no read reported; %d reads taken, none with a gap, the last %+v; the relay published through event %d",
					taken, last, relayPublished(relay)), false
			}
		})
	}
	if got := acquires(deps.Store); got != before {
		t.Fatalf("the relay's %d gap re-reads took %d shared-pool connections while %d stayed open, want none", gapRereads, got-before, low)
	}
	if len(reads) != 0 {
		t.Fatalf("the relay read the event log %d times while the gap below %d stood open, want none", len(reads), high)
	}

	commitLow()
	receiveRelayedThrough(t, subscription, database, opened, high)
}

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped from every open page's feed.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, _ := newCountedRelay(t, deps)
	// The relay's coalescing window, its read's retry and its gap re-reads run on this fake, which
	// moves only as the row moves it: a read a timer of the relay's own would make, a poll, is a
	// deadline armed on it first (Armed).
	fake := clocktest.New(time.Now())
	relay.clock = fake
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

	// Committed while the relay has no connection: its notification reaches nobody, and the
	// redial's catch-up is what sends it. The relay reads only on a notification, a LISTEN's
	// catch-up, a gap's re-read or a read's retry, and each of the last two, as a poll would,
	// waits on the fake first. The catch-up the redial's LISTEN wakes waits the coalescing window
	// on the fake (eventStreamRelay.window): by then the relay has waited for a wake since its
	// generation began, and the fake has stood still, so every timer it armed while it had no
	// connection is still armed beside the window, which must be the only one.
	committed := seedEvents(t, database, issue, 1, 0)
	down.Store(false)
	testwait.Until(t, "the catch-up the relay's redial wakes to wait its coalescing window", func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, slices.ContainsFunc(armed, fake.Now().Add(relay.window()).Equal)
	})
	if armed, moved := fake.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
		window := now.Add(relay.window())
		return window, len(armed) == 1 && armed[0].Equal(window)
	}); !moved {
		t.Fatalf("the relay had %v armed on its clock as its redial's catch-up waited its %s coalescing window, want that window alone: "+
			"a timer it armed while it had no connection reads the log by itself, a poll", armed, relay.window())
	}
	receiveRelayedThroughOn(t, fake, subscription, database, opened, committed)

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
	testwait.Eventually(t, "the relay to redial after its connection was terminated", func() bool {
		return len(dials.dialed()) > len(dialed)
	})
	next := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThroughOn(t, fake, subscription, database, committed, next)
}

// TestEventStreamRelayRecoversWhenTheEventLogIsReset: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the new rows'
// ids restart at 1, below the cursor the relay has published through, and `id > cursor` matches
// nothing. A relay that only ever published forward would fall silent there for good: every live
// update of every later scenario would be lost, with no error anywhere. The relay reads the reset
// through events.EventCursorCheck and ends its generation with every subscription it holds -- the
// stream's answer to a reader it can no longer serve in order, which ends each open events
// subscription with an ended frame (behind) and sends its page through a resubscribe
// (TestTheEventFeedResumesAfterTheEventLogIsResetBeneathTheReader) -- and the next subscription
// starts a generation on the new log.
func TestEventStreamRelayRecoversWhenTheEventLogIsReset(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	relay, _ := newCountedRelay(t, deps)
	dials := recordRelayDials(relay, deps)
	subscription, _, opened := subscribeRelay(t, relay)

	// A cursor well above the ids the reset log will hand out.
	committed := seedEvents(t, database, issue, 1, 0)
	receiveRelayedThrough(t, subscription, database, opened, committed)

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
	for {
		event := testwait.Receive(t, "the end of the subscription open across the reset", subscription)
		if event.Event.ID == 0 {
			break
		}
		t.Fatalf("the relay published event %d into a subscription whose log was reset under it", event.Event.ID)
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
	receiveRelayedThrough(t, reopened, database, head, next)
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
// subscription made meanwhile starts the relay's next generation, whose start reads the log with
// a's id open below b's and then waits for a horizon a holds back (firstRelayCursor), and is sent
// a, which committed after that read, below that new cursor.
func TestEventLogResetClearsTheRelaysHorizon(t *testing.T) {
	t.Parallel()
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, first, "OTHER", "Other issue", "A spec")
	handler, processB := secondProcess(t, database)
	second := httptest.NewServer(handler)
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
	// B's relay finds the reset once it hears the refill's commit, and the finding ends its
	// generation with every subscription on it (resetCursor): the subscription open across the
	// reset ending is that finding, and it ends without an event of the new log, whose ids it has
	// already seen stand for other events.
	for ended := false; !ended; {
		switch frame := across.next(t, "the end of the subscription open across the reset").(type) {
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
	// This subscription starts B's relay again, and its start reads the log once, finding a's id
	// open below b's, before it waits for the horizon a holds back (firstRelayCursor). An event that
	// commits before that read counts as committed before the subscription began, so a commits
	// once the start has read: the count of starts that have read the log is the one fact the
	// start leaves of that read (eventStreamRelay.startsRead).
	relay := processB.eventRelay
	startsRead := func() int {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		return relay.startsRead
	}
	reads := startsRead()
	waiting := dial()
	sendEventsSubscribe(t, waiting, "events", nil)
	testwait.Eventually(t, "the start of B's relay for the subscription made while a is open to read the log", func() bool {
		return startsRead() > reads
	})
	commitLow()
	renameIssue(t, first, issue.Key, "Renamed after the reset")

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
	t.Parallel()
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
