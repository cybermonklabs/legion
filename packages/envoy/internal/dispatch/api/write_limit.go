package api

import (
	"context"
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

// writeGrantKey carries the write limit a request took at its entry, so the transaction it opens
// runs on that one rather than taking a second (server.begin).
type writeGrantKey struct{}

// writeGrantFrom is the write limit ctx's request already holds, or nil for a caller holding
// none: a server task of its own, a test that calls begin directly.
func writeGrantFrom(ctx context.Context) func() {
	release, _ := ctx.Value(writeGrantKey{}).(func())
	return release
}

// holdWriteLimit takes the API's write limit at a request's entry, before the handler reads its
// body, resolves its artifact or opens anything, and gives it back when the handler returns. It
// wraps every route that is not a GET.
//
// The limit used to be taken inside server.begin, after the handler had read the request's
// artifact, and that read is not free: measured over 320 concurrent writes to documents of a
// mebibyte on the two-processor pod, every one of the 320 paid 1.34 s at the median for its
// artifact read before 305 of them were refused at the limit, so the writes that would be
// refused burned the processors the admitted ones needed (dispatch://INFRA-179). Taken here, a
// write the pod has no room for costs one wait on a semaphore and nothing else: no body read, no
// pooled read, no lock, no copy.
//
// The handler's own transaction runs on this grant (server.begin, writeGrantFrom), so a request
// holds one of the limit however many transactions it opens in turn, and the limit still bounds
// how many of the pool's connections the API's writes hold at once.
func (s *server) holdWriteLimit(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, err := s.writes.take(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		defer release()
		handler(w, r.WithContext(context.WithValue(r.Context(), writeGrantKey{}, release)))
	}
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
