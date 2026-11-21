// Package testport is a loopback address that refuses every dial for as long as the test that
// asked for it runs. A test that needs a peer that is not there takes one rather than the address
// of a server or listener it closed: a freed port is any process's to take, and on a machine that
// runs other tests beside this one another process took it and answered HTTP.
//
// It is a public package of the envoy module so that the daemon module imports it as it is: Go's
// internal rule keeps the daemon from importing a package under packages/envoy/internal, and a
// copy in the daemon would be a second file to keep equal.
package testport

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// Refused is the address, 127.0.0.1 and a port the kernel chose, of a TCP socket the test holds
// bound and never listening until it ends: every dial to it is refused (`connect: connection
// refused`, syscall.ECONNREFUSED), as a dial to a server that is down is. No other process can bind
// the port meanwhile, with or without SO_REUSEADDR or SO_REUSEPORT, because the held socket sets
// neither, and a port is shared only between sockets that all set one of them.
func Refused(t testing.TB) string {
	t.Helper()
	// Under ForkLock, so no child started between the two calls inherits the descriptor; Go's own
	// net package does the same on a platform without SOCK_CLOEXEC.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("testport: make the socket that holds a refusing port: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("testport: bind a loopback port to refuse dials at: %v", err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("testport: read the refusing port: %v", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(name.(*syscall.SockaddrInet4).Port))
}
