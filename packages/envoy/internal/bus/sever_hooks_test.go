package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Sever calls the hook a test sets (AfterSeverResetForTest) once it has reset every socket of the
// connection and before it closes the connection: at the hook the dialer refuses every dial, its
// sockets reset, and the connection is not closed yet; once Sever returns, it is. The function the
// hook answers puts back the hook it found, so a later Sever calls nothing. The negative controls
// are a Sever that calls the hook before it resets the sockets, which fails the first verdict, and
// one that calls it after the close, which fails the second.
func TestSeverCallsItsTestHookBetweenTheResetAndTheClose(t *testing.T) {
	// Not parallel: it sets the package's sever hook (AfterSeverResetForTest), which every Sever of
	// the process reads.
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect to the test's server: %v", err)
	}
	t.Cleanup(client.Close)
	current := client.severable.Load()
	if current == nil || current.dialer == nil {
		t.Fatal("the client's connection is not severable")
	}
	var calls int
	var severedAtHook, closedAtHook bool
	restore := AfterSeverResetForTest(func() {
		calls++
		current.dialer.mu.Lock()
		severedAtHook = current.dialer.severed
		current.dialer.mu.Unlock()
		closedAtHook = current.conn.IsClosed()
	})
	client.Sever()
	restore()
	if calls != 1 {
		t.Fatalf("Sever called its hook %d times, want once", calls)
	}
	if !severedAtHook {
		t.Error("Sever called its hook before it reset the connection's sockets")
	}
	if closedAtHook {
		t.Error("Sever called its hook after it closed the connection")
	}
	if !current.conn.IsClosed() {
		t.Error("control: the connection is still open once Sever returned")
	}
	if severResetHook.Load() != nil {
		t.Error("the hook is still set once the function AfterSeverResetForTest answered ran")
	}
}

// ConfirmMarkedBy confirms the marks through the caller's flush, as ConfirmMarked does through
// FlushTimeout: a flush that fails confirms nothing and leaves the mark, and one that answers
// confirms a mark whose write nats.go had taken. The flush here runs under the shared wait's
// context, the bound a caller on a clock of its own passes; the control is that flush answering.
func TestConfirmMarkedByConfirmsThroughTheCallersFlush(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect to the test's server: %v", err)
	}
	t.Cleanup(client.Close)
	written := client.MarkUnconfirmed()
	written()
	ended := errors.New("the caller's bound ended")
	var flushes int
	if client.ConfirmMarkedBy(func(*nats.Conn) error { flushes++; return ended }) {
		t.Error("ConfirmMarkedBy confirmed the marks although the caller's flush failed")
	}
	if !client.CarriesUnconfirmed() {
		t.Error("a failed flush cleared the mark of a write it had not confirmed")
	}
	if !client.ConfirmMarkedBy(func(conn *nats.Conn) error { flushes++; return conn.FlushWithContext(testwait.Context(t)) }) {
		t.Error("control: ConfirmMarkedBy did not confirm a taken write once the caller's flush answered")
	}
	if client.CarriesUnconfirmed() {
		t.Error("control: a mark is left after a flush that confirmed every marked write")
	}
	if flushes != 2 {
		t.Errorf("ConfirmMarkedBy called the caller's flush %d times over two confirmations, want 2", flushes)
	}
}
