package kvwatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unansweredWatchKV is a watch handle whose first fails watches fail transiently, with cause
// (context.DeadlineExceeded when nil), as a loaded start's watcher create does, and whose later
// watches are the bucket's own.
type unansweredWatchKV struct {
	jetstream.KeyValue
	fails   int32
	cause   error
	watched atomic.Int32
}

func (k *unansweredWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if k.watched.Add(1) <= k.fails {
		cause := k.cause
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		return nil, fmt.Errorf("create the watcher: %w", cause)
	}
	return k.KeyValue.Watch(ctx, keys, opts...)
}

// listed is every key the copy holds, sorted.
func listed(s *seen) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.keys.All() {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

const firstAttemptFailed = "test cache first watch attempt failed; it is made again"

// A watcher made WithFirstWatchRetry makes a first watch whose failure was transient again (one
// unanswered, one made while the connection reconnected), after the re-read's wait on its own
// clock, and the copy it then reads is the copy a first watch that succeeded reads, key for key.
// The failed attempt is a WARN naming it, and nothing is an ERROR; meanwhile the copy is a read
// under way, so the self-health rebuild does not take it for one to repair.
func TestAFirstWatchThatGoesUnansweredIsMadeAgainOnTheWatchersClock(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, natsgo.ErrReconnectBufExceeded} {
		t.Run(cause.Error(), func(t *testing.T) { firstWatchMadeAgain(t, cause) })
	}
}

func firstWatchMadeAgain(t *testing.T, cause error) {
	_, kv := bucket(t, testnats.URL(t))
	for _, key := range []string{"claim-a", "claim-b", "claim-c"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	firstTry := newSeen()
	control := kvwatch.New("control cache", kv, firstTry, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(control.Stop)
	control.Start()
	testwait.Eventually(t, "the first try's read to complete", control.Following)

	seam := &unansweredWatchKV{KeyValue: kv.WatchHandle(), fails: 1, cause: cause}
	logger, records := jsonLogger(t)
	arm := &armed{}
	into := newSeen()
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), seam, kv.Streams()), into, kvwatch.WithLogger(logger),
		kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithFirstWatchRetry())
	t.Cleanup(w.Stop)
	w.Start()

	warned := testwait.Until(t, "the first attempt's WARN", func() ([]map[string]any, bool) {
		found := matching(records(), "WARN", firstAttemptFailed)
		return found, len(found) == 1
	})
	if warned[0]["attempt"] != float64(1) {
		t.Fatalf("the WARN = %v, want it to name attempt 1", warned[0])
	}
	if logged := atError(records()); len(logged) != 0 {
		t.Fatalf("a first watch that went unanswered once logged %v, want no ERROR", logged)
	}
	if w.NeedsRead() || w.Err() != nil {
		t.Fatalf("while its next attempt is armed the copy reads NeedsRead %v, Err %v; want a read under way and no error", w.NeedsRead(), w.Err())
	}
	if arm.waits() != 1 || arm.asked() != 5*time.Second {
		t.Fatalf("the watcher armed %d waits, the last of %s; want one, of the re-read's 5s", arm.waits(), arm.asked())
	}

	arm.fireIt(t)
	testwait.Eventually(t, "the second attempt's read to complete", w.Following)
	if got, want := listed(into), listed(firstTry); !slices.Equal(got, want) {
		t.Fatalf("the copy the second attempt read = %v, want the first try's %v", got, want)
	}
	if logged := atError(records()); len(logged) != 0 {
		t.Fatalf("a first watch that recovered on its second attempt logged %v, want no ERROR", logged)
	}
}

// The first watch is tried again only while that can help, and the ERROR stays where the failure
// is real: a watch that stays unanswered gives up after its attempts with the one ERROR a failed
// first watch logs, its error recorded and readiness released; and a failure the server answered
// (here a closed connection) is the ERROR at once, with no attempt armed.
func TestAFirstWatchIsMadeAgainOnlyWhileItWentUnanswered(t *testing.T) {
	t.Run("a watch that stays unanswered gives up", func(t *testing.T) {
		_, kv := bucket(t, testnats.URL(t))
		seam := &unansweredWatchKV{KeyValue: kv.WatchHandle(), fails: 1 << 20}
		logger, records := jsonLogger(t)
		arm := &armed{}
		w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), seam, kv.Streams()), newSeen(), kvwatch.WithLogger(logger),
			kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithFirstWatchRetry())
		t.Cleanup(w.Stop)
		w.Start()
		failed := testwait.Until(t, "the watcher to give up on its first watch", func() ([]map[string]any, bool) {
			if found := matching(records(), "ERROR", "test cache watch failed"); len(found) > 0 {
				return found, true
			}
			if warned := len(matching(records(), "WARN", firstAttemptFailed)); warned == arm.waits() && warned > 0 {
				arm.fireIt(t)
			}
			return nil, false
		})
		attempts, _ := failed[0]["attempts"].(float64)
		warned := len(matching(records(), "WARN", firstAttemptFailed))
		if len(failed) != 1 || attempts < 2 || warned != int(attempts)-1 {
			t.Fatalf("the watcher gave up with %v after %d WARNs, want one ERROR naming its attempts, one more than its WARNs", failed, warned)
		}
		if err := w.Err(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Err after the watcher gave up = %v, want the last attempt's deadline", err)
		}
		if err := w.WaitReady(testwait.Context(t)); err != nil {
			t.Fatalf("WaitReady after the watcher gave up: %v", err)
		}
		if !w.NeedsRead() {
			t.Fatal("a copy whose first watch gave up reports no read is needed; nothing would rebuild it")
		}
	})

	t.Run("a closed connection is the ERROR at once", func(t *testing.T) {
		conn, kv := bucket(t, testnats.URL(t))
		conn.Close()
		logger, records := jsonLogger(t)
		arm := &armed{}
		w := kvwatch.New("test cache", kv, newSeen(), kvwatch.WithLogger(logger),
			kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithFirstWatchRetry())
		t.Cleanup(w.Stop)
		w.Start()
		testwait.Eventually(t, "the failed first watch's ERROR", func() bool {
			return len(matching(records(), "ERROR", "test cache watch failed")) == 1
		})
		if warned := matching(records(), "WARN", firstAttemptFailed); len(warned) != 0 || arm.waits() != 0 {
			t.Fatalf("a first watch on a closed connection logged %v and armed %d waits, want the ERROR alone and no attempt armed", warned, arm.waits())
		}
	})
}
