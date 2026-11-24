package storetest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testcleanup"
)

// A kernel that does not account interrupt time apart (CONFIG_IRQ_TIME_ACCOUNTING not set, or set
// and off at run time) charges the work a CPU does in an interrupt, and in the softirqs it runs on
// the interrupt's way out, to the thread that was running there. A gap's clocks then read that
// work as CPU time its thread ran, which is the code's. Traced at a load of about 220 on a 288-CPU
// machine (2026-11-20): a forced gap read 5.054 ms made behind about 1 ms of code, and 39 of its
// 50 perf samples were the kernel's network receive (net_rx_action, on interrupt exit).
//
// No source the rig can read without privilege gives that time at a resolution the 5 ms bound can
// use. Measured on that machine (Linux 7.0, 2026-11-20): /proc/stat's per-CPU softirq time moves
// only in steps of 10 ms (USER_HZ 100; 1,023 of 1,023 moves in 20 s were one step); a thread's
// schedstat and CLOCK_THREAD_CPUTIME_ID hold the interrupt's time, which is the fault itself;
// perf_event_open is refused (perf_event_paranoid 4), unprivileged BPF is off
// (unprivileged_bpf_disabled 2), tracefs is root's alone, and /proc/pressure/irq does not exist
// without CONFIG_IRQ_TIME_ACCOUNTING. What the rig can read is a count: it pins a marked
// transaction's thread to the CPU it is on at BEGIN (XidTracer.lockTransaction) and reads that
// CPU's column of /proc/softirqs at each statement's two ends (readXidSoftirqs), so each gap knows
// how many times each softirq ran on its CPU meanwhile (XidGap.Softirqs), and it reads once
// whether the kernel charges interrupt time to the thread it lands on (xidInterruptTimeCharged).
//
// So on such a kernel a gap whose CPU ran a device softirq is held to D6 widened by the time those
// softirqs may have put on its thread (XidGap.SoftirqHold), and that time comes from a measurement
// of the machine, not from the count alone. Every device softirq run on an interrupt's way out,
// in a task's context, on all 288 CPUs of that machine for 600 s at a load of 820 to 1,200
// (bpftrace on irq:softirq_entry and softirq_exit, a run's path read off irq_exit_rcu and
// __local_bh_enable_ip; 2,282,123 runs, 0.07% of events lost):
//
//   - one run: NET_RX p50 0.006 ms, p99 0.065, p99.9 0.149, p99.99 0.432, p99.999 0.944, max
//     9.584 ms; BLOCK, NET_TX and TASKLET each at most 0.470 ms;
//   - device softirq time in a 5 ms stretch of one CPU, over every stretch that begins at a run:
//     p99 0.909 ms, p99.9 1.491, p99.99 2.395, p99.999 2.614, max 5.000 (that 9.584 ms run).
//
// A counted run may stand for XidSoftirqRunHold, the one-run p99.99 rounded up to the next
// 0.1 ms; a gap's counts together for at most XidSoftirqGapHold, the 5 ms stretch's p99.99
// rounded up likewise; and neither for more than the CPU time the gap's thread ran, the only part
// of a gap an interrupt is charged to, so a gap the code slept or waited past the bound is never
// the machine's. A count says a softirq ran on the CPU, not in the thread - the CPU runs other
// threads' softirqs, and a thread's own network sends run their receives in its own system call
// and count the same - which is why a count stands for so little and the gap's counts together
// for no more than the cap. The tick and the scheduler's vectors count for nothing (xidSoftirqVector).
// What this leaves is the tail past those quantiles: a gap whose thread the machine's interrupts
// charged more than its hold, and enough more to take the code's own time past 5 ms, still fails -
// in those 600 s one run on an interrupt's way out took 9.584 ms.
const (
	XidSoftirqRunHold = 500 * time.Microsecond
	XidSoftirqGapHold = 2400 * time.Microsecond
)

// xidSoftirqVector is one softirq vector /proc/softirqs counts: its name there, and whether a
// machine's devices and network traffic raise it (HI, NET_TX, NET_RX, BLOCK, IRQ_POLL, TASKLET),
// so that its count may stand for time on the gap's thread. The tick and the scheduler raise
// TIMER, SCHED, HRTIMER and RCU on every CPU every few milliseconds - on the 288-CPU machine, SCHED
// about 18 and RCU about 11 times in 100 ms on the median CPU - so a count of them would widen the
// bound of every gap long on a CPU, the code's own included.
type xidSoftirqVector struct {
	name   string
	device bool
}

