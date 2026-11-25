package pmdoc

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

// The display-tree cache is one of the two caches with a bound that check c28 of INFRA-1588 holds
// production's heap to: the part of the heap the cache holds is a leak when it passes 64 MiB
// (DisplayCacheBytes). Filled past its bound with the trees of short texts, as a server's comment
// lists and event feeds fill it, the cache must hold no more live heap than that bound: its
// entries, their keys and the trees' allocations included, not only the bytes of the trees'
// JSON. On production's profiles of 2026-11-19 and 2026-11-20 the trees alone held 63.9 MB at the
// window's start and 73.9 MB at its end, and the cache's entries 6.9 MB and 5.4 MB more, against
// a bound of 67.1 MB. Half the trees go in each way a tree enters the cache: read here
// (DisplayJSONOf), and handed over as a stored tree, copied by append as texttrees.Read copies a
// row (PutStoredDisplayTree).
func TestTheDisplayCacheHoldsNoMoreLiveHeapThanItsBound(t *testing.T) {
	useEmptyDisplayCache(t, DisplayCacheBytes)

	before := liveHeap()
	// Texts of 40 to 239 words, a comment's length, each read once: twice the bound's worth of
	// trees, so the cache is full and has evicted for a while.
	built, charged := 0, int64(0)
	for charged < 2*DisplayCacheBytes {
		markdown := fmt.Sprintf("Comment %d says %s", built, strings.Repeat("word ", 40+built%200))
		var tree json.RawMessage
		if built%2 == 0 {
			tree = DisplayJSONOf(TextSum(markdown), markdown, DisplayLatest)
		} else {
			tree = append(json.RawMessage(nil), DisplayTreeToStore(markdown, DisplayLatest)...)
			PutStoredDisplayTree(TextSum(markdown), DisplayLatest, tree)
		}
		charged += int64(len(tree))
		built++
	}
	after := liveHeap()
	holding := after - before
	t.Logf("%d trees read, %d bytes of JSON among them; the cache charges %d bytes of its %d bound, and the live heap rose %d bytes",
		built, charged, displayCache.Held(), int64(DisplayCacheBytes), holding)
	if holding > DisplayCacheBytes {
		t.Errorf("the display-tree cache, full, holds %d bytes of live heap, over its bound of %d bytes (DisplayCacheBytes): it charges the bytes of each tree's JSON, %d, and not what holding the tree and its entry costs the heap",
			holding, int64(DisplayCacheBytes), displayCache.Held())
	}
}

