package daemon

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeConnStatus is reconnectWatchdog's test double for *nats.Conn: a test sets connected and err
// directly, without a real connection or a NATS server.
type fakeConnStatus struct {
	mu        sync.Mutex
	connected bool
	err       error
}

func (c *fakeConnStatus) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *fakeConnStatus) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *fakeConnStatus) set(connected bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected, c.err = connected, err
}

// runWatchdog starts the watchdog on a clock the test holds still (heldClock) and waits for it to
// arm its first poll, so the test's first fire has one to fire. The returned stop ends the
// watchdog and waits for it to return, and runs at cleanup too.
func runWatchdog(t *testing.T, w *workflowRuntime, conn connStatus, clock *heldClock) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.reconnectWatchdogWith(ctx, conn); close(done) }()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	testwait.Eventually(t, "the watchdog arms its first poll", func() bool {
		live, _ := clock.armed(w.reconnectPollInterval)
		return live == 1
	})
	return stop
}

// takePoll fires the one poll the watchdog waits on and waits for it to arm the next, which it
// does only once that poll has been taken: a check that the watchdog said nothing is then about a
// poll that ran rather than one that had not happened yet. The poll interval and the warnEvery
// each test below sets name the timers rather than cost it anything — the clock is held, so the
// test fires each poll and moves the downtime itself.
func takePoll(t *testing.T, w *workflowRuntime, clock *heldClock) {
	t.Helper()
	_, before := clock.armed(w.reconnectPollInterval)
	if fired := clock.fire(w.reconnectPollInterval); fired != 1 {
		t.Fatalf("poll timers fired = %d, want the one the watchdog armed", fired)
	}
	testwait.Eventually(t, "the watchdog takes its poll and arms the next", func() bool {
		live, total := clock.armed(w.reconnectPollInterval)
		return live == 1 && total == before+1
	})
}

// A NATS reconnect that keeps failing a non-auth handshake — a server that accepts the TCP
// connection but never completes the protocol handshake, say — retries forever under
// natsauth.ReconnectForever behind one "NATS connection lost" line, stuck in RECONNECTING, since
// LogEvents' DisconnectedErrCB and ReconnectedCB each fire once per disconnect/reconnect pair and
// say nothing while stuck between them. reconnectWatchdog is what gives an operator a periodic
// signal instead: a warn every reconnectWarnEvery naming the downtime so far and the connection's
// own LastError, and none at all while connected.
func TestReconnectWatchdogWarnsWhileDisconnectedNotWhileConnected(t *testing.T) {
	t.Parallel()
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: true}
	clock := &heldClock{at: time.Now()}
	w := &workflowRuntime{log: log, reconnectPollInterval: 5 * time.Millisecond, reconnectWarnEvery: 20 * time.Millisecond, reconnectClock: clock}
	stop := runWatchdog(t, w, conn, clock)

	// Eight polls spanning twice warnEvery, every one of them on a connection that is up: a
	// watchdog that warned while connected had every chance to here.
	for range 8 {
		clock.advance(w.reconnectWarnEvery / 4)
		takePoll(t, w, clock)
	}
	if strings.Contains(logBuf.String(), "NATS has not reconnected") {
		t.Fatal("the watchdog warned while connected")
	}

	conn.set(false, errors.New("nats: protocol exception, INFO not received"))
	// The poll that finds the connection down starts its downtime; the warn waits for that
	// downtime to reach warnEvery, which the next poll is taken at.
	takePoll(t, w, clock)
	clock.advance(w.reconnectWarnEvery)
	takePoll(t, w, clock)
	testwait.Eventually(t, "the watchdog warns while disconnected", func() bool {
		return strings.Contains(logBuf.String(), "NATS has not reconnected")
	})
	stop()

	msg := logBuf.String()
	if !strings.Contains(msg, "downtime=") {
		t.Fatalf("warn log = %q, want it to name the downtime so far", msg)
	}
	if !strings.Contains(msg, "protocol exception, INFO not received") {
		t.Fatalf("warn log = %q, want it to name conn.LastError()", msg)
	}
}

