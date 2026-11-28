package testnats

import (
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// noSocketQueues is why RequireSocketQueues skips a row, or fails it in CI.
const noSocketQueues = "this kernel reports no socket queues in /proc/net/tcp, so a stalled write cannot be proven"

// RequireSocketQueues is for a row that calls HoldStalledWrite, which reads in /proc/net/tcp that its
// write is stalled for good (stalledForGood). When this kernel reports no socket queues there, as
// gVisor's does not, it skips the row outside CI and fails it under GITHUB_ACTIONS, on one line
// naming why: the job that must run these rows runs on a kernel that reports them, so a CI runner
// that does not is broken, never a green job with the proof gone from it. The rule is the daemon's
// role-namespace tests' (skipOutsideCI). A row calls it first, on the test's own goroutine, since
// HoldStalledWrite may run on a hook's goroutine, where a skip would end the hook rather than the
// test. The kernel is read once per test binary (probeSocketQueues).
func RequireSocketQueues(t *testing.T) {
	t.Helper()
	socketQueues.once.Do(func() { socketQueues.reported, socketQueues.err = probeSocketQueues(t) })
	if socketQueues.err != nil {
		t.Fatalf("read whether this kernel reports socket queues in /proc/net/tcp: %v", socketQueues.err)
	}
	if socketQueues.reported {
		return
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal(noSocketQueues)
	}
	t.Skip(noSocketQueues)
}

// socketQueues is probeSocketQueues' one answer for the test binary.
var socketQueues struct {
	once     sync.Once
	reported bool
	err      error
}

// socketQueuesSpare is what probeSocketQueues leaves of the test binary's deadline for the failed
// test's cleanups.
const socketQueuesSpare = 30 * time.Second

// probeSocketQueues fills a loopback connection whose server end never reads, and reads the client
// socket's line in /proc/net/tcp while its writer is parked on it (writeParked) from before the read
// until after it, with no write returned: the socket's send queue is then full. It reports true when
// the line shows bytes to send, and false when it shows none, or when no line names the socket. It
// reads the state of the writer and the kernel, and no time; its one bound is the test binary's
// deadline less socketQueuesSpare, a guard against a socket that always takes the writes.
func probeSocketQueues(t *testing.T) (bool, error) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
		close(accepted)
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", listener.Addr(), err)
	}
	defer func() { _ = client.Close() }()
	server, ok := <-accepted
	if !ok {
		return false, fmt.Errorf("accept on %s failed", listener.Addr())
	}
	defer func() { _ = server.Close() }()
	writer := make(chan uint64, 1)
	var returned atomic.Int64
	go func() {
		writer <- goroutineID()
		payload := make([]byte, 64<<10)
		for {
			if _, err := client.Write(payload); err != nil {
				return
			}
			returned.Add(1)
		}
	}()
	id := <-writer
	bound, err := testwait.Left(t, socketQueuesSpare)
	if err != nil {
		return false, err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return false, fmt.Errorf("the listener's address %s is not a TCP address", listener.Addr())
	}
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		before := returned.Load()
		if !writeParked(id) {
			continue
		}
		sockets, err := clientSockets(addr)
		if err != nil {
			return false, err
		}
		if !writeParked(id) || returned.Load() != before {
			continue
		}
		for _, socket := range sockets {
			if socket.local == client.LocalAddr().String() {
				return socket.sendQueue > 0, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("the writer to a loopback connection nobody reads was not parked on its socket %s before the test binary's deadline", socketQueuesSpare)
}
