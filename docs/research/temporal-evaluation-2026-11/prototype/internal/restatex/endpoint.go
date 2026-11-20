package restatex

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Endpoint is a loopback port a driver holds for the service processes it starts there,
// from the moment the kernel chooses it until the experiment ends, so no other program
// can take it between a service's start and a replacement's, and the URL Restate calls
// stays this experiment's.
//
// It holds the port with a socket bound with SO_REUSEPORT that never listens. While no
// service serves, a connection to the port is refused, as one to a port nothing holds
// is: that is the dead endpoint experiment 10 measures. A program that binds the port
// without SO_REUSEPORT, as programs do, gets EADDRINUSE. Each service is handed a second
// socket bound to the same port with SO_REUSEPORT and listening (StartService), so a
// replacement serves the very URL its predecessor did. The limit: a program of the same
// user that also sets SO_REUSEPORT could still join the port.
type Endpoint struct {
	hold int
	port int
	once sync.Once
}

// HoldEndpoint holds a loopback port the kernel chooses. The caller must Close it once
// no service of the endpoint runs.
func HoldEndpoint() (*Endpoint, error) {
	loopback := net.ParseIP(Loopback).To4()
	// The socket and its close-on-exec flag under the fork lock, as the net package does
	// where SOCK_CLOEXEC is missing, so no service started meanwhile inherits the hold.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("hold an endpoint port: socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("hold an endpoint port: SO_REUSEPORT: %w", err)
	}
	address := &syscall.SockaddrInet4{}
	copy(address.Addr[:], loopback)
	if err := syscall.Bind(fd, address); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("hold an endpoint port: bind %s:0: %w", Loopback, err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("hold an endpoint port: getsockname: %w", err)
	}
	inet, ok := bound.(*syscall.SockaddrInet4)
	if !ok {
		syscall.Close(fd)
		return nil, fmt.Errorf("hold an endpoint port: the socket is bound to %T, not an IPv4 address", bound)
	}
	return &Endpoint{hold: fd, port: inet.Port}, nil
}

// Address is the endpoint's loopback HOST:PORT.
func (e *Endpoint) Address() string {
	return net.JoinHostPort(Loopback, strconv.Itoa(e.port))
}

// listen returns a socket listening on the endpoint's port beside the hold, as a file a
// service process inherits. The caller closes it once the process has started, so the
// socket lives as long as the process that serves on it, and the port goes back to
// refusing connections when that process dies.
func (e *Endpoint) listen() (*os.File, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var optErr error
		if err := raw.Control(func(fd uintptr) {
			optErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return optErr
	}}
	listener, err := config.Listen(context.Background(), "tcp4", e.Address())
	if err != nil {
		return nil, fmt.Errorf("listen on the held endpoint %s: %w", e.Address(), err)
	}
	defer listener.Close()
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		return nil, fmt.Errorf("hand over the socket listening on %s: %w", e.Address(), err)
	}
	return file, nil
}

// Close gives the port up. It is safe to call more than once.
func (e *Endpoint) Close() {
	e.once.Do(func() { syscall.Close(e.hold) })
}
