package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A signal that interrupts the receive while the server has not sent yet is no failure: the wrapper
// waits on for the descriptor. The Go runtime's own preemption signal, SIGURG, is the one that hits
// it under load, and a recvmsg bounded by SO_RCVTIMEO fails with EINTR on any handled signal.
//
// The test holds the server's send until the receiving thread sleeps in recvmsg, sends that thread
// SIGURG, and then waits until the thread either gave up (the receive returned) or slept in recvmsg
// again, which only a receive that tried again does. Only then does the server send. Each wait ends
// on one of those events, and the receive's own bound is the test binary's deadline, so no span the
// clock measures decides the verdict.
func TestASignalDuringTheReceiveIsNoFailure(t *testing.T) {
	name := fmt.Sprintf("@legion-heldport-receive-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		t.Fatalf("serve the abstract socket %s: %v", name, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	// received is written before returned closes and read only after it has.
	var received struct {
		fd  int
		err error
	}
	returned := make(chan struct{})
	tids := make(chan int, 1)
	bound := testwait.Bounded(t, time.Hour, time.Second)
	go func() {
		defer close(returned)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- syscall.Gettid()
		received.fd, received.err = receive(name, bound)
	}()
	tid := <-tids
	var conn *net.UnixConn
	select {
	case c, ok := <-accepted:
		if !ok {
			t.Fatal("the test's socket failed to accept the receive's connection")
		}
		conn = c
	case <-returned:
		t.Fatalf("the receive returned before it connected: descriptor %d, %v", received.fd, received.err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	before, ok := waitInRecvmsg(t, tid, -1, returned)
	if !ok {
		t.Fatalf("the receive returned before the test signalled it: descriptor %d, %v", received.fd, received.err)
	}
	if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGURG); err != nil {
		t.Fatalf("signal the receiving thread %d: %v", tid, err)
	}
	if _, ok := waitInRecvmsg(t, tid, before, returned); !ok {
		t.Fatalf("the receive returned when a signal interrupted it, before the server sent: descriptor %d, %v", received.fd, received.err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if _, _, err := conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(int(read.Fd())), nil); err != nil {
		t.Fatalf("send the descriptor: %v", err)
	}
	testwait.Eventually(t, "the receive to return once the server sent", func() bool {
		select {
		case <-returned:
			return true
		default:
			return false
		}
	})
	if received.err != nil {
		t.Fatalf("the receive failed after the server sent: %v", received.err)
	}
	_ = syscall.Close(received.fd)
}

// waitInRecvmsg waits until thread tid of this process sleeps in recvmsg with more voluntary
// context switches than after, and returns that count and true, or until returned closes, and then
// returns false. Each sleep in a system call is one voluntary switch, so a count above after means
// the thread slept in recvmsg again since after was read. testwait's bound is only the hang guard
// of a thread that does neither.
func waitInRecvmsg(t *testing.T, tid, after int, returned <-chan struct{}) (int, bool) {
	t.Helper()
	switches, slept := 0, false
	what := fmt.Sprintf("thread %d to sleep in recvmsg with more than %d voluntary switches, or the receive to return", tid, after)
	testwait.Eventually(t, what, func() bool {
		select {
		case <-returned:
			return true
		default:
		}
		in, err := inRecvmsg(tid)
		if err != nil {
			t.Fatal(err)
		}
		if !in {
			return false
		}
		n, err := voluntarySwitches(tid)
		if err != nil {
			t.Fatal(err)
		}
		switches, slept = n, n > after
		return slept
	})
	return switches, slept
}

func inRecvmsg(tid int) (bool, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/syscall", tid))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // the thread ended: the receive returned
	}
	if err != nil {
		return false, fmt.Errorf("read thread %d's system call: %w", tid, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return false, nil
	}
	number, err := strconv.Atoi(fields[0])
	return err == nil && number == syscall.SYS_RECVMSG, nil
}

func voluntarySwitches(tid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/status", tid))
	if err != nil {
		return 0, fmt.Errorf("read thread %d's status: %w", tid, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "voluntary_ctxt_switches:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return 0, fmt.Errorf("thread %d's status line %q: %w", tid, line, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("thread %d's status has no voluntary_ctxt_switches line", tid)
}
