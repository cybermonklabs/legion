package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The background wait holds for the work a request left running past its client (detached): a
// broadcast's remaining deliveries, a keyed write's handler. Production shutdown calls it before
// it closes the database pool, so a wait that skipped them would close the pool under a worker
// still writing.
func TestTheBackgroundWaitHoldsForWorkARequestLeftRunning(t *testing.T) {
	t.Parallel()
	served := register(http.NewServeMux(), t.Context(), Deps{})
	started, release := make(chan struct{}), make(chan struct{})
	served.detached.goDetached(func() {
		close(started)
		<-release
	})
	<-started

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		served.waitBackground()
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a detached worker was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(concurrentWait):
		t.Fatal("the background wait never returned once the detached worker ended")
	}
}

// The background wait holds for every live socket's handler, and so every subscription it ran,
// until the socket ends: production shutdown waits for it before it closes the database pool,
// which a subscription still reading would otherwise lose under it.
func TestTheBackgroundWaitHoldsForAnOpenLiveSocket(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	rig.sockets(t, 1)

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		rig.served.waitBackground()
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a live socket's handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	_ = client.conn.Close()
	select {
	case <-waited:
	case <-time.After(concurrentWait):
		t.Fatal("the background wait never returned once the live socket closed")
	}
}

// The background wait holds for the event stream relay while an events subscription reads it, and
// returns once that subscription leaves: the relay's generation stops with its last subscriber,
// so nothing of it is left for the lifetime to end.
func TestTheBackgroundWaitHoldsForTheEventStreamRelay(t *testing.T) {
	t.Parallel()
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	close(stream.gate)
	running := served.startEventsSubscription(ctx, stream, "feed", nil)
	t.Cleanup(running.stop)
	pollUntil(t, concurrentWait, "the subscription subscribes to the relay", func() bool {
		return relaySubscribers(served.eventRelay) == 1
	})
	requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t, served, "the event stream relay", func() {
		running.stop()
		pollUntil(t, concurrentWait, "the stopped subscription leaves the relay", func() bool {
			return relaySubscribers(served.eventRelay) == 0
		})
	})
}

// The background wait holds for the delivery hub while a view reads it, and returns once that
// view closes: the hub's generation stops with its last view.
func TestTheBackgroundWaitHoldsForTheDeliveryHub(t *testing.T) {
	t.Parallel()
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	seedHubSettings(t, database)
	conn := newFakeLiveConn()
	if !served.deliveryHub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, ""))) {
		t.Fatal("the hub refused the view")
	}
	pollUntil(t, eventFeedWait, "the hub answers the view", func() bool { return len(conn.frames) > 0 })
	requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t, served, "the delivery hub", func() {
		served.deliveryHub.Close(conn)
	})
}

// requireBackgroundWaitHoldsUntilTheSubscriberLeaves requires served's background wait to hold
// while what (the relay or the hub) still runs for an open subscription, and to return once leave
// has taken that subscription away, without the server's lifetime ending.
//
// The hold runs from where the product says the wait stands, not from the moment the goroutine
// above was started: deliveryHub.wait marks the hub closed under its lock and only then waits on
// every generation's goroutines, and waitBackground reaches the hub after the detached work and
// the live sockets, so a wait that has marked it has entered the stretch that must hold - the
// hub's own generation for one caller, the relay behind it for the other. A runner that had not
// scheduled that goroutine inside the fixed 200 ms this replaced left the row asserting nothing.
//
// The return takes concurrentWait, this package's give-up bound for an answer another goroutine
// owes (server_test.go), and no span this row measures: the wait returns once the generation's
// goroutines have ended, its LISTEN connection has closed and the relay subscription it held has
// been cancelled with the relay's own generation behind it, all of it work whose pace is the
// machine's. A wait that never returns still fails the row.
func requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t *testing.T, served *server, what string, leave func()) {
	t.Helper()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		served.waitBackground()
	}()
	pollUntil(t, concurrentWait, "the background wait reaches "+what, func() bool {
		return hubClosed(served.deliveryHub)
	})
	select {
	case <-waited:
		t.Fatalf("the background wait returned while %s still ran for an open subscription", what)
	case <-time.After(backgroundWaitHold):
	}
	leave()
	select {
	case <-waited:
	case <-time.After(concurrentWait):
		t.Fatalf("the background wait never returned once %s's last subscriber left", what)
	}
}

// backgroundWaitHold is how long the row above gives the background wait to return when it must
// not, counted from the hub's own report that the wait reached it. A negative needs a span, and
// this is the one here; what it has to outlast is a goroutine hand-over and nothing else, since
// waitBackground times nothing - it is a chain of waits on counts of running work, so a wait that
// does not hold returns just after that report rather than late. Two hundred milliseconds is
// orders of magnitude over such a hand-over on a runner swapping two dozen tests over four cores,
// and load can only make it safer: the row fails when the wait returns inside it, never when the
// span passes quietly.
const backgroundWaitHold = 200 * time.Millisecond

// hubClosed reports whether the background wait has reached the delivery hub, which is where
// deliveryHub.wait sets closed under mu before it waits on the generations' goroutines.
func hubClosed(hub *deliveryHub) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.closed
}

// A server with no lifetime is refused where it is built, not left running the hub, the relay and
// every live socket on a context that never ends: each would outlive the process's shutdown, or
// the test that built it, and the background wait would never return once a subscription had
// started the relay.
func TestRegisterRefusesANilLifetime(t *testing.T) {
	t.Parallel()
	defer func() {
		refusal, ok := recover().(string)
		if !ok || !strings.Contains(refusal, "the lifetime parameter is required") {
			t.Fatalf("api.Register with no lifetime panicked with %v, want the refusal naming the parameter", refusal)
		}
	}()
	//nolint:staticcheck // the nil lifetime is what this test passes.
	Register(http.NewServeMux(), nil, Deps{})
	t.Fatal("api.Register with no lifetime returned, want a panic")
}
