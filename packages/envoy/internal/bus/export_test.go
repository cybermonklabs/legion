package bus

import (
	"time"

	"github.com/nats-io/nats.go"
)

// WithReconnectWaitTick runs tick at each pass of the client's wait for a reconnect
// (ensureConnWithContext), before the pass blocks, and reads a connection event tick causes before
// the wait looks at its context, so a test outside the package can tell a publish is inside that
// wait, or act at the moment the wait ends. It is compiled into the package's tests only; no
// production connect can set it.
func WithReconnectWaitTick(tick func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.reconnectWaitTick = tick })
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

// WithBeforeBackgroundInstall runs hook in a background start (WithConnectInBackground) between the
// dial and the install of the connection it returned, so a test can have a reconnect attempt hold
// nats.go's connection lock while the install runs. It is compiled into the package's tests only;
// no production connect can set it.
func WithBeforeBackgroundInstall(hook func(*nats.Conn)) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeBackgroundInstall = hook })
}
