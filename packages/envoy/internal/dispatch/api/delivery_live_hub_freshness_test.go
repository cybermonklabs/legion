package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestRefreshFreshnessRereadsDeliverySettings proves the reconnect reader takes every field the
// listener's delivery_freshness notification carries, without depending on that notification.
func TestRefreshFreshnessRereadsDeliverySettings(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	passed := rig.clock.Now().Add(time.Minute).Truncate(time.Microsecond)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, passed); err != nil {
		t.Fatalf("record the successful reconcile: %v", err)
	}
	const failure = "GitHub answered 502"
	if err := delivery.RecordReconcileError(t.Context(), rig.database.Pool, failure); err != nil {
		t.Fatalf("record the failed reconcile: %v", err)
	}

	seen := rig.clock.Now().Add(-time.Minute)
	generation := &hubGeneration{
		ctx: context.Background(), wakeWork: make(chan struct{}, 1),
		fresh: deliveryFreshnessHeard{lastEventAt: &seen},
	}
	if !rig.hub.readFreshness(generation) {
		t.Fatal("the freshness reread failed")
	}
	if got := generation.fresh.lastEventAt; got == nil || !got.Equal(seen) {
		t.Fatalf("the reread last_event_at = %v, want the already-heard %v when its read is nil", got, seen)
	}
	if got := generation.fresh.lastReconcileAt; got == nil || !got.Equal(passed) {
		t.Fatalf("the reread last_reconcile_at = %v, want %v", got, passed)
	}
	if got := generation.fresh.lastError; got == nil || *got != failure {
		t.Fatalf("the reread last_error = %v, want %q", got, failure)
	}
}

// TestFreshnessRereadCannotOverwriteANewerNotification holds the reread after its database
// snapshot, delivers a newer notification, and then releases the older snapshot.
func TestFreshnessRereadCannotOverwriteANewerNotification(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	clock := newNoonHubClock()
	hub := &deliveryHub{clock: clock, server: rig.hub.server}
	generation := &hubGeneration{ctx: context.Background(), wakeWork: make(chan struct{}, 1)}
	hub.generation = generation
	older := clock.Now().Add(-time.Minute).Truncate(time.Microsecond)
	newer := clock.Now().Truncate(time.Microsecond)
	read, release := make(chan struct{}), make(chan struct{})
	hub.getFreshness = func(context.Context, *store.Pool) (delivery.DeliverySettings, error) {
		close(read)
		<-release
		return delivery.DeliverySettings{LastReconcileAt: &older}, nil
	}
	hub.refreshFreshness(generation)
	testwait.Receive(t, "the freshness reread to take its older snapshot", read)
	payload, err := json.Marshal(map[string]any{"last_reconcile_at": newer})
	if err != nil {
		t.Fatalf("encode the newer freshness notification: %v", err)
	}
	hub.applyFreshness(generation, string(payload))
	close(release)
	done := make(chan struct{})
	go func() {
		hub.running.Wait()
		close(done)
	}()
	testwait.Receive(t, "the reread after the newer notification", done)
	if got := generation.fresh.lastReconcileAt; got == nil || !got.Equal(newer) {
		t.Fatalf("the reread overwrote the newer notification: last_reconcile_at = %v, want %v", got, newer)
	}
}