// The display-tree cache holds no more live heap than its bound however long it runs: filled with
// trees within 200 bytes of one length and turned over 60 times, it holds no more than its bound
// at the end, at each length from 1,000 to 8,000 bytes in steps of 200. Each entry is charged a
// fixed cost for itself (displayCacheEntryBytes), which covers its slot in bytelru's map only
// while the map holds no more slots than its live keys need. A Go map keeps the slot of a deleted
// key and splits its tables rather than reuse every one, so bytelru makes its map afresh as the
// cache turns over (renewItems). On go1.27.3 linux/amd64, with that call taken out, an entry held
// up to 447 bytes beyond its tree's array, against the 360 it is charged, and 15 of the 36
// lengths ended past the bound, by up to 0.47 MB (trees of 2,200 bytes); with it, an entry held
// at most 288 bytes and every length ended 0.21 MB or more under the bound.
//
// The cache is limited to 16 MiB rather than DisplayCacheBytes, which takes the 36 lengths 26 s
// instead of 87 s. A quarter of the limit holds a quarter of the trees, and a Go map grows by
// doubling its tables and splitting them in two, so the map of a quarter of the keys grows the
// same way with a quarter of the tables and an entry costs what it costs in the full cache. Run at
// 64 MiB, this test measured each entry within 0.5 bytes of what it holds here at all 36 lengths
// with renewItems and at 33 without. Without it, the most past the bound was 1.87 MB, four times
// the 0.47 MB here, and the five lengths where an entry held 2 bytes past its charge ended past
// the bound there and not here, since what a 16 MiB cache leaves free under its limit covered
// those bytes.
func TestTheDisplayCacheTurnedOver60TimesHoldsNoMoreLiveHeapThanItsBound(t *testing.T) {
	const limit = 16 << 20
	const turnovers = 60
	// entryCharge is what each tree is charged beyond its array (displayCacheEntrySize).
	const entryCharge = int64(unsafe.Sizeof(displayCacheKey{})) + displayCacheEntryBytes
	// Every key is new and none is read back, so the cache holds the newest trees, and the
	// charges of the newest puts say how many trees it holds and what their arrays are. Made
	// before any reading, and long enough for a cache full of the shortest trees.
	charges := make([]int64, limit/(1_000+entryCharge))
	source := make([]byte, 8_000+200)
	for length := 1_000; length <= 8_000; length += 200 {
		t.Run(fmt.Sprintf("trees of %d to %d bytes", length, length+199), func(t *testing.T) {
			useEmptyDisplayCache(t, limit)
			before := liveHeap()
			puts := 0
			put := func() int64 {
				// Copied by append, as texttrees.Read copies a stored row.
				tree := append(json.RawMessage(nil), source[:length+puts%200]...)
				PutStoredDisplayTree(TextSum(strconv.Itoa(puts)), DisplayLatest, tree)
				charge := displayCacheEntrySize(tree)
				charges[puts%len(charges)] = charge
				puts++
				return charge
			}
			// Full at the first put that evicts; then 60 times as many trees again.
			for charged := int64(0); charged <= limit; {
				charged += put()
			}
			for full := puts; puts < (1+turnovers)*full; {
				put()
			}
			holding := liveHeap() - before
			held := displayCache.Held()
			entries, arrays := 0, int64(0)
			for sum := int64(0); sum < held; entries++ {
				charge := charges[(puts-1-entries)%len(charges)]
				sum += charge
				arrays += charge - entryCharge
			}
			entryHeap := float64(holding-arrays) / float64(entries)
			t.Logf("%d trees put; the cache charges %d bytes of its %d bound for %d trees, the live heap rose %d bytes, and each entry holds %.1f bytes beyond its tree's array, its key included, against the %d it is charged",
				puts, held, int64(limit), entries, holding, entryHeap, entryCharge)
			if holding > limit {
				t.Errorf("after turning over %d times the display-tree cache holds %d bytes of live heap, %d over its bound of %d: each of its %d entries holds %.1f bytes beyond its tree's array, more than the %d it is charged, so the entries cost more the longer the cache evicts",
					turnovers, holding, holding-limit, int64(limit), entries, entryHeap, entryCharge)
			}
		})
	}
}

// Each way a tree enters the display cache charges it its whole array, its key and its entry
// (displayCacheEntrySize), whatever part of the array its JSON fills: a tree this process read,
// and a stored tree handed over in an array longer than the tree, as an append's array is. A
// charge of the JSON's length alone leaves the rest of the allocation uncounted.
func TestEachWayIntoTheDisplayCacheChargesATreeItsArrayItsKeyAndItsEntry(t *testing.T) {
	const markdown = "A comment *only* this test reads."
	ways := []struct {
		name string
		put  func() json.RawMessage
	}{
		{"a tree this process read", func() json.RawMessage {
			return DisplayJSONOf(TextSum(markdown), markdown, DisplayLatest)
		}},
		{"a stored tree", func() json.RawMessage {
			read := DisplayTreeToStore(markdown, DisplayLatest)
			tree := make(json.RawMessage, len(read), len(read)+100)
			copy(tree, read)
			PutStoredDisplayTree(TextSum(markdown), DisplayLatest, tree)
			return tree
		}},
	}
	for _, way := range ways {
		t.Run(way.name, func(t *testing.T) {
			useEmptyDisplayCache(t, DisplayCacheBytes)
			tree := way.put()
			want := int64(cap(tree)) + int64(unsafe.Sizeof(displayCacheKey{})) + displayCacheEntryBytes
			if got := displayCache.Held(); got != want {
				t.Errorf("the cache charges %d bytes for a tree of %d bytes in an array of %d, want %d: the array, the key's %d bytes and the entry's %d",
					got, len(tree), cap(tree), want, unsafe.Sizeof(displayCacheKey{}), displayCacheEntryBytes)
			}
		})
	}
}

// useEmptyDisplayCache stands an empty display cache of limit bytes in for the process's own
// until the test ends.
func useEmptyDisplayCache(t *testing.T, limit int64) {
	t.Helper()
	held := displayCache
	displayCache = bytelru.New[displayCacheKey, json.RawMessage](limit)
	t.Cleanup(func() { displayCache = held })
}

// liveHeap is the bytes of heap still live after two collections.
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}
