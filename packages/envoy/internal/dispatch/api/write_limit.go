package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/semaphore"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// This file holds the limiter every transaction the API opens waits at, and the transaction it
// wraps so that the limit is given back exactly when the connection is (server.begin).

// apiWriteLimit is how many of the API's transactions may be open at once, out of the
// store.sharedPoolSize connections every request reads and writes through. A caller over it
// waits here, before it has taken a connection or any lock, and the pool keeps the rest of its
// connections for callers that take one without a transaction: the room workers' durable
// appends (docs' PgVersioned.withRoomLock), the pooled reads a document's text and version are
// answered from (getArtifactText, latestVersionNumber), and every other request.
//
// It is 48 of the pool's 64, leaving 16. The figure is measured rather than apportioned.
// Instrumented over a burst of 320 concurrent API writes to documents of a mebibyte on the
// two-processor pod, with no limit at all: the pool stood at 63 and then 64 of its 64 acquired
// with no connection idle, its waiting acquires climbing past a thousand, and the room worker's
// own append of a person's keystroke waited 58,217 ms of its 58,557 for a connection. Behind
// that wait ygo's 256-deep hand-off channel for the room filled, and the keystroke's apply
// blocked on the hand-off for 9,908 ms - which is the whole of the worst keystroke
// (dispatch://INFRA-179, the keystroke measurement).
//
// What the 16 have to carry is small and short: one append at a time per resident room, each a
// transaction of a few statements under the document's advisory lock; the two pooled reads a
// text read makes; and the reads every other route makes. They are not what fills a pool - the
// writes are, because a write holds its connection from the transaction it opens until the
// commit or rollback that ends it, across the fork of a whole document and its render. Sixteen
// is over ten times what the drive's own reader and room worker need at once, and 48 is still
// three quarters of the pool, so a burst of writes is bounded by the pod's load concurrency as
// it was and not by this (cmd/dispatch, TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory
// measures the keystroke and an ordinary read through the burst).
//
// It is below the pool's size and not equal to it, which is the whole rule: a limit at 64 would
// leave the pool exactly as it was, and a limit over it would bound nothing.
const apiWriteLimit = 48

// writeLimiter is apiWriteLimit held as a counting semaphore, made on first use so a server a
// test builds directly needs no construction step.
type writeLimiter struct {
	once sync.Once
	free *semaphore.Weighted
}

func (l *writeLimiter) semaphore() *semaphore.Weighted {
	l.once.Do(func() { l.free = semaphore.NewWeighted(apiWriteLimit) })
	return l.free
}

// take holds one of the limit for a caller about to open a transaction and answers what gives it
// back.
//
// A free slot is no wait: the first attempt is TryAcquire, which takes a slot when one is free
// and no caller is queued ahead of this one, so it refuses nobody the pod has room for and jumps
// no queue. What the server bounds is waiting, not asking. With the bound around every acquire
// instead, a request's second transaction was refused SERVICE_BUSY 15.5 s into the request with
// all 48 slots free, because docs.BoundWriteWait runs from the request's start and the second
// acquire began past it (dispatch://INFRA-179, review 6's finding 5).
//
// A wait, when the slot is not free, runs within ctx and within the server's own bound on what a
// caller's request spends waiting at the pod's memory bounds (docs.BoundWriteWait). A wait the
// caller's request ends is answered 503, which is what every other wait a request outlives is
// answered: the caller has gone or run out of time, and nothing of its write has been opened,
// read or locked. A wait that reaches the server's bound first is answered the same 503, with the
// caller's patience left for the retry rather than spent waiting here - a write left unanswered
// until its caller gives up is resent, which is worse for the pod than a refusal it can act on
// (dispatch://INFRA-179). The bound is shared with the load slot a write's fork waits for (docs'
// liveWrite.coverFork), so a write that waits at both is answered within it, and the server's own
// writes are not bounded.
func (l *writeLimiter) take(ctx context.Context) (func(), error) {
	free := l.semaphore()
	if !free.TryAcquire(1) {
		waiting, cancel := docs.BoundWriteWait(ctx)
		defer cancel()
		if err := free.Acquire(waiting, 1); err != nil {
			return nil, errWriteLimitFull
		}
	}
	var once sync.Once
	return func() { once.Do(func() { free.Release(1) }) }, nil
}

// probe reports whether the limit has a slot free for a write about to begin, and holds nothing:
// it takes one and gives it straight back, so the answer is what the limit stood at a moment ago
// and nothing waits behind a probe. A request's entry sheds a write the pod has no room for by it
// (holdWriteLimit); the slot such a write goes on to hold is taken by the transaction itself
// (server.begin).
func (l *writeLimiter) probe() bool {
	free := l.semaphore()
	if !free.TryAcquire(1) {
		return false
	}
	free.Release(1)
	return true
}

// errWriteLimitFull refuses a write the pod has no room for, at the request's entry and at the
// transaction alike, so one cause reads as one refusal whichever of the two met it.
var errWriteLimitFull = errorf(http.StatusServiceUnavailable, "SERVICE_BUSY",
	"the server is serving as many writes at once as its connection pool allows; retry")

