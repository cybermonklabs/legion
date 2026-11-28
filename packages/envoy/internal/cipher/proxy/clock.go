// packages/envoy/internal/cipher/proxy/clock.go
// The clock the proxy's certificate authority signs leaves and renews its intermediate by: the
// system's in every deployment, and on the local stack the system's moved by an offset a file
// holds (BROKER_PROXY_TEST_CLOCK_OFFSET_FILE), so a check drives days of one running broker's
// life - its renewal at 5 days, a leaf at 6 days and 12 hours - without waiting them out, and
// without a restart, which would make the pod a new key and so a new intermediate.
package proxy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Clock is the time an Authority signs and renews by.
type Clock interface {
	// Now is the time.
	Now() time.Time
	// SleepUntil waits until Now reads t or later, and answers false when ctx ends first.
	SleepUntil(ctx context.Context, t time.Time) bool
}

// systemClock is the system's time.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// systemSleepStep is the longest one timer of systemClock.SleepUntil runs: a wait of days is
// taken in steps, so a wall clock that moved under the pod (a correction after a suspended host)
// is read again within this rather than at the end of a timer set days ago.
const systemSleepStep = time.Hour

func (systemClock) SleepUntil(ctx context.Context, t time.Time) bool {
	return sleepUntil(ctx, t, time.Now, systemSleepStep)
}

// sleepUntil waits until now reads t or later, in timers of step at most so now is read again
// after each, and answers false when ctx ends first.
func sleepUntil(ctx context.Context, t time.Time, now func() time.Time, step time.Duration) bool {
	for {
		wait := t.Sub(now())
		if wait <= 0 {
			return ctx.Err() == nil
		}
		timer := time.NewTimer(min(wait, step))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// offsetSleepStep is how often OffsetFileClock.SleepUntil reads its file again: a check that
// moves the clock sees the broker act on it within this.
const offsetSleepStep = time.Second

// OffsetFileClock is BROKER_PROXY_TEST_CLOCK_OFFSET_FILE's clock: the system's, moved by the Go
// duration the file holds ("0s", "120h1m"), which it reads again at every reading, so writing the
// file moves the clock of a broker that is running. A reading that finds the file unreadable or
// half written keeps the offset it read last, since a writer may be in the middle of writing it;
// a check writes a new file and renames it over the old one.
type OffsetFileClock struct {
	path string

	mu     sync.Mutex
	offset time.Duration
}

// NewOffsetFileClock reads path once and refuses a file it cannot read or that holds no
// duration, so a local stack named wrongly fails at its start rather than running on the
// system's clock.
func NewOffsetFileClock(path string) (*OffsetFileClock, error) {
	c := &OffsetFileClock{path: path}
	offset, err := c.read()
	if err != nil {
		return nil, err
	}
	c.offset = offset
	return c, nil
}

func (c *OffsetFileClock) read() (time.Duration, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return 0, err
	}
	offset, err := time.ParseDuration(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("%s holds no duration such as 120h1m: %w", c.path, err)
	}
	return offset, nil
}

func (c *OffsetFileClock) Now() time.Time {
	offset, err := c.read()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.offset = offset
	}
	return time.Now().Add(c.offset)
}

func (c *OffsetFileClock) SleepUntil(ctx context.Context, t time.Time) bool {
	return sleepUntil(ctx, t, c.Now, offsetSleepStep)
}
