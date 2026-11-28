// packages/envoy/internal/cipher/proxy/clock_test.go
// The wait the proxy's clocks take until a moment, by sleepUntil and by OffsetFileClock on the
// file a check writes: no return before it, a return within one step of it, and a return when
// the wait's context ends.
package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// waitClock is the clock one row of TestSleepUntilReturnsAtItsMomentAndNoSooner waits on, made
// inside the row's bubble: the wait until a moment, the clock's reading, and the test's way of
// moving that reading.
type waitClock struct {
	sleepUntil func(ctx context.Context, at time.Time) bool
	now        func() time.Time
	readAt     func(at time.Time)
}

// movedWait is sleepUntil itself, with step, on a movedClock.
func movedWait(step time.Duration) func(*testing.T) waitClock {
	return func(*testing.T) waitClock {
		moved := &movedClock{}
		return waitClock{
			sleepUntil: func(ctx context.Context, at time.Time) bool { return sleepUntil(ctx, at, moved.now, step) },
			now:        moved.now,
			readAt:     moved.readAt,
		}
	}
}

// offsetFileWait is OffsetFileClock itself, made by NewOffsetFileClock from a file under
// t.TempDir() that holds "0s", whose reading the test moves by writing that file.
func offsetFileWait(t *testing.T) waitClock {
	path := filepath.Join(t.TempDir(), "clock-offset")
	writeOffset(t, path, 0)
	clock, err := NewOffsetFileClock(path)
	if err != nil {
		t.Fatal(err)
	}
	return waitClock{
		sleepUntil: clock.SleepUntil,
		now:        clock.Now,
		readAt:     func(at time.Time) { writeOffset(t, path, at.Sub(time.Now())) },
	}
}

