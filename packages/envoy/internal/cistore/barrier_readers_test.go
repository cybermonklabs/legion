package cistore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The CI store drops a read barrier (bus.BarrierKey), the delete marker every read writes
// (bus.KeyValue.WriteBarrier): its copy keeps no row and no fence for it. A CI record's key is
// `<owner>.<repo>.pr<number>.<sha>` (Key), so it is never the barrier's.
func TestTheCIStoreDropsTheReadBarrier(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	kv := s.watcher.KV()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := kv.WriteBarrier(ctx); err != nil {
		t.Fatalf("write a read barrier: %v", err)
	}
	// Written to the bucket directly, so only the watcher's delivery, which follows the barrier's,
	// brings it to the copy.
	key := Key("example-org", "example-repo", "1", "abc123")
	raw, err := json.Marshal(State{Owner: "example-org", Repo: "example-repo", Number: "1", SHA: "abc123", LastEventAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the state: %v", err)
	}
	if _, err := kv.Put(key, raw); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	testwait.Eventually(t, "the watcher's delivery of "+key, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		_, delivered := s.cache.Get(key)
		return delivered
	})
	s.mu.RLock()
	_, row := s.cache.Get(bus.BarrierKey)
	fence := s.cache.FenceFor(s.watcher.Epoch(), bus.BarrierKey)
	s.mu.RUnlock()
	if row || fence != 0 {
		t.Errorf("the CI copy holds the barrier: row %v, fence %d", row, fence)
	}
}
