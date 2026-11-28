// packages/envoy/internal/cipher/proxy/clock_test.go
// The wait the proxy's clocks take until a moment: no return before it, a return within one step
// of it, and a return when the wait's context ends.
package proxy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// movedClock is the bubble's clock moved by an offset the test sets, as
// BROKER_PROXY_TEST_CLOCK_OFFSET_FILE moves a broker's: now reads time.Now() plus the offset. The
// offset is held under a lock, since the wait reads it from a goroutine of its own.
type movedClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *movedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// readAt moves the offset so that now reads at, as a check that writes the offset file does.
func (c *movedClock) readAt(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = at.Sub(time.Now())
}

// TestSleepUntilReturnsAtItsMomentAndNoSooner: sleepUntil, the wait both clocks' SleepUntil take
// and so the wait of the renewal loop, does not return while its now reads before the moment it
// waits for, and returns true within one step once now reaches that moment. It runs with each
// clock's step, OffsetFileClock's offsetSleepStep and systemClock's systemSleepStep, in a synctest
// bubble, on a clock an offset moves as BROKER_PROXY_TEST_CLOCK_OFFSET_FILE moves a broker's. The
// moment is an hour ahead, and synctest.Wait shows the wait has not returned at once. The offset
// moves now to a minute before the moment, and holds it there for three steps by moving it back
// every second, and after each step synctest.Wait shows that the wait has not returned. Then the
// offset moves now to the moment, and one step later the wait has returned true. A wait whose
// context ends returns false at once, and a wait whose context has ended returns false even at its
// moment.
//
// cmd/cipherd's TestABrokerRenewsAThreeDayIntermediateWithTwoDaysLeft rests on this test for the
// claim that the broker takes no renewal before the moment its "proxy intermediate renewal" line
// records. That test proves the moment the broker waits for, and no longer holds the broker's
// clock a minute short of it, so a wait that returns early passes there and fails here.
//
// The named reverts: in sleepUntil, return once an hour or less is left (wait <= time.Hour), and
// the wait returns at once, an hour before its moment; or return once a minute or less is left
// (wait <= time.Minute), and the wait returns while now is held a minute before its moment. Either
// way the held rows fail, naming when the wait returned before its moment; the first fails the
// context rows too, since its wait has returned true before its context ends.
func TestSleepUntilReturnsAtItsMomentAndNoSooner(t *testing.T) {
	// heldSteps is how many steps of the wait now is held a minute before the moment.
	const heldSteps = 3
	// rewrite is how often the offset is moved back while now is held. It is less than a
	// minute, so now, which runs on with the bubble's clock between two writes, does not reach
	// the moment.
	const rewrite = time.Second
	for _, clock := range []struct {
		name string
		step time.Duration
	}{
		{"OffsetFileClock's step", offsetSleepStep},
		{"systemClock's step", systemSleepStep},
	} {
		t.Run(clock.name+", now held a minute short and then moved to the moment", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var moved movedClock
				ctx := t.Context()
				target := time.Now().Add(time.Hour)
				short := target.Add(-time.Minute)
				returned := make(chan bool, 1)
				go func() { returned <- sleepUntil(ctx, target, moved.now, clock.step) }()
				// stillWaiting fails t when the wait has returned, naming when.
				stillWaiting := func(when string) {
					t.Helper()
					synctest.Wait()
					select {
					case got := <-returned:
						t.Fatalf("%s, the wait for %s with a step of %v returned %v: the wait returned before its moment",
							when, target.UTC().Format(time.RFC3339), clock.step, got)
					default:
					}
				}
				stillWaiting("with now an hour before the moment and not yet moved")
				moved.readAt(short)
				for held := 1; held <= heldSteps; held++ {
					for end := time.Now().Add(clock.step); time.Now().Before(end); {
						time.Sleep(rewrite)
						moved.readAt(short)
					}
					stillWaiting(fmt.Sprintf("with now held at %s, a minute before the moment, for %d step(s)", short.UTC().Format(time.RFC3339), held))
				}
				moved.readAt(target)
				time.Sleep(clock.step)
				synctest.Wait()
				select {
				case got := <-returned:
					if !got {
						t.Fatalf("the wait with a step of %v returned false after now reached its moment %s; want true, since its context has not ended",
							clock.step, target.UTC().Format(time.RFC3339))
					}
				default:
					t.Fatalf("one step (%v) after now reached the moment %s, the wait has not returned; want it to return within one step",
						clock.step, target.UTC().Format(time.RFC3339))
				}
			})
		})
		t.Run(clock.name+", its context ended", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var moved movedClock
				ctx, cancel := context.WithCancel(t.Context())
				target := time.Now().Add(time.Hour)
				returned := make(chan bool, 1)
				go func() { returned <- sleepUntil(ctx, target, moved.now, clock.step) }()
				synctest.Wait()
				cancel()
				synctest.Wait()
				select {
				case got := <-returned:
					if got {
						t.Fatalf("the wait with a step of %v, for a moment an hour ahead, returned true, and its context ended before that moment; want false", clock.step)
					}
				default:
					t.Fatalf("the wait with a step of %v has not returned when its context ended; want it to return false at once", clock.step)
				}
				if sleepUntil(ctx, time.Now(), moved.now, clock.step) {
					t.Fatalf("a wait with a step of %v whose context had ended returned true at its moment; want false", clock.step)
				}
			})
		})
	}
}