// writeOffset writes offset into the clock file at path as scripts/proxy-chain-check.sh's
// set_clock_at does: into a new file beside it, renamed over it, so no reading finds half a write.
func writeOffset(t *testing.T, path string, offset time.Duration) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(offset.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// TestSleepUntilReturnsAtItsMomentAndNoSooner: the wait the renewal loop takes until the moment
// its "proxy intermediate renewal" line records does not return while the clock reads before that
// moment, and returns true within one step once the clock reaches it. Three rows wait, each in a
// synctest bubble, on a clock moved as BROKER_PROXY_TEST_CLOCK_OFFSET_FILE moves a broker's. Two
// are sleepUntil itself, with each clock's step (OffsetFileClock's offsetSleepStep and
// systemClock's systemSleepStep), on the bubble's clock plus an offset the test sets. The third is
// OffsetFileClock.SleepUntil itself, the wait of a broker on that file, on a clock
// NewOffsetFileClock made from a file under t.TempDir(), whose reading the test moves by writing
// a new file and renaming it over the old, as scripts/proxy-chain-check.sh's set_clock_at does;
// it proves too that the wrapper waits for the moment it is given and that Now reads the file
// again at every reading. A read of the file is a system call, which does not durably block a
// goroutine, so synctest.Wait returns only once the wait's goroutine has read the file and blocks
// on its timer.
//
// In each row the moment is an hour ahead, and synctest.Wait shows the wait has not returned at
// once. The clock is moved to a minute before the moment, and its Now reads that; it is held there
// for three steps by moving it back every second, and after each step synctest.Wait shows that the
// wait has not returned. Then the clock is moved to the moment, its Now reads that, and one step
// later the wait has returned true. A wait whose context ends returns false at once, and a wait
// whose context has ended returns false even at its moment.
//
// cmd/cipherd's TestABrokerRenewsAThreeDayIntermediateWithTwoDaysLeft rests on this test for the
// claim that the broker takes no renewal before the moment its "proxy intermediate renewal" line
// records. That test proves the moment the broker waits for, and no longer holds the broker's
// clock a minute short of it, so a wait that returns early passes there and fails here.
//
// The named reverts: in sleepUntil, return once an hour or less is left (wait <= time.Hour), and
// every row's wait returns at once, an hour before its moment; or return once a minute or less is
// left (wait <= time.Minute), and every row's wait returns while the clock is held a minute before
// its moment. In OffsetFileClock.SleepUntil, wait for an hour before the moment it is given
// (t.Add(-time.Hour)), and the file row's wait returns at once. In OffsetFileClock.Now, keep the
// offset NewOffsetFileClock read (time.Now().Add(c.offset) with no read of the file), and the file
// row's Now does not read the moments the file is moved to, nor has its wait returned a step after
// the moment. Each fails the held rows, naming that; the two hour reverts fail the context rows of
// the rows they reach too, since their wait returns true before its context ends.
func TestSleepUntilReturnsAtItsMomentAndNoSooner(t *testing.T) {
	// heldSteps is how many steps of the wait the clock is held a minute before the moment.
	const heldSteps = 3
	// rewrite is how often the clock is moved back while it is held. It is less than a minute,
	// so the clock, which runs on with the bubble's between two moves, does not reach the moment.
	const rewrite = time.Second
	for _, row := range []struct {
		name string
		step time.Duration
		// moved is what moves the clock's reading, as a failure names it.
		moved string
		clock func(*testing.T) waitClock
	}{
		{"sleepUntil with OffsetFileClock's step", offsetSleepStep, "the offset", movedWait(offsetSleepStep)},
		{"sleepUntil with systemClock's step", systemSleepStep, "the offset", movedWait(systemSleepStep)},
		{"OffsetFileClock.SleepUntil on its file", offsetSleepStep, "the offset file", offsetFileWait},
	} {
		t.Run(row.name+", the clock held a minute short and then moved to the moment", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := row.clock(t)
				ctx := t.Context()
				target := time.Now().Add(time.Hour)
				short := target.Add(-time.Minute)
				returned := make(chan bool, 1)
				go func() { returned <- clock.sleepUntil(ctx, target) }()
				// moveTo moves the clock to read at, and fails t when its Now does not read at.
				moveTo := func(at time.Time) {
					t.Helper()
					clock.readAt(at)
					if got := clock.now(); !got.Equal(at) {
						t.Errorf("after %s was moved so the clock reads %s, its Now reads %s: it did not read %s again",
							row.moved, at.UTC().Format(time.RFC3339), got.UTC().Format(time.RFC3339), row.moved)
					}
				}
				// stillWaiting fails t when the wait has returned, naming when.
				stillWaiting := func(when string) {
					t.Helper()
					synctest.Wait()
					select {
					case got := <-returned:
						t.Fatalf("%s, the wait for %s with a step of %v returned %v: the wait returned before its moment",
							when, target.UTC().Format(time.RFC3339), row.step, got)
					default:
					}
				}
				stillWaiting("with the clock an hour before the moment and not yet moved")
				moveTo(short)
				for held := 1; held <= heldSteps; held++ {
					for end := time.Now().Add(row.step); time.Now().Before(end); {
						time.Sleep(rewrite)
						clock.readAt(short)
					}
					stillWaiting(fmt.Sprintf("with the clock held at %s, a minute before the moment, for %d step(s)", short.UTC().Format(time.RFC3339), held))
				}
				moveTo(target)
				time.Sleep(row.step)
				synctest.Wait()
				select {
				case got := <-returned:
					if !got {
						t.Fatalf("the wait with a step of %v returned false after the clock reached its moment %s; want true, since its context has not ended",
							row.step, target.UTC().Format(time.RFC3339))
					}
				default:
					t.Fatalf("one step (%v) after %s moved the clock to the moment %s, the wait has not returned; want it to return within one step",
						row.step, row.moved, target.UTC().Format(time.RFC3339))
				}
			})
		})
		t.Run(row.name+", its context ended", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := row.clock(t)
				ctx, cancel := context.WithCancel(t.Context())
				target := time.Now().Add(time.Hour)
				returned := make(chan bool, 1)
				go func() { returned <- clock.sleepUntil(ctx, target) }()
				synctest.Wait()
				cancel()
				synctest.Wait()
				select {
				case got := <-returned:
					if got {
						t.Fatalf("the wait with a step of %v, for a moment an hour ahead, returned true, and its context ended before that moment; want false", row.step)
					}
				default:
					t.Fatalf("the wait with a step of %v has not returned when its context ended; want it to return false at once", row.step)
				}
				if clock.sleepUntil(ctx, clock.now()) {
					t.Fatalf("a wait with a step of %v whose context had ended returned true at its moment; want false", row.step)
				}
			})
		})
	}
}
