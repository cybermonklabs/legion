package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/timewords"
)

// A lock-out is a claim whose current process the stream refuses because an older generation's
// connection is still bound: the daemon launched that generation only after its runtime reported
// the older process gone, so the connection belongs to a process the daemon no longer runs (R3).
// On 2026-11-12 one of them refused a tree's architect and implementer for 12 minutes 50 seconds,
// until the daemon itself stopped.
//
// The older connection is told to stop at the newer process's first hello, and a shim that still
// reads its stream ends its process within its 5-second grace. The connection closes with it, the
// claim is free, and the newer process binds on its next hello, which it sends at most 5 seconds
// later.
//
// Whether a process is still on the far end of the older connection is read from the connection
// itself. From the stop frame on, the daemon asks it for its state (get_state, which a shim
// forwards to Oh My Pi and Oh My Pi answers even mid-turn), and asks again each time a request's
// RPC timeout ends before an answer. Any line the connection sends after its stop frame, that
// answer or any other, says a process is there. A connection that sends nothing at all for
// LockOutSilenceAfter has nobody reading or writing it: its far end is a kernel that still
// acknowledges every byte, which keepalive and the user timeout never end (keepalive.go), as when
// a pod's sandbox outlives its force delete. One such connection refused its claim's current
// process for over 1 hour 45 minutes. The daemon ends that connection itself, and its close ends
// the lock-out as the older process's own close does. A shim that was there after all redials;
// its hello names a generation its claim has left, and it is told to stop again (tellToStop).
//
// What is left after LockOutReportAfter is a process that answers and will not stop, which only a
// person can end on its node: closing its connection here would leave two processes writing one
// Oh My Pi session and one branch.
const (
	// LockOutReportAfter is how long a newer process is refused before the lock-out is reported
	// to the project's controller and a person is told by R6's line.
	LockOutReportAfter = 5 * time.Minute
	// LockOutSilenceAfter is how long an older generation's connection may send nothing at all
	// after its stop frame before the daemon ends it. With the at most 5 seconds the newer
	// process waits for its next hello, a connection whose far end vanished frees the claim
	// within R3's 3 minutes, and well before LockOutReportAfter tells a person of a process that
	// will not stop.
	LockOutSilenceAfter = 2 * time.Minute
)

// LockOut is one such claim, as the daemon reports it: the generation refused, the generation
// still bound, and the first hello the newer process was refused at.
type LockOut struct {
	Claim claim.Token
	Newer uint64
	Older uint64
	Since time.Time
}

// Reason is the sentence the controller's record.WorkerLockedOutNotice carries.
func (o LockOut) Reason() string {
	return fmt.Sprintf("generation %d has been refused the worker stream for %s; generation %d is still connected",
		o.Newer, timewords.Words(LockOutReportAfter), o.Older)
}

// Clock is the time the stream waits on: a lock-out's report and its older connection's silence,
// and nothing else.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f after d on a goroutine of its own, and answers with the timer that ends
	// that wait.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a wait a Clock registered, which Stop ends before it fires.
type Timer interface{ Stop() bool }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// lockOut is one claim's lock-out as the listener keeps it.
type lockOut struct {
	// newer is the highest generation refused, and older the generation bound when the first of
	// those hellos arrived.
	newer, older uint64
	// since is that first hello: what LockOutReportAfter is counted from, so a claim whose shim
	// redials every 5 seconds is reported once at five minutes rather than restarting the count.
	since time.Time
	// told is the bound connection having been sent its stop frame, so the 314 hellos of one
	// 2026-11-12 lock-out would send one.
	told bool
	// reported is the controller and the alert having been told, which happens once per lock-out.
	reported bool
	timer    Timer
	// silence is the wait that ends the older connection if it sends nothing for
	// LockOutSilenceAfter after its stop frame (endSilentOlder). It is registered once that frame
	// is written.
	silence Timer
}

// LockedOut is the claim's lock-out, if the stream is refusing its current process because an
// older generation's connection is still bound.
//
// It is what the supervisor reads to hold the claim's registration deadline while the lock-out
// lasts: a lock-out is not the newer process's fault, and charging it would spend the claim's
// launch failures, one per registration deadline, for a process the daemon never ran.
func (l *Listener) LockedOut(token claim.Token) (LockOut, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.lockOuts[token]
	if !ok {
		return LockOut{}, false
	}
	return LockOut{Claim: token, Newer: o.newer, Older: o.older, Since: o.since}, true
}

// refuseBound is the refusal of a hello for a claim a live connection already holds, and the
// decisions that go with it. It runs under l.mu, and the stop frame it decides on is written
// outside it (stopOlder), since that write can wait on a peer that has stopped reading.
func (l *Listener) refuseBound(bound *Conn, token claim.Token, generation uint64) refusal {
	attrs := []any{"claim", token, "generation", generation, "boundGeneration", bound.generation}
	if generation <= bound.generation {
		// A second process of the claim's current generation, which is refused as it always was:
		// nothing says the bound one is gone, and both would write one session.
		return refusal{reason: "already bound to a live stream", attrs: attrs}
	}
	o, open := l.lockOuts[token]
	if !open {
		o = &lockOut{older: bound.generation, since: l.clock.Now()}
		o.timer = l.clock.AfterFunc(LockOutReportAfter, func() { l.reportLockOut(token) })
		l.lockOuts[token] = o
	}
	o.newer = max(o.newer, generation)
	if !o.told {
		o.told = true
		l.stopOlder(token, o, bound)
	}
	return refusal{reason: "already bound to a live stream of an older generation, which has been told to stop", attrs: attrs}
}