// xidSoftirqVectors are the vectors in the kernel's order (include/linux/interrupt.h), the order
// of /proc/softirqs' lines.
var xidSoftirqVectors = [...]xidSoftirqVector{
	{"HI", true}, {"TIMER", false}, {"NET_TX", true}, {"NET_RX", true}, {"BLOCK", true},
	{"IRQ_POLL", true}, {"TASKLET", true}, {"SCHED", false}, {"HRTIMER", false}, {"RCU", false},
}

// XidSoftirqs is how many times each softirq vector's handler ran on one CPU over a stretch,
// indexed as xidSoftirqVectors.
type XidSoftirqs [len(xidSoftirqVectors)]int64

// Device is how many times a device vector ran (xidSoftirqVector.device).
func (s XidSoftirqs) Device() int64 {
	var n int64
	for i, count := range s {
		if xidSoftirqVectors[i].device {
			n += count
		}
	}
	return n
}

// String names each vector that ran, with its count: "NET_RX +40, SCHED +2".
func (s XidSoftirqs) String() string {
	var moved []string
	for i, count := range s {
		if count != 0 {
			moved = append(moved, fmt.Sprintf("%s +%d", xidSoftirqVectors[i].name, count))
		}
	}
	if len(moved) == 0 {
		return "no softirq"
	}
	return strings.Join(moved, ", ")
}

// xidInterruptTimeCharged reports whether this kernel charges a CPU's interrupt time to the
// thread the interrupt lands on (xidReadInterruptTimeCharged). Only there can a gap's softirqs
// hold any of its time. A variable so that a test can name either kernel (XidChargeInterruptTime).
var xidInterruptTimeCharged = xidReadInterruptTimeCharged

// XidChargeInterruptTime makes the rig judge t's windows as on a kernel that charges interrupt
// time to the thread it lands on, whatever this kernel does, until t ends. A test of the widened
// bound calls it, so that the bound it holds is the one it tests on every kernel, one that
// accounts interrupt time apart included. Tests in this repository do not run window tests in
// parallel.
func XidChargeInterruptTime(t testing.TB) {
	t.Helper()
	xidInterruptTimeCharged = func() bool { return true }
	testcleanup.Add(t, func() { xidInterruptTimeCharged = xidReadInterruptTimeCharged })
}

// xidSoftirqReading is one reading of the softirq counts of the CPU a held thread is pinned to
// (readXidSoftirqs): the counts, the CPU, and the thread's CPU time the read took, which is the
// rig's own work inside a statement and not the statement's. ok is false where the thread is not
// held and pinned, or was not on its CPU.
type xidSoftirqReading struct {
	counts XidSoftirqs
	cpu    int
	cost   time.Duration
	ok     bool
}

// countGap fills in gap's softirq counts from before's end reading to after's start reading, where
// both were taken on one CPU of a gap read off its thread's clocks. The kernel keeps each count in
// 32 bits (kstat's softirqs, an unsigned int) and prints it unsigned, so a count that wrapped in
// the gap reads less after than before; the difference is taken in 32 bits, which reads it right.
func countGap(gap *XidGap, before, after XidStatement) {
	from, to := before.endedIRQ, after.startedIRQ
	if !gap.Read || !from.ok || !to.ok || from.cpu != to.cpu {
		return
	}
	gap.Counted = true
	gap.SoftirqCPU = from.cpu
	for i := range gap.Softirqs {
		gap.Softirqs[i] = int64(uint32(to.counts[i] - from.counts[i]))
	}
}

// SoftirqHold is the time the device softirqs the gap's CPU ran may have put on its thread, which
// D6 allows the gap beside its own bound (Bound): each counted run XidSoftirqRunHold, all of them
// together at most XidSoftirqGapHold, and never more than the CPU time the thread ran. It is zero
// for a gap whose softirqs went uncounted, whose CPU ran none of the device vectors, or on a
// kernel that does not charge interrupt time to the thread it lands on.
func (g XidGap) SoftirqHold() time.Duration {
	if !g.Counted || !xidInterruptTimeCharged() {
		return 0
	}
	return min(time.Duration(g.Softirqs.Device())*XidSoftirqRunHold, XidSoftirqGapHold, max(g.Ran, 0))
}

// Bound is the most time D6 lets the code make in the gap: XidMaxGap, and beside it the time its
// CPU's device softirqs may have put on its thread (SoftirqHold).
func (g XidGap) Bound() time.Duration { return XidMaxGap + g.SoftirqHold() }
