package bus

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// WithReconnectWaitTick runs tick at each pass of the client's wait for a reconnect
// (ensureConnWithContext), before the pass blocks, and reads a connection event tick causes before
// the wait looks at its context, so a test outside the package can tell a publish is inside that
// wait, or act at the moment the wait ends. It is compiled into the package's tests only; no
// production connect can set it.
func WithReconnectWaitTick(tick func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.reconnectWaitTick = tick })
}

// ReconnectWorkDone reports whether the client has worked through every connection event its
// callbacks handed it (queueReconnect): nothing is queued and no goroutine of the client's is
// working one. The work of a reconnect no longer runs on nats.go's callback goroutine, so a test
// that reads what that work decided waits for this first. It is compiled into the package's
// tests only.
func (c *Client) ReconnectWorkDone() bool {
	c.reconnectQueueMu.Lock()
	defer c.reconnectQueueMu.Unlock()
	return !c.reconnectRunning && len(c.reconnectQueue) == 0
}

// WithBeforeRequestFlush runs before on the goroutine that flushes a Request's publish, right
// before that flush begins, so a test outside the package can have the connection lost by the time
// the flush starts - the interleaving in which nats.go answers the flush with nothing, having
// cleared its pending flushes as the connection turned reconnecting. The wait for the request's
// answer runs beside that goroutine and is not held by the hook. It is compiled into the package's
// tests only; no production connect can set it.
func WithBeforeRequestFlush(before func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeRequestFlush = before })
}

// WithBeforeRequestWait runs before on a Request's wait for its answer each time the wait is about
// to sleep, with the channel whose close is the loss the wait answers, so a test outside the
// package can tell whether the wait sleeps on a loss it has already seen. The wait does not sleep
// until before returns. It is compiled into the package's tests only; no production connect can
// set it.
func WithBeforeRequestWait(before func(loss <-chan struct{})) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeRequestWait = before })
}

// BeforeReportEndsLife runs before on the drain of conn's lives (connLife) each time it has taken a
// report of nats.go's, and before it ends the life at that report; the life ends only once before
// returns. It follows conn from now on when nothing follows it yet (lifeOf). A test outside the
// package holds the end of a lost connection's life with it, so the client hears of the loss
// first. It is compiled into the package's tests only.
func BeforeReportEndsLife(conn *nats.Conn, before func()) {
	lifeOf(conn).reportTaken.Store(&before)
}

// RequireLostWithTheConnection is requireLostWithTheConnection (request_lost_test.go) for a test
// outside the package: call's request on conn is dropped by relay, the link is then cut, and call
// must answer ErrRequestLostWithConnection at the reconnect.
func RequireLostWithTheConnection(t *testing.T, relay *testnats.UnansweredRelay, conn *nats.Conn, what string, call func(context.Context) error) {
	t.Helper()
	requireLostWithTheConnection(t, relay, conn, what, call)
}

// WithBeforeCoreSendFlush runs before on the goroutine of a send of the core subscription
// (sendCore), after the bind and right before the flush that waits for the server's answer, so a
// test can replace the connection's session while the send is open. It is compiled into the
// package's tests only; no production connect can set it.
func WithBeforeCoreSendFlush(before func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeCoreSendFlush = before })
}

// WithAfterCoreSendFlush runs after on the goroutine of a send of the core subscription
// (sendCore), right after the flush returns and before the server's answer is read, so a test can
// drop the connection between the answer's arrival and its read. It is compiled into the package's
// tests only; no production connect can set it.
func WithAfterCoreSendFlush(after func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.afterCoreSendFlush = after })
}

// WithRequestExpiry has each Request's timeout fire when the channel expiry returns for it does,
// in place of a timer at the request's deadline; a nil channel keeps that timer. The func expiry
// returns beside it, when not nil, runs once the Request has settled a flush that returned before
// its timeout fired, on the Request's own goroutine: a test that fires the timeout from it has the
// Request settle the flush first, every time. A Request whose timeout fires while its flush has
// not returned waits for the flush. Its deadline bounds what follows the flush's take of nats.go's
// connection lock, from the moment nats.go lets the flush take it (confirmFlushed): a flush that
// waited X for the lock waits for the server's answer until X after the deadline. So a test that
// fires the timeout at once and gives the Request a long deadline has the flush's verdict in the
// answer however late the scheduler runs the flush. It is compiled into the package's tests only;
// no production connect can set it.
func WithRequestExpiry(expiry func(deadline time.Time) (expired <-chan time.Time, flushSettled func())) ConnectOption {
	return clientOption(func(o *connectOpts) { o.requestExpiry = expiry })
}

// WithClosedHook runs hook at the start of the client's handling of each close of a connection it
// dialled (onClosed), before the recovery the close starts, so a test can hold that work and catch
// a caller that returned before it ran. It is compiled into the package's tests only; no
// production connect can set it.
func WithClosedHook(hook func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.closedHook = hook })
}

// WithCloseSettleBound has the close of a failed start wait for its callbacks and its recovery for
// up to bound (closeSettled), in place of failedStartCloseBound, so a test that holds that close
// (WithClosedHook) decides when it ends, not the production bound. It is compiled into the
// package's tests only; no production connect can set it.
func WithCloseSettleBound(bound time.Duration) ConnectOption {
	return clientOption(func(o *connectOpts) { o.closeSettleBound = bound })
}

// WithWatchWentOn runs went on the goroutine of each connection watch of the client
// (connWatch.lost, connWatch.bound) each time a connection event it woke on left its request
// unlost, once it has taken the channel it waits on next and before it waits there, so a test can
// land the reconnect that follows a disconnect in that moment. It is compiled into the package's
// tests only; no production connect can set it.
func WithWatchWentOn(went func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.watchWentOn = went })
}
