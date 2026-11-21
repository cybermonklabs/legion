package cputest

import (
	"testing"
	"time"
)

// OnThread counts the work its op does on its own goroutine and leaves out the work op waits on in
// another goroutine, which is what the api's replacement ladder rests on: there the other work is
// the garbage collector's mark workers. Both cases burn the same fixed work and the readings are
// compared with the process clock over the same window, so they compare work, and a loaded machine
// stretches neither side.
func TestOnThreadCountsItsOwnGoroutineAlone(t *testing.T) {
	ownThread, ownProcess := onBothClocks(burn)
	if ownThread < ownProcess/2 {
		t.Fatalf("OnThread read %s for work its own goroutine did, where the process clock read %s over the same window", ownThread, ownProcess)
	}
	otherThread, otherProcess := onBothClocks(func() {
		done := make(chan struct{})
		go func() {
			burn()
			close(done)
		}()
		<-done
	})
	if otherThread > otherProcess/10 {
		t.Fatalf("OnThread read %s while its goroutine waited on another one's work, where the process clock read %s over the same window", otherThread, otherProcess)
	}
	t.Logf("own work: thread %s, process %s; another goroutine's work: thread %s, process %s", ownThread, ownProcess, otherThread, otherProcess)
}

// onBothClocks is op's time on OnThread and the process's CPU time over the same window.
func onBothClocks(op func()) (thread, process time.Duration) {
	before := Process()
	thread = OnThread(op)
	return thread, Process() - before
}

// burnSink keeps burn's loop from being compiled away.
var burnSink uint64

// burn is a fixed amount of CPU work, tens of milliseconds of it, that allocates nothing.
func burn() {
	sum := uint64(0)
	for i := range uint64(100_000_000) {
		sum = sum*31 + i
	}
	burnSink += sum
}
