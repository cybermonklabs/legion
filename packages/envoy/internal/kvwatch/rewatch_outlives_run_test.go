package kvwatch_test

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A reconnect hook passes its run's context to Rewatch, and that context ends once the run is over
// or its connection is lost; the watcher the Rewatch installed is the cache's from then on, so it
// must not end with that context. Only the move's requests are made under it (watch): a watcher
// living in the run's context would end as the hook returned, leave the cache frozen with a
// terminal error, and wait for the listener's self-health rebuild to notice.
func TestAWatcherARewatchInstalledOutlivesTheRunThatInstalledIt(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	into := newSeen()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithLogger(logger))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	_, kv2 := bucket(t, uri)
	run, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	var subscription context.Context
	capture := func(ctx context.Context, handle jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		subscription = ctx
		return handle.WatchAll(ctx)
	}
	// WatchUnder runs the same watch(run, kv) as Rewatch. Rewatch's OpenKeyValue only uses
	// run to bound the open requests. It does not give the handle or subscription that lifetime.
	if err := kvwatch.WatchUnder(w, run, scripted(kv2, capture).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	if subscription == nil {
		t.Fatal("the replacing watch did not create a subscription")
	}
	ending := make(chan uint64, 1)
	go func() {
		ending <- goroutinetest.ID()
		end()
	}()
	id := testwait.Receive(t, "the goroutine ending the run", ending)
	returned(t, "the run's cancellation to return", id)
	testwait.Until(t, "any cancellation callbacks to return", func() ([]string, bool) {
		children := startedIn(id, "")
		return children, len(children) == 0
	})
	if err := subscription.Err(); err != nil {
		t.Fatalf("the subscription context ended with the run that installed it: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("the watcher ended with the run that installed it: %v", err)
	}
	if _, err := w.KV().Put("after-the-run", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the watcher to deliver a write made after its run ended", func() bool { return into.has("after-the-run") })
	if err := w.Err(); err != nil {
		t.Fatalf("the watcher ended after its run: %v", err)
	}
	if logged := atError(records()); len(logged) != 0 {
		t.Fatalf("the watcher logged %v after its run ended, want no ERROR", logged)
	}
	if ended := matching(records(), "WARN", stoppedWarn); len(ended) != 0 {
		t.Fatalf("the watcher logged its end after the run that installed it ended: %v", ended)
	}
}
