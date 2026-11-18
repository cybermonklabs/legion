package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// detailReach reads one issue's detail and answers its reach as reachText writes it.
func detailReach(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	issue := decodeBody[routedIssue](t, response)
	return reachText(issue.RouteStatus, issue.RouteHolder)
}

// sreReach judges the route role:sre against roster, as reachText writes it.
func sreReach(roster routeRoster) string {
	route := "role:sre"
	reach := roster.reach(&route)
	return reachText(reach.RouteStatus, reach.RouteHolder)
}

// readRoster is served's readRouteRoster for a read whose database answers: a failed read of the
// stored answer fails the test. It reports with Errorf, so a read on a goroutine of its own may
// use it.
func readRoster(t *testing.T, served *server, ctx context.Context) routeRoster {
	t.Helper()
	roster, err := served.readRouteRoster(ctx)
	if err != nil {
		t.Errorf("read the route roster: %v", err)
	}
	return roster
}

// rosterTestStart is where the fake clock of a roster test stands: a fixed moment, since nothing
// these tests judge is read from the server's clock, only the deadlines armed on it.
var rosterTestStart = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// heldRosterServer is a test server on a database of its own whose listener's GET /v1/sessions
// lists ses-sre holding sre, but only once release is called: until then every call waits, so a
// test can hold one fetch open while other reads arrive. calls counts the requests that reached
// it. clock is the server's, a fake that stands still, so neither the listener client's window
// nor the fetch's bound comes due until a test moves it there itself.
func heldRosterServer(t *testing.T) (served *server, calls *atomic.Int32, release func(), clock *clocktest.Fake) {
	t.Helper()
	return heldRosterServerOn(t, nil)
}

// heldRosterServerOn is heldRosterServer on the lifetime given, nil for the test's own.
func heldRosterServerOn(t *testing.T, lifetime context.Context) (served *server, calls *atomic.Int32, release func(), clock *clocktest.Fake) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	clock = clocktest.New(rosterTestStart)
	_, _, _, served = newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, clock: clock, lifetime: lifetime})
	return served, calls, release, clock
}

// heldRosterMux is heldRosterServer mounted on mux over deps, whose Envoy it sets to the held
// listener's client: deps carries the Store the stored answer is read from, and a test that serves
// requests through mux gives it an Identity too.
func heldRosterMux(t *testing.T, mux *http.ServeMux, deps Deps) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	deps.Clock = clocktest.New(rosterTestStart)
	deps.Envoy = envoy.New(listenerURL, envoy.WithClock(deps.Clock))
	served = register(mux, t.Context(), deps)
	return served, calls, release
}

// heldRosterListener is the listener heldRosterServer and heldRosterMux ask: its GET
// /v1/sessions lists ses-sre holding sre once release is called, calls counting the requests that
// reached it.
func heldRosterListener(t *testing.T) (url string, calls *atomic.Int32, release func()) {
	t.Helper()
	calls = &atomic.Int32{}
	gate := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		<-gate
		_, _ = w.Write([]byte(`[{"session_id":"ses-sre","title":"worker","roles":["sre"]}]`))
	}))
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Release before the close, which waits for every request in flight to finish.
	t.Cleanup(listener.Close)
	t.Cleanup(release)
	return listener.URL, calls, release
}

// awaitCalls waits until the listener has seen want calls.
func awaitCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the listener to see %d calls", want), func() (int32, bool) {
		seen := calls.Load()
		return seen, seen >= want
	})
}

// The deployment's TTLs are the ones the route_status comment promises: an answer serves five
// seconds, a listener that did not answer is left alone for five as well, and a fetch is bounded
// by routeRosterTimeout. It reads the server register mounts, the only one a deployment runs.
func TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds(t *testing.T) {
	rosters := &register(http.NewServeMux(), t.Context(), Deps{}).rosters
	if rosters.ttl != 5*time.Second || rosters.silenceTTL != 5*time.Second {
		t.Fatalf("the server a deployment runs keeps an answer %s and a silence %s, want 5s and 5s", rosters.ttl, rosters.silenceTTL)
	}
	if routeRosterTimeout != 2*time.Second {
		t.Fatalf("the server a deployment runs bounds a fetch by %s, want 2s", routeRosterTimeout)
	}
}

// Two reads inside the TTL make one listener call, and the second reads the first's answer
// even though the listener's own answer has changed since: a route_status as old as the TTL is
// the contract.
func TestTwoIssueReadsWithinTheTTLMakeOneListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, _ := cachedRouteStatusServer(t, roster)
	// The deployment's own five seconds: the two reads below are a few milliseconds apart, well
	// inside it (TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds reads it).
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("second read inside the TTL: %s, want the cached live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("two reads inside the TTL made %d listener calls, want 1", calls)
	}
}

