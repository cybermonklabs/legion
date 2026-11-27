package testnats

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A gate's port must lie below the machine's ephemeral range, where no :0 bind can take it while
// the gate is closed. The range is fed in as text, each kernel's answer in turn, so the band is
// pinned whatever range the machine running this test has: a CI runner's default range would hide
// a band that lands inside the 16000-65535 a Legion worker pod (gVisor) has.
func TestTheGateBandLiesBelowTheEphemeralRangeItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		low   int
		first int
	}{
		{name: "gVisor's range in a Legion worker pod", text: "16000\t65535\n", low: 16000, first: 15000},
		{name: "Linux's default range", text: "32768\t60999\n", low: 32768, first: 31768},
		{name: "text that is no range", text: "not a range\n", low: 16000, first: 15000},
		{name: "empty text", text: "", low: 16000, first: 15000},
		{name: "the lowest range that leaves a band", text: "3048\t65535\n", low: 3048, first: 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, count, err := gatePorts(tc.text)
			if err != nil {
				t.Fatalf("gatePorts(%q) = %v, want a band", tc.text, err)
			}
			if first != tc.first || count != 1000 {
				t.Errorf("gatePorts(%q) = %d ports from %d, want 1000 from %d", tc.text, count, first, tc.first)
			}
			if last := first + count - 1; last >= tc.low {
				t.Errorf("gatePorts(%q) = %d-%d, which reaches the range starting at %d", tc.text, first, last, tc.low)
			}
		})
	}
	t.Run("a range too low to leave a band", func(t *testing.T) {
		first, count, err := gatePorts("1024\t65535\n")
		if err == nil {
			t.Fatalf("gatePorts = %d ports from %d, want a refusal", count, first)
		}
		if !strings.Contains(err.Error(), "1024") {
			t.Errorf("refusal %v does not name the range's first port, 1024", err)
		}
	})
}

// StartGate takes its port from just below this machine's ephemeral range, which it reads from
// ephemeralRangePath. The test reads that file itself, as the rig does, and checks the ports of 20
// gates against it: a StartGate that stopped reading the file and took a band of its own fails
// here on every machine whose range does not start where that band ends.
func TestEveryGatePortLiesJustBelowThisMachinesEphemeralRange(t *testing.T) {
	low := fallbackEphemeralLow
	text, err := os.ReadFile(ephemeralRangePath)
	if fields := strings.Fields(string(text)); err == nil && len(fields) == 2 {
		first, firstErr := strconv.Atoi(fields[0])
		last, lastErr := strconv.Atoi(fields[1])
		if firstErr == nil && lastErr == nil && 0 < first && first <= last && last <= 65535 {
			low = first
		}
	}
	server, _ := Start(t)
	for range 20 {
		gate := StartGate(t, server)
		address, err := url.Parse(gate.URL())
		if err != nil {
			t.Fatalf("parse the gate's URL %s: %v", gate.URL(), err)
		}
		port, err := strconv.Atoi(address.Port())
		if err != nil {
			t.Fatalf("read the port of the gate's URL %s: %v", gate.URL(), err)
		}
		if port >= low || port < low-gatePortCount {
			t.Errorf("a gate took port %d, want one of the %d ports below %d, the first port of this machine's ephemeral range (%s holds %q)", port, gatePortCount, low, ephemeralRangePath, text)
		}
	}
}

// A closed gate keeps its port. While it is closed a dial of its address is refused, as a client's
// failed reconnect needs, a program that binds the port without SO_REUSEADDR is refused, and no
// other gate can take the port; Open then listens on the same address again and a client connects
// through it. Each row is the control of one hold: with the reservation gone the bind succeeds and
// Open fails, with the claim gone the second gate's hold succeeds, and with the reservation
// listening the dial succeeds.
func TestAClosedGateKeepsItsPort(t *testing.T) {
	server, _ := Start(t)
	gate := StartGate(t, server)
	address, err := net.ResolveTCPAddr("tcp", gate.address)
	if err != nil {
		t.Fatalf("resolve the gate's address %s: %v", gate.address, err)
	}
	gate.Close()

	conn, err := (&net.Dialer{}).DialContext(testwait.Context(t), "tcp", gate.address)
	if err == nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a dial of the closed gate at %s: %v, want connection refused", gate.address, err)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("open the other program's socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: address.Port, Addr: [4]byte(address.IP.To4())})
	if err == nil {
		// The port was free: hold it listening, as a program that took it would, so the Open below
		// meets it.
		_ = syscall.Listen(fd, 16)
		t.Errorf("another program bound the closed gate's port %d", address.Port)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("another program's bind of the closed gate's port %d: %v, want address already in use", address.Port, err)
	}

	if claim, reservation, err := holdGatePort(address.IP, address.Port); err == nil {
		_ = syscall.Close(reservation)
		_ = claim.Close()
		t.Errorf("a second gate held the closed gate's port %d", address.Port)
	}

	gate.Open()
	testwait.Until(t, fmt.Sprintf("a connection through the reopened gate at %s", gate.URL()), func() (dialAttempt, bool) {
		nc, err := natsgo.Connect(gate.URL(), natsgo.NoReconnect())
		if err == nil {
			nc.Close()
		}
		return dialAttempt{conn: nc, err: err}, err == nil
	})
}
