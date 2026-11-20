// The lives of a NATS connection, as nats.go itself reports them (connLife), for a check that must
// not wait for nats.go's connection lock.

package bus

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// lifeCheckEvery is how often connLife reads the connection's state itself, on a goroutine of its
// own: to stop following a connection nats.go closed without reporting it, and to replace a
// listener nats.go dropped (connLife.check). It is read as each connection is first followed, and
// is a variable so a row can keep the watch quiet and drive the check itself.
var lifeCheckEvery atomic.Int64

func init() { lifeCheckEvery.Store(int64(10 * time.Second)) }

// connLife follows one nats.go connection through its lives. A life ends when nats.go reports the
// connection reconnecting, disconnected or closed, which it does from inside its own status change
// (changeConnStatus sends to a status listener under its connection lock, before it starts any
// reconnect attempt), and not from its callback goroutine, where the client hears of the same
// events (onDisconnect, onReconnect) behind any callback queued before them. A goroutine of the
// listener's own (drain) takes each report and ends the current life at once, so a check of whether
// a life has ended (window.current) takes no lock nats.go holds and waits for nothing.
//
// The report reaches that goroutine the moment nats.go makes it; the life ends when the goroutine
// runs. A check made between the two reads the life as current. A reconnect cannot complete in that
// moment unless the goroutine is kept from running for a whole reconnect attempt: the dial, the
// handshake, and with one server the reconnect wait (1 s) before them.
//
// nats.go drops a listener that still has a report unread when it makes the next one
// (sendStatusEvent reads a buffered value as the channel's close). The drain goroutine reads each
// report as it comes and does nothing else, so that takes a report nats.go made while the goroutine
// could not run. A second goroutine (watch) reads the connection itself every lifeCheckEvery
// (check): it stops following a closed connection, and replaces a listener nats.go dropped. It tells
// a dropped report from one still on its way by counting: nats.go reports RECONNECTING at a drop and
// counts the reconnect (Stats().Reconnects) only once a later attempt's dial succeeds, so every
// reconnect it counts was reported first. A count past the RECONNECTING reports the drain has taken
// is a reconnect whose report was dropped, unless the report is still in the channel; so only a
// shortfall that two checks in a row read is taken for a dropped listener.
type connLife struct {
	conn *nats.Conn

	mu sync.Mutex
	// life is closed when the current life ends; end makes the next one at once.
	life chan struct{}
	// heard is how many RECONNECTING reports the drain has taken.
	heard uint64
	// baseCount and baseHeard are nats.go's reconnect count and heard as the last check found them
	// agreeing, and owedBefore whether the check before this one found a shortfall.
	baseCount, baseHeard uint64
	owedBefore           bool
	// reports is the listener the drain reads, and quit ends that drain.
	reports chan nats.Status
	quit    chan struct{}
	closed  bool
}

// connLives holds the life of every connection some window has pinned, until nats.go closes it.
var connLives struct {
	mu sync.Mutex
	of map[*nats.Conn]*connLife
}

// lifeOf returns conn's life, following it from now on when nothing follows it yet. It brackets the
// listener registration with connection snapshots: Status() and Stats().Reconnects each take
// nats.go's connection lock, so a caller that must not wait for that lock makes it inside a bound
// (Bounded), as window.pinFirst does. The reports read after registration identify a reconnect that
// began after it; a RECONNECTING snapshot, or a count change, not matched by one of those reports
// identifies the one drop that began before the listener was registered. It counts that missed report
// once, so a listener that is later dropped is not hidden by a surplus report. connLives' lock is
// not held across these reads.
func lifeOf(conn *nats.Conn) *connLife {
	connLives.mu.Lock()
	life := connLives.of[conn]
	connLives.mu.Unlock()
	if life != nil {
		return life
	}
	before := connSnapshotOf(conn)
	reports := conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
	if entered := lifeOfAfterListener.Load(); entered != nil {
		(*entered)()
	}
	after := connSnapshotOf(conn)
	heard := reconnectReports(reports)
	missed := before.status == nats.RECONNECTING ||
		before.reconnectChanged ||
		(after.status == nats.RECONNECTING && heard == 0) ||
		(after.reconnects-before.reconnects > heard)
	if missed {
		heard++
	}
	connLives.mu.Lock()
	if existing := connLives.of[conn]; existing != nil {
		connLives.mu.Unlock()
		go conn.RemoveStatusListener(reports)
		return existing
	}
	if connLives.of == nil {
		connLives.of = map[*nats.Conn]*connLife{}
	}
	life = &connLife{conn: conn, life: make(chan struct{}), heard: heard, baseCount: before.reconnectsBefore, reports: reports, quit: make(chan struct{})}
	connLives.of[conn] = life
	connLives.mu.Unlock()
	go life.drain(reports, life.quit)
	go life.watch(time.Duration(lifeCheckEvery.Load()))
	return life
}

