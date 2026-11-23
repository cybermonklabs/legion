package bus

import (
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A Close or a Drain made while the hub holds the handshake of the client's reconnect leaves the
// client's lock free while nats.go makes it wait: nats.go's Close and Stats wait for nats.go's
// connection lock, which the handshake holds, and the client calls them with its own lock let go
// (Close, Drain), so every caller of that lock - a probe, Connection, a request's life - is answered
// meanwhile, rather than waiting for the handshake behind a shutdown.
//
// The row decides by events and reads no clock to pass. The stop reaches nats.go through its seam
// (stopHook), and only then does the row call Connection, which must answer while the handshake is
// still held; a row that fails ends at the test's deadline.
//
// The controls are Close calling nats.go's Close under the client's lock, and Drain reading Stats
// under it: the stop's hook runs with the lock held, Connection gives no answer while the
// handshake is held, and the row fails.
func TestAStopDuringAHeldHandshakeLeavesTheClientsLockFree(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{"Close", func(c *Client) { c.Close() }},
		{"Drain", func(c *Client) { _ = c.Drain(heldAnswerWait) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			client, hold := proxiedClient(t)
			awaitHeld(t, hold.Hold())
			reached := make(chan struct{})
			var once sync.Once
			// The test's cleanup closes the client again, so the hook runs more than once.
			client.stopHook = func() { once.Do(func() { close(reached) }) }
			stopped := make(chan struct{})
			go func() {
				stop.stop(client)
				close(stopped)
			}()
			select {
			case <-reached:
			case <-time.After(testnats.Remaining(t)):
				hold.Release()
				t.Fatalf("%s never reached its call into nats.go by the test's deadline", stop.name)
			}
			answered := make(chan struct{})
			go func() {
				client.Connection()
				close(answered)
			}()
			select {
			case <-answered:
			case <-hold.Ended():
				t.Fatalf("Connection gave no answer while %s waited for the held handshake: the stop held the client's lock", stop.name)
			case <-time.After(testnats.Remaining(t)):
				hold.Release()
				t.Fatalf("Connection gave no answer by the test's deadline while %s waited for the held handshake: the stop held the client's lock", stop.name)
			}
			select {
			case <-hold.Ended():
				t.Fatalf("Connection answered only once the held handshake had ended")
			default:
			}
			hold.Release()
			select {
			case <-stopped:
			case <-time.After(testnats.Remaining(t)):
				t.Fatalf("%s had not returned by the test's deadline, though the hold had ended", stop.name)
			}
		})
	}
}
