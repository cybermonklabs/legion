//go:build linux || darwin

package testnats

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// holdPort binds a TCP socket to 127.0.0.1 on a port the kernel chooses and never listens on it,
// and returns the socket's address and the func that closes it. The socket sets SO_REUSEPORT, so
// a listener of this process that sets it too can bind the same address beside it
// (listenBesideHold), and sets no SO_REUSEADDR (Gate).
func holdPort() (address string, release func(), err error) {
	// The socket is made close-on-exec under ForkLock, as the net package makes its own where the
	// kernel has no SOCK_CLOEXEC (Darwin), so no process this test starts inherits the hold.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return "", nil, fmt.Errorf("make the socket that holds the port: %w", err)
	}
	release = func() { _ = unix.Close(fd) }
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		release()
		return "", nil, fmt.Errorf("set SO_REUSEPORT on the socket that holds the port: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		release()
		return "", nil, fmt.Errorf("bind the socket that holds the port: %w", err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		release()
		return "", nil, fmt.Errorf("read the address of the socket that holds the port: %w", err)
	}
	inet4, ok := bound.(*unix.SockaddrInet4)
	if !ok {
		release()
		return "", nil, fmt.Errorf("the socket that holds the port is bound to a %T, want an IPv4 address", bound)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(inet4.Port)), release, nil
}

// listenBesideHold listens on address, which a socket of holdPort holds, with SO_REUSEPORT.
func listenBesideHold(address string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, conn syscall.RawConn) error {
		var set error
		if err := conn.Control(func(fd uintptr) {
			set = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return set
	}}
	return config.Listen(context.Background(), "tcp4", address)
}
