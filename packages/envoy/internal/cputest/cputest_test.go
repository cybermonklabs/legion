package cputest

import (
	"testing"
	"time"
)

// OnThread counts the work its op does on its own goroutine and leaves out the work op waits on in
// another goroutine, which is what the api's replacement ladder rests on: there the other work is
// the garbage collector's mark workers. Both cases burn the same amount of CPU and the readings are
// compared with the process clock over the same window, so they compare work, and a loaded machine
// stretches neither side.
//
// Each burn lasts burnSteps steps of the coarser clock, and at least minBurn, so the second check's
// bound, a tenth of the burn, is several clock steps wide on any clock: on gVisor, whose clocks
// move in 10 ms steps, the burn lasts 500 ms and one step charged to the waiting thread is a fifth
// of the bound. The process clock, read where OnThread reads the thread's, still reads the whole
// burn and fails it.
func TestOnThreadCountsItsOwnGoroutineAlone(t *testing.T) {
	work := max(minBurn, burnSteps*max(resolution(), threadResolution()))
	ownThread, ownProcess := onBothClocks(func() { burnFor(work) })
	if ownThread < ownProcess/2 {
		t.Fatalf("OnThread read %s for work its own goroutine did, where the process clock read %s over the same window", ownThread, ownProcess)
	}
	otherThread, otherProcess := onBothClocks(func() {
		done := make(chan struct{})
		go func() {
			burnFor(work)
			close(done)
		}()
		<-done
	})
	if otherThread > otherProcess/10 {
		t.Fatalf("OnThread read %s while its goroutine waited on another one's work, where the process clock read %s over the same window", otherThread, otherProcess)
	}
	t.Logf("clock steps: process %s, thread %s; burn %s; own work: thread %s, process %s; another goroutine's work: thread %s, process %s",
		resolution(), threadResolution(), work, ownThread, ownProcess, otherThread, otherProcess)
}

// burnSteps is how many steps of the coarser clock each burn lasts, so a tenth of it, the second
// check's bound, is five steps.
const burnSteps = 50

// minBurn is the shortest burn, where both clocks step far under a millisecond: long enough that
// the waiting goroutine's own cost, tens of microseconds to start another goroutine and park on
// its channel, is far inside a tenth of it.
const minBurn = 80 * time.Millisecond

// onBothClocks is op's time on OnThread and the process's CPU time over the same window.
func onBothClocks(op func()) (thread, process time.Duration) {
	before := Process()
	thread = OnThread(op)
	return thread, Process() - before
}

// burnSink keeps burnFor's loop from being compiled away.
var burnSink uint64

// burnFor spends at least d of this process's CPU time on the calling goroutine, allocating
// nothing.
func burnFor(d time.Duration) {
	sum := uint64(0)
	for start := Process(); Process()-start < d; {
		for i := range uint64(1_000_000) {
			sum = sum*31 + i
		}
	}
	burnSink += sum
}
