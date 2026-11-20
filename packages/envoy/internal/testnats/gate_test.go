package testnats

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gateRivalVariable, when it names an address, makes the test binary a gate's rival rather than a
// test run (TestMain): it listens on that address as any Go server does (net.Listen sets
// SO_REUSEADDR and not SO_REUSEPORT), prints on one line what its bind met, and when the bind
// succeeded, keeps the port until its standard input ends.
const gateRivalVariable = "TESTNATS_GATE_RIVAL"

// The rival's answers: its bind succeeded, or the kernel refused it because a socket holds the
// address. Any other failure is printed as "error: " and its text.
const (
	rivalBound = "bound"
	rivalInUse = "EADDRINUSE"
)

// runGateRival is the whole run of a rival process, and returns its exit code.
func runGateRival(address string) int {
	listener, err := net.Listen("tcp", address)
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		fmt.Println(rivalInUse)
		return 0
	case err != nil:
		fmt.Printf("error: %v\n", err)
		return 0
	}
	fmt.Println(rivalBound)
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = listener.Close()
	return 0
}

// bindFromAnotherProcess starts this test binary as a rival of address and returns what its bind
// met. A rival that bound keeps the port until the test ends, so a gate that gave its port up
// meets the rival's listener when it opens again. The rival ends when its standard input closes,
// at the test's end.
func bindFromAnotherProcess(t *testing.T, address string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("find this test binary to start a rival: %v", err)
	}
	rival := exec.Command(binary, "-test.run=^$")
	rival.Env = append(os.Environ(), gateRivalVariable+"="+address)
	rival.Stderr = os.Stderr
	stdin, err := rival.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := rival.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := rival.Start(); err != nil {
		t.Fatalf("start the rival of %s: %v", address, err)
	}
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		answer <- strings.TrimSpace(line)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = rival.Wait()
	})
	select {
	case line := <-answer:
		return line
	case <-time.After(30 * time.Second):
		t.Fatalf("the rival of %s said nothing within 30 s", address)
		return ""
	}
}

// dialsNothing fails the test when a dial of address connects. On Linux the kernel answers a dial
// of a port where no socket listens with a reset, so the dial must be refused at once
// (ECONNREFUSED). Darwin drops the dial's SYN without an answer when a socket that does not listen
// holds the port (Gate), so there the dial need only fail.
func dialsNothing(t *testing.T, address, what string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("a dial of %s at %s connected, want it to fail", what, address)
		return
	}
	if runtime.GOOS == "linux" && !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a dial of %s at %s failed with %v, want ECONNREFUSED", what, address, err)
	}
}

// A gate holds its port while it is closed, against a bind from another process too: the other
// process's bind of the gate's address fails, and a dial of the address fails. Once the gate opens
// again, a dial reaches the server behind it, and once it closes again, the port is still the
// gate's. A gate that gave its port up at Close lets the other process take it, and its Open then
// fails, or its clients reach the other process's listener.
func TestAClosedGateHoldsItsPortAgainstAnotherProcess(t *testing.T) {
	server, _ := Start(t)
	gate := StartGate(t, server)
	address := strings.TrimPrefix(gate.URL(), "nats://")

	gate.Close()
	dialsNothing(t, address, "the closed gate")
	if got := bindFromAnotherProcess(t, address); got != rivalInUse {
		t.Errorf("another process's bind of the closed gate's address %s met %q, want %q", address, got, rivalInUse)
	}

	gate.Open()
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		t.Fatalf("dial the open gate at %s: %v", address, err)
	}
	// A NATS server writes its INFO line to each client at once, so reading it shows that the dial
	// reached the server behind the gate and no other listener.
	readFor(t, conn, "INFO ", "the first bytes through the open gate")
	_ = conn.Close()

	gate.Close()
	dialsNothing(t, address, "the gate closed again")
	if got := bindFromAnotherProcess(t, address); got != rivalInUse {
		t.Errorf("another process's bind of the address %s of the gate closed again met %q, want %q", address, got, rivalInUse)
	}
}

// Every gate holds a port of its own while it is closed: 20 gates, each closed, hold 20 different
// ports, and a bind of each fails. A StartGate that handed two gates one port, or whose closed gate
// gave its port up, fails here.
func TestEveryClosedGateHoldsAPortOfItsOwn(t *testing.T) {
	server, _ := Start(t)
	held := map[string]bool{}
	for range 20 {
		gate := StartGate(t, server)
		gate.Close()
		address := strings.TrimPrefix(gate.URL(), "nats://")
		if held[address] {
			t.Errorf("two gates hold %s", address)
		}
		held[address] = true
		listener, err := net.Listen("tcp", address)
		if err == nil {
			_ = listener.Close()
			t.Errorf("a bind of the closed gate's address %s succeeded, want EADDRINUSE", address)
			continue
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("a bind of the closed gate's address %s failed with %v, want EADDRINUSE", address, err)
		}
	}
}
