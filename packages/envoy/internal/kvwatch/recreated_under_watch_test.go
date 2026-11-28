package kvwatch_test

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// recreateNoticeBound is the longest the watcher's own stream probe may wait before it finds, by
// itself, that its bucket was deleted and created again: well inside the listener's 30 s
// self-health tick, which is the only caller of Check today and so the only other thing that finds
// it.
const recreateNoticeBound = 20 * time.Second

// A bucket deleted and created again under a live watcher is a new incarnation the copy no longer
// follows. nats.go's ordered consumer does not end on it: it resets onto the new stream at the old
// stream's last sequence + 1, so it skips the new bucket's first records, every one of them when
// the old stream was the longer. The copy must stop following before it applies any record of the
// new bucket, and the watcher must read the new bucket whole by itself, with no Check and no
// Rewatch, so a list never answers from a copy that silently misses records its bucket holds
// (INFRA-261). Both shapes run: the old stream shorter than the new one (the reset consumer
// delivers the new bucket's tail as if it were followed) and longer (it delivers nothing).
func TestABucketRecreatedUnderALiveWatcherIsReadWholeWithoutCheck(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct {
		name       string
		oldWrites  int
		newRecords int
	}{
		{name: "old stream shorter", oldWrites: 3, newRecords: 40},
		{name: "old stream longer", oldWrites: 200, newRecords: 40},
	} {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
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
				newApplied    []string
				whileFollowed []string
			)
			into := withApply{seen: newSeen(), before: func(entry natsgo.KeyValueEntry) {
				if entry.Key() == "ghost" {
					return
				}
				following := w.Following()
				mu.Lock()
				defer mu.Unlock()
				newApplied = append(newApplied, entry.Key())
				if following {
					whileFollowed = append(whileFollowed, entry.Key())
				}
			}}
			timers := &pending{}
			w = kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()))
			t.Cleanup(w.Stop)
			w.Start()
			testwait.Until(t, "the first read's completion", func() (bool, bool) {
				following := w.Following()
				return following, following
			})

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

			// No Check and no Rewatch: the watcher alone must find the new incarnation and read it.
			// Its stream probe is the one wait its first read armed besides that read's idle wait, so
			// the delay checked here is the one the watcher asked for, whatever its default.
			probe := lastWaitBut(t, timers, kvwatch.ScanIdleTimeout, rereadNow, kvwatch.AbandonedDrain)
			if probe.delay > recreateNoticeBound {
				t.Fatalf("stream probe delay = %v, want at most %v", probe.delay, recreateNoticeBound)
			}
			probe.fire()
			timers.last(t, rereadNow).fire()

			whole := func() (string, bool) {
				ghost, following, epoch := into.has("ghost"), w.Following(), w.Epoch()
				missing := 0
				for _, key := range keys {
					if !into.has(key) {
						missing++
					}
				}
				mu.Lock()
				applied := len(newApplied)
				mu.Unlock()
				reading := fmt.Sprintf("ghost kept %v, %d of %d new records missing, Following %v, Epoch %d (%d new records applied)",
					ghost, missing, len(keys), following, epoch, applied)
				return reading, !ghost && following && epoch == 1 && missing == 0
			}
			testwait.Until(t, "the new bucket to be read whole in epoch 1 without Check", whole)
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

// lastWaitBut is the wait timers armed last whose delay is none of skip, failing the test when it
// armed none.
func lastWaitBut(t *testing.T, timers *pending, skip ...time.Duration) *pendingWait {
	t.Helper()
	timers.mu.Lock()
	waits := slices.Clone(timers.waits)
	timers.mu.Unlock()
	for i := len(waits) - 1; i >= 0; i-- {
		if !slices.Contains(skip, waits[i].delay) {
			return waits[i]
		}
	}
	t.Fatalf("the watcher armed no wait but of %v", skip)
	return nil
}