// A read after the stored answer's window has run out asks the listener again and reads its new
// answer. The window is the row's own expiry, which the database judges on its own clock, so the
// test moves that row past it rather than waiting the TTL out.
func TestAnIssueReadAfterTheTTLAsksTheListenerAgain(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, database, _ := cachedRouteStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	expireStoredRoster(t, database)
	if got := detailReach(t, handler, key); got != "no_holder null" {
		t.Fatalf("read after the TTL: %s, want no_holder null", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a TTL apart made %d listener calls, want 2", calls)
	}
}

// expireStoredRoster moves the stored listener answer's expiry into the past, which is what a
// read finds once the answer's window has run out. The window is the database's, judged against
// its own clock so that every process agrees, and no clock of the server's reaches it: a test
// past the window says so on the row.
func expireStoredRoster(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update route_roster set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("expire the stored listener answer: %v", err)
	}
}

// A listener that fails is asked once within the silence window, and every read in it answers
// as an unreachable listener always has: unknown on the detail, 503 for a judged filter. The
// answered-roster TTL is zero here, so a cache that held a failure for that TTL would ask again.
func TestAFailingListenerIsAskedOnceWithinTheSilenceWindow(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	// The silence window is lengthened to a minute, and the answered TTL zeroed, so the three
	// reads below stay inside the silence however slow the machine, and a cache that held the
	// failure under the answered TTL would ask again. Neither shortens a window this test reads.
	served.rosters.ttl, served.rosters.silenceTTL = 0, time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	for read := 1; read <= 2; read++ {
		if got := detailReach(t, handler, key); got != "unknown null" {
			t.Fatalf("read %d with the listener down: %s, want unknown null", read, got)
		}
	}
	blind := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status=no_holder", nil, "alice")
	if blind.Code != http.StatusServiceUnavailable {
		t.Fatalf("route_status=no_holder with the listener down: status=%d body=%s, want 503", blind.Code, blind.Body.String())
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("three reads with the listener down made %d listener calls, want 1", calls)
	}
}

// A cached failure lasts only its silence window: once it has run out, a read asks the listener
// again and reads its answer. The answered TTL is lengthened to a minute, so a cache that held a
// failure under that TTL, or for ever, would still read unknown; the silence window is the
// deployment's, and the test moves the stored row past it rather than waiting it out.
func TestAListenerThatFailedIsAskedAgainOnceItsSilenceWindowPasses(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, database, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "unknown null" {
		t.Fatalf("read with the listener down: %s, want unknown null", got)
	}
	roster.down.Store(false)
	roster.set("ses-sre", "sre")
	expireStoredRoster(t, database)
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("read after the silence window: %s, want live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a silence window apart made %d listener calls, want 2", calls)
	}
}

// A listener that does not answer holds a fetch only for the fetch's own bound, not the client's
// window, and the reads after it read unknown at once without asking again. The bound is armed
// on the server's clock for routeRosterTimeout from the fetch, and the test moves the clock
// there: a fetch bounded by the client's window instead would arm a later deadline, and one
// bounded by nothing would arm none.
func TestAListenerThatDoesNotAnswerHoldsAReadOnlyForTheFetchBound(t *testing.T) {
	served, calls, _, clock := heldRosterServer(t)
	// The silence window is a minute, so the second read below is inside it however slow the
	// machine; it lengthens a wait this test does not read.
	served.rosters.silenceTTL = time.Minute

	started := clock.Now()
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	awaitArmedAt(t, clock, "the fetch's bound", started.Add(routeRosterTimeout))
	// The bound is armed before the fetch's request leaves, so the clock moves only once that
	// request has reached the listener: moved sooner, it ends the fetch before the call is made.
	awaitCalls(t, calls, 1)
	clock.Set(started.Add(routeRosterTimeout))
	if roster := testwait.Receive(t, "the read to answer once the fetch's bound ran out", answer); roster.answered {
		t.Fatal("a read answered a roster though the listener never answered")
	}
	first := calls.Load()
	if first != 1 {
		t.Fatalf("the first read made %d listener calls, want 1", first)
	}
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if got := calls.Load(); got != first {
		t.Fatalf("a read inside the silence window asked the listener again: %d calls, want %d", got, first)
	}
}

