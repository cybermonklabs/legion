// A read of a NATS connection's status that waits for no reconnect's handshake
// (boundedStatusReader), for a caller that holds a lock of its own.

package bus

import (
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// statusReadLockWait is how long a read of a connection's status (boundedStatusReader) waits for
// the locks it takes. nats.go v1.53.1 holds its connection lock for microseconds to write or read a
// frame, and for a whole reconnect attempt, the dial and the handshake (doReconnect,
// nats.go:3458-3485), up to the attempt's connect timeout (connectTimeout), or the hub's
// authorization timeout while its auth service holds the CONNECT. A read kept waiting longer finds
// the connection in a reconnect, and reads it down. On a box loaded enough to keep the read's
// goroutine waiting this long, the caller answers a loss it would not have had to, which costs it
// what a loss costs it and holds it no longer.
const statusReadLockWait = 100 * time.Millisecond

// boundedStatusReader reads a connection's status and nats.go's reconnect count of it on a
// goroutine of its own, and answers within statusReadLockWait. Each of nats.go's reads of a
// connection's state (IsConnected, Status, Stats) takes nats.go's connection lock, so a caller
// holding a lock of its own that read the connection directly while a reconnect's handshake was in
// progress held its lock for the whole handshake: an outbox window's pin (window.pin), and an
// intake's stop through Client.ConnectedNow. Such a caller asks the reader instead, and waits at
// most statusReadLockWait.
//
// One read runs at a time, so a long handshake leaves one goroutine waiting on nats.go's lock
// however many callers ask. Reads are numbered, and a caller takes the answer of a read begun after
// it asked, never one that may have read the state before its question: a caller that comes while
// an earlier read is in flight waits for that one to end and then joins the next, within its one
// wait.
type boundedStatusReader struct {
	// read reads the status and the reconnect count, under whatever locks it takes.
	read func() (nats.Status, uint64)

	mu sync.Mutex
	// begun counts the reads begun.
	begun uint64
	// reading is the read in flight, nil when none is.
	reading *statusRead
}

// statusRead is one read's answer, set before done is closed.
type statusRead struct {
	done       chan struct{}
	seq        uint64
	status     nats.Status
	reconnects uint64
}

// newBoundedStatusReader is the reader of conn's own status and reconnect count.
func newBoundedStatusReader(conn *nats.Conn) *boundedStatusReader {
	return &boundedStatusReader{read: func() (nats.Status, uint64) { return conn.Status(), conn.Stats().Reconnects }}
}

// connectedNow reports whether the connection is connected as nats.go's IsConnected reads it -
// CONNECTED, or a drain in progress (DRAINING_SUBS, DRAINING_PUBS), which still serves - and
// nats.go's reconnect count with a true answer. A read kept waiting past statusReadLockWait answers
// false: the connection is in a reconnect's dial or handshake. A read left waiting ends when the
// lock it waits for is let go, at the latest at the end of the attempt.
func (r *boundedStatusReader) connectedNow() (up bool, reconnects uint64) {
	wait := time.NewTimer(statusReadLockWait)
	defer wait.Stop()
	r.mu.Lock()
	asked := r.begun
	for {
		read := r.reading
		if read == nil {
			r.begun++
			read = &statusRead{done: make(chan struct{}), seq: r.begun}
			r.reading = read
			go func() {
				status, reconnects := r.read()
				r.mu.Lock()
				read.status, read.reconnects = status, reconnects
				r.reading = nil
				r.mu.Unlock()
				close(read.done)
			}()
		}
		r.mu.Unlock()
		select {
		case <-read.done:
			if read.seq > asked {
				if read.status != nats.CONNECTED && read.status != nats.DRAINING_SUBS && read.status != nats.DRAINING_PUBS {
					return false, 0
				}
				return true, read.reconnects
			}
		case <-wait.C:
			return false, 0
		}
		r.mu.Lock()
	}
}

// ConnectedNow reports whether the client's connection is connected now, waiting for no reconnect:
// the client's lock, which a caller reading nats.go's state under it can hold through a handshake
// (Connected), and nats.go's connection lock, which a reconnect's dial and handshake hold, are
// waited for at most statusReadLockWait, and a read kept waiting longer answers false. It is for a
// caller that holds a lock of its own and asks before a call that would wait for the connection,
// so that it answers the loss rather than holding its lock through the handshake (the intake's
// stop, internal/dispatch/delivery). Each call reads afresh, on a goroutine of its own.
func (c *Client) ConnectedNow() bool {
	reader := boundedStatusReader{read: func() (nats.Status, uint64) {
		conn := c.Connection()
		if conn == nil {
			return nats.DISCONNECTED, 0
		}
		return conn.Status(), conn.Stats().Reconnects
	}}
	up, _ := reader.connectedNow()
	return up
}
