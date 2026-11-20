//go:build linux

package storetest

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xidThread is the calling OS thread's id.
func xidThread() int { return unix.Gettid() }

// xidSchedstatPath is /proc/thread-self/schedstat as openat takes it, NUL-terminated, built once
// so that no read of a thread's clocks allocates (readXidThreadClock). xidAtCWD is AT_FDCWD as a
// variable, so its negative value converts to the syscall's word.
var (
	xidSchedstatPath     = []byte("/proc/thread-self/schedstat\x00")
	xidAtCWD         int = unix.AT_FDCWD
)

// xidClockTries is how many times readXidThreadClock tries to read the thread's clocks around its
// stamp without the thread being switched out between: a switch out and back in takes the
// scheduler microseconds at least, and the read pair takes about as long, so on a loaded machine
// one try in many lands across one. Sixteen in a row would take a thread preempted in every one.
const xidClockTries = 16

// readXidThreadClock stamps the calling OS thread's present moment (at) and reads its scheduler
// clocks at that moment: the CPU time it has run (CLOCK_THREAD_CPUTIME_ID, which the kernel brings
// up to date as it is read), and the time it has waited runnable on a run queue for a CPU
// (run_delay, the second field of /proc/thread-self/schedstat). The kernel adds a wait to run_delay
// when the thread gets a CPU back, and counts each time it does (the file's third field), so the
// file is read just before and just after the stamp, and a reading is kept only when both say the
// same: then the thread was not switched out between them, and run_delay at the stamp is exactly
// the value read. A tracer that took its stamp and read run_delay apart would read a wait between
// the two on the wrong side of the stamp, and a gap would count it as time its thread spent off a
// CPU of its own accord (traced at a load of 250 to 350 on a 288-CPU machine, 2026-11-20: 1.6 and
// 2.1 ms). It answers the zero clock when the clocks cannot be read, when the goroutine moved to
// another thread while it read them, or when no try of xidClockTries landed between two switches.
//
// It allocates nothing and, on a thread the rig holds (XidTracer.lockTransaction), makes no system
// call but the clock reads: the schedstat file is opened once, when the thread is held, and read on
// that descriptor (xidHeldClocks); and each read goes into a buffer on the stack and is parsed in
// place. The tracer reads the clocks at a gap's two ends, so an open or a close there would run
// inside the gap the rig keeps for the code; and while the collector is held off
// (runWithoutCollector) an allocation there would land on a page the process never touched - a
// first touch that, on a loaded machine, costs milliseconds of the thread's CPU in the kernel. On
// a thread the rig does not hold it opens the file for the one reading.
func readXidThreadClock() xidThreadClock {
	thread := unix.Gettid()
	if fd, held := (*xidHeldClocks.Load())[thread]; held {
		return readXidThreadClockOn(fd, thread)
	}
	fd, ok := xidOpenThreadClock()
	if !ok {
		return xidThreadClock{}
	}
	defer xidCloseThreadClock(fd)
	return readXidThreadClockOn(fd, thread)
}

// readXidThreadClockOn is readXidThreadClock's reading, on thread's schedstat open as fd.
func readXidThreadClockOn(fd uintptr, thread int) xidThreadClock {
	for range xidClockTries {
		before, ok := xidReadSchedstat(fd)
		if !ok {
			return xidThreadClock{}
		}
		at := time.Now()
		var cpu unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu); err != nil {
			return xidThreadClock{}
		}
		after, ok := xidReadSchedstat(fd)
		if !ok || unix.Gettid() != thread {
			return xidThreadClock{}
		}
		if before == after {
			return xidThreadClock{thread: thread, at: at, ran: time.Duration(cpu.Nano()), waited: time.Duration(before.waited)}
		}
	}
	return xidThreadClock{}
}

// xidHeldClocks is, for each thread the rig holds, its schedstat open as a descriptor. It is read
// at every statement's two ends without a lock, as an immutable map an atomic pointer swaps, and
// changed only when a thread is held or let go (xidWatchThreadClock, xidDropThreadClock).
var (
	xidHeldClocks      atomic.Pointer[map[int]uintptr]
	xidHeldClocksWrite sync.Mutex
)

func init() { xidHeldClocks.Store(&map[int]uintptr{}) }

// xidOpenThreadClock opens the calling thread's schedstat. /proc/thread-self names the thread at
// the open, so the descriptor stays that thread's for as long as it is open.
func xidOpenThreadClock() (uintptr, bool) {
	fd, _, errno := unix.Syscall6(unix.SYS_OPENAT, uintptr(xidAtCWD),
		uintptr(unsafe.Pointer(&xidSchedstatPath[0])), uintptr(unix.O_RDONLY|unix.O_CLOEXEC), 0, 0, 0)
	return fd, errno == 0
}

func xidCloseThreadClock(fd uintptr) { unix.Syscall(unix.SYS_CLOSE, fd, 0, 0) }

// xidWatchThreadClock opens the calling thread's schedstat and keeps it open as the thread's own
// descriptor until the returned stop, which a held thread's COMMIT or ROLLBACK calls
// (xidDropThreadClock); ok is false when the file cannot be opened.
func xidWatchThreadClock() (stop func(), ok bool) {
	fd, ok := xidOpenThreadClock()
	if !ok {
		return func() {}, false
	}
	thread := unix.Gettid()
	xidSwapHeldClocks(func(held map[int]uintptr) { held[thread] = fd })
	return func() { xidDropThreadClock(thread) }, true
}