// connSnapshot is conn's status and reconnect count around the registration of a connLife listener
// (lifeOf). Stats before and after Status make a reconnect that crossed this snapshot visible: if it
// did, that status change happened before lifeOf registered its listener. reconnectsBefore is the
// baseline before that drop; counts and reports after it stay paired as they arrive. The listener
// brackets the snapshots, and its reports reconcile changes after registration (reconnectReports).
type connSnapshot struct {
	status           nats.Status
	reconnectsBefore uint64
	reconnects       uint64
	reconnectChanged bool
}

func connSnapshotOf(conn *nats.Conn) connSnapshot {
	before := conn.Stats().Reconnects
	status := conn.Status()
	after := conn.Stats().Reconnects
	return connSnapshot{
		status:           status,
		reconnectsBefore: before,
		reconnects:       after,
		reconnectChanged: before != after,
	}
}

// reconnectReports takes the reports nats.go sent after lifeOf registered its listener and before
// its first life was made. The drain takes every later report (connLife.drain).
func reconnectReports(reports <-chan nats.Status) (heard uint64) {
	for {
		select {
		case status, open := <-reports:
			if !open {
				return heard
			}
			if status == nats.RECONNECTING {
				heard++
			}
		default:
			return heard
		}
	}
}

// lifeOfAfterListener is nil outside a row. The row makes a drop between StatusChanged and the
// snapshots, an interleaving nats.go otherwise does not expose (conn_lives_check_test.go).
var lifeOfAfterListener atomic.Pointer[func()]

// current is the channel closed when the connection's current life ends.
func (l *connLife) current() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.life
}

// drain takes each report on reports until quit closes, the connection closes or nats.go closes
// the channel, ending the current life at each.
func (l *connLife) drain(reports chan nats.Status, quit chan struct{}) {
	for {
		select {
		case <-quit:
			return
		case status, open := <-reports:
			if !open {
				return
			}
			if l.end(status) {
				return
			}
		}
	}
}

// watch runs check every interval until the connection is closed.
func (l *connLife) watch(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		if l.check() {
			return
		}
	}
}

// check reads the connection itself (connLife) and reports whether it is closed, which ends the
// following.
func (l *connLife) check() (closed bool) {
	if l.conn.IsClosed() {
		l.end(nats.CLOSED)
		return true
	}
	count := l.conn.Stats().Reconnects
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return true
	}
	counted, heard := count-l.baseCount, l.heard-l.baseHeard
	if counted <= heard {
		// Every counted reconnect was reported; a report heard ahead of its count is a reconnect
		// still dialing, so the baseline moves only when the two agree.
		if counted == heard {
			l.baseCount, l.baseHeard = count, l.heard
		}
		l.owedBefore = false
		l.mu.Unlock()
		return false
	}
	if !l.owedBefore {
		l.owedBefore = true
		l.mu.Unlock()
		return false
	}
	// Two checks in a row found a reconnect counted with no report taken: nats.go dropped the
	// listener. A new one is registered, its drain replaces the old, and the life ends, since the
	// connection reconnected in it.
	l.owedBefore = false
	old, quit := l.reports, l.quit
	l.mu.Unlock()
	l.conn.RemoveStatusListener(old)
	reports := l.conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
	l.mu.Lock()
	close(quit)
	l.reports, l.quit = reports, make(chan struct{})
	l.baseCount, l.baseHeard = count, l.heard
	l.endLocked()
	go l.drain(reports, l.quit)
	l.mu.Unlock()
	return false
}

// end ends the current life at status, nats.go's report, and starts the next, or, once the
// connection is closed, stops following it and reports true.
func (l *connLife) end(status nats.Status) bool {
	l.mu.Lock()
	if status == nats.RECONNECTING {
		l.heard++
	}
	closed := status == nats.CLOSED
	if closed && !l.closed {
		l.closed = true
		select {
		case <-l.quit:
		default:
			close(l.quit)
		}
	}
	l.endLocked()
	l.mu.Unlock()
	if closed {
		connLives.mu.Lock()
		if connLives.of[l.conn] == l {
			delete(connLives.of, l.conn)
		}
		connLives.mu.Unlock()
	}
	return closed
}

// endLocked closes the current life and makes the next, unless the connection is closed.
func (l *connLife) endLocked() {
	select {
	case <-l.life:
	default:
		close(l.life)
	}
	if !l.closed {
		l.life = make(chan struct{})
	}
}
