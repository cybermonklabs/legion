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
// own: to stop following a connection nats.go closed without reporting it, and to end a life whose
// report nats.go dropped (connLife.check). It is read as each connection is first followed, and
// is a variable so a row can keep the watch quiet and drive the check itself.
var lifeCheckEvery atomic.Int64

func init() { lifeCheckEvery.Store(int64(10 * time.Second)) }

// connLife follows one NATS connection through its lives. A life ends when nats.go reports the
// connection reconnecting, disconnected or closed, which it does from inside its own status change
// (changeConnStatus sends to a status listener under its connection lock, before it starts any
// reconnect attempt), and not from its callback goroutine, where the client hears of the same
// events behind any callback queued before them. A goroutine of the listener's own (drain) takes
// each report and ends the current life at once, so a check of whether a life has ended
// (window.current) takes no lock nats.go holds and waits for nothing.
//
// nats.go sends a report to a listener only when its channel has room, and drops the report
// otherwise; it removes a listener only once that listener's channel is closed (nats.go@v1.55.0
// sendStatusEvent and notifyStatusListener, nats.go:6877-6900). The channel holds ten reports
// (StatusChanged) and the drain does nothing but take them, so it normally has room. The listener
// stays registered while the connection is open, so a report nats.go dropped is followed by the
// reports the full channel held, each of which the drain takes after the drop. The watch is the
// check that does not count on them: the first window pinned in a life records nats.go's reconnect
// count after it has read CONNECTED. A later count means that connection has reconnected in that
// pinned life, whether the drain took the report or not, so the life ends. A new window then records
// the count of the new life. The watch holds no window lock while it reads nats.go, and hands the
// count it read to the drain, which ends a life at it only between two reports (check).
type connLife struct {
	conn *nats.Conn
	// reports is the listener the drain takes nats.go's reports from, and checks carries the
	// watch's reads of the reconnect count to the drain. quit closes when the connection closes, and
	// ends the drain.
	reports chan nats.Status
	checks  chan reconnectRead
	quit    chan struct{}

	mu sync.Mutex
	// life is closed when the current life ends; end makes the next one at once.
	life chan struct{}
	// reconnects is nats.go's count when the first window pinned life. pinned is false until a
	// window has read CONNECTED and recorded that count (pin).
	reconnects uint64
	pinned     bool
	closed     bool

	// checkRead, when set, runs once check has read nats.go's reconnect count and before the drain
	// acts on it. It is nil outside the row that ends the life and pins the next one in that gap.
	checkRead atomic.Pointer[func()]
	// reportTaken, when set, runs when the drain has taken a report of nats.go's and before it ends
	// the life at that report. It is nil outside the row that holds the end of a lost connection's
	// life (BeforeReportEndsLife, export_test.go). Its cost in production is one atomic load for
	// each status report nats.go sends, which comes only on a change of the connection's status.
	reportTaken atomic.Pointer[func()]
}

// reconnectRead is the reconnect count check read, and the channel the drain closes once it has
// acted on that count.
type reconnectRead struct {
	reconnects uint64
	acted      chan struct{}
}

// connLives holds the life of every connection a window or a request follows (lifeOf), until
// nats.go closes it.
var connLives struct {
	mu sync.Mutex
	of map[*nats.Conn]*connLife
}

