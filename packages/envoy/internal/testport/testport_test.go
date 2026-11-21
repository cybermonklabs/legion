package testport

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// squat binds address as another program on the machine would, with SO_REUSEADDR and SO_REUSEPORT
// both set, the widest a bind can ask to share a port. It answers the listener, or the bind's error.
func squat(address string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var set error
		if err := raw.Control(func(fd uintptr) {
			set = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if set == nil {
				set = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
		}); err != nil {
			return err
		}
		return set
	}}
	return config.Listen(context.Background(), "tcp", address)
}

// The control of the squat below: the squatter takes a port whose holder lets it share, so a squat
// TestRefusedRefusesEveryDialAndHoldsItsPort sees refused was refused by the hold, not by a
// squatter that can take no port at all. The holder keeps its port throughout, so no other process
// can take it in between.
func TestTheSquatterTakesAPortItsHolderShares(t *testing.T) {
	t.Parallel()
	holder, err := squat("127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port that lets others share it: %v", err)
	}
	defer holder.Close()
	second, err := squat(holder.Addr().String())
	if err != nil {
		t.Fatalf("the squatter could not share %s with a holder that shares it: %v", holder.Addr(), err)
	}
	_ = second.Close()
}

func TestRefusedRefusesEveryDialAndHoldsItsPort(t *testing.T) {
	t.Parallel()
	address := Refused(t)
	for range 3 {
		conn, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("a dial to %s connected, want it refused", address)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("a dial to %s failed with %v, want ECONNREFUSED", address, err)
		}
	}
	if listener, err := net.Listen("tcp", address); err == nil {
		_ = listener.Close()
		t.Fatalf("a plain bind took %s while the test held it", address)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a plain bind of %s failed with %v, want EADDRINUSE", address, err)
	}
	if listener, err := squat(address); err == nil {
		_ = listener.Close()
		t.Fatalf("a bind with SO_REUSEADDR and SO_REUSEPORT took %s while the test held it", address)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a sharing bind of %s failed with %v, want EADDRINUSE", address, err)
	}
}

// A reservation refuses a dial and a bind without SO_REUSEADDR, yet takes a Go listener's bind
// beside it twice, as a server that starts and then restarts on the port makes it. The listener
// rows fail when Reserve drops SO_REUSEADDR, and the dial row when the reservation listens.
func TestReserveTakesAListenerAndRefusesADialAndAPlainBind(t *testing.T) {
	t.Parallel()
	fd, port, err := Reserve(&syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a dial of the reserved %s: %v, want ECONNREFUSED", address, err)
	}
	plain, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("make the plain bind's socket: %v", err)
	}
	err = syscall.Bind(plain, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
	_ = syscall.Close(plain)
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a bind of the reserved %s without SO_REUSEADDR: %v, want EADDRINUSE", address, err)
	}
	for _, start := range []string{"start", "restart"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("a Go listener's bind of the reserved %s at its %s: %v", address, start, err)
		}
		_ = listener.Close()
	}
}
