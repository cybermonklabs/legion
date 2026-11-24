// Package testconn is the accept loop of a loopback listener that the code under test dials, for
// a test's stand-in of the daemon's end of a worker stream: internal/launcher's daemonEnd, which a
// launcher under test dials, and internal/shim/shimtest's shimsConnection, which the real shim
// dials. Another process of the machine can reach such a listener too, since the kernel can hand
// it a loopback port that a client of another test binary still redials, so neither may take the
// first connection it accepts as the code's: each tells the code's connection from the rest by its
// first line, and the rest are closed. The loop that reads those lines, and the close of every
// connection it does not hand on, is here once (Loop).
//
// It is an ordinary package rather than a _test.go file of one of them because a test file's
// declarations belong to its own package's tests, and no other package can import them. Only
// tests import it. It fails no test and takes no *testing.T: shimtest's Bundle runs on a goroutine
// of the daemon's cadence, where no test may be failed, so what a caller logs it logs through the
// functions it passes, and what fails is its own to answer.
package testconn

import (
	"net"
	"sync"
)

// Loop accepts every connection that reaches a listener and reads each one's first line on a
// goroutine of its own, so one that sends nothing holds up no other (Accept). It closes every
// connection its caller does not keep, and, when it stops, every connection whose first line is
// still being read (Stop), so no connection outlives it but those its caller handed on, and no
// goroutine of it outlives Stop.
type Loop struct {
	listener net.Listener
	read     func(net.Conn) (keep func() bool)
	// accepting closes once the accept loop has returned, which it does once the listener closes.
	accepting chan struct{}
	// reads is every goroutine of a first line's read.
	reads sync.WaitGroup
	mu    sync.Mutex
	// stopped is set by Stop: from then on no keep runs, and each read closes the connection it
	// read.
	stopped bool
	// pending is each connection whose first line is still being read.
	pending map[net.Conn]bool
}

// Accept starts the loop on listener, which no other code accepts from. read reads one accepted
// connection's first line, on that connection's own goroutine and outside the loop's lock, and
// answers keep. Once read returns, the loop runs keep under its lock, unless the loop has stopped,
// and closes the connection unless keep answers true, which says keep handed it on. So one keep
// runs at a time, and what only keeps and Locked read and write needs no lock of its own.
func Accept(listener net.Listener, read func(conn net.Conn) (keep func() bool)) *Loop {
	l := &Loop{listener: listener, read: read, accepting: make(chan struct{}), pending: map[net.Conn]bool{}}
	go l.accept()
	return l
}

// accept is the loop: it runs until the listener closes.
func (l *Loop) accept() {
	defer close(l.accepting)
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		l.pending[conn] = true
		l.reads.Add(1)
		l.mu.Unlock()
		go l.first(conn)
	}
}

// first is conn's read and, unless the loop has stopped, its keep; it closes conn unless keep
// handed it on.
func (l *Loop) first(conn net.Conn) {
	defer l.reads.Done()
	keep := l.read(conn)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, conn)
	if l.stopped || !keep() {
		_ = conn.Close()
	}
}

// Locked runs f under the loop's lock, one at a time with every keep, with whether the loop has
// stopped: for a caller that hands something on beside the connections its keeps hand on, and that
// must hand nothing on once the loop has stopped.
func (l *Loop) Locked(f func(stopped bool)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l.stopped)
}

// Reading is whether a connection from remote, the address of its far end, is one whose first line
// the loop is still reading.
func (l *Loop) Reading(remote string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for conn := range l.pending {
		if conn.RemoteAddr().String() == remote {
			return true
		}
	}
	return false
}

// Stop closes the listener, waits until the loop accepts no more, and closes every connection whose
// first line is still being read, which sent nothing while the loop ran. logSilent, where it is
// set, is called with each such connection once it is closed, under the loop's lock, for the caller
// to log it. Stop returns once every read has ended, and no keep runs once it has taken that lock.
func (l *Loop) Stop(logSilent func(net.Conn)) {
	_ = l.listener.Close()
	<-l.accepting
	l.mu.Lock()
	l.stopped = true
	for conn := range l.pending {
		_ = conn.Close()
		if logSilent != nil {
			logSilent(conn)
		}
	}
	l.mu.Unlock()
	l.reads.Wait()
}
