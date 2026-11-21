package main

import (
	"errors"
	"fmt"
	"io/fs"
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
// waits on for the descriptor. A recvmsg bounded by SO_RCVTIMEO fails with EINTR on any handled
// signal (signal(7)). The Go runtime sends its own threads SIGURG to preempt goroutines, and this
// test sends that one.
//
// The test holds the server's send until the receiving thread sleeps in recvmsg, sends that thread
// SIGURG, and then waits until the receive returned or the thread slept again and sleeps in recvmsg
// now, which a receive that returned does not do. Only then does the server send. Each wait ends on
// one of those events, under testwait's hang guard. The receive's bound is
// testwait.Bounded(t, time.Hour, time.Second): the test binary's deadline less a second, and at most
// an hour, so it runs out only after the test binary's own timeout would have.
func TestASignalDuringTheReceiveIsNoFailure(t *testing.T) {
	requireThreadRecord(t)
	name, accepted := serveOnce(t)
	r := startReceive(t, name, testwait.Bounded(t, time.Hour, time.Second))
	var conn *net.UnixConn
	select {
	case c, ok := <-accepted:
		if !ok {
			t.Fatal("the test's socket failed to accept the receive's connection")
		}
		conn = c
	case <-r.returned:
		t.Fatalf("the receive returned before it connected: descriptor %d, %v", r.fd, r.err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	before, ok := waitInRecvmsg(t, r, -1)
	if !ok {
		t.Fatalf("the receive returned before the test signalled it: descriptor %d, %v", r.fd, r.err)
	}
	signal(t, r.tid)
	if _, ok := waitInRecvmsg(t, r, before); !ok {
		t.Fatalf("the receive returned when a signal interrupted it, before the server sent: descriptor %d, %v", r.fd, r.err)
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
	testwait.Eventually(t, "the receive to return once the server sent", r.ended)
	if r.err != nil {
		t.Fatalf("the receive failed after the server sent: %v", r.err)
	}
	_ = syscall.Close(r.fd)
}

// A receive whose server accepts and never sends ends with EAGAIN once its bound has passed, however
// many signals interrupt it, and each of its tries is bounded by the time left of that one bound.
// The test sends the receiving thread SIGURG each time it sleeps in recvmsg again, until the receive
// returns, and boundTry records each try's receive timeout. The verdict reads those facts:
//   - Each try's timeout is at most the time left of the bound (checkTries). A receive that bounds a
//     try by more, as one that takes a later deadline or gives each try a whole bound, fails it.
//   - The receive ends with EAGAIN. One that sets no receive timeout never returns, since the kernel
//     restarts its recvmsg after each signal, and testwait's hang guard fails it.
//   - A signal ended a try and the receive tried again: boundTry saw a second try. A receive the test
//     did not interrupt before its bound passed proves nothing of that, so the row starts another; a
//     row that sees no receive tried again before its hang guard fails, naming that.
func TestASignalledReceiveEndsAtItsBound(t *testing.T) {
	requireThreadRecord(t)
	const bound = time.Second
	var last *signalledReceive
	// Registered before serveOnce, so it runs after serveOnce's cleanup closed the socket, which
	// ends the last receive whatever it does; only then does it take the hook away.
	t.Cleanup(func() {
		if last != nil && last.r != nil {
			<-last.r.returned
		}
		boundTry = nil
	})
	// The kernel completes each connection in the listener's backlog, so a receive waits on a
	// connection the server holds and never writes to, accepted or not.
	name, _ := serveOnce(t)
	giveUp := time.Now().Add(testwait.Bounded(t, time.Minute, time.Second))
	for receives := 1; ; receives++ {
		last = &signalledReceive{}
		signalReceive(t, last, name, bound, giveUp)
		if !last.ended && last.signals > 0 {
			t.Fatalf("receive %d still waited after %d signals, long past its bound of %s: its tries are not bounded by the time left of that bound", receives, last.signals, bound)
		}
		if last.ended {
			t.Logf("receive %d: %d signals, %d tries", receives, last.signals, len(last.tries))
			checkTries(t, last, bound)
			if !errors.Is(last.r.err, syscall.EAGAIN) {
				t.Fatalf("receive %d returned descriptor %d, %v; want EAGAIN once its bound of %s passed", receives, last.r.fd, last.r.err, bound)
			}
			if len(last.tries) >= 2 {
				return
			}
		}
		if !last.ended || !time.Now().Before(giveUp) {
			t.Fatalf("no receive of %d was tried again after a signal ended a try, so the row proves nothing of the bound under signals", receives)
		}
	}
}

// signalledReceive is one receive of the bound row: when the test started it, its result, the tries
// boundTry recorded, the signals the test sent it, and whether it returned before the row gave up.
type signalledReceive struct {
	start   time.Time
	r       *receiving
	tries   []boundedTry
	signals int
	ended   bool
}

// boundedTry is one try of a receive as boundTry saw it: its receive timeout, and when it was set.
type boundedTry struct {
	timeout time.Duration
	at      time.Time
}

// signalReceive starts receive(name, bound) as s, with boundTry recording its tries, and sends its
// thread SIGURG each time it sleeps in recvmsg again, until it returns or giveUp passes.
func signalReceive(t *testing.T, s *signalledReceive, name string, bound time.Duration, giveUp time.Time) {
	t.Helper()
	s.start = time.Now()
	// The receiving thread appends to tries; the test reads them only after the receive returned.
	boundTry = func(timeout syscall.Timeval) {
		s.tries = append(s.tries, boundedTry{timeout: time.Duration(timeout.Nano()), at: time.Now()})
	}
	s.r = startReceive(t, name, bound)
	after := -1
	s.ended = testwait.Holds(time.Until(giveUp), testwait.Poll, func() bool {
		if s.r.ended() {
			return true
		}
		switches, again := sleepsInRecvmsgAgain(t, s.r.tid, after)
		if !again {
			return false
		}
		signal(t, s.r.tid)
		s.signals, after = s.signals+1, switches
		return false
	})
}

// checkTries fails the row when a try of s was bounded by more than the time left of its bound. The
// receive took its deadline before it bounded its first try, so that deadline is no later than the
// first try's record plus the bound; and it read each try's time left after s started and after
// the try before was recorded. A try's timeout is that time left rounded up to the microsecond.
func checkTries(t *testing.T, s *signalledReceive, bound time.Duration) {
	t.Helper()
	if len(s.tries) == 0 {
		t.Fatalf("the receive returned descriptor %d, %v before boundTry saw it bound a try", s.r.fd, s.r.err)
	}
	latest := s.tries[0].at.Add(bound)
	from := s.start
	over, first := 0, ""
	for i, try := range s.tries {
		if most := latest.Sub(from) + time.Microsecond; try.timeout > most {
			if over++; over == 1 {
				first = fmt.Sprintf("try %d was bounded by %s, past the %s at most left", i+1, try.timeout, most)
			}
		}
		from = try.at
	}
	if over > 0 {
		t.Fatalf("%d of %d tries were bounded by more than the time left of the receive's bound of %s; the first: %s", over, len(s.tries), bound, first)
	}
}

// Where the kernel keeps no record of what each thread is asleep in, as gVisor keeps none, the check
// the rows make first answers errNoThreadRecord, which requireThreadRecord turns into a skip outside
// CI and a failure under it, and not a read error that fails the row.
func TestAKernelThatKeepsNoThreadRecordIsNamed(t *testing.T) {
	previous := threadRecord
	threadRecord = "no-such-record"
	t.Cleanup(func() { threadRecord = previous })
	if err := threadRecordKept(); !errors.Is(err, errNoThreadRecord) {
		t.Fatalf("threadRecordKept = %v, want errNoThreadRecord", err)
	}
}

// threadRecord is the file under /proc/self/task/<tid> where the kernel writes what that thread is
// asleep in. It is a variable for TestAKernelThatKeepsNoThreadRecordIsNamed, which points it at a
// name no thread has, to stand for a kernel that keeps none.
var threadRecord = "syscall"

// errNoThreadRecord is the answer where the kernel keeps no record of what each thread of this
// process is asleep in. The rows read that record to see the receiving thread sleep in recvmsg.
var errNoThreadRecord = errors.New("the kernel keeps no record of what each thread of this process is asleep in (/proc/self/task/<tid>/syscall), as gVisor keeps none")

// threadRecordPath is the record of thread tid of this process.
func threadRecordPath(tid int) string {
	return fmt.Sprintf("/proc/self/task/%d/%s", tid, threadRecord)
}

// threadRecordKept reads the record of the thread it runs on: nil where the kernel keeps it,
// errNoThreadRecord where the file does not exist, and the read's own error otherwise.
func threadRecordKept() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_, err := os.ReadFile(threadRecordPath(syscall.Gettid()))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %v", errNoThreadRecord, err)
	}
	return err
}

// requireThreadRecord stops a row where the kernel keeps no thread record, under the rule the
// daemon's launcher tests follow for a test this machine cannot run (skipOutsideCI): under
// GITHUB_ACTIONS it fails, since CI's runners keep the record and a runner that keeps none is
// broken, never a green job with the proof gone from it; anywhere else it skips, naming why. Any
// other failure to read the record fails the row: then it has no answer, not a clean one. This
// module cannot import the daemon's test helpers, so the rule is written here.
func requireThreadRecord(t *testing.T) {
	t.Helper()
	err := threadRecordKept()
	switch {
	case err == nil:
		return
	case errors.Is(err, errNoThreadRecord):
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("%v: CI's runners keep it, so this runner is broken", err)
		}
		t.Skipf("%v: this row cannot see the receiving thread sleep in recvmsg here", err)
	default:
		t.Fatalf("cannot tell whether the kernel keeps a record of what each thread is asleep in: %v", err)
	}
}

// receiving is a receive running on a goroutine locked to its own thread. fd and err are written
// before returned closes and read only after it has.
type receiving struct {
	tid      int
	returned chan struct{}
	fd       int
	err      error
}

// startReceive runs receive(name, bound) on a goroutine locked to its thread, and returns once it
// knows that thread's id.
func startReceive(t *testing.T, name string, bound time.Duration) *receiving {
	t.Helper()
	r := &receiving{returned: make(chan struct{})}
	tids := make(chan int, 1)
	go func() {
		defer close(r.returned)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- syscall.Gettid()
		r.fd, r.err = receive(name, bound)
	}()
	r.tid = <-tids
	return r
}

// ended reports whether the receive has returned.
func (r *receiving) ended() bool {
	select {
	case <-r.returned:
		return true
	default:
		return false
	}
}

// serveOnce serves a fresh abstract unix socket and returns its name and the one connection it
// accepts. When the test ends it closes the socket and any connection no row took.
func serveOnce(t *testing.T) (string, <-chan *net.UnixConn) {
	t.Helper()
	name := fmt.Sprintf("@legion-heldport-receive-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		t.Fatalf("serve the abstract socket %s: %v", name, err)
	}
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		defer close(accepted)
		if conn, err := listener.AcceptUnix(); err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for conn := range accepted {
			_ = conn.Close()
		}
	})
	return name, accepted
}

