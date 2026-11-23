package bus

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
)

// heldBucketWatch is a watch of every key of a bucket of its own, made through a handle on a client of its
// own connected through a hold (heldClient), with its first nil read, so the watch is built and its
// ordered consumer created. The hold holds nothing yet.
func heldBucketWatch(t *testing.T) (*Client, *testnats.ReplyHold, KeyValue, jetstream.KeyWatcher) {
	t.Helper()
	client, hold := heldClient(t)
	kv, err := EnsureKeyValue(t.Context(), client.Connection(), client.JS(), &nats.KeyValueConfig{Bucket: fmt.Sprintf("stop_watcher_%d", time.Now().UnixNano()), Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("ensure the bucket: %v", err)
	}
	watcher, err := kv.WatchKeys(t.Context(), jetstream.AllKeys)
	if err != nil {
		t.Fatalf("watch the bucket: %v", err)
	}
	select {
	case entry := <-watcher.Updates():
		if entry != nil {
			t.Fatalf("the empty bucket's watch delivered %s before its nil", entry.Key())
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the watch delivered no nil by the test's deadline")
	}
	return client, hold, kv, watcher
}

// deleteSubject is the prefix of the request nats.go's Stop sends after it unsubscribes a watch: the
// delete of the ordered consumer it created for it ($JS.API.CONSUMER.DELETE.<stream>.<consumer>).
const deleteSubject = "$JS.API.CONSUMER.DELETE."

// A watcher's stop is waited for only within its caller's context and the connection's life
// (KeyValue.StopWatcher). nats.go's Stop unsubscribes the watch and then deletes the ordered
// consumer it created for it, a request that takes no context and waits out its JetStream context's
// 5 s default when no answer comes; the hold withholds that answer (testnats.ReplyHold), as a
// connection's drop or a server that does not answer would. Each row decides by order: after, which
// StopWatcher calls once the stop has returned, has not run when StopWatcher answers, so the answer
// did not wait for the stop; and the stop itself went on and returned afterwards. Every wait is
// bounded by the test's deadline alone. The rows log how long each answer and each stop took.
//
//   - A deadline of 500 ms ends the wait: the answer is ctx's deadline error.
//   - The connection's drop ends it (the hold cuts the connection once it has withheld the delete's
//     answer, and refuses every reconnect): the answer is ErrConnectionLost while the row's own
//     context still runs and the client is still disconnected (requireLostAtTheDrop).
//   - A context that has already ended answers at once, and the stop still runs: the watch is
//     unsubscribed, which closes its updates, and the delete is sent, which the hold withholds.
//
// The control is these rows with StopWatcher calling Stop and waiting for it, as every stop did
// before: each answers only once nats.go's own 5 s have passed, after after has run. A second
// control sends the stop through RequestBounded: the ended context starts no stop, so the watch is
// never unsubscribed and its updates never close.
func TestAWatchersStopIsWaitedForOnlyWithinItsBound(t *testing.T) {
	// answeredFirst fails the row unless after had not run when StopWatcher answered err, which the
	// row calls the moment StopWatcher returns.
	answeredFirst := func(t *testing.T, stopped <-chan struct{}, started time.Time, err error) {
		t.Helper()
		took := time.Since(started)
		select {
		case <-stopped:
			t.Fatalf("StopWatcher answered %v after %s, once the stop it waits for had returned: it waited for the consumer's delete", err, took)
		default:
		}
		t.Logf("STOP answered after %s with %v", took.Round(time.Millisecond), err)
	}
	// stopReturned waits for after, which shows the stop went on by itself and returned.
	stopReturned := func(t *testing.T, stopped <-chan struct{}, started time.Time) {
		t.Helper()
		select {
		case <-stopped:
			t.Logf("STOP the stop itself returned %s after it began", time.Since(started).Round(time.Millisecond))
		case <-time.After(testnats.Remaining(t)):
			t.Fatal("the stop StopWatcher left had not returned by the test's deadline")
		}
	}

	t.Run("a deadline ends the wait", func(t *testing.T) {
		_, hold, kv, watcher := heldBucketWatch(t)
		held := hold.Hold(deleteSubject)
		stopped := make(chan struct{})
		const bound = 500 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		started := time.Now()
		err := kv.StopWatcher(ctx, watcher, func() { close(stopped) })
		answeredFirst(t, stopped, started, err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StopWatcher under a %s deadline answered %v, want the deadline's error", bound, err)
		}
		select {
		case <-held.Withheld():
		case <-time.After(testnats.Remaining(t)):
			t.Fatal("the hold withheld no answer to the consumer's delete, so the stop sent none")
		}
		stopReturned(t, stopped, started)
	})

	t.Run("the connection's drop ends the wait", func(t *testing.T) {
		client, hold, kv, watcher := heldBucketWatch(t)
		stopped := make(chan struct{})
		started := time.Now()
		// The request runs on requireLostAtTheDrop's goroutine, so it reads whether the stop had
		// returned as it answers, and the row decides once requireLostAtTheDrop has the answer.
		var err error
		var took time.Duration
		var waited bool
		requireLostAtTheDrop(t, hold, client, deleteSubject, noFlush, func(ctx context.Context) error {
			err = kv.StopWatcher(ctx, watcher, func() { close(stopped) })
			took = time.Since(started)
			select {
			case <-stopped:
				waited = true
			default:
			}
			return err
		})
		if waited {
			t.Fatalf("StopWatcher answered %v after %s, once the stop it waits for had returned: it waited for the consumer's delete", err, took)
		}
		t.Logf("STOP answered after %s with %v", took.Round(time.Millisecond), err)
		stopReturned(t, stopped, started)
	})

	t.Run("a context already ended answers at once and the stop still runs", func(t *testing.T) {
		_, hold, kv, watcher := heldBucketWatch(t)
		held := hold.Hold(deleteSubject)
		stopped := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		started := time.Now()
		err := kv.StopWatcher(ctx, watcher, func() { close(stopped) })
		answeredFirst(t, stopped, started, err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StopWatcher under an ended context answered %v, want its error", err)
		}
		for open := true; open; {
			select {
			case _, open = <-watcher.Updates():
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the watch's updates were still open at the test's deadline: StopWatcher under an ended context did not unsubscribe it")
			}
		}
		select {
		case <-held.Withheld():
		case <-time.After(testnats.Remaining(t)):
			t.Fatal("the hold withheld no answer to the consumer's delete: StopWatcher under an ended context sent none")
		}
		stopReturned(t, stopped, started)
	})
}
