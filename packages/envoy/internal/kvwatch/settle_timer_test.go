package kvwatch_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The scan's idle wait and the wait past the barrier signal the same channel, and stopping a wait
// (Timer.Stop) does not hold back a callback already running. So the scan's retired wait can
// signal after the wait past the barrier was armed. That signal must not end the read: the wait
// past the barrier ends only once its own timeout has gone, measured on the watcher's clock, as the
// scan's idle wait is (idleExpired). Here the retired wait's callback runs after the barrier's
// wait was armed, and the read keeps waiting; the barrier's own wait, once its timeout has gone,
// still fails it.
func TestARetiredScanWaitDoesNotEndTheWaitPastTheBarrier(t *testing.T) {
	const scanIdle = 7 * time.Hour
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	clock := newClock()
	timers := &pending{}
	into := newSeen()
	// The barrier is acknowledged at a sequence the consumer never reaches, so only a wait ends
	// the read.
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}), into,
		kvwatch.WithNow(clock.Now), kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(scanIdle))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the wait past the barrier to be armed", func() bool { return len(timers.armed(scanIdle)) == 2 })
	waits := timers.armed(scanIdle)
	if !waits[0].stopped.Load() {
		t.Fatalf("the scan's idle wait is still armed after its scan ended")
	}

	// The scan's retired wait signals, as its callback would had it started before Stop.
	waits[0].fire()
	eventually(t, "the retired signal to be taken", func() bool {
		return len(timers.armed(scanIdle)) == 3 || len(timers.armed(5*time.Second)) > 0
	})
	if rereads := timers.armed(5 * time.Second); len(rereads) != 0 {
		t.Fatalf("the scan's retired wait ended the wait past the barrier: the read was failed and read again (%d re-reads armed)", len(rereads))
	}
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr after the retired signal = %v, want nil: the read is still waiting", err)
	}

	// The wait past the barrier still ends the read once its own timeout has gone.
	clock.advance(scanIdle + time.Second)
	timers.last(t, scanIdle).fire()
	eventually(t, "the read to be failed and read again", func() bool { return len(timers.armed(5*time.Second)) == 1 })
	if done := len(into.completions()); done != 0 {
		t.Fatalf("%d reads completed, want none", done)
	}
}
