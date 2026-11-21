//go:build linux

package storetest

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The rig reads a thread's clocks at both ends of every gap, and while the collector is held off
// an allocation there would be a first touch of a page the process never used: on a loaded
// machine, milliseconds of the thread's CPU in the kernel that the gap reads as the code's
// (runWithoutCollector). So a read allocates nothing, on a thread the rig holds (its clock open
// once, xidWatchThreadClock) or not. The reader before this rule, which read the file through
// os.ReadFile and split it with bytes.Fields, allocates on every read and fails here.
func TestReadingAThreadsClocksAllocatesNothing(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if readXidThreadClock().thread == 0 {
		// A kernel that shows no thread's clocks judges no gap (XidNoGapJudgment), so this stops.
		RequireXidGapJudgment(t)
		t.Fatal("this thread's scheduler clocks read nothing, yet the rig judges gaps")
	}
	if allocs := testing.AllocsPerRun(100, func() { readXidThreadClock() }); allocs != 0 {
		t.Fatalf("a read of a thread's clocks the rig does not hold allocates %v times, want none", allocs)
	}
	stop, ok := xidWatchThreadClock()
	if !ok {
		t.Fatal("open this thread's clock to hold it")
	}
	defer stop()
	if readXidThreadClock().thread == 0 {
		t.Fatal("a held thread's clock read nothing off its own descriptor")
	}
	if allocs := testing.AllocsPerRun(100, func() { readXidThreadClock() }); allocs != 0 {
		t.Fatalf("a read of a held thread's clocks allocates %v times, want none", allocs)
	}
}

// On a machine NUMA balancing runs on, a held thread allocates on its own node, as by default,
// but has no page moved for it while it is held, and gets back the policy it had when it is let
// go (xidHoldThread). On a machine NUMA balancing leaves alone the hold changes nothing.
func TestHoldingAThreadKeepsNUMABalancingOffItUntilItIsLetGo(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := threadMempolicy(t)
	release, held := xidHoldThread()
	if !held {
		t.Fatal("the rig could not hold this thread out of NUMA balancing")
	}
	during := threadMempolicy(t)
	release()
	after := threadMempolicy(t)
	want := before
	if xidNUMABalancing() {
		want = xidMempolicyLocal
	}
	if during != want {
		t.Errorf("a held thread's memory policy is mode %d, want %d", during, want)
	}
	if after != before {
		t.Errorf("a thread let go has memory policy mode %d, want the %d it had", after, before)
	}
}

// threadMempolicy is the calling thread's memory policy mode.
func threadMempolicy(t *testing.T) int {
	t.Helper()
	var mode int
	var nodes [16]uint64
	if _, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)),
		uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)*64), 0, 0, 0); errno != 0 {
		t.Fatalf("read this thread's memory policy: %v", errno)
	}
	return mode
}

// On a machine NUMA balancing runs on, the window's memory is held out of it (xidHoldMemory): every
// private writable mapping of the process - here, the one a fresh heap object lies in, and the one
// this goroutine's stack lies in - has a policy of its own, MPOL_LOCAL, so no thread's fault moves
// a page of it and the balancing scan passes over it. With the hold planted to do nothing, the
// mappings keep the default policy and this test fails. On a machine NUMA balancing leaves alone
// there is nothing to hold, and the test skips.
func TestHoldingTheMemoryKeepsNUMABalancingOffItsMappings(t *testing.T) {
	if !xidNUMABalancing() {
		t.Skip("NUMA balancing moves no page on this machine, so there is no memory to hold")
	}
	object := new([4096]byte)
	object[0] = 1
	var onStack byte
	if !xidHoldMemory() {
		t.Fatal("the rig could not hold the process's memory out of NUMA balancing")
	}
	for _, address := range []struct {
		name string
		at   unsafe.Pointer
	}{{"a heap object", unsafe.Pointer(object)}, {"this goroutine's stack", unsafe.Pointer(&onStack)}} {
		if mode := mappingMempolicy(t, address.at); mode != xidMempolicyLocal {
			t.Errorf("the mapping of %s has memory policy mode %d after the hold, want %d (MPOL_LOCAL)", address.name, mode, xidMempolicyLocal)
		}
	}
}

