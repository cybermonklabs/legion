package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

// readAgents is served's readAgentsRoster, the read an agents list makes, for a read whose
// database answers: a failed read fails the test. It reports with Errorf, so a read on a goroutine
// of its own may use it.
func readAgents(t *testing.T, served *server, ctx context.Context) routeRoster {
	t.Helper()
	roster, err := served.readAgentsRoster(ctx)
	if err != nil {
		t.Errorf("read the route roster for an agents list: %v", err)
	}
	return roster
}

// awaitRoster waits for served's refresh loop to store an answer within its bound and answers it
// as an issue read is served it: the listener's list, or its stored silence. An issue read on a
// cold process waits on no fetch - it reads every route unknown at once (routeRosterPastBound) -
// so a row that reads the listener's answer through the copy waits for the loop's first fetch
// with this. Its first read starts the loop. Its one bound is the test binary's deadline
// (testwait.UntilPolling), and its failure names the error the last read was served.
func awaitRoster(t *testing.T, served *server) routeRoster {
	t.Helper()
	var roster routeRoster
	testwait.UntilPolling(t, 5*time.Millisecond, "the refresh loop to store a listener answer within its bound (the last read's error)", func() (error, bool) {
		roster = readRoster(t, served, context.Background())
		return roster.err, !errors.Is(roster.err, errRouteRosterPastBound)
	})
	return roster
}

