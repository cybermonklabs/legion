//go:build linux

package storetest

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xidSoftirqsPath is /proc/softirqs as openat takes it, NUL-terminated.
var xidSoftirqsPath = []byte("/proc/softirqs\x00")

// xidSoftirqNames are the vector names as /proc/softirqs labels its lines, so a read compares
// bytes and allocates nothing.
var xidSoftirqNames = func() (names [len(xidSoftirqVectors)][]byte) {
	for i, vector := range xidSoftirqVectors {
		names[i] = []byte(vector.name + ":")
	}
	return names
}()

// xidReadInterruptTimeCharged reads, once, whether this kernel charges the time a CPU spends in
// interrupts to the thread they land on (xidInterruptTimeCharged). The first line of /proc/stat
// sums every CPU's time by kind, and its sixth field is the time spent in hard interrupts. A
// kernel that accounts interrupt time apart from the thread it interrupts (CONFIG_IRQ_TIME_ACCOUNTING
// on, and on at run time) adds to that field at every interrupt; one that does not can only
// sample it at the tick, and the tick, itself a hard interrupt, never finds the CPU in another
// one, so the field stays 0 - as it reads on the 288-CPU machine these tests were traced on
// (Linux 7.0, CONFIG_IRQ_TIME_ACCOUNTING not set: "cpu ... 175918797 0 62420379 ...", 2026-11-20).
// A file the rig cannot read or parse reads as a kernel that does not charge it.
var xidReadInterruptTimeCharged = sync.OnceValue(func() bool {
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return false
	}
	charged, ok := xidInterruptTimeChargedIn(stat)
	return ok && charged
})

// xidInterruptTimeChargedIn reads /proc/stat's first line: charged is true when its hard
// interrupt time is 0 (xidReadInterruptTimeCharged); ok is false when the line is not the sum of
// every CPU or has no such field.
func xidInterruptTimeChargedIn(stat []byte) (charged, ok bool) {
	line, _, _ := bytes.Cut(stat, []byte("\n"))
	fields := bytes.Fields(line)
	if len(fields) < 7 || !bytes.Equal(fields[0], []byte("cpu")) {
		return false, false
	}
	irq, err := strconv.ParseUint(string(fields[6]), 10, 64)
	if err != nil {
		return false, false
	}
	return irq == 0, true
}

// xidSoftirqReader reads the softirq counts of the one CPU a held thread is pinned to: /proc/softirqs
// open on fd, the CPU's column in it, a buffer written whole before the window so that no read
// faults on a page of it, and the CPUs the thread had before the pin.
type xidSoftirqReader struct {
	fd      uintptr
	cpu     int
	column  int
	buf     []byte
	restore unix.CPUSet
}

// xidCurrentCPU is the CPU the calling thread is running on.
func xidCurrentCPU() (int, bool) {
	var cpu uint32
	_, _, errno := unix.RawSyscall(unix.SYS_GETCPU, uintptr(unsafe.Pointer(&cpu)), 0, 0)
	return int(cpu), errno == 0
}

// xidPinForSoftirqs pins the calling thread, which must be locked to its goroutine, to the CPU it
// is on, and opens /proc/softirqs to read that CPU's counts. A per-CPU count is a gap's only where
// the gap ran on that one CPU, so the pin holds the thread there until its transaction ends
// (unpin). The CPU is the one the thread is already on, so the pin moves nothing and leaves the
// scheduler's spread of the process's threads as it was. It answers nil, with the thread's CPUs as
// they were, when the thread cannot be pinned or the file cannot be read; that thread's gaps go
// uncounted, and none of them is given the time its CPU's softirqs may hold (XidGap.SoftirqHold).
func xidPinForSoftirqs() *xidSoftirqReader {
	cpu, ok := xidCurrentCPU()
	if !ok {
		return nil
	}
	reader := &xidSoftirqReader{cpu: cpu}
	if err := unix.SchedGetaffinity(0, &reader.restore); err != nil {
		return nil
	}
	var pin unix.CPUSet
	pin.Set(cpu)
	if err := unix.SchedSetaffinity(0, &pin); err != nil {
		return nil
	}
	first, err := os.ReadFile("/proc/softirqs")
	if err == nil {
		reader.column = xidSoftirqColumn(first, cpu)
	}
	if err != nil || reader.column < 0 {
		reader.unpin()
		return nil
	}
	fd, ok := xidOpenProc(xidSoftirqsPath)
	if !ok {
		reader.unpin()
		return nil
	}
	reader.fd = fd
	// Twice the file and a page more, written now, so the reads in the window touch no fresh page.
	reader.buf = make([]byte, 2*len(first)+os.Getpagesize())
	for i := range reader.buf {
		reader.buf[i] = ' '
	}
	if _, ok := reader.read(); !ok {
		xidCloseProc(fd)
		reader.unpin()
		return nil
	}
	return reader
}

// unpin puts back the CPUs the thread had before the pin.
func (r *xidSoftirqReader) unpin() { unix.SchedSetaffinity(0, &r.restore) }

