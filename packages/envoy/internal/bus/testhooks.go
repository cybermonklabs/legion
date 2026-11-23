package bus

import "sync/atomic"

// firstConnectGate holds the channel the first-connect callback of a client that connects in the
// background waits on before it logs the connection and runs onConnect (connectInBackground). Only
// tests set it (HoldFirstConnectForTest); it is nil otherwise, and the callback waits for nothing.
var firstConnectGate atomic.Pointer[<-chan struct{}]

// HoldFirstConnectForTest makes the first-connect callback of every client that connects in the
// background from here on wait until gate is closed, until the function it answers puts the gate
// it found back. It is for tests alone: it holds the moment nats.go reports the connection
// CONNECTED and the moment the client hears of it apart, so a test can land that callback inside
// work it chooses.
func HoldFirstConnectForTest(gate <-chan struct{}) (restore func()) {
	previous := firstConnectGate.Swap(&gate)
	return func() { firstConnectGate.Store(previous) }
}

// AwaitingConnectionForTest reports whether c connects in the background and has yet to run its
// first-connect callback (onConnect). It is for tests alone, which read it beside
// HoldFirstConnectForTest.
func (c *Client) AwaitingConnectionForTest() bool { return c.awaitingFirstConnection() }

// ProbeReadsDownForTest makes each read of c's connection's status through the probe (connProber)
// that its callers make within the probe's budget answer "unknown", as a read nats.go's lock kept
// waiting past ConnLockWait does, while down reports true, until the function it answers takes
// the seam out; a caller that holds a lock reads that as the connection down. Those reads are
// LostNow's and the wait for a connection of a call whose caller holds a lock (connForRequest,
// connForCall) on any client - a client whose callers all hold a lock (withLossAtOnce), and a call
// made under a context marked so (WithLockHeld) - and, on a client whose callers all hold a lock, a
// request's own reads (requestLife, closedUnder); a life's own goroutine reads past it. A row that
// means one of those reads keys down to it (the caller's frames, say) rather than to a count of
// reads, since a marked call adds reads of its own. It is for tests alone: it is the slow read a
// loaded box can give, made on demand.
func (c *Client) ProbeReadsDownForTest(down func() bool) (restore func()) {
	previous := c.probeDown.Swap(&down)
	return func() { c.probeDown.Store(previous) }
}

// ProbesForTest is the number of reads the probe of c's current connection has begun. It is for
// tests alone.
func (c *Client) ProbesForTest() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn == nil {
		return 0
	}
	return c.proberLocked().probes.Load()
}

// AnswerWaitForTest makes tick run each time a request on c whose caller holds no lock begins to
// wait for nats.go's own answer about its connection (requestReadUntil), until the function it
// answers takes the hook out. It is for tests alone: a row of another package reads it to know that
// a call made under a lock (WithLockHeld) never begins that wait.
func (c *Client) AnswerWaitForTest(tick func()) (restore func()) {
	previous := c.answerWaitTick.Swap(&tick)
	return func() { c.answerWaitTick.Store(previous) }
}

// ProbeReadsDownForTest is Client.ProbeReadsDownForTest on the session consumers' own client.
func (s *SessionConsumers) ProbeReadsDownForTest(down func() bool) (restore func()) {
	return s.client.ProbeReadsDownForTest(down)
}
