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
// (StatusChanged) and the drain does nothing but take them, so it normally has room. The watch
// handles the exceptional dropped report: the first window pinned in a life records nats.go's
// reconnect count after it has read CONNECTED. A later count means that connection has reconnected
// in that pinned life, whether the drain took the report or not, so the watch ends the life, with
// a new listener in place of the old. A new window then records the count of the new life. The
// watch holds no window lock while it reads nats.go.
type connLife struct {
	conn *nats.Conn

	mu sync.Mutex
	// life is closed when the current life ends; end makes the next one at once.
	life chan struct{}
	// reconnects is nats.go's count when the first window pinned life. pinned is false until a
	// window has read CONNECTED and recorded that count (pin).
	reconnects uint64
	pinned     bool
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

// lifeOf returns conn's life, following it from now on when nothing follows it yet. Registering the
// status listener can wait for nats.go's connection lock, so a caller that must not wait for that
// lock makes it inside a bound (Bounded), as window.pinFirst does. connLives' lock is not held
// across that registration.
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
	life = &connLife{conn: conn, life: make(chan struct{}), reports: reports, quit: make(chan struct{})}
	connLives.of[conn] = life
	connLives.mu.Unlock()
	go life.drain(reports, life.quit)
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

// check reads the connection itself and reports whether it is closed, which ends the following. A
// count past the first pin of a life means nats.go reconnected in that life without the drain
// ending it, a report dropped, so check ends the life and puts a new listener in place of the old.
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
	if !l.pinned || count <= l.reconnects {
		l.mu.Unlock()
		return false
	}
	old, quit := l.reports, l.quit
	l.mu.Unlock()
	l.conn.RemoveStatusListener(old)
	reports := l.conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		go l.conn.RemoveStatusListener(reports)
		return true
	}
	close(quit)
	l.reports, l.quit = reports, make(chan struct{})
	l.endLocked()
	go l.drain(reports, l.quit)
	l.mu.Unlock()
	return false
}

// end ends the current life at status, nats.go's report, and starts the next, or, once the
// connection is closed, stops following it and reports true.
func (l *connLife) end(status nats.Status) bool {
	l.mu.Lock()
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
		l.pinned = false
	}
}
