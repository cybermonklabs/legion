package routes

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Every tab of another build asks for the same chunks, and a stale tab asks again on each route it
// opens, so what the store answered once is kept: a second request for it is served from memory
// without asking the store, and without a reservation, so it is served even while stalled readers
// hold every byte of the cap and a miss would be refused.
func TestStaticHandlerServesACachedAssetWithoutTheStore(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	readers := int(maxRetainedBytesHeld / MaxRetainedAssetSize)
	stallRetainedReaders(t, handler, readers, testwait.Receive[struct{}])

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/stalled-0.js", nil))

	if recorder.Code != http.StatusOK || int64(recorder.Body.Len()) != MaxRetainedAssetSize {
		t.Fatalf("a cached asset at the cap: status %d with %d body bytes, want 200 with %d", recorder.Code, recorder.Body.Len(), MaxRetainedAssetSize)
	}
	if got := recorder.Header().Get("Cache-Control"); got != assetCacheControl {
		t.Errorf("a cached asset: Cache-Control %q, want %q", got, assetCacheControl)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("a cached asset: Content-Type %q, want text/javascript", got)
	}
	if got := store.calls.Load(); got != int32(readers) {
		t.Errorf("the store was asked %d times, want %d (none for the cached asset)", got, readers)
	}
}

// gatheringStore holds every read until want reads have arrived or wait runs out, and counts the
// reads: requests that each read the store arrive together, while one read they share arrives
// alone and is answered after wait. Its one user runs in a synctest bubble, where wait is fake
// time that moves only once every request is blocked, so every request has made its read or
// joined the shared one before wait runs out, whatever the machine's load.
type gatheringStore struct {
	object  []byte
	want    int32
	wait    time.Duration
	calls   atomic.Int32
	arrived chan struct{} // closed when the want-th read arrives
}

func (s *gatheringStore) GetAsset(ctx context.Context, _ string) ([]byte, error) {
	if s.calls.Add(1) == s.want {
		close(s.arrived)
	}
	select {
	case <-s.arrived:
	case <-time.After(s.wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.object, nil
}

// A route of a stale tab, or many tabs of one build at once, ask for one chunk together before the
// store has answered any of them: the store is read once and every request is answered with it.
func TestStaticHandlerReadsTheStoreOnceForConcurrentMisses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler, context, stop := bubbleTestRouter(t)
		defer stop()
		context.WebDistDir = t.TempDir()
		const object = "console.log('another build')"
		requests := int(maxRetainedBytesHeld / MaxRetainedAssetSize)
		store := &gatheringStore{object: []byte(object), want: int32(requests), wait: time.Second, arrived: make(chan struct{})}
		context.AssetStore = store

		recorders := make([]*httptest.ResponseRecorder, requests)
		var wg sync.WaitGroup
		for i := range recorders {
			recorders[i] = httptest.NewRecorder()
			wg.Add(1)
			go func() {
				defer wg.Done()
				handler.ServeHTTP(recorders[i], httptest.NewRequest(http.MethodGet, "/assets/shared.js", nil))
			}()
		}
		wg.Wait()

		for i, recorder := range recorders {
			if recorder.Code != http.StatusOK || recorder.Body.String() != object {
				t.Errorf("request %d: status %d body %q, want 200 %q", i, recorder.Code, recorder.Body.String(), object)
			}
		}
		if got := store.calls.Load(); got != 1 {
			t.Errorf("%d concurrent misses of one asset read the store %d times, want once", requests, got)
		}
	})
}

// The cache keeps at most maxCachedAssetBytes however much it is handed, and evicts the object used
// least recently: one read again since it was stored stays, so the chunks a tab is still opening
// are not the ones lost. That the bytes it counts are the bytes it holds is the shared cache's
// own invariant (bytelru).
func TestAssetCacheKeepsAtMostItsLimitEvictingTheLeastRecentlyUsed(t *testing.T) {
	cache := newAssetCache(maxCachedAssetBytes)
	sizes := []int64{MaxRetainedAssetSize, 3 << 20, 1, MaxRetainedAssetSize - 1, 5 << 20, 1747955}
	for i := range 60 {
		cache.Put(fmt.Sprintf("assets/chunk-%d.js", i), make([]byte, sizes[i%len(sizes)]))
		if held := cache.Held(); held > maxCachedAssetBytes {
			t.Fatalf("after %d objects the cache holds %d bytes, want at most %d", i+1, held, maxCachedAssetBytes)
		}
	}

	cache = newAssetCache(maxCachedAssetBytes)
	full := int(maxCachedAssetBytes / MaxRetainedAssetSize)
	for i := range full {
		cache.Put(fmt.Sprintf("assets/chunk-%d.js", i), make([]byte, MaxRetainedAssetSize))
	}
	if _, ok := cache.Get("assets/chunk-0.js"); !ok {
		t.Fatal("the first object was evicted from a cache it fits in")
	}
	cache.Put("assets/one-more.js", make([]byte, MaxRetainedAssetSize))
	if _, ok := cache.Get("assets/chunk-0.js"); !ok {
		t.Error("the object read most recently was evicted")
	}
	if _, ok := cache.Get("assets/chunk-1.js"); ok {
		t.Error("the object used least recently was kept past the limit")
	}
	if _, ok := cache.Get("assets/one-more.js"); !ok {
		t.Error("the newest object was not kept")
	}
}
