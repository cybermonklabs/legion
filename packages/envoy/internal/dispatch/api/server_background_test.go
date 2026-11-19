package api

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The background wait holds for the work a request left running past its client (detached): a
// broadcast's remaining deliveries, a keyed write's handler. Production shutdown calls it before
// it closes the database pool, so a wait that skipped them would close the pool under a worker
// still writing.
func TestTheBackgroundWaitHoldsForWorkARequestLeftRunning(t *testing.T) {
	// A server with no store runs nothing but the detached worker, all of it in the bubble.
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		select {
		case <-waited:
			t.Fatal("the background wait returned while a detached worker was still running")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-waited:
		default:
			t.Fatal("the background wait never returned once the detached worker ended")
		}
	})
}

// backgroundWaitWhy is why a test holds the background wait quiet for a span: the wait returns on
// its own goroutine once the last piece of work it waits for ends.
const backgroundWaitWhy = "the background wait returns on its own goroutine, and no state says it is still blocked " +
	"rather than about to return, the socket and the database are real I/O (no synctest), and no later action of " +
	"the test is ordered after a return that should not come"

// The background wait holds for every live socket's handler, and so every subscription it ran,
// until the socket ends: production shutdown waits for it before it closes the database pool,
// which a subscription still reading would otherwise lose under it.
func TestTheBackgroundWaitHoldsForAnOpenLiveSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	rig.sockets(t, 1)

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		rig.served.waitBackground()
	}()
	testwait.Quiet(t, "the background wait's return while a live socket's handler still runs", waited, 200*time.Millisecond, backgroundWaitWhy)
	_ = client.conn.Close()
	testwait.Receive(t, "the background wait's return once the live socket closed", waited)
}

// The background wait holds for the event stream relay while an events subscription reads it, and
// returns once that subscription leaves: the relay's generation stops with its last subscriber,
// so nothing of it is left for the lifetime to end.
func TestTheBackgroundWaitHoldsForTheEventStreamRelay(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	close(stream.gate)
	running := served.startEventsSubscription(ctx, stream, "feed", nil)
	t.Cleanup(running.stop)
	testwait.Eventually(t, "the subscription subscribes to the relay", func() bool {
		return relaySubscribers(served.eventRelay) == 1
	})
	requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t, served, "the event stream relay", func() {
		running.stop()
		testwait.Eventually(t, "the stopped subscription leaves the relay", func() bool {
			return relaySubscribers(served.eventRelay) == 0
		})
	})
}

// The background wait holds for the delivery hub while a view reads it, and returns once that
// view closes: the hub's generation stops with its last view.
func TestTheBackgroundWaitHoldsForTheDeliveryHub(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	seedHubSettings(t, database)
	conn := newFakeLiveConn()
	if !served.deliveryHub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, ""))) {
		t.Fatal("the hub refused the view")
	}
	testwait.Eventually(t, "the hub answers the view", func() bool { return len(conn.frames) > 0 })
	requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t, served, "the delivery hub", func() {
		served.deliveryHub.Close(conn)
	})
}

// requireBackgroundWaitHoldsUntilTheSubscriberLeaves requires served's background wait to hold
// while what (the relay or the hub) still runs for an open subscription, and to return once leave
// has taken that subscription away, without the server's lifetime ending.
func requireBackgroundWaitHoldsUntilTheSubscriberLeaves(t *testing.T, served *server, what string, leave func()) {
	t.Helper()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		served.waitBackground()
	}()
	testwait.Quiet(t, "the background wait's return while "+what+" still runs for an open subscription", waited, 200*time.Millisecond, backgroundWaitWhy)
	leave()
	testwait.Receive(t, "the background wait's return once "+what+"'s last subscriber left", waited)
}

// A server with no lifetime is refused where it is built, not left running the hub, the relay and
// every live socket on a context that never ends: each would outlive the process's shutdown, or
// the test that built it, and the background wait would never return once a subscription had
// started the relay.
func TestRegisterRefusesANilLifetime(t *testing.T) {
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
