// A read of a connection's status that waits for no reconnect's handshake (connProber).

package bus

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// ConnLockWait is how long a caller that holds a lock of its own, or that something holding one
// waits for, waits for nats.go's connection lock before it takes that lock as held by a reconnect:
// a read of nats.go's state through the prober (connProber.probe), and an intake's call to NATS
// during its stop (internal/dispatch/delivery). nats.go v1.55.0 holds that lock for microseconds to
// write or read a frame, and for a whole reconnect attempt, the dial and the handshake
// (doReconnect, nats.go:3554-3581), up to the attempt's connect timeout (connectTimeout), or the
// hub's authorization timeout while its auth service holds the CONNECT. A read kept waiting longer
// answers "unknown": the connection may be mid-reconnect, or the box too loaded to run the read's
// goroutine in time. A caller that holds a lock of its own treats "unknown" as the loss, lets its
// lock go and retries once the connection is back, which costs it a retry and holds it no longer;
// a caller that holds none waits for nats.go's own answer, within its own context (probeUntil). An
// intake's stop goes on without a call NATS has not answered by then, which costs it what a loss
// costs on a box loaded enough to keep a healthy call waiting this long.
const ConnLockWait = 100 * time.Millisecond

// connProber reads one connection's status and reconnect count from nats.go, on a goroutine of its
// own. Every read of nats.go's state takes nats.go's connection lock (IsConnected, Status, Stats),
// so a caller that holds a lock of its own - a session's consumer lock, the shared stream read, a
// relay's restart turn, a role's keep guard - and asked nats.go directly while a reconnect's
// handshake was in progress held what it holds for the whole handshake. Such a caller asks the
// prober instead and waits at most ConnLockWait (probe): the session consumers' client's requests
// (connForRequest, requestLife, closedUnder), the main client's caller before a call it makes under
// such a lock (LostNow), and any request whose caller marks its context (WithLockHeld). A request
// whose caller holds nothing asks the prober too, and waits on past the budget for the read's own
// answer, until its context ends (probeUntil).
//
// One read runs at a time, so a long handshake leaves one goroutine waiting on nats.go's lock,
// however many callers ask. Reads are numbered, and a caller takes the answer of a read begun after
// it asked, never one that may have read nats.go's state before its question: a caller that comes
// while an earlier read is in flight waits for that one to end and then joins the next, within its
// one bound.
type connProber struct {
	conn *nats.Conn
	// probes counts the reads begun; a row reads it.
	probes atomic.Uint64

	mu sync.Mutex
	// probing is the read in flight, nil when none is.
	probing *connProbe
}

// connProbe is one read's answer, set before done is closed.
type connProbe struct {
	done       chan struct{}
	status     nats.Status
	reconnects uint64
	// seq is the read's number among the prober's reads (probes).
	seq uint64
}

// newConnProber is the prober of conn's status.
func newConnProber(conn *nats.Conn) *connProber {
	return &connProber{conn: conn}
}

// probe is nats.go's own status of the connection and its reconnect count, read on a goroutine of
// its own by a read begun after the call and waited for up to ConnLockWait: answered is false,
// "unknown", when nats.go's connection lock was held longer, which a dial or a handshake does, and
// a box too loaded to run the read in time. A read left waiting ends when nats.go lets its lock go,
// at the end of the attempt.
func (p *connProber) probe() (status nats.Status, reconnects uint64, answered bool) {
	budget := time.NewTimer(ConnLockWait)
	defer budget.Stop()
	return p.read(budget.C, nil)
}

// probeUntil is probe bounded by ctx rather than ConnLockWait: the read of a caller that holds
// no lock, which waits for nats.go's own answer for as long as it waits for any answer. answered is
// false only once ctx has ended.
func (p *connProber) probeUntil(ctx context.Context) (status nats.Status, reconnects uint64, answered bool) {
	return p.read(nil, ctx.Done())
}

// read is probe's and probeUntil's read, which ends at budget or at done, whichever is not nil.
func (p *connProber) read(budget <-chan time.Time, done <-chan struct{}) (status nats.Status, reconnects uint64, answered bool) {
	p.mu.Lock()
	asked := p.probes.Load()
	for {
		run := p.probing
		if run == nil {
			run = &connProbe{done: make(chan struct{}), seq: p.probes.Add(1)}
			p.probing = run
			go func() {
				status, reconnects := p.conn.Status(), p.conn.Stats().Reconnects
				p.mu.Lock()
				run.status, run.reconnects = status, reconnects
				p.probing = nil
				p.mu.Unlock()
				close(run.done)
			}()
		}
		p.mu.Unlock()
		select {
		case <-run.done:
			if run.seq > asked {
				return run.status, run.reconnects, true
			}
		case <-budget:
			return 0, 0, false
		case <-done:
			return 0, 0, false
		}
		p.mu.Lock()
	}
}

// statusUp reports whether status is connected as nats.go's IsConnected reads it.
func statusUp(status nats.Status) bool {
	return status == nats.CONNECTED || status == nats.DRAINING_SUBS || status == nats.DRAINING_PUBS
}
