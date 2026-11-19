package testenvoy

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// autobindReuseSetting is where Linux keeps net.ipv4.ip_autobind_reuse.
const autobindReuseSetting = "/proc/sys/net/ipv4/ip_autobind_reuse"

// ReservePort returns a TCP port the kernel chose, reserved until t ends for a server the test
// starts on it and starts again on it after that server's exit: a socket of the test's own, bound
// to the port on every IPv4 and IPv6 address with SO_REUSEADDR, that never listens. A port the
// test let go of, or one a server freed by exiting, is the kernel's to give to the next bind to
// port 0 or the next connect of any program on the machine, and a server started on it again then
// fails with `address already in use`: the rig's nats-server did so at its restart after w4's NATS
// outage, when an outbound connection had been given its port while it was down. A reserved port
// is given to no other socket, on Linux, and still takes the server's bind and every rebind:
//
//   - A Go server, whose listener sets SO_REUSEADDR (net/sockopt_linux.go,
//     setDefaultListenerSockopts), binds beside the reservation, since the kernel refuses a bind
//     beside a socket that also sets SO_REUSEADDR only when that socket listens
//     (net/ipv4/inet_connection_sock.c, inet_bind_conflict). nats-server, the listener and the
//     daemon are each such a server.
//   - A bind to port 0 passes over a port a socket is bound to (inet_csk_find_open_port), and a
//     connect passes over a port a bind made (net/ipv4/inet_hashtables.c, __inet_hash_connect).
//   - A dial of the port is refused while no server listens there, as a dial of a stopped server's
//     closed port is.
//
// The second rule holds only while net.ipv4.ip_autobind_reuse is 0, its default, so ReservePort
// fails the test on a host where it reads otherwise, and reads a host with no such setting as 0: a
// kernel before Linux 5.7, which added it, never made the search that shares a bound port, and
// gVisor, which Legion's worker pods run, has no such file and keeps the reservation.
// docs/solutions/testing/ has the rule this follows ("A server a test starts serves on a port the
// kernel chose and something holds until it serves"), and packages/envoy's reservePorts
// (infra/gcp/clusteredhub_live_test.go) is the same reservation in the Envoy module.
func ReservePort(t testing.TB) int {
	t.Helper()
	if err := autobindReuseOff(autobindReuseSetting); err != nil {
		t.Fatalf("testenvoy: %v", err)
	}
	port, err := reserveDualStack(t)
	if err != nil {
		t.Fatalf("testenvoy: reserve a port for a server the test restarts: %v", err)
	}
	return port
}

// reserveDualStack binds a socket of its own to [::]:0 with IPV6_V6ONLY off, so it holds the port
// on every IPv4 address too, and with SO_REUSEADDR, closes it when t ends, and answers its port.
func reserveDualStack(t testing.TB) (int, error) {
	// Under ForkLock, so no child started between the two calls inherits the descriptor.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return 0, fmt.Errorf("make the reservation's socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("clear IPV6_V6ONLY on the reservation: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("set SO_REUSEADDR on the reservation: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet6{}); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("bind the reservation: %w", err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("read the reservation's port: %w", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return name.(*syscall.SockaddrInet6).Port, nil
}

// autobindReuseOff answers nil when the setting at path reads 0 or path does not exist, and an
// error for any other value or a read that fails.
func autobindReuseOff(path string) error {
	value, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read net.ipv4.ip_autobind_reuse, which a port reservation depends on: %w", err)
	}
	if setting := strings.TrimSpace(string(value)); setting != "0" {
		return fmt.Errorf("net.ipv4.ip_autobind_reuse is %q; it must be 0 for a reserved port to be given to no other socket (ReservePort)", setting)
	}
	return nil
}