// signal sends thread tid of this process SIGURG, the signal the Go runtime preempts with.
func signal(t *testing.T, tid int) {
	t.Helper()
	if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGURG); err != nil {
		t.Fatalf("signal the receiving thread %d: %v", tid, err)
	}
}

// waitInRecvmsg waits until r's thread sleeps in recvmsg again (sleepsInRecvmsgAgain) and returns
// its voluntary switches and true, or until the receive returns, and then returns false.
// testwait's bound is only the hang guard of a thread that does neither.
func waitInRecvmsg(t *testing.T, r *receiving, after int) (int, bool) {
	t.Helper()
	switches, again := 0, false
	what := fmt.Sprintf("thread %d to sleep in recvmsg with more than %d voluntary switches, or the receive to return", r.tid, after)
	testwait.Eventually(t, what, func() bool {
		if r.ended() {
			return true
		}
		switches, again = sleepsInRecvmsgAgain(t, r.tid, after)
		return again
	})
	return switches, again
}

// sleepsInRecvmsgAgain reports whether thread tid of this process sleeps in recvmsg now with more
// voluntary context switches than after, with that count. Every sleep raises the count, one in
// recvmsg or any other (a futex, a page fault), so a count above after means the thread slept at
// least once since after was read, and sleeps in recvmsg now.
func sleepsInRecvmsgAgain(t *testing.T, tid, after int) (int, bool) {
	t.Helper()
	in, err := inRecvmsg(tid)
	if err != nil {
		t.Fatal(err)
	}
	if !in {
		return 0, false
	}
	switches, err := voluntarySwitches(tid)
	if err != nil {
		t.Fatal(err)
	}
	return switches, switches > after
}

// inRecvmsg reports whether thread tid of this process sleeps in recvmsg: the kernel's
// /proc/<pid>/task/<tid>/syscall starts with the number of the system call a thread is blocked in,
// and reads "running" or a negative number otherwise.
func inRecvmsg(tid int) (bool, error) {
	raw, err := os.ReadFile(threadRecordPath(tid))
	if err != nil {
		return false, fmt.Errorf("read thread %d's system call: %w", tid, err)
	}
	return strings.HasPrefix(string(raw), strconv.Itoa(syscall.SYS_RECVMSG)+" "), nil
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
