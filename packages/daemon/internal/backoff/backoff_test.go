package backoff

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Each wait lies between half its step and the whole of it, the step doubling from base with each
// failure and growing no more after the sixth; the draws spread over that span rather than landing
// on one point.
func TestEachWaitLiesBetweenHalfItsStepAndTheWholeOfIt(t *testing.T) {
	const base = 2 * time.Second
	for failures, step := range map[int]time.Duration{
		1: base, 2: 2 * base, 3: 4 * base, 4: 8 * base, 5: 16 * base, 6: 32 * base, 7: 32 * base, 20: 32 * base,
	} {
		least, most := step, time.Duration(0)
		for range 1000 {
			wait := Wait(base, failures)
			if wait < step/2 || wait > step {
				t.Fatalf("Wait(%s, %d) = %s, want between %s and %s", base, failures, wait, step/2, step)
			}
			least, most = min(least, wait), max(most, wait)
		}
		if most-least < step/4 {
			t.Errorf("1000 draws of Wait(%s, %d) spanned %s to %s, want them spread over %s to %s", base, failures, least, most, step/2, step)
		}
	}
}

// Sleep returns at once when its context has ended, with that context's error, and otherwise after
// the whole wait with none. The row runs in a synctest bubble, whose clock moves only once every
// goroutine in it waits: "at once" is returning before the clock moves at all, and "the whole wait"
// is still waiting a nanosecond before the wait ends and done once it has.
func TestSleepEndsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ended := make(chan error, 1)
		go func() { ended <- Sleep(ctx, time.Hour) }()
		synctest.Wait()
		select {
		case err := <-ended:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Sleep on an ended context = %v, want context.Canceled", err)
			}
		default:
			t.Fatalf("Sleep on an ended context still waits before the clock has moved, want context.Canceled at once")
		}

		slept := make(chan error, 1)
		go func() { slept <- Sleep(context.Background(), 20*time.Millisecond) }()
		time.Sleep(20*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-slept:
			t.Fatalf("Sleep(20ms) = %v a nanosecond before its wait ended, want it still waiting", err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-slept:
			if err != nil {
				t.Fatalf("Sleep(20ms) = %v after the whole wait, want nil", err)
			}
		default:
			t.Fatalf("Sleep(20ms) still waits once its whole wait has passed")
		}
	})
}