// storeRosterSentAgo stores, as the refresh loop does, an answer listing ses-sre holding sre
// whose call was sent ago before the database's now(), under served's TTL: within its bound while
// ago is under the TTL, past it otherwise. It becomes the process's built roster, so the process
// is no longer cold.
func storeRosterSentAgo(t *testing.T, served *server, ago time.Duration) {
	t.Helper()
	var now time.Time
	if err := served.deps.Store.Pool.QueryRow(context.Background(), `select now()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	answer := buildRouteRoster([]envoy.Session{{SessionID: "ses-sre", Title: "worker", Roles: []string{"sre"}, State: envoy.StateLive}})
	if stored := served.storeRouteRoster(context.Background(), served.deps.Store.Pool, now.Add(-ago), answer); !stored.answered {
		t.Fatalf("store the listener answer: %v", stored.err)
	}
}

// heldRosterServer is a test server on a database of its own whose listener's GET /v1/sessions
// lists ses-sre holding sre, but only once release is called: until then every call waits, so a
// test can hold one fetch open while other reads arrive. calls counts the requests that reached
// it. The fetch's bound, the listener client's window and the TTL are each past any run
// (pastTheRun), so however long a loaded machine takes to release the fetch, it reads no timeout
// as unknown and no expiry calls again, and a read that waited on the fetch would not answer
// before the test binary's deadline.
func heldRosterServer(t *testing.T) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	_, _, _, served = newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: pastTheRun})
	served.rosters.ttl, served.rosters.timeout = pastTheRun, pastTheRun
	return served, calls, release
}

// heldRosterMux is heldRosterServer mounted on mux over deps, whose Envoy it sets to the held
// listener's client: deps carries the Store the stored answer is read from, and a test that serves
// requests through mux gives it an Identity too.
func heldRosterMux(t *testing.T, mux *http.ServeMux, deps Deps) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	listenerURL, calls, release := heldRosterListener(t)
	deps.Envoy = envoy.New(listenerURL, envoy.WithTimeout(pastTheRun))
	served = register(mux, t.Context(), deps)
	served.rosters.ttl, served.rosters.timeout = pastTheRun, pastTheRun
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
// the contract. The reads come once the refresh loop has stored its first answer (awaitRoster),
// since a cold process's first issue read waits on no fetch.
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
	awaitRoster(t, served)

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

// An answer past its TTL is replaced without any read asking: the refresh loop asks the listener
// ahead of each answer's expiry, so once the listener's answer changes, a read a TTL later reads
// the new answer and never the old one. The 50 ms TTL is refreshed every 25 ms or so (half the
// TTL, under the fetch bound), and a read between a copy's expiry and its successor's store
// finds no answer within its bound and reads unknown; once twice the TTL has passed since the
// listener stopped listing ses-sre, no copy that lists it is still within its bound.
func TestAnIssueReadAfterTheTTLReadsTheListenersNewAnswer(t *testing.T) {
	t.Parallel()
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = 50 * time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := sreReach(awaitRoster(t, served)); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	ended := time.Now()
	// Twice the TTL: every copy that lists ses-sre was sent before the listener stopped listing it,
	// so each has passed its bound by now, however late the reads below come.
	time.Sleep(100 * time.Millisecond)
	testwait.UntilPolling(t, 5*time.Millisecond, "a read to read the listener's new answer, no_holder null (the last read)", func() (string, bool) {
		got := detailReach(t, handler, key)
		if got == "live ses-sre" {
			t.Fatalf("a read %v after the listener stopped listing ses-sre, past twice the 50 ms TTL, read the old live ses-sre", time.Since(ended))
		}
		return got, got == "no_holder null"
	})
	if calls := roster.lookups.Load() - before; calls < 2 {
		t.Fatalf("reads a TTL apart saw %d listener calls, want the refresh to have asked again", calls)
	}
}

// A listener that fails is asked once within the silence window, and every read in it answers
// as an unreachable listener always has: unknown on the detail, 503 for a judged filter. The
// answered-roster TTL is zero here, so a store that kept the silence for that TTL would leave no
// answer within its bound for awaitRoster to find, and a refresh that waited out that TTL would
// ask again.
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
	awaitRoster(t, served)

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

// A cached failure lasts only its silence window: once it has passed, the refresh asks the
// listener again and the reads read its answer. The answered TTL is a minute, so a cache that held
// a failure for that TTL, or for ever, would still read unknown when the row gives up.
func TestAListenerThatFailedIsAskedAgainOnceItsSilenceWindowPasses(t *testing.T) {
	t.Parallel()
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = time.Minute, 50*time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := sreReach(awaitRoster(t, served)); got != "unknown null" {
		t.Fatalf("read with the listener down: %s, want unknown null", got)
	}
	roster.down.Store(false)
	roster.set("ses-sre", "sre")
	testwait.UntilPolling(t, 5*time.Millisecond, "the reads to read live ses-sre once the listener came back and its 50 ms silence window passed (the last read)", func() (string, bool) {
		got := detailReach(t, handler, key)
		return got, got == "live ses-sre"
	})
	if calls := roster.lookups.Load() - before; calls < 2 {
		t.Fatalf("reads a silence window apart saw %d listener calls, want the listener asked again", calls)
	}
}

// A listener that does not answer holds the refresh's fetch only for the fetch's own bound, not
// the client's window, and holds no read at all: a read while that fetch runs reads unknown, the
// fetch's silence is stored once the bound has passed, and the reads inside it read unknown without
// asking again. The listener client's window is that bound and a millisecond, so a fetch that held
// its call past the bound it was given is ended by the client's window instead, whatever the
// machine's speed, and the error says which ended it: the fetch's own context (listRouteRoster)
// takes its deadline before the call begins and the window starts with the call, so the call's
// context is the sooner of the two, net/http sets no deadline of its own and its error says
// "context deadline exceeded"; a call the window ended says "Client.Timeout exceeded" (net/http's
// setRequestCancel). That error is the fetch's own, which no read is served (a stored silence is
// read as errListenerSilence), so the row makes the fetch's read itself, before any read starts the
// loop. How long a read takes says nothing here, the fetch ending at its half-second bound; that a
// read waits on no fetch is decided by order where the listener holds every call past any run
// (TestReadsPastTheBoundAnswerAtOnceWhileTheListenerIsHeld). Whether a later read asked again is
// read off the stored answer, not off the listener's count of calls: a fetch asked for again would
// have stored a silence of its own, sent later, while a call a loaded machine sends past the
// half-second bound reaches the listener late or not at all, so a count read after the read
// returns can miss it.
func TestAListenerThatDoesNotAnswerHoldsTheFetchOnlyForItsBound(t *testing.T) {
	t.Parallel()
	const bound = 500 * time.Millisecond
	listenerURL, calls, _ := heldRosterListener(t)
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: bound + time.Millisecond})
	served.rosters.ttl, served.rosters.timeout, served.rosters.silenceTTL = time.Minute, bound, time.Minute

	if own := served.listRouteRoster(context.Background()); own.answered || own.err == nil ||
		!strings.Contains(own.err.Error(), context.DeadlineExceeded.Error()) || strings.Contains(own.err.Error(), "Client.Timeout exceeded") {
		t.Fatalf("a fetch of a listener that does not answer ended with answered=%t err=%v, want its own %s bound's %v, not the listener client's window",
			own.answered, own.err, bound, context.DeadlineExceeded)
	}
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read answered a roster though the listener never answered")
	}
	if roster := awaitRoster(t, served); roster.answered {
		t.Fatal("the stored answer of a fetch the listener never answered is an answer")
	}
	sent, _ := storedRosterTimes(t, served)
	if roster := readRoster(t, served, context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if got := calls.Load(); got > 2 {
		t.Fatalf("the listener saw %d calls, want at most two, the fetch made here and the refresh's: a read inside the silence window had it asked again", got)
	}
	if again, _ := storedRosterTimes(t, served); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window had the listener asked again: the stored answer was sent at %s, after the first fetch's silence sent at %s", again, sent)
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
// age of the answers a read is served past the TTL: the answer is stored to expire the TTL after
// its send, and the refresh turn whose fetch the listener held past the answer's refresh point (the
// TTL less the lead, here half of it, after the send) asks for its next turn at once, where a TTL
// counted from the answer would sleep that span again after the answer; the next turn then finds
// the stored answer with the lead or less left and asks the listener again. The row runs the
// refresh's turns itself (refreshRouteRosterTurn, whose answer the loop sleeps), so the sleep a
// turn asks for is read rather than timed, and the listener holds the first fetch until its
// refresh point has passed, however long the machine takes to get there: the order of the hold
// and the answer decides, and no margin of the clock's.
func TestAnAnswersTTLCountsFromWhenItsCallWasSent(t *testing.T) {
	t.Parallel()
	const ttl = time.Second
	served, calls, release := heldRosterServer(t)
	served.rosters.ttl = ttl
	lead := served.rosters.refreshLead(true)
	turn := func() <-chan time.Duration {
		next := make(chan time.Duration, 1)
		go func() { next <- served.refreshRouteRosterTurn(context.Background()) }()
		return next
	}

	first := turn()
	awaitCalls(t, calls, 1)
	// The turn counted its call sent before the call reached the listener, so a hold of the
	// span to the refresh point from the arrival holds the fetch past that point.
	time.Sleep(ttl - lead)
	release()
	if next := testwait.Receive(t, "the refresh turn whose fetch the listener held past its refresh point", first); next != 0 {
		t.Fatalf("the turn whose fetch was held past its refresh point, %v after its send, sleeps %v before the next turn, want 0: an answer whose TTL counts from its send is due again at once, where one counted from the answer sleeps %v after it",
			ttl-lead, next, ttl-lead)
	}
	var answered bool
	var life float64
	if err := served.deps.Store.Pool.QueryRow(context.Background(),
		`select answered, extract(epoch from expires_at - sent_at)::float8 from route_roster`).Scan(&answered, &life); err != nil {
		t.Fatalf("read the stored answer: %v", err)
	}
	if !answered || life != ttl.Seconds() {
		t.Fatalf("the stored answer is answered=%t and expires %v s after its call was sent, want the listener's list and the %v TTL", answered, life, ttl)
	}
	testwait.Receive(t, "the next refresh turn", turn())
	if got := calls.Load(); got != 2 {
		t.Fatalf("after the next refresh turn the listener saw %d calls, want 2: an answer sent longer ago than its refresh point has the lead or less left and is fetched again", got)
	}
}

// A failure's window counts from the moment the failure is known, so a listener that hangs is left
// alone for the whole window after its call times out. A call bound at 1 s with a minute's window
// is stored expiring at least a minute and a second after the call was sent, on the database's
// clock, where a window counted from the send would expire a second sooner; and a read inside the
// window is served that stored silence and asks the listener nothing, the refresh's next call
// being due a lead before the window ends. That read is inside the minute however slow the
// machine. The listener sees at most the refresh's one call, which a loaded machine can deliver
// late or not at all, so the count is held to one or fewer; the stored answer, still the first
// fetch's silence, says nothing has fetched since whether a call would have arrived or not.
func TestAFailuresWindowCountsFromWhenTheFailureIsKnown(t *testing.T) {
	t.Parallel()
	const timeout, window = time.Second, time.Minute
	served, calls := timedRosterServer(t, 0, true, nil)
	served.rosters.ttl, served.rosters.silenceTTL, served.rosters.timeout = time.Minute, window, timeout
	if awaitRoster(t, served).answered {
		t.Fatal("a listener that hangs answered")
	}
	sent, expires := storedRosterTimes(t, served)
	if left := expires.Sub(sent); left < timeout+window {
		t.Fatalf("the silence expires %s after its call was sent, want at least the call's %s bound and the %s window: the window must count from when the call timed out", left, timeout, window)
	}
	if roster := readRoster(t, served, context.Background()); roster.answered || errors.Is(roster.err, errRouteRosterPastBound) {
		t.Fatalf("a read inside the silence window was served answered=%t err=%v, want the stored silence of the call that timed out", roster.answered, roster.err)
	}
	if got := calls.Load(); got > 1 {
		t.Fatalf("the listener saw %d calls with a read inside the silence window, want at most the refresh's 1", got)
	}
	if again, _ := storedRosterTimes(t, served); !again.Equal(sent) {
		t.Fatalf("a read inside the silence window had the listener asked again: the stored answer was sent at %s, after the first fetch's silence sent at %s", again, sent)
	}
}

// A shutdown ends a fetch in flight: it runs on the server's lifetime, so a cold process's agents
// list waiting on the refresh's fetch of a listener that hangs returns unanswered once the lifetime
// ends, not when the fetch's bound (a minute here) runs out. What ended the fetch is in the
// answer's error, which the list is handed as the fetch's own: the lifetime's cancel, where the
// fetch's bound or the client's window would each read as a deadline.
func TestAShutdownEndsAFetchInFlight(t *testing.T) {
	t.Parallel()
	lifetime, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	served, calls := timedRosterServer(t, 0, true, lifetime)
	served.rosters.timeout = time.Minute
	answer := make(chan routeRoster, 1)
	go func() { answer <- readAgents(t, served, context.Background()) }()
	awaitCalls(t, calls, 1)
	shutdown()
	roster := testwait.Receive(t, "the answer of an agents list waiting on a listener that hangs, once the shutdown began", answer)
	if roster.answered {
		t.Fatal("an agents list answered a roster after the shutdown")
	}
	if roster.err == nil || !strings.Contains(roster.err.Error(), context.Canceled.Error()) {
		t.Fatalf("the agents list's fetch ended with %v, want the shutdown's %v: a fetch the shutdown did not end ran out of a bound instead", roster.err, context.Canceled)
	}
}

// Reads that miss a cold cache together share one listener call, and none of them waits on it but
// a cold process's agents list: every issue read answers at once with no answer within its bound,
// and every agents list waits for the refresh loop's one fetch rather than starting its own.
func TestConcurrentReadsOnAColdCacheMakeOneListenerCall(t *testing.T) {
	t.Parallel()
	served, calls, release := heldRosterServer(t)
	const readers = 16
	lists := make([]routeRoster, readers)
	issueReads := make(chan routeRoster, readers)
	var started, done sync.WaitGroup
	started.Add(2 * readers)
	done.Add(readers)
	for index := range readers {
		go func() {
			started.Done()
			issueReads <- readRoster(t, served, context.Background())
		}()
		go func() {
			defer done.Done()
			started.Done()
			lists[index] = readAgents(t, served, context.Background())
		}()
	}
	started.Wait()
	for range readers {
		roster := testwait.Receive(t, "an issue read on a cold cache while the listener holds the refresh's one fetch", issueReads)
		if !errors.Is(roster.err, errRouteRosterPastBound) {
			t.Fatalf("an issue read on a cold cache read %s (%v), want no answer within its bound", sreReach(roster), roster.err)
		}
	}
	awaitCalls(t, calls, 1)
	// Every reader has had time to reach the listener if it were going to: a cache without one
	// shared fetch shows more than one call here.
	time.Sleep(200 * time.Millisecond)
	release()
	done.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d concurrent issue reads and %d agents lists on a cold cache made %d listener calls, want 1", readers, readers, got)
	}
	for index, roster := range lists {
		if got := sreReach(roster); got != "live ses-sre" {
			t.Fatalf("agents list %d read %s, want the shared answer live ses-sre", index, got)
		}
	}
}

// A cold process's agents list whose request ends while it waits on the refresh's fetch returns
// at once, reading the listener as unanswered, whether it asked for that fetch or joined it; the
// fetch runs on for the other lists, and its answer is the one the cache keeps.
func TestAColdAgentsListWhoseRequestEndsWhileItWaitsReturnsPromptly(t *testing.T) {
	t.Parallel()
	served, calls, release := heldRosterServer(t)
	read := func(ctx context.Context) <-chan routeRoster {
		answer := make(chan routeRoster, 1)
		// A list whose request ends may end before its read of the stored answer does, which
		// reads unanswered as well.
		go func() {
			roster, _ := served.readAgentsRoster(ctx)
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
	for name, answer := range map[string]<-chan routeRoster{"the list that asked for the fetch": leader, "a list that joined it": joiner} {
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
		t.Fatal("a list whose request is still open returned before the listener answered")
	default:
	}

	release()
	if got := sreReach(testwait.Receive(t, "the answer of the list that waited, once the listener answered", patient)); got != "live ses-sre" {
		t.Fatalf("the list that waited read %s, want live ses-sre", got)
	}
	if got := sreReach(readRoster(t, served, context.Background())); got != "live ses-sre" {
		t.Fatalf("the cached answer reads %s, want live ses-sre: the fetch whose lists left must still fill the cache", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("four reads made %d listener calls, want 1", got)
	}
}

// routedIssueRead is one issue read's route reach, as reachText writes it, with its reason.
func routedIssueRead(t *testing.T, answer timedAnswer) string {
	t.Helper()
	var issue routedIssue
	if err := json.Unmarshal([]byte(answer.body), &issue); err != nil {
		t.Fatalf("decode the issue read: %v: %s", err, answer.body)
	}
	reason := "null"
	if issue.RouteStatusReason != nil {
		reason = *issue.RouteStatusReason
	}
	return reachText(issue.RouteStatus, issue.RouteHolder) + " " + reason
}

// answeredWhileHeld serves request on a goroutine of its own and answers how it was answered,
// waiting for that with testwait.Receive. Its row's listener holds every call, and the fetch's
// bound and the listener client's window are past any run, so a read that waited on the listener
// would not answer before the test binary's deadline: the answer coming at all is what proves the
// read waited on nothing, not how long it took.
func answeredWhileHeld(t *testing.T, handler http.Handler, request *http.Request, what string) timedAnswer {
	t.Helper()
	answered := make(chan timedAnswer, 1)
	go func() { answered <- serveTimed(handler, request) }()
	return testwait.Receive(t, what, answered)
}

// A read never waits on the listener while the stored copy is within its bound: with the listener
// holding every call, an issue read and an agents list are answered from the copy, and neither they
// nor the refresh loop they start asks the listener, the copy having more than the refresh's lead
// left (half its TTL, which is past any run).
func TestReadsWithinTheBoundAreAnsweredFromTheCopyWhileTheListenerIsHeld(t *testing.T) {
	t.Parallel()
	listenerURL, calls, _ := heldRosterListener(t)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: pastTheRun})
	served.rosters.ttl, served.rosters.timeout = pastTheRun, pastTheRun
	key := routedIssueKey(t, handler, "role:sre")
	storeRosterSentAgo(t, served, 0)

	issue := answeredWhileHeld(t, handler, testRequestAs(t, http.MethodGet, "/api/v1/issues/"+key, "alice"),
		"an issue read within the copy's bound while the listener holds every call")
	if issue.status != http.StatusOK {
		t.Fatalf("an issue read within the copy's bound: status=%d body=%s, want 200", issue.status, issue.body)
	}
	if got := routedIssueRead(t, issue); got != "live ses-sre null" {
		t.Fatalf("an issue read within the copy's bound reads %s, want the copy's live ses-sre", got)
	}
	agents := answeredWhileHeld(t, handler, testRequestAs(t, http.MethodGet, "/api/v1/agents", "alice"),
		"an agents list within the copy's bound while the listener holds every call")
	if agents.status != http.StatusOK || !strings.Contains(agents.body, `"session_id":"ses-sre"`) {
		t.Fatalf("an agents list within the copy's bound: status=%d body=%s, want 200 listing ses-sre", agents.status, agents.body)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("reads within the copy's bound had the listener asked %d times, want none", got)
	}
}

// A read past the copy's bound waits on nothing either, the listener holding the refresh's fetch:
// an issue read answers with every route unknown for the unreachable reason, a judged route_status
// filter refuses 503 ENVOY_UNAVAILABLE, and an agents list on a process that has built a copy
// answers 503 ENVOY_UNAVAILABLE saying the copy is past its bound, each while the listener holds
// that fetch past any run; and the refresh asks the listener once for them all.
func TestReadsPastTheBoundAnswerAtOnceWhileTheListenerIsHeld(t *testing.T) {
	t.Parallel()
	listenerURL, calls, _ := heldRosterListener(t)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: pastTheRun})
	served.rosters.ttl, served.rosters.timeout = pastTheRun, pastTheRun
	key := routedIssueKey(t, handler, "role:sre")
	// Sent twice the TTL ago: the copy expired a TTL ago, and this process built it, so it is no
	// longer cold.
	storeRosterSentAgo(t, served, 2*pastTheRun)

	issue := answeredWhileHeld(t, handler, testRequestAs(t, http.MethodGet, "/api/v1/issues/"+key, "alice"),
		"an issue read past the copy's bound while the listener holds the refresh's fetch")
	if issue.status != http.StatusOK {
		t.Fatalf("an issue read past the copy's bound: status=%d body=%s, want 200", issue.status, issue.body)
	}
	if got := routedIssueRead(t, issue); got != "unknown null unreachable" {
		t.Fatalf("an issue read past the copy's bound reads %s, want unknown null unreachable", got)
	}
	awaitCalls(t, calls, 1)
	filtered := answeredWhileHeld(t, handler, testRequestAs(t, http.MethodGet, "/api/v1/issues?project=TEST&route_status=no_holder", "alice"),
		"a judged route_status filter past the copy's bound while the listener holds the refresh's fetch")
	if filtered.status != http.StatusServiceUnavailable || !strings.Contains(filtered.body, `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("route_status=no_holder past the copy's bound: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", filtered.status, filtered.body)
	}
	agents := answeredWhileHeld(t, handler, testRequestAs(t, http.MethodGet, "/api/v1/agents", "alice"),
		"an agents list past the copy's bound while the listener holds the refresh's fetch")
	if agents.status != http.StatusServiceUnavailable ||
		!strings.Contains(agents.body, `"code":"ENVOY_UNAVAILABLE"`) || !strings.Contains(agents.body, "older than its bound") {
		t.Fatalf("an agents list past the copy's bound: status=%d body=%s, want 503 ENVOY_UNAVAILABLE saying the copy is past its bound",
			agents.status, agents.body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("three reads past the copy's bound had the listener asked %d times, want once, by the refresh", got)
	}
}

// The refresh runs ahead of the copy's bound with no reader: a copy with a 4 s TTL, refreshed a
// lead of 2 s before it expires (half the TTL, under the fetch's bound, which is past any run),
// has its one fetch reach the listener though nothing reads it, once the copy has the lead or less
// left and before it expires, and only one: the listener holds that fetch until the copy is a lead
// past its expiry and no second fetch starts beside it. When the fetch arrived is read off the
// database's clock as the listener sees it, not off the stored answer's sent_at: once the held
// fetch is let go the loop, finding that answer expired, asks again at once, so the stored row may
// already be the second fetch's. A fetch arrives after it was sent, and a turn sends one only once
// the copy has the lead or less left, so an arrival with more than the lead left is a refresh that
// asked at once; that it arrived before the copy expired is the refresh's own promise, kept by the
// lead's two seconds.
func TestTheRefreshStartsOneFetchAheadOfTheBoundWithNoReader(t *testing.T) {
	t.Parallel()
	served, calls, release := heldRosterServer(t)
	const ttl = 4 * time.Second
	served.rosters.ttl = ttl
	lead := served.rosters.refreshLead(true)
	storeRosterSentAgo(t, served, 0)
	_, copyExpires := storedRosterTimes(t, served)
	// As a first read starts it; no read follows.
	served.startRouteRosterRefresh()

	awaitCalls(t, calls, 1)
	var arrived time.Time
	if err := served.deps.Store.Pool.QueryRow(context.Background(), `select now()`).Scan(&arrived); err != nil {
		t.Fatalf("read the database's clock when the fetch arrived: %v", err)
	}
	if left := copyExpires.Sub(arrived); left > lead {
		t.Fatalf("the refresh's fetch arrived with %v of the copy's %v TTL left, want its %v lead or less: the refresh asks ahead of the bound at its lead, not at once", left, ttl, lead)
	} else if left <= 0 {
		t.Fatalf("the refresh's fetch arrived %v after the copy expired, want before, at its %v lead: the refresh runs ahead of the bound with no reader", -left, lead)
	}
	waitUntil(t, "the copy to be a lead past its bound by the database's clock", func() bool {
		var past bool
		if err := served.deps.Store.Pool.QueryRow(context.Background(), `select now() >= $1::timestamptz`, copyExpires.Add(lead)).Scan(&past); err != nil {
			t.Fatalf("read the database's clock: %v", err)
		}
		return past
	})
	if got := calls.Load(); got != 1 {
		t.Fatalf("the listener saw %d calls with the refresh's fetch held past the copy's expiry, want 1: one fetch at a time", got)
	}
	release()
}

// While the listener answers within the fetch bound, the refresh keeps every read's copy within
// its TTL: no read is served an answer whose call was sent a TTL or more before the read, and
// none finds the copy past its bound, while the copy is replaced every two seconds or so. The listener
// names each answer by its call and takes 100 ms over it; the TTL is 4 s and the bound 2 s, so
// each copy is replaced about 1.9 s before it would expire, a margin a loaded machine running the
// race detector still keeps.
func TestAListenerAnsweringWithinTheBoundKeepsEveryReadsCopyWithinTheTTL(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []time.Time
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		sent = append(sent, time.Now())
		call := len(sent)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		_, _ = fmt.Fprintf(w, `[{"session_id":"ses-sre","title":"call-%d","roles":["sre"]}]`, call)
	}))
	t.Cleanup(listener.Close)
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Minute})
	const ttl = 4 * time.Second
	served.rosters.ttl, served.rosters.timeout = ttl, 2*time.Second
	awaitRoster(t, served)

	// The database's clock and this process's are one host's; the slack is for reading them in two
	// processes.
	const slack = 100 * time.Millisecond
	reads, past := 0, 0
	for deadline := time.Now().Add(3 * ttl); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		asked := time.Now()
		roster := readRoster(t, served, context.Background())
		reads++
		if !roster.answered {
			past++
			continue
		}
		var call int
		if _, err := fmt.Sscanf(roster.sessions[0].Title, "call-%d", &call); err != nil {
			t.Fatalf("read the call that answered %q: %v", roster.sessions[0].Title, err)
		}
		mu.Lock()
		callSent := sent[call-1]
		mu.Unlock()
		if age := asked.Sub(callSent); age >= ttl+slack {
			t.Fatalf("a read was served the answer of call %d, sent %v before the read, past the %v TTL", call, age, ttl)
		}
	}
	if past != 0 {
		t.Errorf("%d of %d reads found no answer within its bound while the listener answered within 100 ms of a 2 s bound, want none", past, reads)
	}
	mu.Lock()
	made := len(sent)
	mu.Unlock()
	if made < 3 {
		t.Errorf("the listener saw %d calls over three TTLs, want the copy replaced at least twice ahead of its bound", made)
	}
}

