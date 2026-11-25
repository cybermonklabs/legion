package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
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

// awaitCalls waits until the listener has seen want calls. Its one bound is the test binary's
// deadline (testwait.Until), and its failure names the calls the listener last saw.
func awaitCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the listener to see %d calls (the calls it saw)", want), func() (int32, bool) {
		seen := calls.Load()
		return seen, seen >= want
	})
}

// The deployment's TTLs are the ones the route_status comment promises: an answer serves five
// seconds, a listener that did not answer is left alone for five as well, and a fetch is bounded
// by routeRosterTimeout. It reads the server register mounts, the only one a deployment runs.
func TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	// The answer serves stagedAhead here, so the second read is inside it however slow the
	// machine: the deployment's five seconds, which the database judges on its own clock, would
	// ask again after a stall between the reads
	// (TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds reads the deployment's own).
	served.rosters.ttl = stagedAhead
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
	t.Parallel()
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
	t.Parallel()
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	// The silence window is lengthened to stagedAhead, and the answered TTL zeroed, so the three
	// reads below stay inside the silence however slow the machine, and a cache that held the
	// failure under the answered TTL would ask again. Neither shortens a window this test reads.
	served.rosters.ttl, served.rosters.silenceTTL = 0, stagedAhead
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
// failure under that TTL, or for ever, would store an expiry a minute or more past the fetch; the
// row reads the stored failure's own window, both ends stamped by the database, and requires it
// shorter than that TTL before it moves the row past the window rather than waiting it out.
func TestAListenerThatFailedIsAskedAgainOnceItsSilenceWindowPasses(t *testing.T) {
	t.Parallel()
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, database, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "unknown null" {
		t.Fatalf("read with the listener down: %s, want unknown null", got)
	}
	var answered bool
	var micros int64
	if err := database.Pool.QueryRow(context.Background(),
		`select answered, (extract(epoch from expires_at - sent_at) * 1000000)::bigint from route_roster`).Scan(&answered, &micros); err != nil {
		t.Fatalf("read the stored listener answer: %v", err)
	}
	if window := time.Duration(micros) * time.Microsecond; answered || window >= served.rosters.ttl {
		t.Fatalf("the stored answer of the failed fetch is answered=%t and lasts %v, want a failure kept for its silence window, shorter than the %v answered TTL",
			answered, window, served.rosters.ttl)
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
// bounded by nothing would arm none. The error says which ended it: either end says "context
// deadline exceeded", and a call the window ended also names envoy.ErrCallWindow, which the client
// adds only when its caller's context had not ended (a read's error carries its cause as text).
func TestAListenerThatDoesNotAnswerHoldsAReadOnlyForTheFetchBound(t *testing.T) {
	t.Parallel()
	served, calls, _, clock := heldRosterServer(t)
	// The silence window is stagedAhead, so the second read below is inside it however slow the
	// machine; it lengthens a wait this test does not read.
	served.rosters.silenceTTL = stagedAhead

	started := clock.Now()
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	clock.AwaitArmedAt(t, "the fetch's bound", started.Add(routeRosterTimeout))
	// The bound is armed before the fetch's request leaves, so the clock moves only once that
	// request has reached the listener: moved sooner, it ends the fetch before the call is made.
	awaitCalls(t, calls, 1)
	clock.Set(started.Add(routeRosterTimeout))
	roster := testwait.Receive(t, "the read to answer once the fetch's bound ran out", answer)
	if roster.answered {
		t.Fatal("a read answered a roster though the listener never answered")
	}
	// The fetch's own deadline ended the call, not the listener client's window: either end says
	// "context deadline exceeded", and a call the window ended also names envoy.ErrCallWindow.
	if roster.err == nil || !strings.Contains(roster.err.Error(), context.DeadlineExceeded.Error()) || strings.Contains(roster.err.Error(), envoy.ErrCallWindow.Error()) {
		t.Fatalf("the first read's fetch ended with %v, want its own %s bound's %v, not the listener client's window (%v)", roster.err, routeRosterTimeout, context.DeadlineExceeded, envoy.ErrCallWindow)
	}
	first := calls.Load()
	if first != 1 {
		t.Fatalf("the first read made %d listener calls, want 1", first)
	}
	sent, _ := storedRoster(t, served.deps.Store)
	// The second read runs inside the shared wait: a read that asked the listener again would wait
	// on a fetch the held listener never answers, bounded on the fake that stands still, so made on
	// the test goroutine it would hang the test binary to Go's alarm rather than fail naming it.
	var second routeRoster
	testwait.Returns(t, "a read inside the silence window to answer from the stored silence without asking the listener", func() {
		second = readRoster(t, served, context.Background())
	})
	if second.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if again, _ := storedRoster(t, served.deps.Store); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window asked the listener again: the stored answer was sent at %s, after the first read's silence sent at %s", again, sent)
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
	t.Parallel()
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
// and the fetch's bound is the deployment's own two seconds. The silence is stagedAhead here, so
// the second read is inside it however slow the machine; a window counted from the send still
// falls the fetch's bound short of the one stored.
func TestAFailuresWindowCountsFromWhenTheFailureIsKnown(t *testing.T) {
	t.Parallel()
	listenerURL, calls, _ := heldRosterListener(t)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL})
	served.rosters.silenceTTL = stagedAhead
	if readRoster(t, served, context.Background()).answered {
		t.Fatal("a listener that hangs answered")
	}
	sent, expires := storedRoster(t, database)
	if window := expires.Sub(sent); window < routeRosterTimeout+served.rosters.silenceTTL {
		t.Fatalf("the stored silence serves reads for %s from the call's send, want at least the fetch's bound (%s) "+
			"and the silence window (%s): the window must count from when the failure was known",
			window, routeRosterTimeout, served.rosters.silenceTTL)
	}
	// Inside that window no read asks the listener again.
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	// The listener sees at most the first read's call, which a loaded machine can deliver late or
	// not at all, so the count is held to one or fewer; the stored answer, still the first read's
	// silence, says the read made no fetch of its own whether its call would have arrived or not.
	if got := calls.Load(); got > 1 {
		t.Fatalf("two reads, the second inside the silence window, made %d listener calls in all, want at most the first read's 1", got)
	}
	if again, _ := storedRoster(t, database); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window asked the listener again: the stored answer was sent at %s, after the first read's silence sent at %s", again, sent)
	}
}