// mappingMempolicy is the memory policy mode of the mapping address lies in (MPOL_F_ADDR).
func mappingMempolicy(t *testing.T, address unsafe.Pointer) int {
	t.Helper()
	const byAddress = 1 << 1
	var mode int
	var nodes [16]uint64
	if _, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)),
		uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)*64), uintptr(address), byAddress, 0); errno != 0 {
		t.Fatalf("read the memory policy at %p: %v", address, errno)
	}
	return mode
}

// A /proc/self/maps address range is read as the addresses mbind takes, each parsed at the
// platform's integer size (xidMappingRange, xidMappingRangeAt). An address that does not fit is
// refused, not cut short: given a 32-bit platform's size, the range just above 4 GiB is refused,
// where parsing at 64 bits and converting, as before, would read it on a 32-bit build as the range
// at address 0. At 64 bits it is read whole, and only a range past 64 bits is refused. The module
// cannot be built for a 32-bit platform, so the 32-bit rows give the parser that size directly.
func TestAMappingRangeIsReadAtThePlatformsIntegerSize(t *testing.T) {
	for _, test := range []struct {
		field      string
		bits       int
		start, end uint64
	}{
		{field: "7f00-7f80", bits: 32, start: 0x7f00, end: 0x7f80},
		{field: "ffff0000-ffff1000", bits: 32, start: 0xffff0000, end: 0xffff1000},
		{field: "100000000-100001000", bits: 32},
		{field: "ffff0000-100000000", bits: 32},
		{field: "100000000-100001000", bits: 64, start: 0x100000000, end: 0x100001000},
		{field: "10000000000000000-10000000000001000", bits: 64},
		{field: "7f80-7f00", bits: 64},
		{field: "7f00", bits: 64},
		{field: "-7f00", bits: 64},
	} {
		start, end, ok := xidMappingRangeAt([]byte(test.field), test.bits)
		want := test.end != 0
		if ok != want || uint64(start) != test.start || uint64(end) != test.end {
			t.Errorf("xidMappingRangeAt(%q, %d) = %#x, %#x, %t, want %#x, %#x, %t",
				test.field, test.bits, start, end, ok, test.start, test.end, want)
		}
	}
	start, end, ok := xidMappingRange([]byte("100000000-100001000"))
	if fits := strconv.IntSize == 64; ok != fits || (fits && (start != 0x100000000 || end != 0x100001000)) {
		t.Errorf("at strconv.IntSize %d, xidMappingRange of the range above 4 GiB = %#x, %#x, %t", strconv.IntSize, start, end, ok)
	}
}

// The rig reads a schedstat line's fields in place (xidFields): the first three decimal fields,
// and no reading from a line that does not hold them.
func TestASchedstatLineIsReadInPlace(t *testing.T) {
	for _, test := range []struct {
		line string
		want []int64
	}{
		{"123 456 7\n", []int64{123, 456, 7}},
		{"0 0 1\n", []int64{0, 0, 1}},
		{"18446744073 9 12", []int64{18446744073, 9, 12}},
		{"123 456\n", nil},
		{"123  456 7\n", nil},
		{"", nil},
	} {
		var fields [3]int64
		ok := xidFields([]byte(test.line), fields[:])
		if test.want == nil {
			if ok {
				t.Errorf("xidFields(%q) read %v, want no reading", test.line, fields)
			}
			continue
		}
		if !ok || fields != [3]int64(test.want) {
			t.Errorf("xidFields(%q) = %v, %t, want %v", test.line, fields, ok, test.want)
		}
	}
}

// The window runs after a collection, with the collector already off (runWithoutCollector): with
// it off the heap only grows, so without that collection every allocation of the window would be
// the first touch of a page. The test reads the collector's cycle count inside the operation.
func TestTheWindowRunsAfterACollection(t *testing.T) {
	cycles := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(cycles)
	before := cycles[0].Value.Uint64()
	var during uint64
	var percent int
	runWithoutCollector(func() {
		metrics.Read(cycles)
		during = cycles[0].Value.Uint64()
		percent = debug.SetGCPercent(-1)
	})
	if during <= before {
		t.Errorf("the operation ran after %d collections, the same as before it: no collection ran first", during)
	}
	if percent != -1 {
		t.Errorf("the operation ran with GOGC %d, want the collector off", percent)
	}
}
