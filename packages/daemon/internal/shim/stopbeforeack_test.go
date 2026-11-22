package shim_test

import (
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The daemon refuses the hello of a generation its claim has left, and tells the shim so with the
// stop frame before it closes the connection (R3). The shim ends its process on it rather than
// redialling for ever: this is the older process of a lock-out whose connection the network or a
// daemon restart cut, which would otherwise come back the moment the newer process had bound and
// write the session and the branch they share.
//
// A hello refused with nothing written — an unknown boot token — is not this: that shim keeps
// running (TestARefusedHelloIsRedialledWithBackoffAndNeverSpawnsOMP).
func TestAStopFrameBeforeTheHelloAckEndsTheShimsProcess(t *testing.T) {
	t.Run("with no wrapped process spawned", func(t *testing.T) {
		path := socketPath(t)
		daemon := listen(t, path)
		child := newOMP(t)
		clock := newClock()
		sh := run(t, config(t, path, child), clock)

		p := daemon.accept(t)
		p.expectHello(t)
		p.send(t, shimwire.Shutdown{})

		if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
			t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
		}
		if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
			t.Fatalf("OMP was spawned although the daemon never acked the hello; log:\n%s", sh.log)
		}
		if got := sh.log.count("told to stop before its hello was acked"); got != 1 {
			t.Fatalf("the shim said so %d times; log:\n%s", got, sh.log)
		}
	})

	t.Run("with the wrapped process already running", func(t *testing.T) {
		path := socketPath(t)
		daemon := listen(t, path)
		child := newOMP(t)
		clock := newClock()
		sh := run(t, config(t, path, child), clock)
		p := daemon.accept(t)
		p.open(t)

		// The connection is cut, as a network cut or a daemon restart cuts it, and the redial's
		// hello is refused with the stop frame: the claim has left this generation.
		_ = p.conn.Close()
		sh.log.await(t, "reconnecting", 1)
		redial := daemon.accept(t)
		redial.expectHello(t)
		redial.send(t, shimwire.Shutdown{})

		sh.log.await(t, "the daemon's stop frame before hello_ack: sending SIGTERM to the wrapped process", 1)
		if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
			t.Fatalf("the shim exited %d, want the child's %d", code, 128+int(syscall.SIGTERM))
		}
	})
}
