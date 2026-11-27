//go:build linux

package storetest

import (
	"bytes"
	"fmt"
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
// a thread the rig does not hold it opens the file for the one reading, and so it does where the
// thread's held clock is dropped between the read's finding it and its reading it (Measure's
// dropOpenClocks, once the capture's statements are copied, may drop the clock of a transaction a
// goroutine of the operation is still running). The held clock is read under its lock
// (xidHeldClock.readClock), so no read uses a descriptor the drop closed, or one whose number
// another open has taken since.
func readXidThreadClock() xidThreadClock {
	thread := unix.Gettid()
	if held, ok := (*xidHeldClocks.Load())[thread]; ok {
		if xidHeldClockFound != nil {
			xidHeldClockFound()
		}
		if reading, read := held.readClock(thread); read {
			return reading
		}
	}
	fd, ok := xidOpenThreadClock()
	if !ok {
		return xidThreadClock{}
	}
	defer xidCloseProc(fd)
	return readXidThreadClockOn(fd, thread)
}

// xidHeldClockFound, where set, runs in readXidThreadClock and readXidSoftirqs right after each
// finds the calling thread's held clock and before it takes the clock's lock: the tests of a clock
// dropped in between set it (TestAHeldClockDroppedWhileItIsReadStillReadsTheThread,
// TestASoftirqReadOfAClockDroppedWhileItIsReadCountsNothing). xidHeldClockReading, where set, runs
// in readClock under the clock's lock, right before it reads the descriptor: the test of a drop
// that starts during a read sets it (TestADropThatStartsDuringAReadWaitsForIt).
var xidHeldClockFound, xidHeldClockReading func()

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

// xidHeldClocks is, for each thread the rig holds, its schedstat open as a descriptor and its
// pinned CPU's softirq counts open beside it (xidHeldClock). The map is read at every statement's
// two ends without a lock, as an immutable map an atomic pointer swaps, and changed only when a
// thread is held or let go (xidWatchThreadClock, xidDropThreadClock); a clock found in it is read
// under its own lock (xidHeldClock).
var (
	xidHeldClocks      atomic.Pointer[map[int]*xidHeldClock]
	xidHeldClocksWrite sync.Mutex
)

// xidHeldClock is what the rig keeps open for a thread it holds: its schedstat, and the reader of
// the softirq counts of the CPU it is pinned to, nil where the rig could not pin it or count them.
// The thread's own goroutine reads them at its statements' ends, and another goroutine may drop
// them meanwhile: Measure drops the clock of every transaction still open once it has copied the
// capture's statements (dropOpenClocks), and a goroutine of the operation may still be running
// such a transaction, as a broadcast's delivery may be. So mu holds each read against the close:
// the drop closes them under it and sets dropped, and a read that finds dropped set reads nothing
// off them. Without the lock a read that had found the clock in the map could read its descriptor
// after the drop closed it (envoy-go job 139692608395, where the drop still came before the copy
// and a delivery's COMMIT, read at its start, was judged not read at its end).
type xidHeldClock struct {
	mu        sync.Mutex
	dropped   bool
	schedstat uintptr
	softirqs  *xidSoftirqReader
}

// readClock reads thread's clocks off its held schedstat (readXidThreadClockOn), under the
// clock's lock, and answers false, having read nothing, once the clock is dropped.
func (c *xidHeldClock) readClock(thread int) (xidThreadClock, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped {
		return xidThreadClock{}, false
	}
	if xidHeldClockReading != nil {
		xidHeldClockReading()
	}
	return readXidThreadClockOn(c.schedstat, thread), true
}

func init() { xidHeldClocks.Store(&map[int]*xidHeldClock{}) }

// xidOpenThreadClock opens the calling thread's schedstat. /proc/thread-self names the thread at
// the open, so the descriptor stays that thread's for as long as it is open.
func xidOpenThreadClock() (uintptr, bool) { return xidOpenProc(xidSchedstatPath) }

// xidOpenProc opens a /proc file read-only: path as openat takes it, NUL-terminated, so that the
// open is one system call and allocates nothing. ok is false when the file cannot be opened.
func xidOpenProc(path []byte) (fd uintptr, ok bool) {
	fd, _, errno := unix.Syscall6(unix.SYS_OPENAT, uintptr(xidAtCWD),
		uintptr(unsafe.Pointer(&path[0])), uintptr(unix.O_RDONLY|unix.O_CLOEXEC), 0, 0, 0)
	return fd, errno == 0
}

// xidCloseProc closes a descriptor xidOpenProc opened.
func xidCloseProc(fd uintptr) { unix.Syscall(unix.SYS_CLOSE, fd, 0, 0) }

// xidWatchThreadClock opens the calling thread's schedstat and keeps it open as the thread's own
// descriptor until the returned stop, which a held thread's COMMIT or ROLLBACK calls
// (xidDropThreadClock); ok is false when the file cannot be opened. It pins the thread to the CPU
// it is on, with that CPU's softirq counts open beside its clock (xidPinForSoftirqs), and stop puts
// the thread's CPUs back. A thread it cannot pin keeps its clock, and its gaps go uncounted.
func xidWatchThreadClock() (stop func(), ok bool) {
	fd, ok := xidOpenThreadClock()
	if !ok {
		return func() {}, false
	}
	thread := unix.Gettid()
	softirqs := xidPinForSoftirqs()
	xidSwapHeldClocks(func(held map[int]*xidHeldClock) { held[thread] = &xidHeldClock{schedstat: fd, softirqs: softirqs} })
	return func() {
		if softirqs != nil {
			softirqs.unpin()
		}
		xidDropThreadClock(thread)
	}, true
}

// xidDropThreadClock stops reading thread's held clock and closes it and its softirq counts, if
// it is still held. A transaction's COMMIT drops its thread's clock, after its own reads, and
// Measure drops the clock of every transaction still open once it has copied the capture's
// statements (dropOpenClocks): one whose goroutine a Fatalf stopped, so that no later thread the
// kernel gives the same id reads a dead thread's descriptor, and one a goroutine of the operation
// is still running, which may be reading the clock as it is dropped. The close waits under the
// clock's lock for a read in progress, and marks the clock dropped, so a read that finds it
// dropped opens the thread's file for itself (readXidThreadClock). Dropping twice closes once. A
// thread whose goroutine stopped while locked ends with it, so its CPUs are not put back.
func xidDropThreadClock(thread int) {
	var clock *xidHeldClock
	xidSwapHeldClocks(func(held map[int]*xidHeldClock) {
		clock = held[thread]
		delete(held, thread)
	})
	if clock == nil {
		return
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.dropped = true
	xidCloseProc(clock.schedstat)
	if clock.softirqs != nil {
		xidCloseProc(clock.softirqs.fd)
	}
}

func xidSwapHeldClocks(change func(map[int]*xidHeldClock)) {
	xidHeldClocksWrite.Lock()
	defer xidHeldClocksWrite.Unlock()
	held := make(map[int]*xidHeldClock, len(*xidHeldClocks.Load())+1)
	for thread, clock := range *xidHeldClocks.Load() {
		held[thread] = clock
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

// xidHoldWindowMemory holds the process's memory still for a window, in two ways, and answers why
// it could not hold it, empty when it did.
//
// On a machine NUMA balancing runs on, it keeps NUMA balancing off every page the process has
// mapped private and writable - its heap, its stacks, its data - by setting each such mapping's
// memory policy to MPOL_LOCAL (mbind, which moves no page). A mapping with a policy of its own but
// no migrate-on-fault is one NUMA balancing's scan passes over, and one no thread's fault moves a
// page of. xidHoldThread keeps the measured thread from moving pages; this keeps every other
// thread from moving one under it: a page another thread moves is unmapped while it is copied,
// and a thread that touches it meanwhile sleeps until the move ends (migration_entry_wait), a
// sleep whose length is the machine's - traced at a load of about 250 on a 288-CPU machine,
// 2026-11-20, inside a forced gap.
//
// On every machine, it makes every page of the execution tracer's memory present
// (MADV_POPULATE_WRITE, which faults each page in writable and writes nothing to it): the trace's
// buffers and the blocks of its tables (xidTracerRanges, through xidFindTracerMemory). The tracer
// writes events from the measured thread inside every gap, and the first write to a page is a page
// fault, which takes the page table's lock; another thread of the process may hold it, and on a
// loaded machine one fault waited 11.3 ms for it on the thread's CPU (xidTrace.warm). Only that
// memory is made present: all the process's private writable mappings came to 460 MiB for a test
// process of 66 MiB, its threads' 8 MiB stacks and the heap's reservation, and the tracer's to
// under 1 MiB.
//
// lockTransaction holds the memory at each marked BEGIN, inside that statement, since a mapping
// the process made since the last window - a trace buffer among them - has no policy of its own
// and pages not yet present. The thread-clock probe holds it once too, through xidHoldMemory, to
// learn whether it can (xidThreadClocksUnavailable). Nothing puts the old policy back: once a
// mapping is set to MPOL_LOCAL here, it stays so for the rest of the process's life, which for a
// test binary is the rest of its tests.
//
// It says why it could not hold the memory when there is memory to hold and /proc/self/maps cannot
// be read, when NUMA balancing is on and the kernel refuses mbind, and when an execution trace the
// rig started runs (startXidTrace) and none of the tracer's memory is found: then the tracer's
// first writes inside a gap can land on pages never touched, which the gap would read as the code's
// time, so the BEGIN's window is UNKNOWN by that reason (analyzeXidWindows). A mapping that goes
// away between the read of /proc/self/maps and its mbind or madvise is one there is nothing left
// to hold. A kernel without MADV_POPULATE_WRITE (before Linux 5.14) leaves the tracer's pages to
// fault in as the tracer writes them, and so does any other refusal, since this call does not read
// what madvise answers. Such a fault is not one-sided. Inside a gap it reads as the code's time,
// and can fail a window the code did not make slow. Inside a statement it lands on the statements'
// side, which rule 2 weighs the gaps against (XidWindow.StatementTime, XidWindow.Violations), so
// it can also hide a window that breaks rule 2: a 4 ms gap against 1 ms of statements breaks that
// rule and not XidMaxGap, and 5 ms of fault time inside those statements takes the breach away.
// What shows a populate that made nothing present is the burst test, which fails naming the pages
// this search found (TestTheTracersEventsInAWindowTouchNoFreshPage).
func xidHoldWindowMemory() (unheld string) {
	balancing := xidNUMABalancing()
	tracing := xidMappedBeforeTrace.Load() != nil
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		if !balancing && !tracing {
			return ""
		}
		return fmt.Sprintf("the process's mappings could not be read to hold them: %v", err)
	}
	refused := false
	if balancing {
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
				refused = true
			}
		}
	}
	tracer := xidFindTracerMemory(maps)
	for _, r := range tracer {
		// What madvise answers is not read, so a refusal is not reported: what a page left
		// unmade costs a window is in this function's comment, on both sides of rule 2.
		unix.Syscall(unix.SYS_MADVISE, r.start, r.end-r.start, unix.MADV_POPULATE_WRITE)
	}
	if refused {
		return "the kernel refused mbind MPOL_LOCAL on the process's mappings, so NUMA balancing may move a page under the window's thread"
	}
	if !tracing || len(tracer) > 0 {
		return ""
	}
	searched := "no mapping carries the runtime's names, and no anonymous writable mapping but the Go heap's and threads' stacks was mapped since the rig started the trace"
	if xidRuntimeNamesMappings(maps) {
		searched = "no mapping is named the tracer's ([anon: Go: trace buffer] or [anon: Go: trace arena alloc])"
	}
	return fmt.Sprintf("none of the execution tracer's memory was found to make present (%s), so the tracer's first writes inside a gap could land on pages never touched, which the gap would read as the code's time", searched)
}

// xidHoldMemory holds the process's memory as xidHoldWindowMemory does and answers whether it held
// it. The thread-clock probe asks it before any trace starts, so its answer is of NUMA balancing
// alone there (xidThreadClocksUnavailable).
func xidHoldMemory() bool { return xidHoldWindowMemory() == "" }

// xidFindTracerMemory finds the execution tracer's memory in a read of /proc/self/maps for the
// hold (xidHoldWindowMemory): xidTracerRanges, in a variable so that the test of a BEGIN that finds
// none can plant that (TestABeginThatFindsNoTracerMemoryIsUnknown).
var xidFindTracerMemory = xidTracerRanges

// xidTracerRange is an address range of the execution tracer's memory, or, in
// xidMappedBeforeTrace, of anything the process had mapped.
type xidTracerRange struct{ start, end uintptr }

// xidMappedBeforeTrace is every address range the process had mapped right before the rig started
// its execution trace, and nil while no trace the rig started runs (startXidTrace, xidTrace.stop).
// What the tracer maps once it runs lies outside these ranges (xidUnnamedTracerRanges). A pointer
// read and swapped whole, since each marked BEGIN of an operation reads it from a goroutine of its
// own.
var xidMappedBeforeTrace atomic.Pointer[[]xidTracerRange]

// xidMappedRanges is every address range of a read of /proc/self/maps, in its ascending order.
func xidMappedRanges(maps []byte) []xidTracerRange {
	var ranges []xidTracerRange
	for line := range bytes.Lines(maps) {
		if fields := bytes.Fields(line); len(fields) > 0 {
			if start, end, ok := xidMappingRange(fields[0]); ok {
				ranges = append(ranges, xidTracerRange{start, end})
			}
		}
	}
	return ranges
}

// xidTracerRanges is the execution tracer's memory in a read of /proc/self/maps, found as the
// kernel lets it be. Where the runtime's names are on the process's mappings (its
// decoratemappings, on by default since Go 1.25, on a kernel that keeps names for anonymous
// mappings), it is the mappings named the tracer's (xidTracerMapping). A kernel built without
// CONFIG_ANON_VMA_NAME keeps no name, and the runtime's own GODEBUG=decoratemappings=0 sets none:
// then no line carries one, and the tracer's memory is what was mapped after the rig started the
// trace (xidUnnamedTracerRanges), none where no trace the rig started runs.
func xidTracerRanges(maps []byte) []xidTracerRange {
	before := xidMappedBeforeTrace.Load()
	if !xidRuntimeNamesMappings(maps) && before != nil {
		probe := new([64]byte)
		xidHeapProbe.Store(probe)
		return xidUnnamedTracerRanges(maps, *before, uintptr(unsafe.Pointer(probe)))
	}
	var named []xidTracerRange
	for line := range bytes.Lines(maps) {
		if !xidTracerMapping(line) {
			continue
		}
		if start, end, ok := xidMappingRange(bytes.Fields(line)[0]); ok {
			named = append(named, xidTracerRange{start, end})
		}
	}
	return named
}

// xidRuntimeNamesMappings reports whether the runtime's names are on the process's mappings, in a
// read of /proc/self/maps: the runtime names its heap and its other memory "[anon: Go: ...]".
func xidRuntimeNamesMappings(maps []byte) bool {
	return bytes.Contains(maps, []byte("[anon: Go: "))
}

// xidHeapProbe holds the heap object xidTracerRanges allocates, so that the object is the heap's:
// its address names the Go heap's mapping.
var xidHeapProbe atomic.Pointer[[64]byte]

// xidUnnamedTracerRanges is the execution tracer's memory on a kernel that names no anonymous
// mapping: each part of an anonymous private writable mapping of maps that no range of before
// (ascending, xidMappedBeforeTrace) covers. The runtime maps each trace buffer, and each block of
// the trace's tables, as a mapping of its own, and the kernel joins it to a neighbour with the same
// flags; what else the process mapped since the trace started is left out where it can be told
// apart: the Go heap's mapping, the one that holds the heap object at heap, which grows as the
// trace's reader keeps the trace, and each thread's stack, a mapping right above a one-page guard
// of no access, as pthread_create maps it. Any other memory the process mapped meanwhile, the
// runtime's own small blocks, is made present with the tracer's.
func xidUnnamedTracerRanges(maps []byte, before []xidTracerRange, heap uintptr) []xidTracerRange {
	page := uintptr(os.Getpagesize())
	var ranges []xidTracerRange
	var guardEnd uintptr
	for line := range bytes.Lines(maps) {
		fields := bytes.Fields(line)
		if len(fields) < 5 {
			continue
		}
		start, end, ok := xidMappingRange(fields[0])
		if !ok {
			continue
		}
		aboveGuard := guardEnd == start
		guardEnd = 0
		perms := fields[1]
		if string(perms) == "---p" && end-start == page {
			guardEnd = end
		}
		// An anonymous mapping has no sixth field, the path; private and writable is rw-p.
		if len(fields) != 5 || len(perms) < 4 || perms[1] != 'w' || perms[3] != 'p' || aboveGuard || (heap >= start && heap < end) {
			continue
		}
		for _, covered := range before {
			if covered.end <= start || covered.start >= end {
				continue
			}
			if covered.start > start {
				ranges = append(ranges, xidTracerRange{start, covered.start})
			}
			start = max(start, covered.end)
			if start >= end {
				break
			}
		}
		if start < end {
			ranges = append(ranges, xidTracerRange{start, end})
		}
	}
	return ranges
}

// xidTracerMapping reports whether a /proc/self/maps line is the execution tracer's memory, by the
// name the runtime gives it: its buffers, and the blocks its string and stack tables are made of.
func xidTracerMapping(line []byte) bool {
	return bytes.HasSuffix(bytes.TrimRight(line, "\n"), []byte("[anon: Go: trace buffer]")) ||
		bytes.HasSuffix(bytes.TrimRight(line, "\n"), []byte("[anon: Go: trace arena alloc]"))
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
