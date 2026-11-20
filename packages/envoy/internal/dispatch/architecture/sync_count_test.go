package architecture

import (
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A call of Sync counts as a sync running (Syncing) from before it looks for a flight: until the
// flight's goroutine begins and counts itself, the call's own count is the only one the sync has,
// and a caller that leaves then leaves its flight to start and write without it. The test holds a
// call once it has stamped itself (afterCalledAt), before any flight of its source exists, and
// Syncing must answer true; once the call is let go and has answered, Syncing falls.
func TestSyncingCountsACallBeforeItsFlightBegins(t *testing.T) {
	rig := newRefusersRig(t, 0)
	stamped, held := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	// Cleanups run last registered first: a test that fails while the call is held lets it go
	// before Close waits for the syncs.
	t.Cleanup(release)
	rig.process.afterCalledAt = func() {
		close(stamped)
		<-held
	}
	answer := make(chan syncAnswer, 1)
	rig.syncOf(archRepo, answer)
	receiveBeforeTheDeadline(t, "the call to stamp itself", stamped)
	if taken, lookups := rig.taken.Load(), rig.lookups.Load(); taken != 0 || lookups != 0 {
		t.Fatalf("UNKNOWN: the held call had taken %d flights and GitHub had answered %d head lookups, so this run shows nothing", taken, lookups)
	}
	if !rig.process.Syncing() {
		t.Fatal("Syncing answered false while a call of Sync that had stamped itself was held before its flight began")
	}
	release()
	if got := receiveBeforeTheDeadline(t, "the call's answer", answer); got.err != nil {
		t.Fatalf("the call answered %v once let go", got.err)
	}
	testwait.WithinPolling(t, beforeTheDeadline(t), testwait.Poll, "Syncing to fall once the call had answered", func() bool { return !rig.process.Syncing() })
}
