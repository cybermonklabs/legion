package testenvoy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A reserved port takes a server's bind as Go's listeners make it, at its first start and again
// after its exit, and between the two it stays held: a bind without SO_REUSEADDR is refused on
// 127.0.0.1 and on ::, and a dial is refused, as a dial of a stopped server is. The plain-bind rows
// fail when the port is let go of with the server, as a port the kernel chose for the server and
// freed at its exit is (the rig's nats-server before ReservePort); the 127.0.0.1 row alone when the
// reservation leaves IPV6_V6ONLY on; and the first start when the reservation drops SO_REUSEADDR or
// listens.
func TestAReservedPortTakesAServerAndItsRestartAndNoOtherSocketBetween(t *testing.T) {
	port := ReservePort(t)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	server, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("a server's bind of the reserved %s at its first start: %v", address, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	for _, other := range []struct {
		name   string
		family int
		addr   syscall.Sockaddr
	}{
		{"127.0.0.1", syscall.AF_INET, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}},
		{"::", syscall.AF_INET6, &syscall.SockaddrInet6{Port: port}},
	} {
		fd, err := syscall.Socket(other.family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Bind(fd, other.addr)
		_ = syscall.Close(fd)
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("a bind of %s:%d without SO_REUSEADDR while the server is down = %v, want EADDRINUSE", other.name, port, err)
		}
	}
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a dial of %s while the server is down = %v, want ECONNREFUSED", address, err)
	}

	server, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("a server's bind of the reserved %s at its restart: %v", address, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

// The reservation's check reads a missing net.ipv4.ip_autobind_reuse as 0 and refuses a 1 and a
// setting it cannot read. The absent row fails when a missing file is an error; the 1 and
// unreadable rows when the check accepts whatever it reads.
func TestTheReservationsCheckReadsAMissingAutobindSettingAsZero(t *testing.T) {
	dir := t.TempDir()
	setting := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, row := range []struct {
		name, path, refusal string
	}{
		{"absent", filepath.Join(dir, "absent"), ""},
		{"0", setting("zero", "0\n"), ""},
		{"1", setting("one", "1\n"), `net.ipv4.ip_autobind_reuse is "1"`},
		{"unreadable", dir, "read net.ipv4.ip_autobind_reuse"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := autobindReuseOff(row.path)
			switch {
			case row.refusal == "" && err != nil:
				t.Fatalf("the setting at %s: %v, want none", row.path, err)
			case row.refusal != "" && (err == nil || !strings.Contains(err.Error(), row.refusal)):
				t.Fatalf("the setting at %s: %v, want an error naming %q", row.path, err, row.refusal)
			}
		})
	}
}
