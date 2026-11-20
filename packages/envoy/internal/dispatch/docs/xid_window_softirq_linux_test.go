//go:build !race

package docs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// xidSoftirqBurst is the CPU time of loopback sends the first attempt's gap runs: past the bound,
// so the window breaks D6 with all of it on the thread's CPU.
const xidSoftirqBurst = storetest.XidMaxGap + storetest.XidMaxGap/2

// xidRigWindow is the rig transaction's window: its insert, its two reads and its COMMIT.
var xidRigWindow = storetest.XidWindowOf(4, "the rig transaction's insert, its two reads and its COMMIT")

// loopbackBurstFor sends loopback datagrams from this thread until it has run cpu of CPU time.
// The kernel delivers each in a network receive softirq on this thread's CPU as the send returns,
// charged to this thread: the work a machine's own traffic puts on a thread, made here on purpose.
func loopbackBurstFor(t *testing.T, cpu time.Duration) {
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
	for from := threadCPU(t); threadCPU(t)-from < cpu; {
		for range 16 {
			if err := unix.Sendto(sender, payload, 0, bound); err != nil {
				t.Fatalf("send a datagram over the loopback interface: %v", err)
			}
		}
	}
}

// spinFor runs this thread for cpu of CPU time.
func spinFor(t *testing.T, cpu time.Duration) {
	t.Helper()
	for from := threadCPU(t); threadCPU(t)-from < cpu; {
	}
}

// A gap past the bound that is all its thread's CPU, while that CPU runs network receive softirqs,
// is one the machine's interrupts may hold, so the rig measures its window again rather than
// judging it, and the next attempt, a gap of a moment of Go, is judged and passes. The first
// attempt's gap runs loopback sends, whose receives the kernel runs on the thread's CPU and
// charges to it, as it charges the machine's own traffic. With the count read planted to read
// nothing, the first attempt fails at once.
func TestXidWindowRigMeasuresAgainAGapItsCPUsSoftirqsMayHold(t *testing.T) {
	skipWhereNoGapIsJudged(t)
	attempts := 0
	var first string
	storetest.XidAttempts(t, func(t *testing.T) {
		attempts++
		attempt := attempts
		_, database, tracer := xidService(t)
		report := tracer.Measure(t, database.Pool, func() {
			runRigTransactionDoing(t, database, func() {
				if attempt == 1 {
					loopbackBurstFor(t, xidSoftirqBurst)
					return
				}
				spinFor(t, xidCrowdedWork)
			})
		})
		if len(report.Problems) > 0 || len(report.Windows) != 1 {
			t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
		}
		if attempt == 1 {
			err := report.Err()
			var again interface{ XidMeasureAgain() string }
			if !errors.As(err, &again) || !strings.Contains(again.XidMeasureAgain(), "NET_RX") {
				t.Fatalf("a gap of %s of loopback sends, its CPU running their receives: verdict %v, want its window measured again for the CPU's NET_RX",
					xidSoftirqBurst, err)
			}
			first = again.XidMeasureAgain()
		}
		storetest.AssertXidWindows(t, report, xidRigWindow)
	})
	if first == "" || attempts < 2 {
		t.Fatalf("%d attempts, the first measured again for %q: want the first measured again for its CPU's softirqs and a later one judged", attempts, first)
	}
	t.Logf("the first attempt was measured again: %s", first)
}

// A gap the code slept past the bound is the code's whatever its CPU ran meanwhile: the counts can
// excuse only the CPU time its thread ran. Here the gap's CPU runs network receives for the sends
// before the sleep, and the window still breaks D6 at once, not to be measured again.
func TestXidWindowRigNeverExcusesASleepForSoftirqs(t *testing.T) {
	skipWhereNoGapIsJudged(t)
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func() {
			loopbackBurstFor(t, storetest.XidMaxGap/5)
			time.Sleep(rigInjectedGap)
		})
	})
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	if gap := window.Gaps[0]; !gap.Counted || !strings.Contains(gap.Softirqs.String(), "NET_RX") {
		t.Fatalf("the gap's CPU ran %s (counted: %t): no network receive was counted in it, so this run proves nothing\n%s", gap.Softirqs, gap.Counted, window.Detail())
	}
	err := report.Err()
	var again interface{ XidMeasureAgain() string }
	if err == nil || errors.As(err, &again) || !strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a %s sleep after loopback sends: verdict %v, want a D6 violation after the insert, not one to measure again\n%s", rigInjectedGap, err, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}
