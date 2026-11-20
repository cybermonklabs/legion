//go:build linux

package storetest

import (
	"bytes"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xidThreadClocksAvailable reports whether this process can judge a window's gaps: whether it can
// read an OS thread's scheduler clocks, and whether it can hold a thread out of NUMA balancing
// (xidHoldThread). Linux does not promise either: gVisor, for example, exposes no
// /proc/thread-self/schedstat, and a container's seccomp profile may refuse set_mempolicy. The
// first caller probes once while locked to an OS thread, as a marked transaction's goroutine is;
// every later caller reads that same fact, so a kernel that cannot give the rig either follows
// the race build and judges window shape alone.
//
// xidThreadClockAvailability is a pointer so its readers and its once can be replaced together by
// the test that proves an unreadable clock, or a thread NUMA balancing cannot be kept off, judges
// no gap. Tests in this package do not run in parallel.
var xidThreadClockAvailability = &xidThreadClockProbe{read: readXidThreadClock, hold: xidHoldThread}

type xidThreadClockProbe struct {
	once      sync.Once
	read      func() xidThreadClock
	hold      func() (release func(), held bool)
	available bool
}

func xidThreadClocksAvailable() bool {
	probe := xidThreadClockAvailability
	probe.once.Do(func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if probe.read().thread == 0 {
			return
		}
		release, held := probe.hold()
		release()
		probe.available = held
	})
	return probe.available
}

// xidThread is the calling OS thread's id.
func xidThread() int { return unix.Gettid() }

// xidSchedstatPath is /proc/thread-self/schedstat as openat takes it, NUL-terminated, built once
// so that no read of a thread's clocks allocates (readXidThreadClock). xidAtCWD is AT_FDCWD as a
// variable, so its negative value converts to the syscall's word.
var (
	xidSchedstatPath     = []byte("/proc/thread-self/schedstat\x00")
	xidAtCWD         int = unix.AT_FDCWD
)

// readXidThreadClock reads the calling OS thread's scheduler clocks: the CPU time it has run
// (CLOCK_THREAD_CPUTIME_ID, which the kernel brings up to date as it is read), and the time it has
// waited runnable on a run queue for a CPU (run_delay, the second field of
// /proc/thread-self/schedstat). The kernel adds a wait to run_delay when the thread gets a CPU
// back, so a reading the thread takes while it runs holds every wait it has had. It answers the
// zero clock when either cannot be read, or when the goroutine moved to another thread between
// the two reads.
//
// It allocates nothing, so it touches no memory the process has not used before: the file is
// opened by a path built once, read into a buffer on the stack and parsed in place. The tracer
// reads the clocks at a gap's two ends, and while the collector is held off (runWithoutCollector)
// an allocation there would land on a page the process never touched - a first touch that, on a
// loaded machine, costs milliseconds of the thread's CPU in the kernel (runWithoutCollector).
func readXidThreadClock() xidThreadClock {
	thread := unix.Gettid()
	var cpu unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu); err != nil {
		return xidThreadClock{}
	}
	fd, _, errno := unix.Syscall6(unix.SYS_OPENAT, uintptr(xidAtCWD),
		uintptr(unsafe.Pointer(&xidSchedstatPath[0])), uintptr(unix.O_RDONLY|unix.O_CLOEXEC), 0, 0, 0)
	if errno != 0 {
		return xidThreadClock{}
	}
	var raw [64]byte
	n, _, errno := unix.Syscall(unix.SYS_READ, fd, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)))
	unix.Syscall(unix.SYS_CLOSE, fd, 0, 0)
	if errno != 0 || unix.Gettid() != thread {
		return xidThreadClock{}
	}
	waited, ok := xidSecondField(raw[:n])
	if !ok {
		return xidThreadClock{}
	}
	return xidThreadClock{thread: thread, ran: time.Duration(cpu.Nano()), waited: time.Duration(waited)}
}

// xidSecondField parses the second space-separated decimal field of a schedstat line, in place.
func xidSecondField(line []byte) (int64, bool) {
	first := bytes.IndexByte(line, ' ')
	if first < 0 {
		return 0, false
	}
	var value int64
	digits := 0
	for _, c := range line[first+1:] {
		if c < '0' || c > '9' {
			break
		}
		value = value*10 + int64(c-'0')
		digits++
	}
	return value, digits > 0
}

// The mempolicy modes xidHoldThread sets and reads (linux/mempolicy.h): x/sys/unix names the
// syscalls but not the modes.
const (
	xidMempolicyDefault = 0
	xidMempolicyLocal   = 4
)

// xidNUMABalancing reports whether the kernel balances this machine's memory across NUMA nodes:
// kernel.numa_balancing on, and more than one node online (the online list holds a range or a
// second node, as "0-5" or "0,2" does, where one node reads as "0"). On one node, or with it off,
// no page of the process is ever moved for it.
var xidNUMABalancing = sync.OnceValue(func() bool {
	setting, err := os.ReadFile("/proc/sys/kernel/numa_balancing")
	if err != nil || string(bytes.TrimSpace(setting)) == "0" {
		return false
	}
	online, err := os.ReadFile("/sys/devices/system/node/online")
	return err == nil && bytes.ContainsAny(online, "-,")
})

// xidHoldThread keeps NUMA balancing off the calling OS thread, which must be locked to its
// goroutine, until release: it sets the thread's memory policy to MPOL_LOCAL, which allocates on
// the node the thread runs on, as the default policy does, but without the default's
// migrate-on-fault. With that, a page the thread faults on is not moved to its node, and the
// scan NUMA balancing runs as task work on whichever thread of the process it lands passes over
// every mapping when it lands on this one. Both are the kernel's own work at moments of its own
// choosing, and both would put the time of a TLB shootdown - every CPU the process ran on
// interrupted, waited for in turn - on the thread as CPU time: on a loaded machine, milliseconds
// a gap's clocks would read as the code's. release puts back the policy the thread had.
//
// held is false when the thread cannot be held: NUMA balancing is on and the kernel refused the
// policy. On a machine NUMA balancing leaves alone there is nothing to hold, and it answers true.
func xidHoldThread() (release func(), held bool) {
	if !xidNUMABalancing() {
		return func() {}, true
	}
	var mode int
	var nodes [16]uint64
	if _, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)),
		uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)*64), 0, 0, 0); errno != 0 {
		return func() {}, false
	}
	if _, _, errno := unix.Syscall(unix.SYS_SET_MEMPOLICY, xidMempolicyLocal, 0, 0); errno != 0 {
		return func() {}, false
	}
	return func() {
		if mode == xidMempolicyDefault {
			unix.Syscall(unix.SYS_SET_MEMPOLICY, xidMempolicyDefault, 0, 0)
			return
		}
		unix.Syscall(unix.SYS_SET_MEMPOLICY, uintptr(mode), uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)*64))
	}, true
}
