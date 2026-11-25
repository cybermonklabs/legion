//go:build unix

// A reservation's socket is a descriptor, an int, which is a unix socket's type; on Windows a
// socket is a syscall.Handle and this file does not compile, so it builds on unix alone. Every
// caller of Reserve runs its tests on Linux (testnats's Gate, envoy-infra's reservePorts).

package testport

import (
	"fmt"
	"syscall"
)

// Reserve binds a TCP socket of its own to addr, with SO_REUSEADDR, and never listens on it. It
// returns the socket's descriptor, which the caller closes when it gives the port up, and the port
// the socket is bound to, which the kernel chose when addr names port 0. Each of setup runs on the
// socket before SO_REUSEADDR is set and before the bind, for a caller's own options (IPV6_V6ONLY
// on an AF_INET6 socket, say). On any error the socket is closed and nothing stays bound.
//
// On Linux, a reserved port takes the bind of a server whose own socket sets SO_REUSEADDR, as Go's
// listeners do (net/sockopt_linux.go, setDefaultListenerSockopts): the kernel refuses a bind beside
// a socket that also sets SO_REUSEADDR only when that socket listens (net/ipv4/inet_connection_sock.c,
// inet_bind_conflict). A dial of the port is refused while nothing listens there, and a socket that
// binds it without SO_REUSEADDR is refused. What each caller holds a reservation against, and on
// which addresses, is its own policy (testnats's Gate, envoy-infra's reservePorts).
//
// Unlike Refused, the reservation sets SO_REUSEADDR, so a process the caller starts can bind the
// port; Refused sets neither reuse option, so nothing can.
//
// The daemon module's copy of this package (testport.go's comment) has no Reserve: it has no
// caller there.
func Reserve(addr syscall.Sockaddr, setup ...func(fd int) error) (int, int, error) {
	family := syscall.AF_INET
	if _, ok := addr.(*syscall.SockaddrInet6); ok {
		family = syscall.AF_INET6
	}
	// Under ForkLock, so no child started between the two calls inherits the descriptor; Go's own
	// net package does the same on a platform without SOCK_CLOEXEC.
	syscall.ForkLock.RLock()
	sock, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(sock)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, 0, fmt.Errorf("testport: make a reservation's socket: %w", err)
	}
	port, err := bindReservation(sock, addr, setup)
	if err != nil {
		_ = syscall.Close(sock)
		return -1, 0, err
	}
	return sock, port, nil
}

// bindReservation sets sock up as Reserve describes, binds it to addr and returns its port.
func bindReservation(sock int, addr syscall.Sockaddr, setup []func(fd int) error) (int, error) {
	for _, option := range setup {
		if err := option(sock); err != nil {
			return 0, fmt.Errorf("testport: set up a reservation's socket: %w", err)
		}
	}
	if err := syscall.SetsockoptInt(sock, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return 0, fmt.Errorf("testport: set SO_REUSEADDR on a reservation: %w", err)
	}
	if err := syscall.Bind(sock, addr); err != nil {
		return 0, fmt.Errorf("testport: bind a reservation: %w", err)
	}
	name, err := syscall.Getsockname(sock)
	if err != nil {
		return 0, fmt.Errorf("testport: read a reservation's port: %w", err)
	}
	switch bound := name.(type) {
	case *syscall.SockaddrInet4:
		return bound.Port, nil
	case *syscall.SockaddrInet6:
		return bound.Port, nil
	}
	return 0, fmt.Errorf("testport: a reservation is bound to %T, not a TCP address", name)
}