// read reads the pinned CPU's count of each softirq vector, in one pread into the reader's buffer;
// a file that fills the buffer may have been cut short, and is not a reading.
func (r *xidSoftirqReader) read() (XidSoftirqs, bool) {
	n, _, errno := unix.Syscall6(unix.SYS_PREAD64, r.fd, uintptr(unsafe.Pointer(&r.buf[0])), uintptr(len(r.buf)), 0, 0, 0)
	if errno != 0 || int(n) >= len(r.buf) {
		return XidSoftirqs{}, false
	}
	return xidSoftirqCounts(r.buf[:n], r.column)
}

// xidSoftirqColumn is the field of each count line of /proc/softirqs that holds cpu's count, after
// the line's vector name: the place of "CPU<cpu>" in the header line. -1 when the header has none.
func xidSoftirqColumn(file []byte, cpu int) int {
	header, _, _ := bytes.Cut(file, []byte("\n"))
	want := []byte("CPU" + strconv.Itoa(cpu))
	for i, field := range bytes.Fields(header) {
		if bytes.Equal(field, want) {
			return i
		}
	}
	return -1
}

// xidSoftirqCounts parses column's count of each vector out of /proc/softirqs, in place: each line
// after the header is a vector's name and colon, then one count for each CPU.
func xidSoftirqCounts(file []byte, column int) (XidSoftirqs, bool) {
	var counts XidSoftirqs
	var found [len(xidSoftirqVectors)]bool
	_, rest, _ := bytes.Cut(file, []byte("\n"))
	for len(rest) > 0 {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		line = bytes.TrimLeft(line, " ")
		vector := -1
		for i, name := range xidSoftirqNames {
			if bytes.HasPrefix(line, name) {
				vector = i
				line = line[len(name):]
				break
			}
		}
		if vector < 0 {
			continue
		}
		count, ok := xidField(line, column)
		if !ok {
			return XidSoftirqs{}, false
		}
		counts[vector], found[vector] = count, true
	}
	for _, ok := range found {
		if !ok {
			return XidSoftirqs{}, false
		}
	}
	return counts, true
}

// xidField parses the decimal field at index of line's space-separated fields.
func xidField(line []byte, index int) (int64, bool) {
	field := -1
	var value int64
	digits := 0
	for i := 0; i <= len(line); i++ {
		if i < len(line) && line[i] >= '0' && line[i] <= '9' {
			if digits == 0 {
				field++
				value = 0
			}
			value = value*10 + int64(line[i]-'0')
			digits++
			continue
		}
		if digits > 0 && field == index {
			return value, true
		}
		digits = 0
	}
	return 0, false
}

// readXidSoftirqs reads the softirq counts of the CPU the calling thread is pinned to, when the rig
// holds it (xidWatchThreadClock), with the thread's CPU time the read took. A reading taken off
// that CPU is not one: the thread was moved, so the CPU's counts are not its. The counts are read
// under the held clock's lock, and a clock dropped since the read found it counts nothing, as a
// thread the rig does not hold does: its descriptor is closed, and its number may be another
// file's by now (xidHeldClock).
func readXidSoftirqs() xidSoftirqReading {
	held := (*xidHeldClocks.Load())[unix.Gettid()]
	if held == nil || held.softirqs == nil {
		return xidSoftirqReading{}
	}
	if xidHeldClockFound != nil {
		xidHeldClockFound()
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.dropped {
		return xidSoftirqReading{}
	}
	var before, after unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &before)
	counts, ok := held.softirqs.read()
	cpu, onCPU := xidCurrentCPU()
	_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &after)
	return xidSoftirqReading{
		counts: counts,
		cpu:    held.softirqs.cpu,
		cost:   time.Duration(after.Nano() - before.Nano()),
		ok:     ok && onCPU && cpu == held.softirqs.cpu,
	}
}

// XidLoopbackSends sends 512-byte datagrams over the loopback interface from the calling thread,
// in batches of 16, while more says to; more is given how many it has sent so far. The kernel runs
// each datagram's receive as a network receive softirq (NET_RX) on the sending CPU, in the sending
// thread's own system call as the send returns, so the CPU's NET_RX count rises with them, and on
// a kernel that charges softirq time to the thread it lands on (xidInterruptTimeCharged) that
// receive is the sending thread's CPU time: the code's own work, which no count can tell from the
// machine's.
func XidLoopbackSends(t testing.TB, more func(sent int) bool) {
	t.Helper()
	receiver, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open the receiving socket: %v", err)
	}
	defer unix.Close(receiver)
	if err := unix.Bind(receiver, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind the receiving socket: %v", err)
	}
	bound, err := unix.Getsockname(receiver)
	if err != nil {
		t.Fatalf("read the receiving socket's address: %v", err)
	}
	sender, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open the sending socket: %v", err)
	}
	defer unix.Close(sender)
	payload := make([]byte, 512)
	for sent := 0; more(sent); {
		for range 16 {
			if err := unix.Sendto(sender, payload, 0, bound); err != nil {
				t.Fatalf("send a datagram over the loopback interface: %v", err)
			}
			sent++
		}
	}
}
