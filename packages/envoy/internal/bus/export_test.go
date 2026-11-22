package bus

import "time"

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

// WithRequestExpiry has each Request's timeout fire when the channel expiry returns for it does,
// in place of a timer at the request's deadline; a nil channel keeps that timer. The func expiry
// returns beside it, when not nil, runs once the Request has settled a flush that returned before
// its timeout fired, on the Request's own goroutine: a test that fires the timeout from it has the
// Request settle the flush first, every time. A Request whose timeout fires while its flush has
// not returned waits for the flush, which its deadline still bounds, so a test that fires the
// timeout at once and gives the Request a long deadline has the flush's verdict in the answer
// however late the scheduler runs the flush. It is compiled into the package's tests only; no
// production connect can set it.
func WithRequestExpiry(expiry func(deadline time.Time) (expired <-chan time.Time, flushSettled func())) ConnectOption {
	return clientOption(func(o *connectOpts) { o.requestExpiry = expiry })
}
