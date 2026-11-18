package kvwatch_test

import (
	"fmt"
	"sync"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A bucket deleted and created again under a live watcher is a new incarnation the copy no longer
// follows. nats.go's ordered consumer does not end on it: it resets onto the new stream at the old
// stream's last sequence + 1, so it skips the new bucket's first records, every one of them when
// the old stream was the longer. The copy must stop following before it applies any record of the
// new bucket, and the watcher must read the new bucket whole by itself, with no Check and no
// Rewatch, so a list never answers from a copy that silently misses records its bucket holds
// (INFRA-261). Both shapes run: the old stream shorter than the new one (the reset consumer
// delivers the new bucket's tail as if it were followed) and longer (it delivers nothing).
func TestABucketRecreatedUnderALiveWatcherIsReadWholeWithoutCheck(t *testing.T) {
	for _, shape := range []struct {
		name       string
		oldWrites  int
		newRecords int
	}{
		{name: "old stream shorter", oldWrites: 3, newRecords: 40},
		{name: "old stream longer", oldWrites: 200, newRecords: 40},
	} {
		t.Run(shape.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			for i := range shape.oldWrites {
				if _, err := kv.Put("ghost", fmt.Appendf(nil, "%d", i)); err != nil {
					t.Fatalf("put ghost: %v", err)
				}
			}
			var (
				mu            sync.Mutex
				w             *kvwatch.Watcher
				whileFollowed []string
			)
			into := withApply{seen: newSeen(), before: func(entry natsgo.KeyValueEntry) {
				if entry.Key() == "ghost" {
					return
				}
				following := w.Following()
				mu.Lock()
				defer mu.Unlock()
				if following {
					whileFollowed = append(whileFollowed, entry.Key())
				}
			}}
			w = kvwatch.New("test cache", kv, into)
			t.Cleanup(w.Stop)
			w.Start()
			testwait.Eventually(t, "the first read's completion", w.Following)

			js := jetStream(t, conn)
			deleteTestBucket(t, js)
			recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
			keys := make([]string, shape.newRecords)
			for i := range keys {
				keys[i] = fmt.Sprintf("fresh-%02d", i)
				if _, err := recreated.Put(keys[i], []byte("new")); err != nil {
					t.Fatalf("put %s: %v", keys[i], err)
				}
			}

			whole := func() bool {
				if into.has("ghost") || !w.Following() || w.Epoch() != 1 {
					return false
				}
				for _, key := range keys {
					if !into.has(key) {
						return false
					}
				}
				return true
			}
			// No Check and no Rewatch: the watcher's own periodic probe must find the new
			// incarnation and read it whole. The probe runs on the real clock (the watcher is on a
			// real NATS connection), so the shared wait holds for it with no bound of its own.
			testwait.Eventually(t, "the watcher to read the recreated bucket whole in epoch 1", whole)
			mu.Lock()
			followed := append([]string(nil), whileFollowed...)
			mu.Unlock()
			if len(followed) > 0 {
				t.Errorf("%d records of the new bucket were applied while the copy still reported Following (first %q); want following stopped before any of them",
					len(followed), followed[0])
			}
		})
	}
}
