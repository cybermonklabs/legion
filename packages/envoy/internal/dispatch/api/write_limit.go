package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

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

// take holds one of the limit for a caller about to open a transaction, waiting within ctx and
// within the server's own bound on what a caller's request spends waiting at the pod's memory
// bounds (docs.BoundWriteWait), and answers what gives it back. A wait the caller's request ends
// is answered 503, which is what every other wait a request outlives is answered: the caller has
// gone or run out of time, and nothing of its write has been opened, read or locked. A wait that
// reaches the server's bound first is answered the same 503, with the caller's patience left for
// the retry rather than spent waiting here - a write left unanswered until its caller gives up is
// resent, which is worse for the pod than a refusal it can act on (dispatch://INFRA-179). The
// bound is shared with the load slot a write's fork waits for (docs' liveWrite.coverFork), so a
// write that waits at both is answered within it, and the server's own writes are not bounded.
func (l *writeLimiter) take(ctx context.Context) (func(), error) {
	ctx, cancel := docs.BoundWriteWait(ctx)
	defer cancel()
	if err := l.semaphore().Acquire(ctx, 1); err != nil {
		return nil, errorf(http.StatusServiceUnavailable, "SERVICE_BUSY",
			"the server is serving as many writes at once as its connection pool allows; retry")
	}
	var once sync.Once
	return func() { once.Do(func() { l.semaphore().Release(1) }) }, nil
}

// writeGrant is the write limit a request took at its entry (holdWriteLimit). A transaction
// borrows it while it is unspent and gives it back as it ends; once spent, a later transaction of
// the same request takes a slot of its own, because the limit counts the API's open transactions
// and a request that opens two in turn holds one at a time, never none (server.begin).
type writeGrant struct {
	mu   sync.Mutex
	held bool
	give func()
}

// claim hands the grant to a transaction while it is held, and answers nil once it is spent: the
// transaction that claimed it gave it back as it committed or rolled back (limitedTx), and a
// later begin on the same request's context must take one of its own. Without this a request's
// second transaction, and the work a handler detaches that keeps its context's values, ran
// outside the limit altogether (dispatch://INFRA-179, review 5820480859's finding 11).
func (g *writeGrant) claim() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		return nil
	}
	g.held = false
	return g.release
}

// release gives the grant's slot back once, whoever calls it: the transaction that claimed it, or
// the handler's own return for a request whose grant no transaction claimed.
func (g *writeGrant) release() {
	g.mu.Lock()
	g.held = false
	g.mu.Unlock()
	g.give()
}

// writeGrantKey carries the write limit a request took at its entry, so the transaction it opens
// runs on that one rather than taking a second (server.begin).
type writeGrantKey struct{}

// writeGrantFrom is the write limit ctx's request holds, or nil for a caller holding none: a
// request whose grant a transaction has already spent, a server task of its own, a test that
// calls begin directly.
func writeGrantFrom(ctx context.Context) func() {
	grant, _ := ctx.Value(writeGrantKey{}).(*writeGrant)
	if grant == nil {
		return nil
	}
	return grant.claim()
}

// holdWriteLimit takes the API's write limit at a request's entry, before the handler resolves
// its artifact or opens anything, and gives it back when the handler returns. It wraps every
// route that is not a GET.
//
// The limit used to be taken inside server.begin, after the handler had read the request's
// artifact, and that read is not free: measured over 320 concurrent writes to documents of a
// mebibyte on the two-processor pod, every one of the 320 paid 1.34 s at the median for its
// artifact read before 305 of them were refused at the limit, so the writes that would be
// refused burned the processors the admitted ones needed (dispatch://INFRA-179). Taken here, a
// write the pod has no room for costs the caller's own resolution and nothing else: no artifact
// read, no lock, no copy.
//
// Two rules keep that cheap entry from being a way to refuse everyone else's writes, both from
// review 5820480859's finding 4, which held every one of the 48 slots with 48 anonymous requests
// whose bodies stopped after one byte and read 503 SERVICE_BUSY for a signed-in person's comment:
//
//   - A request whose caller does not resolve takes no slot. It is refused by the handler's own
//     authentication a moment later, and until then it waits holding nothing; a transaction it
//     somehow opens takes a slot of its own inside begin, as every caller holding no grant does.
//     Resolving the caller costs one indexed read at most, against the whole-document read the
//     entry limit exists to skip.
//   - A body that stops arriving while the grant is held ends at a read deadline of its own
//     (readFor, the same bound the drain uses), lifted the moment the body reaches its end, so a
//     client that sends headers and stalls holds its slot for that bound and not for as long as
//     it keeps the connection open. The server sets no ReadTimeout of its own, deliberately,
//     because its event stream is long-lived (cmd/dispatch's newHTTPServer).
//
// The handler's own transaction runs on this grant (server.begin, writeGrantFrom), so a request
// holds one of the limit while it has one open, and the limit still bounds how many of the pool's
// connections the API's writes hold at once.
func (s *server) holdWriteLimit(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.optionalActor(r); err != nil {
			handler(w, r)
			return
		}
		give, err := s.writes.take(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		grant := &writeGrant{held: true, give: give}
		defer grant.release()
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &deadlinedBody{ReadCloser: r.Body, bound: func() func() { return s.readFor(w) }}
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), writeGrantKey{}, grant)))
	}
}

// deadlinedBody is a request's body read under a deadline while the write limit is held: its
// first read sets the deadline, and the body reaching its end lifts it. The deadline is the
// drain's own (readFor), so a body that stops arriving ends the read rather than holding one of
// the limit's slots for as long as the client keeps the connection open.
//
// It lifts at the body's end and nowhere else, which is readFor's own rule and not a choice of
// this type's: a read that failed, or a body that ran past its route's limit, leaves the deadline
// in place, because net/http reads on after the answer - it discards up to 256 KiB more before it
// writes the header, and again when it closes the body - and with the deadline those reads end
// where this one did and the connection closes. Lifting at the handler's return instead left a
// client that stopped sending holding its connection open after its answer, which
// TestADrainStopsWaitingForABodyThatStalls caught: "the connection was still open 1s after the
// answer to a stalled 100000-byte body".
type deadlinedBody struct {
	io.ReadCloser
	bound  func() func()
	once   sync.Once
	lifted sync.Once
	undo   func()
}

func (b *deadlinedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.undo = b.bound() })
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.lifted.Do(func() {
			if b.undo != nil {
				b.undo()
			}
		})
	}
	return n, err
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