// stopOlder sends the bound connection of an older generation its stop frame, then opens the
// window in which that connection must show a process is still behind it (watchSilence) and asks
// that process for its state (askState). It runs on a goroutine of its own, registered with the
// listener's wait group as an accepted connection is, because the write is bounded by the RPC
// timeout and a shim that has stopped reading would otherwise hold the lock every hello is judged
// under. It is called under l.mu, after the l.closed check that keeps an Add from racing the
// listener's own end.
//
// A stop frame that could not be written opens no window: its write closed the connection
// (connWriter), or the connection had already closed, and either close ends the lock-out.
func (l *Listener) stopOlder(token claim.Token, o *lockOut, bound *Conn) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		// Read before the frame is written, so every line counted from here on was sent after
		// the stop frame, or at most a moment before it.
		mark := bound.heard.Load()
		ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
		err := bound.Shutdown(ctx)
		cancel()
		if err != nil {
			l.log.Warn("worker-stream: could not tell an older generation's connection to stop",
				"claim", bound.claim, "boundGeneration", bound.generation, "error", err)
			return
		}
		if l.watchSilence(token, o, bound, mark) {
			l.askState(bound, mark)
		}
	}()
}

// watchSilence opens the older connection's window, LockOutSilenceAfter on the listener's Clock
// from its written stop frame. It answers false when there is nothing to watch: the lock-out has
// already ended with that connection's close, or the listener is ending.
func (l *Listener) watchSilence(token claim.Token, o *lockOut, bound *Conn, mark uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.lockOuts[token] != o {
		return false
	}
	o.silence = l.clock.AfterFunc(LockOutSilenceAfter, func() { l.endSilentOlder(token, o, bound, mark) })
	return true
}

// askState asks the older connection's process for its state until something comes back on the
// connection. A request whose own RPC timeout ends first is sent again, so the window never runs
// without a question outstanding; any other end of it stops the asking, because an answer or a
// refusal is a line heard and a closed connection has no one to ask. Every request is bounded by
// the RPC timeout (Conn.request), so the asking ends with the window, whose end either finds a
// line heard or closes the connection.
func (l *Listener) askState(bound *Conn, mark uint64) {
	for {
		_, err := bound.GetState(context.Background())
		if !errors.Is(err, ErrNoAnswer) || bound.heard.Load() != mark {
			return
		}
	}
}

// endSilentOlder is the end of an older connection's window. A connection that has sent no line
// since its stop frame has nobody behind it reading or writing, and is ended here, with one line
// naming the claim, both generations and the silence. Its close ends the lock-out as any close
// of the older connection does (serve, endLockOut): nothing is reported, its Closed event carries
// the older generation, which is not the claim's own close, and the newer process binds on its
// next hello. A connection that sent anything has a process behind it, and is left to that
// process's own close or to the report.
func (l *Listener) endSilentOlder(token claim.Token, o *lockOut, bound *Conn, mark uint64) {
	if bound.heard.Load() != mark {
		return
	}
	l.mu.Lock()
	silent := !l.closed && l.lockOuts[token] == o && l.conns[token] == bound
	if silent {
		_ = bound.nc.Close()
	}
	newer := o.newer
	l.mu.Unlock()
	if silent {
		l.log.Warn("worker-stream: ended an older generation's connection that sent nothing after its stop frame",
			"claim", token, "generation", newer, "boundGeneration", bound.generation, "silent", LockOutSilenceAfter)
	}
}

// reportLockOut is the timer of a lock-out that has lasted LockOutReportAfter: the project's
// controller is told once, and R6's line is written once, because only a person can end a
// process that cannot be stopped.
func (l *Listener) reportLockOut(token claim.Token) {
	l.mu.Lock()
	o, open := l.lockOuts[token]
	if l.closed || !open || o.reported {
		l.mu.Unlock()
		return
	}
	o.reported = true
	out := LockOut{Claim: token, Newer: o.newer, Older: o.older, Since: o.since}
	l.mu.Unlock()

	if l.alerts != nil {
		l.alerts.Open(fleetalert.NewerProcessRefused, string(token), "generation", out.Newer, "boundGeneration", out.Older)
	}
	if l.reportLocked != nil {
		l.reportLocked(out)
	}
}

// endLockOut ends the claim's lock-out, if it has one: the older connection has closed, so the
// stream refuses its newer process no longer. It runs under l.mu and answers whether a person
// was told, since closing that line takes no lock of this listener's.
func (l *Listener) endLockOut(token claim.Token) (reported bool) {
	o, open := l.lockOuts[token]
	if !open {
		return false
	}
	delete(l.lockOuts, token)
	if o.timer != nil {
		o.timer.Stop()
	}
	if o.silence != nil {
		o.silence.Stop()
	}
	return o.reported
}
