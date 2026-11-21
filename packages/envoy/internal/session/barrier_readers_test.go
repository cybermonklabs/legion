package session

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Every reader of the session bucket drops a read barrier (bus.BarrierKey), the delete marker every
// read writes (bus.KeyValue.WriteBarrier): the copy keeps no row and no fence for it,
// so the sessions route (List) never lists it and the reaper (goneCandidates, ReapSessions' input)
// never takes it for a session gone silent, however late its clock reads.
func TestSessionReadersDropTheReadBarrier(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	if err := reg.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	if err := reg.Put("ses_before", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("register ses_before: %v", err)
	}
	writeBarrier(t, reg.watcher.KV())
	// ses_after is written to the bucket directly, not through the registry, so only the watcher's
	// delivery brings it to the copy, and that delivery follows the barrier's in the stream.
	putDirect(t, reg.watcher.KV(), "ses_after", SessionEntry{Port: 1, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
	testwait.Eventually(t, "the watcher's delivery of ses_after", func() bool {
		reg.mu.RLock()
		defer reg.mu.RUnlock()
		_, delivered := reg.cache.Get("ses_after")
		return delivered
	})

	listed, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, entry := range listed {
		ids = append(ids, entry.SessionID)
	}
	slices.Sort(ids)
	if want := []string{"ses_after", "ses_before"}; !slices.Equal(ids, want) {
		t.Errorf("List over a bucket holding a barrier = %v, want %v", ids, want)
	}
	reg.mu.RLock()
	_, row := reg.cache.Get(bus.BarrierKey)
	fence := reg.cache.FenceFor(reg.watcher.Epoch(), bus.BarrierKey)
	reg.mu.RUnlock()
	if row || fence != 0 {
		t.Errorf("the session copy holds the barrier: row %v, fence %d", row, fence)
	}
	for _, candidate := range reg.goneCandidates(time.Now().Add(365 * 24 * time.Hour)) {
		if candidate.sessionID == bus.BarrierKey {
			t.Errorf("the reaper takes the barrier for a session gone silent: %+v", candidate)
		}
	}
}

// The known-session store drops a read barrier too: no record by id or by topic, and no row or
// fence in its copy.
func TestKnownSessionsDropTheReadBarrier(t *testing.T) {
	known := openKnown(t, time.Now)
	writeBarrier(t, known.watcher.KV())
	// Written to the bucket directly, so only the watcher's delivery, which follows the barrier's,
	// brings it to the copy.
	putDirect(t, known.watcher.KV(), "ses_known", KnownSession{SessionID: "ses_known", Topics: []string{"notifications.agent.ses_known"}, LastSeen: time.Now().UnixMilli()})
	testwait.Eventually(t, "the watcher's delivery of ses_known", func() bool {
		_, delivered := known.Get("ses_known")
		return delivered
	})
	if _, found := known.Get(bus.BarrierKey); found {
		t.Errorf("the known-session store answers a record for the barrier")
	}
	if ids := matchedIDs(known, ">"); slices.Contains(ids, bus.BarrierKey) {
		t.Errorf("MatchEach visits the barrier: %v", ids)
	}
	known.mu.RLock()
	_, row := known.records.Get(bus.BarrierKey)
	fence := known.records.FenceFor(known.watcher.Epoch(), bus.BarrierKey)
	known.mu.RUnlock()
	if row || fence != 0 {
		t.Errorf("the known-session copy holds the barrier: row %v, fence %d", row, fence)
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

// putDirect writes value, encoded as JSON, under key into the bucket kv is a handle on, past any
// store's copy.
func putDirect(t *testing.T, kv bus.KeyValue, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %s: %v", key, err)
	}
	if _, err := kv.Put(key, raw); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}