// xidDropThreadClock stops reading thread's held clock and closes it, if it is still held. A
// transaction's COMMIT drops its thread's clock, and so does the end of a capture, for a thread
// whose transaction never ended - its goroutine stopped by a Fatalf - so that no later thread the
// kernel gives the same id reads a dead thread's descriptor. Dropping twice closes once.
func xidDropThreadClock(thread int) {
	var fd uintptr
	var open bool
	xidSwapHeldClocks(func(held map[int]uintptr) {
		fd, open = held[thread]
		delete(held, thread)
	})
	if open {
		xidCloseThreadClock(fd)
	}
}

func xidSwapHeldClocks(change func(map[int]uintptr)) {
	xidHeldClocksWrite.Lock()
	defer xidHeldClocksWrite.Unlock()
	held := make(map[int]uintptr, len(*xidHeldClocks.Load())+1)
	for thread, fd := range *xidHeldClocks.Load() {
		held[thread] = fd
	}
	change(held)
	xidHeldClocks.Store(&held)
}

// xidSchedstat is the two fields of a thread's schedstat the rig reads: run_delay, and the number
// of times the thread has been given a CPU.
type xidSchedstat struct {
	waited, runs int64
}

// xidReadSchedstat reads the schedstat file open on fd from its start, into a buffer on the stack.
func xidReadSchedstat(fd uintptr) (xidSchedstat, bool) {
	var raw [64]byte
	n, _, errno := unix.Syscall6(unix.SYS_PREAD64, fd, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), 0, 0, 0)
	if errno != 0 {
		return xidSchedstat{}, false
	}
	var fields [3]int64
	if !xidFields(raw[:n], fields[:]) {
		return xidSchedstat{}, false
	}
	return xidSchedstat{waited: fields[1], runs: fields[2]}, true
}

// xidFields parses the first len(into) space-separated decimal fields of a schedstat line, in place.
func xidFields(line []byte, into []int64) bool {
	field := 0
	digits := 0
	for _, c := range line {
		if c >= '0' && c <= '9' {
			into[field] = into[field]*10 + int64(c-'0')
			digits++
			continue
		}
		if digits == 0 {
			return false
		}
		field++
		digits = 0
		if field == len(into) {
			return true
		}
	}
	return field == len(into)-1 && digits > 0
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

// xidHoldMemory keeps NUMA balancing off every page the process has mapped private and writable -
// its heap, its stacks, its data - by setting each such mapping's memory policy to MPOL_LOCAL
// (mbind, which moves no page). A mapping with a policy of its own but no migrate-on-fault is one
// NUMA balancing's scan passes over, and one no thread's fault moves a page of. xidHoldThread keeps
// the measured thread from moving pages; this keeps every other thread from moving one under it:
// a page another thread moves is unmapped while it is copied, and a thread that touches it
// meanwhile sleeps until the move ends (migration_entry_wait), a sleep whose length is the
// machine's - traced at a load of about 250 on a 288-CPU machine, 2026-11-20, inside a forced gap.
// Measure holds the memory before each window (runWithoutCollector), since a mapping the process
// made since the last window has no policy yet.
//
// It answers false when NUMA balancing is on and the kernel refuses mbind. A mapping that goes
// away between the read of /proc/self/maps and its mbind is one there is nothing left to hold.
func xidHoldMemory() bool {
	if !xidNUMABalancing() {
		return true
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return false
	}
	for line := range bytes.Lines(maps) {
		fields := bytes.Fields(line)
		if len(fields) < 2 || len(fields[1]) < 4 || fields[1][1] != 'w' || fields[1][3] != 'p' {
			continue
		}
		start, end, ok := xidMappingRange(fields[0])
		if !ok {
			continue
		}
		_, _, errno := unix.Syscall6(unix.SYS_MBIND, start, end-start, xidMempolicyLocal, 0, 0, 0)
		if errno == unix.EPERM || errno == unix.ENOSYS {
			return false
		}
	}
	return true
}

// xidMappingRange reads a /proc/self/maps address range, "start-end" in hex, as the addresses
// mbind takes, at the platform's integer size (strconv.IntSize, the size of a uintptr).
func xidMappingRange(field []byte) (start, end uintptr, ok bool) {
	return xidMappingRangeAt(field, strconv.IntSize)
}

// xidMappingRangeAt reads an address range as xidMappingRange does, with each address parsed at
// bitSize bits, so an address that does not fit is refused rather than cut short; a range that is
// empty or runs backwards is refused too. bitSize is strconv.IntSize but in the test that holds a
// 32-bit platform's bound (TestAMappingRangeIsReadAtThePlatformsIntegerSize).
func xidMappingRangeAt(field []byte, bitSize int) (start, end uintptr, ok bool) {
	dash := bytes.IndexByte(field, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, err := strconv.ParseUint(string(field[:dash]), 16, bitSize)
	if err != nil {
		return 0, 0, false
	}
	last, err := strconv.ParseUint(string(field[dash+1:]), 16, bitSize)
	if err != nil || last <= first {
		return 0, 0, false
	}
	return uintptr(first), uintptr(last), true
}
