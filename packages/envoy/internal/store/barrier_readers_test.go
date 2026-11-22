package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// Every reader of the interest and role claim buckets drops a read barrier (bus.BarrierKey), the
// delete marker every read writes (bus.KeyValue.WriteBarrier): the interest copy and the
// role claim copy keep no row and no fence for it, the interest marker collector's scan counts it
// as no marker, the role claim listing (roleClaimsUntil) names no claim under it, and a registry
// opened over buckets that hold one counts it in neither warm-up nor the role claim restore.
func TestInterestAndRoleReadersDropTheReadBarrier(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	first := openRegistry(t, conn)
	waitFor(t, 30*time.Second, first.roleWatcher.Ready)
	putInterest := func(sessionID string) {
		t.Helper()
		if _, err := first.interests().Put(sessionID, []byte(`{"session_id":"`+sessionID+`","machine_id":"m1"}`)); err != nil {
			t.Fatalf("put interest %s: %v", sessionID, err)
		}
	}
	putClaim := func(role string) {
		t.Helper()
		claim, err := json.Marshal(RoleClaim{HolderSessionID: "ses_holder", ClaimedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode the claim: %v", err)
		}
		if _, err := first.roles().Put(role, claim); err != nil {
			t.Fatalf("put claim %s: %v", role, err)
		}
	}
	putInterest("ses_live")
	putClaim("role-live")
	writeBarrier(t, first.interests())
	writeBarrier(t, first.roles())
	putInterest("ses_after")
	putClaim("role-after")
	waitFor(t, 10*time.Second, func() bool {
		first.mu.RLock()
		_, interest := first.cache.Get("ses_after")
		first.mu.RUnlock()
		first.holdersMu.RLock()
		_, claim := first.roleHolders.Get("role-after")
		first.holdersMu.RUnlock()
		return interest && claim
	})

	first.mu.RLock()
	_, interestRow := first.cache.Get(bus.BarrierKey)
	interestFence := first.cache.FenceFor(first.watcher.Epoch(), bus.BarrierKey)
	first.mu.RUnlock()
	first.holdersMu.RLock()
	_, claimRow := first.roleHolders.Get(bus.BarrierKey)
	claimFence := first.roleHolders.FenceFor(first.roleWatcher.Epoch(), bus.BarrierKey)
	first.holdersMu.RUnlock()
	if interestRow || interestFence != 0 || claimRow || claimFence != 0 {
		t.Errorf("the copies hold the barrier: interest row %v fence %d, role claim row %v fence %d", interestRow, interestFence, claimRow, claimFence)
	}

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	markers := -1
	if _, err := collectorOf(first).pass(js, func(pass *interestPass) { markers = pass.scan.Markers }); err != nil {
		t.Fatalf("the collector's pass: %v", err)
	}
	if markers != 0 {
		t.Errorf("the collector's scan of a bucket holding live keys and a barrier counted %d markers, want 0", markers)
	}
	claims, err := first.roleClaimsUntil(context.Background())
	if err != nil {
		t.Fatalf("list the role claims: %v", err)
	}
	for _, claim := range claims {
		if claim.Role == bus.BarrierKey {
			t.Errorf("the role claim listing names the barrier: %+v", claims)
		}
	}

	records := captureJSONLogs(t)
	second := openRegistry(t, conn)
	waitFor(t, 30*time.Second, second.roleWatcher.Ready)
	for msg, want := range map[string]float64{
		"interest registry cache warm-up": 2,
		"role claims cache warm-up":       2,
	} {
		if line := jsonRecord(t, records(), msg); line["entries"] != want || line["delete_markers"] != float64(0) {
			t.Errorf("%s over buckets holding a barrier: entries=%v delete_markers=%v, want %v and 0", msg, line["entries"], line["delete_markers"], want)
		}
	}
	if line := jsonRecord(t, records(), "restored role claims"); line["restored"] != float64(2) || line["delete_markers"] != float64(0) {
		t.Errorf("the role claim restore over a bucket holding a barrier: restored=%v delete_markers=%v, want 2 and 0", line["restored"], line["delete_markers"])
	}
}

// writeBarrier writes a read barrier into the bucket kv is a handle on, as every read does
// (bus.KeyValue.WriteBarrier).
func writeBarrier(t *testing.T, kv bus.KeyValue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := kv.WriteBarrier(ctx); err != nil {
		t.Fatalf("write a read barrier into %s: %v", kv.Bucket(), err)
	}
}
