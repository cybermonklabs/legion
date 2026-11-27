package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
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

// heldRosterServer is a test server on a database of its own whose listener's GET /v1/sessions
// lists ses-sre holding sre, but only once release is called: until then every call waits, so a
// test can hold one fetch open while other reads arrive. calls counts the requests that reached
// it. The fetch's bound, the listener client's window and the TTL are each a minute, so however
// long a loaded machine takes to release the fetch, it reads no timeout as unknown and no expiry
// calls again.
func heldRosterServer(t *testing.T) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	_, _, _, served = newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: time.Minute})
	served.rosters.ttl, served.rosters.timeout = time.Minute, time.Minute
	return served, calls, release
}

// heldRosterMux is heldRosterServer mounted on mux over deps, whose Envoy it sets to the held
// listener's client: deps carries the Store the stored answer is read from, and a test that serves
// requests through mux gives it an Identity too.
func heldRosterMux(t *testing.T, mux *http.ServeMux, deps Deps) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	deps.Envoy = envoy.New(listenerURL, envoy.WithTimeout(time.Minute))
	served = register(mux, t.Context(), deps)
	served.rosters.ttl, served.rosters.timeout = time.Minute, time.Minute
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

// storedRosterTimes reads the stored listener answer's sent_at and expires_at, both on the
// database's clock: the moment the fetch that stored it counts as sent (the database's now() on
// the read that found the row stale, before the call) and the moment it stops serving reads.
func storedRosterTimes(t *testing.T, served *server) (sent, expires time.Time) {
	t.Helper()
	if err := served.deps.Store.Pool.QueryRow(context.Background(), `select sent_at, expires_at from route_roster`).Scan(&sent, &expires); err != nil {
		t.Fatalf("read the stored listener answer: %v", err)
	}
	return sent, expires
}