// storedRoster is the stored listener answer's sent_at and expires_at: when the call that
// brought it was sent, and when the answer stops serving reads.
func storedRoster(t *testing.T, database *store.Store) (sent, expires time.Time) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select sent_at, expires_at from route_roster`).Scan(&sent, &expires); err != nil {
		t.Fatalf("read the stored listener answer: %v", err)
	}
	return sent, expires
}

// An answer's TTL counts from the moment its call was sent, so a slow listener cannot stretch the
// age of the answers a read is served past the TTL. The listener here answers only once the test
// releases it, well after the call was sent, and the answer stored must still expire exactly
// routeRosterTTL after that send; a TTL counted from the answer would expire later.
func TestAnAnswersTTLCountsFromWhenItsCallWasSent(t *testing.T) {
	served, calls, release, _ := heldRosterServer(t)
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	awaitCalls(t, calls, 1)
	release()
	if !testwait.Receive(t, "the read to answer once the listener was released", answer).answered {
		t.Fatal("the first read did not answer")
	}

	sent, expires := storedRoster(t, served.deps.Store)
	if want := sent.Add(routeRosterTTL); !expires.Equal(want) {
		t.Fatalf("the stored answer expires at %v, want %v: the TTL must count from when the call was sent, not from when it answered",
			expires, want)
	}
}

// A failure's window counts from the moment the failure is known, so a listener that hangs is
// left alone for the whole window after its call times out, not from when that call was sent.
// The listener never answers, so the fetch ends at routeRosterTimeout, and the stored silence
// must expire a window after that moment rather than a window after the send. This server runs
// on the real clock: what the test reads is the gap between two moments the database stamped,
// and the fetch's bound is the deployment's own two seconds.
func TestAFailuresWindowCountsFromWhenTheFailureIsKnown(t *testing.T) {
	listenerURL, calls, _ := heldRosterListener(t)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL})
	if readRoster(t, served, context.Background()).answered {
		t.Fatal("a listener that hangs answered")
	}
	sent, expires := storedRoster(t, database)
	if window := expires.Sub(sent); window < routeRosterTimeout+routeRosterSilenceTTL {
		t.Fatalf("the stored silence serves reads for %s from the call's send, want at least the fetch's bound (%s) "+
			"and the silence window (%s): the window must count from when the failure was known",
			window, routeRosterTimeout, routeRosterSilenceTTL)
	}
	// Inside that window no read asks the listener again.
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("a read inside the silence window that began when the fetch's bound ran out made %d listener calls in all, want 1", got)
	}
}

// A shutdown ends a fetch in flight: it runs on the server's lifetime, so a read waiting on a
// listener that hangs returns unknown once the lifetime ends. The server's clock stands still,
// so neither the fetch's bound nor the client's window can end that call instead.
func TestAShutdownEndsAFetchInFlight(t *testing.T) {
	lifetime, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	served, calls, _, _ := heldRosterServerOn(t, lifetime)
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	awaitCalls(t, calls, 1)
	shutdown()
	if roster := testwait.Receive(t, "the read waiting on a listener that hangs to return after the shutdown", answer); roster.answered {
		t.Fatal("a read answered a roster after the shutdown")
	}
}

// Reads that miss a cold cache together share one listener call: each waits for the one fetch
// rather than starting its own.
func TestConcurrentReadsOnAColdCacheMakeOneListenerCall(t *testing.T) {
	served, calls, release, _ := heldRosterServer(t)
	const readers = 16
	results := make([]routeRoster, readers)
	var started, done sync.WaitGroup
	started.Add(readers)
	done.Add(readers)
	for index := range readers {
		go func() {
			defer done.Done()
			started.Done()
			results[index] = readRoster(t, served, context.Background())
		}()
	}
	started.Wait()
	awaitCalls(t, calls, 1)
	// Every read is waiting on the one fetch: the cache holds a fetch in flight and no reader
	// has answered, which a cache without a shared fetch could not show, since each read would
	// have made a call of its own.
	testwait.Eventually(t, "every read to be waiting on the one fetch", func() bool {
		served.rosters.mu.Lock()
		defer served.rosters.mu.Unlock()
		return served.rosters.fetch != nil
	})
	release()
	done.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d concurrent reads on a cold cache made %d listener calls, want 1", readers, got)
	}
	for index, roster := range results {
		if got := sreReach(roster); got != "live ses-sre" {
			t.Fatalf("reader %d read %s, want the shared answer live ses-sre", index, got)
		}
	}
}

// A read whose request ends while it waits on the shared fetch returns at once, reading the
// route as unknown, whether it started that fetch or joined it; the fetch runs on for the other
// readers, and its answer is the one the cache keeps.
func TestAReadWhoseRequestEndsWhileItWaitsReturnsPromptly(t *testing.T) {
	served, calls, release, _ := heldRosterServer(t)
	read := func(ctx context.Context) <-chan routeRoster {
		answer := make(chan routeRoster, 1)
		started := make(chan struct{})
		// A read whose request ends may end before its read of the stored answer does, which
		// reads unknown as well.
		go func() {
			close(started)
			roster, _ := served.readRouteRoster(ctx)
			answer <- roster
		}()
		testwait.Receive(t, "the read to start", started)
		return answer
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := read(leaderCtx)
	awaitCalls(t, calls, 1)
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joiner := read(joinerCtx)
	patient := read(context.Background())

	cancelLeader()
	cancelJoiner()
	for name, answer := range map[string]<-chan routeRoster{"the read that started the fetch": leader, "a read that joined it": joiner} {
		if roster := testwait.Receive(t, name+" to return once its request ended", answer); roster.answered {
			t.Fatalf("%s answered a roster though the listener has not answered yet", name)
		}
	}
	// The read whose request is still open is still waiting: it has not been handed an answer,
	// and the listener has not been asked again.
	select {
	case <-patient:
		t.Fatal("a read whose request is still open returned before the listener answered")
	default:
	}

	release()
	if got := sreReach(testwait.Receive(t, "the read that waited to be handed the fetch's answer", patient)); got != "live ses-sre" {
		t.Fatalf("the read that waited read %s, want live ses-sre", got)
	}
	if got := sreReach(readRoster(t, served, context.Background())); got != "live ses-sre" {
		t.Fatalf("the cached answer reads %s, want live ses-sre: the fetch whose starter left must still fill the cache", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("four reads made %d listener calls, want 1", got)
	}
}