// A shutdown ends a fetch in flight: it runs on the server's lifetime, so a read waiting on a
// listener that hangs returns unknown once the lifetime ends. The server's clock stands still,
// so neither the fetch's bound nor the client's window can end that call instead. What ended the
// fetch is in the answer's error: the lifetime's cancel, where the fetch's bound or the client's
// window would each read as a deadline.
func TestAShutdownEndsAFetchInFlight(t *testing.T) {
	t.Parallel()
	lifetime, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	served, calls, _, _ := heldRosterServerOn(t, lifetime)
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	awaitCalls(t, calls, 1)
	shutdown()
	roster := testwait.Receive(t, "the answer of a read waiting on a listener that hangs, once the shutdown began", answer)
	if roster.answered {
		t.Fatal("a read answered a roster after the shutdown")
	}
	if roster.err == nil || !strings.Contains(roster.err.Error(), context.Canceled.Error()) {
		t.Fatalf("the read's fetch ended with %v, want the shutdown's %v: a fetch the shutdown did not end ran out of a bound instead", roster.err, context.Canceled)
	}
}

// Reads that miss a cold cache together share one listener call: each waits for the one fetch
// rather than starting its own.
func TestConcurrentReadsOnAColdCacheMakeOneListenerCall(t *testing.T) {
	// Not parallel: rosterWaits reads every goroutine of the process (runtime.Stack, all), so a read
	// another test parks in readRouteRoster counts as one of this row's 16.
	served, calls, release, _ := heldRosterServer(t)
	const readers = 16
	results := make([]routeRoster, readers)
	var done sync.WaitGroup
	done.Add(readers)
	for index := range readers {
		go func() {
			defer done.Done()
			results[index] = readRoster(t, served, context.Background())
		}()
	}
	awaitCalls(t, calls, 1)
	// Every read is waiting on a fetch in flight before the listener answers, which a read that
	// had not joined yet would not be: it would read the stored answer after the release and pass
	// the count below with no fetch shared. A cache without a shared fetch has each read make a call
	// of its own, which the count fails.
	testwait.Until(t, fmt.Sprintf("all %d reads to wait on a fetch in flight", readers), func() (int, bool) {
		waits := rosterWaits()
		return waits, waits == readers
	})
	release()
	testwait.Returns(t, fmt.Sprintf("all %d reads to answer once the listener's one fetch was released", readers), done.Wait)
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
	// Not parallel: rosterWaits reads every goroutine of the process (runtime.Stack, all), so a read
	// another test parks in readRouteRoster counts as one of this row's 3.
	served, calls, release, _ := heldRosterServer(t)
	// The answer serves stagedAhead here, so the last read below takes the fetch's answer however
	// long after the fetch's send it comes: the deployment's five seconds, which the database
	// judges, would ask the listener again after a stall.
	served.rosters.ttl = stagedAhead
	read := func(ctx context.Context) <-chan routeRoster {
		answer := make(chan routeRoster, 1)
		go func() {
			roster, _ := served.readRouteRoster(ctx)
			answer <- roster
		}()
		return answer
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := read(leaderCtx)
	awaitCalls(t, calls, 1)
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joiner := read(joinerCtx)
	patient := read(context.Background())
	// All three wait on the one fetch before any request ends, so the joiner's request ends while
	// it waits on a fetch it joined, not before its read of the stored answer.
	testwait.Until(t, "the three reads to wait on the fetch in flight", func() (int, bool) {
		waits := rosterWaits()
		return waits, waits == 3
	})

	cancelLeader()
	cancelJoiner()
	for name, answer := range map[string]<-chan routeRoster{"the read that started the fetch": leader, "a read that joined it": joiner} {
		if roster := testwait.Receive(t, "the answer of "+name+" once its request ended", answer); roster.answered {
			t.Fatalf("%s answered a roster though the listener has not answered yet", name)
		}
	}
	// Each returned while the shared fetch was still in flight, held at the listener until
	// release: a read that waited for the fetch rather than for its own request could return only
	// once the fetch had ended.
	served.rosters.mu.Lock()
	inFlight := served.rosters.fetch != nil
	served.rosters.mu.Unlock()
	if !inFlight {
		t.Fatal("the reads whose requests ended returned only once the shared fetch had ended, not when their requests did")
	}
	// The read whose request is still open is still waiting: it has not been handed an answer,
	// and the listener has not been asked again.
	select {
	case <-patient:
		t.Fatal("a read whose request is still open returned before the listener answered")
	default:
	}

	release()
	if got := sreReach(testwait.Receive(t, "the answer of the read that waited, once the listener answered", patient)); got != "live ses-sre" {
		t.Fatalf("the read that waited read %s, want live ses-sre", got)
	}
	if got := sreReach(readRoster(t, served, context.Background())); got != "live ses-sre" {
		t.Fatalf("the cached answer reads %s, want live ses-sre: the fetch whose starter left must still fill the cache", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("four reads made %d listener calls, want 1", got)
	}
}

// rosterWaits counts the reads parked in readRouteRoster's one wait, its select on a fetch in
// flight and on the read's own request: the goroutines whose stack stands in that select. No state
// of the cache counts the reads waiting on a fetch, and their stacks do, as serveStacks counts the
// mention sender's lanes.
func rosterWaits() int {
	buffer := make([]byte, 1<<22)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	waits := 0
	for _, goroutine := range strings.Split(stacks, "\n\n") {
		header, frames, _ := strings.Cut(goroutine, "\n")
		first, _, _ := strings.Cut(frames, "\n")
		if strings.Contains(header, "[select") && strings.Contains(first, "api.(*server).readRouteRoster(") {
			waits++
		}
	}
	return waits
}