// holdWriteLimit sheds, at a request's entry, a write the pod has no room for - before the
// handler resolves its artifact or opens anything - and bounds the arrival of the body the
// handler reads. It wraps every route that is not a GET.
//
// The entry is where shedding is cheap. The limit used to be taken inside server.begin, after the
// handler had read the request's artifact, and that read is not free: measured over 320
// concurrent writes to documents of a mebibyte on the two-processor pod, every one of the 320
// paid 1.34 s at the median for its artifact read before 305 of them were refused at the limit,
// so the writes that would be refused burned the processors the admitted ones needed
// (dispatch://INFRA-179). Refused here, a write the pod has no room for costs the caller's own
// resolution and nothing else: no artifact read, no lock, no copy.
//
// It takes no slot, it only probes for one. A slot taken here was held for the whole handler, the
// body read included, so 48 signed-in callers whose bodies stopped after a byte held every one of
// the 48, and every other write, signed in or not, was refused 503 SERVICE_BUSY until those
// bodies' deadlines ran out (review 6's finding 7; review 5820480859's finding 4 before it
// answered that hold for callers that do not resolve, and only for those). So the slot a write
// holds is the one its transaction takes (server.begin): a request holds a slot while it has a
// transaction open, and at no other time.
//
// Two rules keep the entry from refusing writes it should not:
//
//   - A request whose caller does not resolve is probed against nothing. It is refused by the
//     handler's own authentication a moment later, and until then it holds nothing. Resolving the
//     caller costs one indexed read at most, against the whole-document read the entry exists to
//     skip.
//   - A body is read under a deadline of its own (deadlinedBody), so a client that sends headers
//     and stalls holds its connection and its handler for that deadline and not for as long as it
//     keeps the connection open. The server sets no ReadTimeout of its own, deliberately, because
//     its event stream is long-lived (cmd/dispatch's newHTTPServer).
func (s *server) holdWriteLimit(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.optionalActor(r); err != nil {
			handler(w, r)
			return
		}
		if !s.writes.probe() {
			s.writeHandlerError(w, errWriteLimitFull)
			return
		}
		if r.Body == nil || r.Body == http.NoBody {
			handler(w, r)
			return
		}
		body := &deadlinedBody{ReadCloser: r.Body, writer: w, idle: s.idleBound(), whole: bodyDrainLimit}
		r.Body = body
		// The drain is this request's writer, and it is what answers a cut body (drainingWriter):
		// routeHandler puts drainBeforeAnswer directly around this layer on every route that is not
		// a GET, and the drain hands on every request with a body under its own writer. Only a
		// chain that lost the drain could hand this layer another, and the assertion refuses it
		// loudly rather than leave a cut body to the 400 that blames the caller's input.
		w.(*drainingWriter).deadlined = body
		handler(w, r)
	}
}

// bodyIdleLimit is how long a handler's read of a request's body waits for the body's next bytes
// before it cuts the body and answers BODY_STALLED (deadlinedBody's idle deadline). Twenty seconds,
// for three reasons:
//
//   - It is the silence Dispatch's own command-line client takes for a dead attempt: an attempt
//     that goes 20 s without progress, none of its body taken by the connection and no answer
//     begun, is cut off and asked again (envoy-client's DISPATCH_WRITE_RETRY.silenceMs). A body
//     silent that long is not a slow link by the measure the client itself uses, and by then the
//     command has given that attempt up.
//   - It sits well under the ceiling over it (bodyDrainLimit, a minute), so a body that stops
//     anywhere in its first 40 s is cut by it and named a stall (deadlinedBody).
//   - A client that stopped sending holds its connection and its handler for 20 s after its last
//     bytes, where under the 45 s ceiling this replaced it held them until 45 s after its first.
const bodyIdleLimit = 20 * time.Second

// idleBound is how long a handler's read of a request's body waits for the body's next bytes
// (bodyIdleLimit; drainReadLimit in a test, which shortens the drain's own wait too, drainBound).
func (s *server) idleBound() time.Duration {
	if s.drainReadLimit > 0 {
		return s.drainReadLimit
	}
	return bodyIdleLimit
}