// Once NATS reconnects, the watchdog's downtime clock and its own warn throttle both reset: a
// later disconnect waits out its own fresh warnEvery before its first warn, rather than reusing
// a downtime or a last-warned time left over from the previous disconnect. The connected interval
// here outlasts warnEvery itself, so a broken reset's leftover clocks would already be overdue the
// moment the second disconnect begins, and would warn within one poll tick of it — which the
// no-warn check below would catch.
func TestReconnectWatchdogResetsAfterReconnecting(t *testing.T) {
	t.Parallel()
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: false, err: errors.New("first outage")}
	warnEvery := 1200 * time.Millisecond
	clock := &heldClock{at: time.Now()}
	w := &workflowRuntime{log: log, reconnectPollInterval: 5 * time.Millisecond, reconnectWarnEvery: warnEvery, reconnectClock: clock}
	stop := runWatchdog(t, w, conn, clock)

	takePoll(t, w, clock)
	clock.advance(warnEvery)
	takePoll(t, w, clock)
	testwait.Eventually(t, "the watchdog warns about the first outage", func() bool {
		return strings.Contains(logBuf.String(), "first outage")
	})
	conn.set(true, nil)
	clock.advance(3 * warnEvery / 2)
	takePoll(t, w, clock)
	logBuf.Reset()
	conn.set(false, errors.New("second outage"))

	// With a broken reset, disconnectedSince and warnedAt would still be the first outage's —
	// already more than warnEvery stale by now — so the very next poll would warn. The poll that
	// finds the connection down again is taken here, and another a quarter of warnEvery after it,
	// and neither may say anything.
	takePoll(t, w, clock)
	clock.advance(warnEvery / 4)
	takePoll(t, w, clock)
	if strings.Contains(logBuf.String(), "second outage") {
		t.Fatalf("warned about the second outage before its own fresh warnEvery elapsed (the reset is broken): %q", logBuf.String())
	}

	clock.advance(warnEvery)
	takePoll(t, w, clock)
	testwait.Eventually(t, "the watchdog warns about the second outage once its own warnEvery elapses", func() bool {
		return strings.Contains(logBuf.String(), "second outage")
	})
	msg := logBuf.String()
	match := reconnectDowntimeRe.FindStringSubmatch(msg)
	if match == nil {
		t.Fatalf("warn log = %q, want a parseable downtime=", msg)
	}
	downtime, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("parse downtime %q: %v", match[1], err)
	}
	// Counted from the second disconnect alone, downtime is close to warnEvery. Counted
	// cumulatively from the first disconnect (connected interval included, as a broken reset
	// would), it would be well over twice that.
	if downtime >= 2*warnEvery {
		t.Fatalf("second outage's downtime = %v, want close to warnEvery (%v), not cumulative since the first outage began", downtime, warnEvery)
	}
	stop()
}

var reconnectDowntimeRe = regexp.MustCompile(`downtime=(\S+)`)

// A watchdog that warned on the very first poll after a disconnect — downtime=0s, rounded down
// from whatever the poll interval happened to be — would be close to as noisy as nats.go's own
// default 2s reconnect wait: the deep review measured about one ordinary reconnect in five
// tripping it. The first warn must wait for the downtime itself to reach warnEvery, exactly as
// pollHoldReleaseWith holds its own first warn for holdWarnAfter, then warn every warnEvery after.
func TestReconnectWatchdogWarnsOnlyOnceDowntimeReachesWarnEvery(t *testing.T) {
	t.Parallel()
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: true}
	warnEvery := 1200 * time.Millisecond
	clock := &heldClock{at: time.Now()}
	w := &workflowRuntime{log: log, reconnectPollInterval: 50 * time.Millisecond, reconnectWarnEvery: warnEvery, reconnectClock: clock}
	stop := runWatchdog(t, w, conn, clock)

	conn.set(false, errors.New("handshake failure"))
	// The poll that first finds the connection down, and another half a warnEvery into the
	// outage: the first warn waits for the downtime itself to reach warnEvery, so neither of them
	// may say anything.
	takePoll(t, w, clock)
	clock.advance(warnEvery / 2)
	takePoll(t, w, clock)
	if strings.Contains(logBuf.String(), "NATS has not reconnected") {
		t.Fatalf("warned before downtime reached warnEvery: %q", logBuf.String())
	}

	clock.advance(warnEvery)
	takePoll(t, w, clock)
	testwait.Eventually(t, "the watchdog warns once downtime reaches warnEvery", func() bool {
		return strings.Contains(logBuf.String(), "NATS has not reconnected")
	})
	stop()

	msg := logBuf.String()
	match := reconnectDowntimeRe.FindStringSubmatch(msg)
	if match == nil {
		t.Fatalf("warn log = %q, want a parseable downtime=", msg)
	}
	downtime, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("parse downtime %q: %v", match[1], err)
	}
	if downtime < time.Second {
		t.Fatalf("first warn's downtime = %v (logged %q), want >= warnEvery (%v) — a downtime this low means it warned near the first poll, not once downtime reached warnEvery", downtime, match[1], warnEvery)
	}
}
