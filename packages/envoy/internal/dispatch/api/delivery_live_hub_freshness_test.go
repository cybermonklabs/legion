package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestRefreshFreshnessRereadsDeliverySettings proves the reconnect reader takes every field the
// listener's delivery_freshness notification carries, without depending on that notification.
func TestRefreshFreshnessRereadsDeliverySettings(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordEventAt(t.Context(), rig.database.Pool); err != nil {
		t.Fatalf("record an event: %v", err)
	}
	passed := rig.clock.Now().Add(time.Minute).Truncate(time.Microsecond)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, passed); err != nil {
		t.Fatalf("record the successful reconcile: %v", err)
	}
	const failure = "GitHub answered 502"
	if err := delivery.RecordReconcileError(t.Context(), rig.database.Pool, failure); err != nil {
		t.Fatalf("record the failed reconcile: %v", err)
	}

	generation := &hubGeneration{ctx: context.Background(), wakeWork: make(chan struct{}, 1)}
	rig.hub.refreshFreshness(generation)
	if got := generation.fresh.lastEventAt; got == nil {
		t.Fatal("the reread left last_event_at nil after the recorded event")
	}
	if got := generation.fresh.lastReconcileAt; got == nil || !got.Equal(passed) {
		t.Fatalf("the reread last_reconcile_at = %v, want %v", got, passed)
	}
	if got := generation.fresh.lastError; got == nil || *got != failure {
		t.Fatalf("the reread last_error = %v, want %q", got, failure)
	}
}
