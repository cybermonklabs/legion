//go:build !race

package storetest

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// A held thread is pinned to the CPU it is on, and the rig reads that CPU's softirq counts beside
// its clocks: a burst of loopback datagrams the thread sends raises the CPU's NET_RX count, a read
// allocates nothing, and the thread's CPUs are put back when it is let go. With the count read
// planted to read nothing, the burst counts no NET_RX; with the unpin planted out, the thread keeps
// its one CPU.
func TestTheRigCountsTheSoftirqsItsPinnedCPURuns(t *testing.T) {
	RequireXidGapJudgment(t)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var before unix.CPUSet
	if err := unix.SchedGetaffinity(0, &before); err != nil {
		t.Fatalf("read this thread's CPUs: %v", err)
	}
	stop, ok := xidWatchThreadClock()
	if !ok {
		t.Fatal("the rig could not hold this thread's clock")
	}
	held := (*xidHeldClocks.Load())[unix.Gettid()]
	if held == nil || held.softirqs == nil {
		stop()
		t.Fatal("the rig could not pin this thread and open its CPU's softirq counts")
	}
	var pinned unix.CPUSet
	if err := unix.SchedGetaffinity(0, &pinned); err != nil || pinned.Count() != 1 || !pinned.IsSet(held.softirqs.cpu) {
		stop()
		t.Fatalf("held, this thread may run on %d CPUs (error %v), want only CPU %d", pinned.Count(), err, held.softirqs.cpu)
	}
	first := readXidSoftirqs()
	XidLoopbackSends(t, func(sent int) bool { return sent < 200 })
	second := readXidSoftirqs()
	allocs := testing.AllocsPerRun(100, func() { _ = readXidSoftirqs() })
	stop()
	if !first.ok || !second.ok {
		t.Fatalf("the rig could not read its CPU's softirq counts: %+v, %+v", first, second)
	}
	var moved XidSoftirqs
	for i := range moved {
		moved[i] = second.counts[i] - first.counts[i]
	}
	t.Logf("200 loopback datagrams on CPU %d: %s; a read took %s of the thread's CPU", second.cpu, moved, XidMillis(second.cost))
	if moved[3] == 0 {
		t.Errorf("200 loopback datagrams sent on CPU %d counted no NET_RX softirq there: %s", second.cpu, moved)
	}
	if allocs != 0 {
		t.Errorf("a read of the softirq counts allocates %v times, want none", allocs)
	}
	var after unix.CPUSet
	if err := unix.SchedGetaffinity(0, &after); err != nil || after != before {
		t.Errorf("let go, this thread may run on %d CPUs (error %v), want its %d back", after.Count(), err, before.Count())
	}
}

// A count line parses at the CPU's column, and a file with a vector missing is not a reading.
func TestASoftirqCountIsReadAtItsCPUsColumn(t *testing.T) {
	file := []byte("                    CPU0       CPU1       CPU2\n" +
		"          HI:          1          2          3\n      TIMER:         10         20         30\n" +
		"     NET_TX:          0          5          0\n     NET_RX:        100        200        300\n" +
		"      BLOCK:          7          8          9\n   IRQ_POLL:          0          0          0\n" +
		"    TASKLET:          4          5          6\n      SCHED:         11         12         13\n" +
		"    HRTIMER:          0          1          0\n        RCU:         21         22         23\n")
	column := xidSoftirqColumn(file, 2)
	counts, ok := xidSoftirqCounts(file, column)
	if want := xidSoftirqsOf(map[string]int64{"HI": 3, "TIMER": 30, "NET_RX": 300, "BLOCK": 9, "TASKLET": 6, "SCHED": 13, "RCU": 23}); column != 2 || !ok || counts != want {
		t.Fatalf("CPU2's counts: column %d, %v, %v; want column 2, %v", column, counts, ok, want)
	}
	cut := file[:len(file)-len("        RCU:         21         22         23\n")]
	if _, ok := xidSoftirqCounts(cut, column); ok {
		t.Fatal("a file without the RCU line read as a reading")
	}
}

// A kernel charges interrupt time to the thread it lands on when /proc/stat's sum of every CPU
// reads no time in hard interrupts (xidReadInterruptTimeCharged); one that accounts it apart
// reads some, and a first line that is not that sum reads as neither.
func TestTheKernelsInterruptAccountingIsReadOffProcStat(t *testing.T) {
	for _, test := range []struct {
		name        string
		stat        string
		charged, ok bool
	}{
		{"no hard interrupt time", "cpu  8915706459 1628920831 4011368725 22745265490 175918797 0 62420379 0 0 0\ncpu0 1 2 3 4 5 0 7 0 0 0\n", true, true},
		{"hard interrupt time accounted apart", "cpu  8915706459 1628920831 4011368725 22745265490 175918797 3120 62420379 0 0 0\n", false, true},
		{"a first line that is one CPU's", "cpu0 1 2 3 4 5 0 7 0 0 0\n", false, false},
		{"a first line cut short", "cpu  1 2 3 4 5\n", false, false},
	} {
		if charged, ok := xidInterruptTimeChargedIn([]byte(test.stat)); charged != test.charged || ok != test.ok {
			t.Errorf("%s: charged %t, ok %t; want %t, %t", test.name, charged, ok, test.charged, test.ok)
		}
	}
}