// lifeOf returns conn's life, following it from now on when nothing follows it yet. Registering the
// status listener can wait for nats.go's connection lock, so a caller that must not wait for that
// lock makes it inside a bound (Bounded), as window.pinFirst does. A request the client builds
// itself registers it on its caller's goroutine, right before its inbox's subscribe, which waits
// for that lock too (requestConfirmedOn, RequestCore, StartRequestStoredTo). connLives' lock is not
// held across that registration.
func lifeOf(conn *nats.Conn) *connLife {
	connLives.mu.Lock()
	life := connLives.of[conn]
	connLives.mu.Unlock()
	if life != nil {
		return life
	}
	reports := conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
	connLives.mu.Lock()
	if existing := connLives.of[conn]; existing != nil {
		connLives.mu.Unlock()
		go conn.RemoveStatusListener(reports)
		return existing
	}
	if connLives.of == nil {
		connLives.of = map[*nats.Conn]*connLife{}
	}
	life = &connLife{conn: conn, reports: reports, checks: make(chan reconnectRead), quit: make(chan struct{}), life: make(chan struct{})}
	connLives.of[conn] = life
	connLives.mu.Unlock()
	go life.drain()
	go life.watch(time.Duration(lifeCheckEvery.Load()))
	return life
}

// pinIf records reconnects for ch's life only while ch is still current and unpinned. pinFirst
// takes ch before it reads the connection as CONNECTED, so a drop between those reads closes ch and
// refuses the pin rather than letting it cross the reconnect.
func (l *connLife) pinIf(ch <-chan struct{}, reconnects uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.life != ch {
		return false
	}
	select {
	case <-ch:
		return false
	default:
	}
	if !l.pinned {
		l.reconnects, l.pinned = reconnects, true
	}
	return true
}

// current is the channel closed when the connection's current life ends.
func (l *connLife) current() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.life
}

// lifeCurrent reports whether life, a channel current returned, is still open: nats.go has
// reported its connection neither reconnecting, disconnected nor closed since. It takes no lock
// nats.go holds.
func lifeCurrent(life <-chan struct{}) bool {
	select {
	case <-life:
		return false
	default:
		return true
	}
}

// drain takes each report on reports and ends the current life at it, and acts on each count the
// watch read (endReconnected), one at a time, until the connection closes. A listener removed while
// the connection is open ends the reports and not the drain, which still acts on the watch's reads.
func (l *connLife) drain() {
	reports := l.reports
	for {
		select {
		case <-l.quit:
			return
		case status, open := <-reports:
			if !open {
				reports = nil
				continue
			}
			if taken := l.reportTaken.Load(); taken != nil {
				(*taken)()
			}
			if l.end(status) {
				return
			}
		case read := <-l.checks:
			l.endReconnected(read.reconnects)
			close(read.acted)
		}
	}
}

// endReconnected ends the current life when the first window pinned in it read a reconnect count
// below reconnects, the count the watch read: nats.go reconnected in that life. The drain calls it
// between two reports. A report still waiting in the listener ends the current life when the drain
// takes it, so the life is left to that report: ending it here as well would leave the report to
// end the next life, which a window may have pinned on the reconnected connection by then.
func (l *connLife) endReconnected(reconnects uint64) {
	if len(l.reports) > 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pinned && reconnects > l.reconnects {
		l.endLocked()
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

// check reads the connection itself and reports whether it is closed, which ends the following. It
// reads nats.go's reconnect count on the watch's goroutine, since the read waits for nats.go's
// connection lock, and the drain acts on that count between two reports (endReconnected). So the
// life the count is compared with is the one current when the drain acts, not one a report or a
// window's pin has replaced since the read, and no report the drain has already taken is left to
// end a life the count ended.
func (l *connLife) check() (closed bool) {
	if l.conn.IsClosed() {
		l.end(nats.CLOSED)
		return true
	}
	read := reconnectRead{reconnects: l.conn.Stats().Reconnects, acted: make(chan struct{})}
	if hook := l.checkRead.Load(); hook != nil {
		(*hook)()
	}
	select {
	case l.checks <- read:
		<-read.acted
		return false
	case <-l.quit:
		return true
	}
}

// end ends the current life at status, nats.go's report, and starts the next, or, once the
// connection is closed, stops following it and reports true.
func (l *connLife) end(status nats.Status) bool {
	l.mu.Lock()
	closed := status == nats.CLOSED
	if closed && !l.closed {
		l.closed = true
		close(l.quit)
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
		l.pinned = false
	}
}