// slowFetchDuration matches the duration a slow-fetch line names.
var slowFetchDuration = regexp.MustCompile(`duration=(\S+)`)

// A fetch whose listener read takes over a second is logged, one line naming how long it took and
// how it ended, since nothing else records the listener's pace: answered with its sessions for a
// listener that answers after 1.2 s, unreachable with the error for one the 1.1 s fetch bound cut
// off. The log is the process's default logger, so the row runs alone.
func TestARosterFetchOverASecondIsLoggedWithItsDurationAndOutcome(t *testing.T) {
	logged := captureLog(t)
	answered, _ := timedRosterServer(t, 1200*time.Millisecond, false, nil)
	answered.rosters.timeout = time.Minute
	cutOff, _ := timedRosterServer(t, 0, true, nil)
	cutOff.rosters.timeout = 1100 * time.Millisecond
	awaitRoster(t, answered)
	awaitRoster(t, cutOff)

	outcomes := map[string]string{}
	for line := range strings.Lines(logged.String()) {
		if !strings.Contains(line, `msg="dispatch: the listener's session list took over a second"`) {
			continue
		}
		match := slowFetchDuration.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("a slow-fetch line names no duration: %s", line)
		}
		took, err := time.ParseDuration(match[1])
		if err != nil || took <= time.Second {
			t.Fatalf("a slow-fetch line names duration %q, want one over a second: %s", match[1], line)
		}
		for _, outcome := range []string{"answered", "unreachable"} {
			if strings.Contains(line, "outcome="+outcome) {
				outcomes[outcome] = line
			}
		}
	}
	if line := outcomes["answered"]; !strings.Contains(line, "sessions=1") {
		t.Errorf("the slow answered fetch's line = %q, want outcome=answered with sessions=1; log:\n%s", line, logged.String())
	}
	if line := outcomes["unreachable"]; !strings.Contains(line, "error=") {
		t.Errorf("the cut-off fetch's line = %q, want outcome=unreachable with its error; log:\n%s", line, logged.String())
	}
}
