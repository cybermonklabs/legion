package supervisetest

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Clock is supervise.Clock as time that moves only when the test advances it, firing each timer
// that falls due, in order, on the test's goroutine: a test reads a claim's machine at the moment
// before a retry and the moment of it (Advance). Its backoff costs nothing (Sleep). The zero value
// stands at the zero time; set Now's start with NewClock.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*clockTimer
}

type clockTimer struct {
	at            time.Time
	f             func()
	stopped, done bool
}

// NewClock is a Clock standing at now.
func NewClock(now time.Time) *Clock { return &Clock{now: now} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) AfterFunc(d time.Duration, f func()) supervise.Cancel {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &clockTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, timer)
	return clockCancel{c, timer}
}

// Sleep returns at once: a launch's backoff costs the test nothing.
func (c *Clock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// Advance moves time forward by d, firing what falls due on the way, and waits for every decision
// a firing started on m, the machine whose timers these are (supervise.Machine.Wait).
func (c *Clock) Advance(m interface{ Wait() }, d time.Duration) {
	c.mu.Lock()
	until := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *clockTimer
		for _, timer := range c.timers {
			if !timer.stopped && !timer.done && !timer.at.After(until) && (next == nil || timer.at.Before(next.at)) {
				next = timer
			}
		}
		if next == nil {
			c.now = until
			c.mu.Unlock()
			m.Wait()
			return
		}
		next.done, c.now = true, next.at
		c.mu.Unlock()
		next.f()
		m.Wait()
	}
}

type clockCancel struct {
	clock *Clock
	timer *clockTimer
}

func (s clockCancel) Stop() bool {
	s.clock.mu.Lock()
	defer s.clock.mu.Unlock()
	if s.timer.stopped || s.timer.done {
		return false
	}
	s.timer.stopped = true
	return true
}
