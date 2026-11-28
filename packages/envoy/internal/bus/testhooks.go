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

// severResetHook is called by Sever once it has reset every socket of the connection and before it
// closes the connection. Only tests set it (AfterSeverResetForTest); it is nil otherwise, and Sever
// calls nothing between the two.
var severResetHook atomic.Pointer[func()]

// AfterSeverResetForTest makes every Sever from here on call reset once it has reset every socket of
// the connection (severableDialer.sever) and before it closes the connection, until the function it
// answers puts the hook it found back. reset runs on the caller of Sever, so it must not wait. It is
// for tests alone. Delivery's row "a hand-back pending when the hold's renewals fail"
// (TestAHandBackPendingWhenTheHoldIsLostNeverReachesTheStandbysDurable) reads it: the row holds
// nats.go's connection lock itself, so the close that follows the reset waits for the row's lock
// and no state of the connection can be read without that lock, and the hook is the one fact that
// the lost hold's sever has reset the sockets the pending hand-back would go out on. The row waits
// for it before it lets the standby take the durable over.
func AfterSeverResetForTest(reset func()) (restore func()) {
	previous := severResetHook.Swap(&reset)
	return func() { severResetHook.Store(previous) }
}
