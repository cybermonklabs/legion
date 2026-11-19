// The lives of a NATS connection, as nats.go itself reports them (connLife), for a check that must
// not wait for nats.go's connection lock.

package bus

import (
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// lifeCheckEvery is how often connLife reads the connection's state itself, on its own goroutine:
// to stop following a connection nats.go closed without reporting it, and to replace a listener
// nats.go dropped (connLife.follow).
const lifeCheckEvery = 10 * time.Second

// connLife follows one nats.go connection through its lives. A life ends when nats.go reports the
// connection reconnecting, disconnected or closed, which it does from inside its own status change
// (changeConnStatus sends to a status listener under its connection lock, before it starts any
// reconnect attempt), and not from its callback goroutine, where the client hears of the same
// events (onDisconnect, onReconnect) behind any callback queued before them. A goroutine of the
// life's own (follow) takes each report and ends the current life at once, so a check of whether a
// life has ended (window.current) takes no lock nats.go holds and waits for nothing.
//
// The report reaches the goroutine the moment nats.go makes it; the life ends when the goroutine
// runs. A check made between the two reads the life as current. A reconnect cannot complete in that
// moment unless the goroutine is kept from running for a whole reconnect attempt: the dial, the
// handshake, and with one server the reconnect wait (1 s) before them.
//
// nats.go drops a listener that still has a report unread when it makes the next one
// (sendStatusEvent reads a buffered value as the channel's close). Two reports come at least a dial
// and a failed handshake apart, but for a close right after a report, so the goroutine reads each
// one as it comes and does nothing else between reads. Should nats.go drop the listener all the
// same, the goroutine's own read of the connection every lifeCheckEvery finds a reconnect it had no
// report of, ends the life and listens again; and it finds a closed connection, and stops.
type connLife struct {
	mu sync.Mutex
	// life is closed when the current life ends; follow makes the next one at once.
	life chan struct{}
}

// connLives holds the life of every connection some window has pinned, until nats.go closes it.
var connLives struct {
	mu sync.Mutex
	of map[*nats.Conn]*connLife
}

// lifeOf returns conn's life, following it from now on when nothing follows it yet. Its first call
// for conn registers a status listener (nats.Conn.StatusChanged), which waits for nats.go's
// connection lock, so a caller that must not wait for that lock makes it inside a bound (Bounded),
// as window.pinFirst does. connLives' lock is not held across that wait.
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
	life = &connLife{life: make(chan struct{})}
	connLives.of[conn] = life
	connLives.mu.Unlock()
	go life.follow(conn, reports)
	return life
}

// current is the channel closed when the connection's current life ends.
func (l *connLife) current() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.life
}

// follow ends the current life at each report, and stops following once nats.go has closed the
// connection. Every lifeCheckEvery it reads the connection itself (connLife).
func (l *connLife) follow(conn *nats.Conn, reports chan nats.Status) {
	check := time.NewTicker(lifeCheckEvery)
	defer check.Stop()
	reconnects := conn.Stats().Reconnects
	heard := false
	for {
		select {
		case status, open := <-reports:
			if !open || l.end(conn, status == nats.CLOSED) {
				return
			}
			heard = true
		case <-check.C:
			if conn.IsClosed() {
				l.end(conn, true)
				return
			}
			now := conn.Stats().Reconnects
			if now != reconnects && !heard {
				conn.RemoveStatusListener(reports)
				reports = conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
				l.end(conn, false)
			}
			reconnects, heard = now, false
		}
	}
}

// end ends the current life and starts the next, or, once the connection is closed, stops
// following it and reports true.
func (l *connLife) end(conn *nats.Conn, closed bool) bool {
	l.mu.Lock()
	select {
	case <-l.life:
	default:
		close(l.life)
	}
	if !closed {
		l.life = make(chan struct{})
	}
	l.mu.Unlock()
	if closed {
		connLives.mu.Lock()
		if connLives.of[conn] == l {
			delete(connLives.of, conn)
		}
		connLives.mu.Unlock()
	}
	return closed
}
