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