// The deployment's TTLs are the ones the route_status comment promises: an answer serves five
// seconds, a listener that did not answer is left alone for five as well, and a fetch is bounded
// by routeRosterTimeout, no test's override. It reads the server register mounts, the only one a
// deployment runs.
func TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds(t *testing.T) {
	t.Parallel()
	rosters := &register(http.NewServeMux(), t.Context(), Deps{}).rosters
	if rosters.ttl != 5*time.Second || rosters.silenceTTL != 5*time.Second {
		t.Fatalf("the server a deployment runs keeps an answer %s and a silence %s, want 5s and 5s", rosters.ttl, rosters.silenceTTL)
	}
	if rosters.timeout != 0 || routeRosterTimeout != 2*time.Second {
		t.Fatalf("the server a deployment runs bounds a fetch by %s over routeRosterTimeout %s, want no override of 2s", rosters.timeout, routeRosterTimeout)
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
	// A minute, not the deployment's five seconds, so a slow machine's second read stays inside it;
	// the deployment's own value is TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds's.
	served.rosters.ttl = time.Minute
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

// A read after the TTL asks the listener again and reads its new answer. The silence TTL is
// left at a second, so a cache that kept an answered roster for the silence TTL would fail too.
func TestAnIssueReadAfterTheTTLAsksTheListenerAgain(t *testing.T) {
	t.Parallel()
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = 50 * time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	time.Sleep(100 * time.Millisecond)
	if got := detailReach(t, handler, key); got != "no_holder null" {
		t.Fatalf("read after the TTL: %s, want no_holder null", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a TTL apart made %d listener calls, want 2", calls)
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
	// A minute, not the deployment's five seconds, so a slow machine's three reads stay inside it;
	// the deployment's own value is TestTheDeploymentsRosterCacheKeepsAnAnswerAndASilenceFiveSeconds's.
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

// A cached failure lasts only its silence window: once it has passed, a read asks the listener
// again and reads its answer. The answered TTL is a minute, so a cache that held a failure for
// that TTL, or for ever, would still read unknown.
func TestAListenerThatFailedIsAskedAgainOnceItsSilenceWindowPasses(t *testing.T) {
	t.Parallel()
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = time.Minute, 50*time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "unknown null" {
		t.Fatalf("read with the listener down: %s, want unknown null", got)
	}
	roster.down.Store(false)
	roster.set("ses-sre", "sre")
	time.Sleep(100 * time.Millisecond)
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("read after the silence window: %s, want live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a silence window apart made %d listener calls, want 2", calls)
	}
}

// A listener that does not answer holds a fetch only for the fetch's own bound, and the reads
// after it read unknown at once without asking again. The listener client's window is that bound
// and a millisecond, so a fetch that held its call past the bound it was given is ended by the
// client's window instead, whatever the machine's speed, and the error says which ended it: the
// fetch's own context (listRouteRoster) takes its deadline before the call begins and the window
// starts with the call, so the call's context is the sooner of the two. Either end is
// context.DeadlineExceeded; a call the window ended is also envoy.ErrCallWindow, which the client
// adds only when its caller's context had not ended. The chain is read, not the text: the listener
// client prints a call's failure by its class (redact.Cause), "timed out" for either. Whether a
// later read asked again is read off the stored answer, not off the listener's count of calls: a
// read that asked would have stored a silence of its own, sent later, while a call a loaded machine
// sends past the half-second bound reaches the listener late or not at all, so a count read after
// the read returns can miss it.
func TestAListenerThatDoesNotAnswerHoldsAReadOnlyForTheFetchBound(t *testing.T) {
	t.Parallel()
	const bound = 500 * time.Millisecond
	listenerURL, _, _ := heldRosterListener(t)
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: bound + time.Millisecond})
	served.rosters.ttl, served.rosters.timeout, served.rosters.silenceTTL = time.Minute, bound, time.Minute

	roster := readRoster(t, served, context.Background())
	if roster.answered {
		t.Fatal("a read answered a roster though the listener never answered")
	}
	if !errors.Is(roster.err, context.DeadlineExceeded) || errors.Is(roster.err, envoy.ErrCallWindow) {
		t.Fatalf("the first read's fetch ended with %v, want its own %s bound's %v, not the listener client's window (%v)", roster.err, bound, context.DeadlineExceeded, envoy.ErrCallWindow)
	}
	sent, _ := storedRosterTimes(t, served)
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if again, _ := storedRosterTimes(t, served); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window asked the listener again: the stored answer was sent at %s, after the first read's silence sent at %s", again, sent)
	}
}

// timedRosterServer is a test server on a database of its own whose listener answers GET
// /v1/sessions with ses-sre holding sre after delay, or, when hang is set, not until the test
// ends. calls counts the requests that reached it. The listener client's own window is a minute,
// so only the fetch's bound ends a call early. lifetime is the lifetime the server runs on, nil
// for the test's own context.
func timedRosterServer(t *testing.T, delay time.Duration, hang bool, lifetime context.Context) (*server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	gate := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if hang {
			<-gate
		}
		time.Sleep(delay)
		_, _ = w.Write([]byte(`[{"session_id":"ses-sre","title":"worker","roles":["sre"]}]`))
	}))
	// Cleanups run last first: the gate opens, then the close waits for the requests it freed.
	t.Cleanup(listener.Close)
	t.Cleanup(func() { close(gate) })
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Minute, lifetime: lifetime})
	return served, calls
}

// An answer's TTL counts from the moment its call was sent, so a slow listener cannot stretch the
// age of the answers a read is served past the TTL. A 1 s fetch with a 1.5 s TTL is stored
// expiring exactly 1.5 s after its call was sent, both on the database's clock (sent_at, the
// database's now() before the call, and expires_at); a TTL counted from the answer would expire a
// second or more later. Once the answer has expired, by the database's clock it expires on, a
// read asks again.
func TestAnAnswersTTLCountsFromWhenItsCallWasSent(t *testing.T) {
	t.Parallel()
	const delay, ttl = time.Second, 1500 * time.Millisecond
	served, calls := timedRosterServer(t, delay, false, nil)
	served.rosters.ttl, served.rosters.silenceTTL, served.rosters.timeout = ttl, time.Minute, time.Minute
	if !readRoster(t, served, context.Background()).answered {
		t.Fatal("the first read did not answer")
	}
	if sent, expires := storedRosterTimes(t, served); expires.Sub(sent) != ttl {
		t.Fatalf("the answer expires %s after its call was sent (at %s), want the %s TTL: the TTL must count from when the call was sent", expires.Sub(sent), sent, ttl)
	}
	testwait.UntilPolling(t, 20*time.Millisecond, "the stored answer to expire by the database's clock (whether it has)", func() (bool, bool) {
		var expired bool
		if err := served.deps.Store.Pool.QueryRow(context.Background(), `select now() >= expires_at from route_roster`).Scan(&expired); err != nil {
			t.Fatalf("read whether the stored answer has expired: %v", err)
		}
		return expired, expired
	})
	_ = readRoster(t, served, context.Background())
	if got := calls.Load(); got != 2 {
		t.Fatalf("a read once the 1.5 s TTL of a 1 s fetch had run out made %d listener calls in all, want 2", got)
	}
}

