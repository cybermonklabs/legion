package kvwatch

import (
	"context"

	"github.com/sjawhar/envoy/internal/bus"
)

// WatchOn installs a new read on kv as Rewatch does once it has opened its handle. Rewatch opens
// that handle itself (bus.OpenKeyValue), so a test that holds, times out or ends a re-read's scan
// installs that read through here, on a handle that does so.
func WatchOn(w *Watcher, kv bus.KeyValue) error {
	return w.watch(context.Background(), kv, 0)
}

// WatchUnder installs a new read on kv under run, as a reconnect hook's Rewatch does once it has
// opened its handle, so a test can end the run at a moment of its choosing through kv.
func WatchUnder(w *Watcher, run context.Context, kv bus.KeyValue) error {
	return w.watch(run, kv, 0)
}

// RereadUnder installs a re-read of generation on kv under run: a generation that is not current
// makes the watch's watcher an overtaken one, which it discards.
func RereadUnder(w *Watcher, run context.Context, kv bus.KeyValue, generation uint64) error {
	return w.watch(run, kv, generation)
}

// Generation is the watcher's current generation: the one a re-read armed now would read again.
func Generation(w *Watcher) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.generation
}

// Waiting reports whether a read is waiting past its barrier (settleSince is set). It turns false
// when the wait ends (endSettle, clearSettleLocked), and that marks only the wait's end, not the
// end of what the path that ended it does afterwards. settleFailed counts a failure
// (readAgainUnverified) after it ends the wait, so a test that reads what such a path left must
// also wait on a fact that comes after it, such as a record the same consume goroutine delivers.
// settleReplaced does nothing after it ends the wait but release readiness.
func Waiting(w *Watcher) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return !w.settleSince.IsZero()
}

// RereadOn installs a re-read of generation on kv as reread does once its wait has passed: it is
// discarded unless generation is still current.
func RereadOn(w *Watcher, kv bus.KeyValue, generation uint64) error {
	return w.watch(context.Background(), kv, generation)
}
