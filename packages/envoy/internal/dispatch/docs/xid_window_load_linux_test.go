//go:build linux

package docs

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The rig reads the machine's load as the machine's, not as the code's gap (storetest.XidGap):
// the gap after the write is spent running a moment of Go on a CPU that other threads crowd, so
// it lasts several times the bound on the clock while the code makes a fraction of it, and the
// window keeps D6. The same crowding with a sleep in the gap still breaks D6: a gap the code
// makes is caught however busy the machine is. Each arm first holds that the crowding did open
// the gap on the clock past the bound, so a run that loaded nothing cannot pass for one that read
// the load away.
func TestXidWindowRigReadsTheMachinesLoadAsTheMachines(t *testing.T) {
	skipWhereNoGapIsJudged(t)
	for _, probe := range []struct {
		name  string
		sleep time.Duration
	}{
		{name: "a crowded CPU", sleep: 0},
		{name: "a crowded CPU and a " + rigInjectedGap.String() + " sleep", sleep: rigInjectedGap},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, database, tracer := xidService(t)
			crowded := crowdACPU(t)
			report := tracer.Measure(t, database.Pool, func() {
				runRigTransactionDoing(t, database, func() {
					crowded.run(t, xidCrowdedWork)
					time.Sleep(probe.sleep)
				})
			})
			crowded.stop()
			if len(report.Problems) > 0 || len(report.Windows) != 1 {
				t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
			}
			window := report.Windows[0]
			if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
				t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s", window.Statements[0].SQL, window.Detail())
			}
			if gap := window.Gaps[0]; gap.Clock <= storetest.XidMaxGap || gap.Runnable <= 0 {
				t.Fatalf("the crowded gap took %s on the clock, %s of it waiting for a CPU: the crowding did not open a gap past %s, so this run read no load\n%s",
					storetest.XidMillis(gap.Clock), storetest.XidMillis(gap.Runnable), storetest.XidMillis(storetest.XidMaxGap), window.Detail())
			}
			err := report.Err()
			switch {
			case probe.sleep == 0 && err != nil:
				t.Fatalf("a gap the machine's load opened broke D6: %v", err)
			case probe.sleep > 0 && (err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
				!strings.Contains(err.Error(), "follows statement 0: insert into projects")):
				t.Fatalf("a %s sleep in a crowded gap: verdict %v, want a D6 violation after the insert\n%s", probe.sleep, err, window.Detail())
			}
			t.Logf("%s\n%s", window.Summary(), window.Detail())
		})
	}
}

// xidCrowdedWork is the CPU time the crowded gap runs, a fifth of storetest.XidMaxGap: short
// enough that the gap the code makes stays under the bound.
const xidCrowdedWork = storetest.XidMaxGap / 5

// xidCrowdedTurns is how many pieces the crowded gap runs its work in, giving up its turn on the
// CPU after each: each time it waits runnable while the crowding threads run their slices, so the
// gap lasts several of the scheduler's slices on the clock.
const xidCrowdedTurns = 4

// xidCrowdingThreads is how many threads crowd the CPU.
const xidCrowdingThreads = 8

// crowdedCPU is one CPU that xidCrowdingThreads threads spin on until stop.
type crowdedCPU struct {
	cpu  unix.CPUSet
	done atomic.Bool
	quit sync.WaitGroup
	// procs is GOMAXPROCS as it was before the crowding threads were given one each.
	procs int
}

// crowdACPU starts the crowding: a CPU this thread may run on, and xidCrowdingThreads goroutines
// each locked to a thread of its own pinned to that CPU and spinning. Each takes a P of its own,
// so GOMAXPROCS is raised by that many first and put back by stop. All of it is done before the
// measured transaction begins, and stop undoes it after, so none of it is inside a window.
func crowdACPU(t *testing.T) *crowdedCPU {
	t.Helper()
	var own unix.CPUSet
	if err := unix.SchedGetaffinity(0, &own); err != nil {
		t.Fatalf("read this thread's CPUs: %v", err)
	}
	crowded := &crowdedCPU{}
	for cpu := range len(own) * 64 {
		if own.IsSet(cpu) {
			crowded.cpu.Set(cpu)
			break
		}
	}
	if crowded.cpu.Count() == 0 {
		t.Fatalf("this thread may run on no CPU of %v", own)
	}
	crowded.procs = runtime.GOMAXPROCS(runtime.GOMAXPROCS(0) + xidCrowdingThreads)
	var started sync.WaitGroup
	started.Add(xidCrowdingThreads)
	crowded.quit.Add(xidCrowdingThreads)
	for range xidCrowdingThreads {
		go func() {
			defer crowded.quit.Done()
			// Never unlocked, so the runtime ends the pinned thread with the goroutine.
			runtime.LockOSThread()
			err := unix.SchedSetaffinity(0, &crowded.cpu)
			started.Done()
			if err != nil {
				t.Errorf("pin a crowding thread: %v", err)
				return
			}
			for !crowded.done.Load() {
			}
		}()
	}
	started.Wait()
	t.Cleanup(crowded.stop)
	return crowded
}

// run runs work of this thread's CPU time pinned to the crowded CPU, in xidCrowdedTurns pieces,
// yielding its turn after each as a thread the scheduler preempts gives it up, so it waits
// runnable behind the crowding threads however the scheduler placed it; and gives the thread back
// its own CPUs. The rig locked the calling goroutine to this thread at its transaction's BEGIN.
func (crowded *crowdedCPU) run(t *testing.T, work time.Duration) {
	t.Helper()
	var own unix.CPUSet
	if err := unix.SchedGetaffinity(0, &own); err != nil {
		t.Fatalf("read this thread's CPUs: %v", err)
	}
	if err := unix.SchedSetaffinity(0, &crowded.cpu); err != nil {
		t.Fatalf("pin this thread to the crowded CPU: %v", err)
	}
	defer func() {
		if err := unix.SchedSetaffinity(0, &own); err != nil {
			t.Errorf("give this thread back its CPUs: %v", err)
		}
	}()
	for range xidCrowdedTurns {
		from := threadCPU(t)
		for threadCPU(t)-from < work/xidCrowdedTurns {
		}
		if _, _, errno := unix.Syscall(unix.SYS_SCHED_YIELD, 0, 0, 0); errno != 0 {
			t.Fatalf("yield this thread's turn: %v", errno)
		}
	}
}

// stop ends the crowding threads and puts GOMAXPROCS back; a second call does nothing.
func (crowded *crowdedCPU) stop() {
	if crowded.done.Swap(true) {
		return
	}
	crowded.quit.Wait()
	runtime.GOMAXPROCS(crowded.procs)
}

// threadCPU is the CPU time this thread has run.
func threadCPU(t *testing.T) time.Duration {
	t.Helper()
	var cpu unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu); err != nil {
		t.Fatalf("read this thread's CPU time: %v", err)
	}
	return time.Duration(cpu.Nano())
}
