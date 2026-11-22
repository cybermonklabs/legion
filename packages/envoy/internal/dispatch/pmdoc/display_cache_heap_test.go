package pmdoc

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

// The display-tree cache is one of the two caches with a bound that check c28 of INFRA-1588 holds
// production's heap to: the part of the heap the cache holds is a leak when it passes 64 MiB
// (DisplayCacheBytes). Filled past its bound with the trees of short texts, as a server's comment
// lists and event feeds fill it, the cache must hold no more live heap than that bound: its
// entries, their keys and the trees' allocations included, not only the bytes of the trees'
// JSON. On production's profiles of 2026-11-19 and 2026-11-20 the trees alone held 63.9 MB at the
// window's start and 73.9 MB at its end, and the cache's entries 6.9 MB and 5.4 MB more, against
// a bound of 67.1 MB.
func TestTheDisplayCacheHoldsNoMoreLiveHeapThanItsBound(t *testing.T) {
	held := displayCache
	displayCache = bytelru.New[displayCacheKey, json.RawMessage](DisplayCacheBytes)
	t.Cleanup(func() { displayCache = held })

	live := func() int64 {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return int64(stats.HeapAlloc)
	}
	before := live()
	// Texts of 40 to 239 words, a comment's length, each read once: twice the bound's worth of
	// trees, so the cache is full and has evicted for a while.
	built, charged := 0, int64(0)
	for charged < 2*DisplayCacheBytes {
		markdown := fmt.Sprintf("Comment %d says %s", built, strings.Repeat("word ", 40+built%200))
		tree := DisplayJSONOf(TextSum(markdown), markdown, DisplayLatest)
		charged += int64(len(tree))
		built++
	}
	after := live()
	holding := after - before
	t.Logf("%d trees read, %d bytes of JSON among them; the cache charges %d bytes of its %d bound, and the live heap rose %d bytes",
		built, charged, displayCache.Held(), int64(DisplayCacheBytes), holding)
	if holding > DisplayCacheBytes {
		t.Errorf("the display-tree cache, full, holds %d bytes of live heap, over its bound of %d bytes (DisplayCacheBytes): it charges the bytes of each tree's JSON, %d, and not what holding the tree and its entry costs the heap",
			holding, int64(DisplayCacheBytes), displayCache.Held())
	}
}