// A failure's window counts from the moment the failure is known, so a listener that hangs is left
// alone for the whole window after its call times out. A call bound at 1 s with a minute's window
// is stored expiring at least a minute and a second after the call was sent, on the database's
// clock, where a window counted from the send would expire a second sooner; and a read inside the
// window asks the listener nothing. That read is inside the minute however slow the machine. The
// listener sees at most the first read's call, which a loaded machine can deliver late or not at
// all, so the count is held to one or fewer; the stored answer, still the first read's silence,
// says the read made no fetch of its own whether its call would have arrived or not.
func TestAFailuresWindowCountsFromWhenTheFailureIsKnown(t *testing.T) {
	t.Parallel()
	const timeout, window = time.Second, time.Minute
	served, calls := timedRosterServer(t, 0, true, nil)
	served.rosters.ttl, served.rosters.silenceTTL, served.rosters.timeout = time.Minute, window, timeout
	if readRoster(t, served, context.Background()).answered {
		t.Fatal("a listener that hangs answered")
	}
	sent, expires := storedRosterTimes(t, served)
	if left := expires.Sub(sent); left < timeout+window {
		t.Fatalf("the silence expires %s after its call was sent, want at least the call's %s bound and the %s window: the window must count from when the call timed out", left, timeout, window)
	}
	if readRoster(t, served, context.Background()).answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if got := calls.Load(); got > 1 {
		t.Fatalf("two reads, the second inside the silence window, made %d listener calls in all, want at most the first read's 1", got)
	}
	if again, _ := storedRosterTimes(t, served); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window asked the listener again: the stored answer was sent at %s, after the first read's silence sent at %s", again, sent)
	}
}

// A shutdown ends a fetch in flight: it runs on the server's lifetime, so a read waiting on a
// listener that hangs returns unknown once the lifetime ends, not when the fetch's bound (a minute
// here) runs out. What ended the fetch is in the answer's error chain: the lifetime's cancel, where
// the fetch's bound or the client's window would each be a deadline. The chain is read, not the
// text, which names a cancel by its class alone, "the request failed" (redact.Cause).
func TestAShutdownEndsAFetchInFlight(t *testing.T) {
	t.Parallel()
	lifetime, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	served, calls := timedRosterServer(t, 0, true, lifetime)
	served.rosters.timeout = time.Minute
	answer := make(chan routeRoster, 1)
	go func() { answer <- readRoster(t, served, context.Background()) }()
	waitUntil(t, "the read's fetch to reach the listener that hangs", func() bool { return calls.Load() >= 1 })
	shutdown()
	roster := testwait.Receive(t, "the answer of a read waiting on a listener that hangs, once the shutdown began", answer)
	if roster.answered {
		t.Fatal("a read answered a roster after the shutdown")
	}
	if !errors.Is(roster.err, context.Canceled) {
		t.Fatalf("the read's fetch ended with %v, want the shutdown's %v: a fetch the shutdown did not end ran out of a bound instead", roster.err, context.Canceled)
	}
}

// Reads that miss a cold cache together share one listener call: each waits for the one fetch
// rather than starting its own.
func TestConcurrentReadsOnAColdCacheMakeOneListenerCall(t *testing.T) {
	t.Parallel()
	served, calls, release := heldRosterServer(t)
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
	// Every reader has had time to reach the listener if it were going to: a cache without a
	// shared fetch shows more than one call here.
	time.Sleep(200 * time.Millisecond)
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
	t.Parallel()
	served, calls, release := heldRosterServer(t)
	read := func(ctx context.Context) <-chan routeRoster {
		answer := make(chan routeRoster, 1)
		// A read whose request ends may end before its read of the stored answer does, which
		// reads unknown as well.
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
	time.Sleep(50 * time.Millisecond)

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
