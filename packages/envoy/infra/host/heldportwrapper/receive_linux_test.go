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
)

// A signal that interrupts the receive while the server has not sent yet is no failure: the wrapper
// waits on for the descriptor. The Go runtime's own preemption signal, SIGURG, is the one that hits
// it under load, and a recvmsg bounded by SO_RCVTIMEO fails with EINTR on any handled signal.
//
// The test holds the server's send until the receiving thread sleeps in recvmsg, sends that thread
// SIGURG, and then waits until the thread either gave up (the receive returned) or slept in recvmsg
// again, which only a receive that tried again does. Only then does the server send.
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

	type result struct {
		fd  int
		err error
	}
	done := make(chan result, 1)
	tids := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- syscall.Gettid()
		fd, err := receive(name)
		done <- result{fd, err}
	}()
	tid := <-tids
	conn, ok := <-accepted
	if !ok {
		t.Fatal("the receive never connected to the test's socket")
	}
	t.Cleanup(func() { _ = conn.Close() })

	before, err := waitInRecvmsg(tid, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGURG); err != nil {
		t.Fatalf("signal the receiving thread %d: %v", tid, err)
	}
	again := make(chan error, 1)
	go func() {
		_, err := waitInRecvmsg(tid, before)
		again <- err
	}()
	select {
	case r := <-done:
		t.Fatalf("the receive returned when a signal interrupted it, before the server sent: descriptor %d, %v", r.fd, r.err)
	case err := <-again:
		if err != nil {
			t.Fatal(err)
		}
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
	r := <-done
	if r.err != nil {
		t.Fatalf("the receive failed after the server sent: %v", r.err)
	}
	_ = syscall.Close(r.fd)
}

// waitInRecvmsg waits until thread tid of this process sleeps in recvmsg with more voluntary
// context switches than after, and returns that count. Each sleep in a system call is one voluntary
// switch, so a count above after means the thread slept in recvmsg again since after was read. It
// gives up after half the receive's own bound, so a receive that returned cannot hold it longer.
func waitInRecvmsg(tid, after int) (int, error) {
	deadline := time.Now().Add(receiveTimeout / 2)
	for time.Now().Before(deadline) {
		in, err := inRecvmsg(tid)
		if err != nil {
			return 0, err
		}
		if in {
			switches, err := voluntarySwitches(tid)
			if err != nil {
				return 0, err
			}
			if switches > after {
				return switches, nil
			}
		}
		time.Sleep(time.Millisecond)
	}
	return 0, fmt.Errorf("thread %d never slept in recvmsg with more than %d voluntary switches within %s", tid, after, receiveTimeout/2)
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
