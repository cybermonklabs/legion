package kvwatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
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

	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	run, end := context.WithCancel(context.Background())
	if err := w.Rewatch(run, conn); err != nil {
		end()
		t.Fatalf("rewatch: %v", err)
	}
	end()
	// Time for a watcher that lives in the run's context to end, so a write below cannot beat it.
	time.Sleep(time.Second)
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
