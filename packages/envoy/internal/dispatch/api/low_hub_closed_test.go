package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAClosedHubStartsNoGoroutine: the delivery hub counts every goroutine it starts (running),
// and wait, which the server's shutdown calls once its lifetime has ended, marks the hub closed
// and then waits for them all. A goroutine started after that would run past the wait, and its
// Add could meet the Wait, which a WaitGroup forbids. A subscribe that raced a re-check of every
// sub's reader starts one to read its own sub again (Subscribe): here a re-check begins, and the
// hub's lifetime ends and wait returns, while the subscribe resolves its reader, before it takes
// the hub's lock. The subscribe is refused, and its reader is never resolved again. Run it under
// -race too: every Add of running is made under the hub's lock before the close.
//
// Controls: Subscribe takes no notice of the close while a generation stands, and goLocked none
// either; the subscribe joins, starts its re-check, which resolves its reader a second time after
// wait returned, and the test fails on both counts. With Subscribe's alone undone the subscribe
// joins, goLocked still starts nothing, and the test fails on the first.
func TestAClosedHubStartsNoGoroutine(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	alice, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	served := directServer(seed.deps)
	relay := newEventStreamRelay(served, lifetime)
	served.eventRelay = relay
	hub := newDeliveryHub(served, lifetime, newNoonHubClock())
	t.Cleanup(func() {
		cancel()
		hub.wait()
		relay.wait()
	})
	first := newFakeLiveConn()
	hub.Subscribe(first, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Reader: alice})
	first.snapshot(t, 5*time.Second, "t")

	var resolved atomic.Int32
	again := make(chan struct{}, 1)
	resolve := func(context.Context) (model.Actor, error) {
		if resolved.Add(1) > 1 {
			signal(again)
			return alice, nil
		}
		// A re-check of every sub's reader begins after the subscribe read their count, and the
		// server shuts down: its lifetime ends, and wait returns once every goroutine of the hub
		// has.
		hub.mu.Lock()
		hub.readerChecks++
		hub.mu.Unlock()
		cancel()
		hub.wait()
		return alice, nil
	}
	if hub.Subscribe(newFakeLiveConn(), deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve}) {
		t.Error("a subscribe the hub's close overtook joined the hub after it was closed")
	}
	select {
	case <-again:
		t.Fatal("the hub started a re-check of the subscribe's reader after it was closed and waited for")
	case <-time.After(time.Second):
	}
}
