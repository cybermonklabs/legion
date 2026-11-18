package bus

// WithReconnectWaitPass runs pass at each pass of the client's wait for a reconnect
// (ensureConnWithContext), before the pass blocks, and reads a connection event pass causes before
// the wait looks at its context, so a test outside the package can act at the moment the wait
// ends. It is compiled into the package's tests only; no production connect can set it.
func WithReconnectWaitPass(pass func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.reconnectWaitPass = pass })
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

// The production spans the tests outside the package move a clocktest.Fake past, rather than
// shortening them: a window's send and a Request's default timeout, the reconnect wait's recheck,
// the keeper's period and the part of Drain's timeout its hooks keep. A publish's bound is the
// package's own PublishBound.
const (
	PublishAcknowledgementTimeout = publishAcknowledgementTimeout
	ReconnectRecheck              = reconnectRecheck
	DefaultKeepInterval           = defaultKeepInterval
	DrainHookReserve              = drainHookReserve
)

// HoldsSeedWork is HoldsSeed on text, with the work it did (seedWork) as one count.
func HoldsSeedWork(text string) (found bool, work int) {
	var spent seedWork
	found = holdsSeed(text, &spent)
	return found, spent.read + spent.moved + spent.checksummed
}

// HoldsSeedLinearBound is the most work HoldsSeed's doc allows on n bytes: maxPercentDecodes+1
// scans, each reading the n bytes, moving each letter at most once more and checksumming a window
// of seedTextLength letters at most once per letter, and maxPercentDecodes decodings, each reading
// the n bytes.
func HoldsSeedLinearBound(n int) int {
	return (maxPercentDecodes+1)*(2+seedTextLength)*n + maxPercentDecodes*n
}
