package sessionanswers

import (
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Every read that ended runs no longer: reads that begin and end on many goroutines at once,
// while another asks Slow throughout as the keyword fill does, leave no read running behind them,
// so once the last has ended a moment past SlowRead finds nothing slow. A read left running would
// hold the fill paused for as long as the process lives.
func TestEveryAnswersReadThatEndedRunsNoLonger(t *testing.T) {
	latency := NewLatency(time.Now)
	const readers, reads = 16, 200
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			for range reads {
				latency.Begin().End()
			}
		})
	}
	stop := make(chan struct{})
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		for {
			select {
			case <-stop:
				return
			default:
				latency.Slow(time.Now())
			}
		}
	}()
	testwait.Returns(t, "every reader's Begins and Ends to return", wg.Wait)
	close(stop)
	testwait.Receive(t, "the goroutine asking Slow to stop once told", asked)
	ended := time.Now()
	if len(latency.running) != 0 {
		t.Fatalf("%d reads that each ended left %d running", readers*reads, len(latency.running))
	}
	if latency.Slow(ended.Add(SlowRead + time.Millisecond)) {
		t.Fatalf("%d reads that each ended at once left one slow past %v", readers*reads, SlowRead)
	}
}