// deadlinedBody is a request's body read under a deadline on the connection it arrives over (the
// drain's own, readFor), so a client that sends headers and stops does not hold the connection
// and the handler reading it for as long as it keeps the connection open.
//
// The deadline is an IDLE one: every read that brings bytes sets it again, idleBound ahead, so
// what it bounds is a body that has stopped arriving and not a body that is slow. One absolute
// deadline at the first read cut a body that was still arriving, steadily: 41 bytes sent three at
// a time every 150 ms - 2.0 s in all, against a drain bound of 1 s - were answered 400
// INVALID_JSON with net's "i/o timeout" behind it, which blames the caller's JSON for a read the
// server itself ended (dispatch://INFRA-179, review 6's finding 6).
//
// Over the idle deadline stands an absolute ceiling, bodyDrainLimit from the first read, so a
// trickle that sends a byte just inside each idle deadline cannot hold a connection for hours. It
// is the drain's own minute, for two reasons:
//
//   - A minute carries the largest body a route takes, an upload (routeBody), over a link of
//     450 KB/s or more, as it does for the drain. Under the 45 s ceiling it replaced, the same
//     upload needed about 600 KB/s, so the server waited longer for an upload it refused (the
//     drain) than it read one it would take.
//   - A body still arriving a minute after its first read is one no `dispatch` command is waiting
//     for: every call's deadline is 60 s (envoy-client's DISPATCH_TOOL_DEADLINE_MS).
//
// The ceiling was docs.WriteRequestBound, 45 s, on the ground that a body still arriving then has
// no time left to be written in. That holds for a document write, which that bound still ends
// from its request's start whatever its body did (docs' Service.Join), and for nothing else: a
// message joins no document and meets no such bound, and its body, 64 bytes sent a byte every
// 800 ms and never silent past 0.8 s, was cut at 45 s and refused BODY_TOO_SLOW
// (dispatch://INFRA-179, review 7).
//
// The idle deadline is a bound only while it falls before the ceiling. Under that 45 s ceiling it
// stood at the drain's minute, so every rearm ran past the ceiling and set the ceiling's instant,
// the first read and 45 s: a body that stopped and a body still arriving were cut at the same
// moment, both answered BODY_TOO_SLOW, and BODY_STALLED was unreachable at the deployed bounds
// (review 7). At bodyIdleLimit under a minute, a body whose bytes stop arriving in its first 40 s
// is cut 20 s after its last bytes and named a stall. Only one that stops in its last 20 s meets
// the ceiling first, and the ceiling cannot tell that stall from a trickle, which is why its
// refusal says only that the body had not arrived whole.
//
// Whichever of the two ended the read, the refusal names it and is a 503 the caller can retry
// (drainingWriter), in place of the 400 a decode makes of a body it never saw.
//
// The deadline lifts at the body's end and nowhere else, which is readFor's own rule and not a
// choice of this type's: a read that failed, or a body that ran past its route's limit, leaves
// the deadline in place, because net/http reads on after the answer - it discards up to 256 KiB
// more before it writes the header, and again when it closes the body - and with the deadline
// those reads end where this one did and the connection closes. Lifting at the handler's return
// instead left a client that stopped sending holding its connection open after its answer, which
// TestADrainStopsWaitingForABodyThatStalls caught: "the connection was still open 1s after the
// answer to a stalled 100000-byte body".
type deadlinedBody struct {
	io.ReadCloser
	// writer answers the request, and the connection beneath it is what carries the deadline. A
	// writer with no connection of its own (a test's recorder) bounds nothing.
	writer http.ResponseWriter
	idle   time.Duration
	whole  time.Duration

	mu        sync.Mutex
	deadline  readDeadline
	begun     bool
	bounded   bool
	ceiling   time.Time
	atCeiling bool
	lifted    bool
	cut       *apiError
}

func (b *deadlinedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if !b.begun {
		b.begun = true
		b.deadline = newReadDeadline(b.writer)
		now := time.Now()
		b.ceiling = now.Add(b.whole)
		b.rearm(now)
	}
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case errors.Is(err, io.EOF):
		if !b.lifted {
			b.lifted = true
			b.deadline.lift()
		}
	case err == nil && n > 0 && b.bounded:
		b.rearm(time.Now())
	case errors.Is(err, os.ErrDeadlineExceeded) && b.cut == nil:
		if b.atCeiling {
			b.cut = errorf(http.StatusServiceUnavailable, "BODY_TOO_SLOW",
				"the request's body had not arrived whole %s after the server began reading it, which is the longest the server reads a body for; send it over a faster link, or in smaller writes, and retry", b.whole)
			break
		}
		b.cut = errorf(http.StatusServiceUnavailable, "BODY_STALLED",
			"the request's body stopped arriving for %s; resend the write with the whole of its body behind it", b.idle)
	}
	return n, err
}

// rearm bounds the next read at the idle deadline, or at the ceiling where the idle deadline
// would run past it, and remembers which of the two it set, since that is what the refusal names.
func (b *deadlinedBody) rearm(now time.Time) {
	at := now.Add(b.idle)
	b.atCeiling = !at.Before(b.ceiling)
	if b.atCeiling {
		at = b.ceiling
	}
	b.bounded = b.deadline.set(at)
}

// cutBy is the refusal the deadline that ended a read answers with, and nil while the body is
// still arriving or has arrived whole.
func (b *deadlinedBody) cutBy() *apiError {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cut
}

// limitedTx is a transaction holding one of the API's write limit, which it gives back as it
// ends. Commit and Rollback are the only two ways a transaction ends, and every caller of
// server.begin defers one of them, so the limit is held exactly as long as the connection
// beneath it is. Giving it back twice gives back nothing: a caller that rolls back a committed
// transaction - which every handler's deferred Rollback does - releases once (take).
type limitedTx struct {
	pgx.Tx
	release func()
}

func (t *limitedTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.release()
	return err
}

func (t *limitedTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.release()
	return err
}
