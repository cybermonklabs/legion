package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The hub's freshness has three writers: the listener's read of the settings row at each
// connection (readFreshness), a delivery_freshness notification (applyFreshness), and the first
// recompute, which sets the first verdict from its own read of the row while !g.known. The first
// two run on the listener's goroutine in the order the row's writes committed; the first recompute
// runs on the worker's, and its read can be older than what the listener wrote while it read. These
// rows hold that recompute between its read and its answer (the "recompute read" note) while the
// listener writes something newer, and want the hub to keep the newer.

// holdFirstRecomputeRead holds the hub's first recompute between its read and its answer: read
// closes as the recompute's read returns, and the recompute answers once release is called. The
// caller registers release as a cleanup once the rig stands, so it runs before the rig's own stop,
// which waits for the held worker: a failure while it holds then lets the hub stop.
func holdFirstRecomputeRead(hub *deliveryHub) (read <-chan struct{}, release func()) {
	reading, releasing := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	observe := hub.observe
	hub.observe = func(note deliveryHubNote) {
		observe(note)
		if note.what == "recompute read" && held.CompareAndSwap(false, true) {
			close(reading)
			<-releasing
		}
	}
	return reading, sync.OnceFunc(func() { close(releasing) })
}

// A reconcile pass whose notification the hub applied while its first recompute was reading is
// the freshness the hub keeps: the recompute's own read, from before the pass, does not roll it
// back. Before this change the first recompute set the whole freshness from its own read, so a
// pass that committed during that read - the settings read and then the population read, which
// can take seconds - stayed unheard until the next freshness change (INFRA-3904).
func TestTheFirstRecomputeKeepsAFreshnessNotificationAppliedDuringItsRead(t *testing.T) {
	t.Parallel()
	var read <-chan struct{}
	var release func()
	rig := newHubRig(t, func(hub *deliveryHub) { read, release = holdFirstRecomputeRead(hub) })
	t.Cleanup(release)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, rig.clock.Now()); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	conn := newFakeLiveConn()
	since := rig.mark()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	rig.waitForEvent(t, since, "the hub's listener to connect", func(note deliveryHubNote) bool { return note.what == "connected" })
	testwait.Receive(t, "the first recompute to read", read)

	passed := rig.clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, passed); err != nil {
		t.Fatalf("record the pass: %v", err)
	}
	// The listener is up, so the pass's notification is applied while the recompute holds.
	rig.waitFreshnessHeard(t, "the hub to hear the pass while its first recompute holds", reconciledAt(passed))
	release()
	conn.snapshot(t, "p")
	rig.waitFreshnessHeard(t, "the hub to keep the pass once its first recompute answered", reconciledAt(passed))
}

// A reconcile pass whose notification no listener heard - it committed before the hub's first
// LISTEN - is the freshness the hub keeps once its listener connects and reads the row, though the
// first recompute read the row before the pass and answers after that connection. Before this
// change the recompute set the whole freshness from its own older read over what the connection
// read (INFRA-3904).
func TestTheFirstRecomputeKeepsTheFreshnessItsListenersConnectionRead(t *testing.T) {
	t.Parallel()
	var read <-chan struct{}
	var release func()
	dialing := make(chan struct{})
	rig := newHubRig(t, func(hub *deliveryHub) {
		read, release = holdFirstRecomputeRead(hub)
		dial := hub.dial
		hub.dial = func(ctx context.Context) (*pgx.Conn, error) {
			select {
			case <-dialing:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return dial(ctx)
		}
	})
	letDial := sync.OnceFunc(func() { close(dialing) })
	t.Cleanup(letDial)
	t.Cleanup(release)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, rig.clock.Now()); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	testwait.Receive(t, "the first recompute to read", read)

	// The pass commits with no LISTEN standing, so its notification reaches nobody.
	passed := rig.clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, passed); err != nil {
		t.Fatalf("record the pass: %v", err)
	}
	since := rig.mark()
	letDial()
	rig.waitForEvent(t, since, "the hub's listener to connect", func(note deliveryHubNote) bool { return note.what == "connected" })
	release()
	conn.snapshot(t, "p")
	rig.waitFreshnessHeard(t, "the hub to keep the pass its connection read once its first recompute answered", reconciledAt(passed))
}

// Once the hub's listener has connected and the row has stopped moving, the freshness the hub holds
// is the row's. The connection's read of the row (readFreshness) and a notification (applyFreshness)
// both write it; were they on two goroutines, a pass whose notification was applied between the
// read's snapshot and its write would be written over by that older snapshot, and stay unheard
// until the next freshness change (INFRA-3904: the hub heard 12:00:00.06 and then wrote its read of
// 12:00:00.004). The first recompute answers before the listener dials here, so it is not one of
// the writers this row reads. Unforced, the window between the read and the write is one query
// wide; the forcing kit commits a pass inside it.
func TestTheHubsFreshnessIsTheRowsOnceItsListenerHasConnected(t *testing.T) {
	t.Parallel()
	dialing := make(chan struct{})
	rig := newHubRig(t, func(hub *deliveryHub) {
		dial := hub.dial
		hub.dial = func(ctx context.Context) (*pgx.Conn, error) {
			select {
			case <-dialing:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return dial(ctx)
		}
	})
	letDial := sync.OnceFunc(func() { close(dialing) })
	t.Cleanup(letDial)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, rig.clock.Now()); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "p")

	since := rig.mark()
	letDial()
	rig.waitForEvent(t, since, "the hub's listener to connect", func(note deliveryHubNote) bool { return note.what == "connected" })
	type reading struct {
		row   delivery.DeliverySettings
		heard deliveryFreshnessHeard
	}
	testwait.Until(t, "the hub's freshness to be the settings row's", func() (reading, bool) {
		row, err := delivery.GetSettings(t.Context(), rig.database.Pool)
		if err != nil {
			t.Fatalf("read the settings row: %v", err)
		}
		rig.hub.mu.Lock()
		heard := rig.hub.generation.fresh
		rig.hub.mu.Unlock()
		return reading{row: row, heard: heard},
			sameMoment(heard.lastReconcileAt, row.LastReconcileAt) && sameMoment(heard.lastEventAt, row.LastEventAt) &&
				equalStringPointers(heard.lastError, row.LastError)
	})
}

// sameMoment is whether a and b are both unset or both the same moment.
func sameMoment(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
