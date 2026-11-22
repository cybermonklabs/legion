package stream

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A newer process still refused LockOutReportAfter after its first hello is reported once to the
// project's controller, naming the claim and both generations, and R6's line is written once
// beside it: only a person can end a process that cannot be stopped, and the controller cannot.
// Both end with the lock-out.
//
// The older process here is that process: it answers what the daemon asks it, as Oh My Pi does
// through its shim, and ignores its stop frame. A connection that answers stays bound past the
// window in which a silent one is ended, and is ended by nobody but its own process.
func TestANewerProcessStillRefusedIsReportedOnce(t *testing.T) {
	clock := &lockOutClock{now: time.Date(2026, 11, 12, 20, 30, 0, 0, time.UTC)}
	reports := make(chan LockOut, 4)
	alertLog := &logRecorder{}
	alerts := fleetalert.New(slog.New(alertLog), "acme")
	h := startListener(t, harnessOptions{
		resolver:      generations(map[string]uint64{testToken: testGeneration, newerToken: testGeneration + 1}),
		clock:         clock,
		alerts:        alerts,
		reportLockOut: func(out LockOut) { reports <- out },
	})
	older := h.connect(testToken)
	bound := h.conn()
	first := clock.Now()

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	older.expect(shimwire.TypeShutdown)
	newer.awaitClosed()
	older.answerState(older.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol))

	want := LockOut{Claim: testClaim, Newer: testGeneration + 1, Older: testGeneration, Since: first}
	if out, ok := h.listener.LockedOut(testClaim); !ok || out != want {
		t.Fatalf("LockedOut = %#v, %t, want %#v", out, ok, want)
	}

	clock.advance(LockOutReportAfter - time.Second)
	select {
	case out := <-reports:
		t.Fatalf("the lock-out was reported after %s, before %s: %#v", LockOutReportAfter-time.Second, LockOutReportAfter, out)
	default:
	}
	if !daemonEndOpen(bound) {
		t.Fatalf("the older connection was ended within %s although it answered", LockOutReportAfter-time.Second)
	}
	clock.advance(time.Second)

	out := awaitReport(t, reports)
	if out != want {
		t.Fatalf("report = %#v, want %#v", out, want)
	}
	if got, want := out.Reason(), "generation 4 has been refused the worker stream for 5 minutes; generation 3 is still connected"; got != want {
		t.Fatalf("the notice's reason = %q, want %q", got, want)
	}

	// Refused again, and the report and the line stay at one for this lock-out.
	repeat := dial(t, h.listener.Addr())
	repeat.hello(newerToken)
	repeat.awaitClosed()
	clock.advance(LockOutReportAfter)
	select {
	case out := <-reports:
		t.Fatalf("the lock-out was reported twice: %#v", out)
	default:
	}
	lines := alertLog.Lines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], fleetalert.Prefix+": ") ||
		!strings.Contains(lines[0], "case="+fleetalert.NewerProcessRefused.Word) ||
		!strings.Contains(lines[0], "claim="+string(testClaim)) ||
		!strings.Contains(lines[0], "generation=4") || !strings.Contains(lines[0], "boundGeneration=3") {
		t.Fatalf("the fleet alert lines = %q, want one naming the case, the claim and both generations", lines)
	}

	// The lock-out ends with the older connection, and so does the line a person was told by.
	older.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim, Generation: testGeneration}) {
		t.Fatalf("event = %#v, want the older connection's close", event)
	}
	if out, ok := h.listener.LockedOut(testClaim); ok {
		t.Fatalf("LockedOut = %#v after the older connection closed, want no lock-out", out)
	}
	if got := alertLog.Lines(); !slices.Equal(got, lines) {
		t.Fatalf("the fleet alert lines = %q, want the one line the lock-out opened", got)
	}
	// The case ended with the lock-out, so the writer holds its line open no more: the next open
	// of it writes the line again.
	if !alerts.Open(fleetalert.NewerProcessRefused, string(testClaim)) {
		t.Fatalf("the fleet alert lines = %q: the line is still open after the lock-out ended", alertLog.Lines())
	}
}

func awaitReport(t *testing.T, reports <-chan LockOut) LockOut {
	t.Helper()
	select {
	case out := <-reports:
		return out
	case <-time.After(wait):
		t.Fatal("the lock-out was never reported")
		return LockOut{}
	}
}

// lockOutClock is the time the listener's lock-out waits on, its report and its older
// connection's silence, moved by the test: five minutes of a lock-out is not five minutes of a
// test.
type lockOutClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*lockOutTimer
}

// lockOutTimer is one registered wait. Its state is the clock's to read and write, under the
// clock's own lock: the listener stops a timer from the goroutine of the connection that ended
// the lock-out, while the test's own goroutine is moving the clock.
type lockOutTimer struct {
	clock   *lockOutClock
	at      time.Time
	fire    func()
	stopped bool
}

func (c *lockOutClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockOutClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &lockOutTimer{clock: c, at: c.now.Add(d), fire: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *lockOutTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	stopped := !t.stopped
	t.stopped = true
	return stopped
}

// awaitTimers waits until n waits have been registered on the clock, stopped ones included: a
// wait the listener registers on a goroutine of its own is not there the moment a hello is
// refused.
func (c *lockOutClock) awaitTimers(t *testing.T, n int) {
	t.Helper()
	testwait.WithinPolling(t, wait, testwait.Poll, fmt.Sprintf("%d waits on the listener's clock", n), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.timers) >= n
	})
}

// advance moves the clock and runs every wait that has come due, in the test's own goroutine, so
// what a report does has happened by the time advance returns.
func (c *lockOutClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	due := make([]*lockOutTimer, 0, len(c.timers))
	for _, timer := range c.timers {
		if !timer.stopped && !timer.at.After(now) {
			timer.stopped = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.fire()
	}
}
