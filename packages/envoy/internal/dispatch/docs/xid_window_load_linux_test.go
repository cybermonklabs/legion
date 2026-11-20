//go:build linux

package docs

import (
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The rig reads the machine's load as the machine's, not as the code's gap (storetest.XidGap):
// the gap after the write is spent running a moment of Go on a CPU that other processes crowd, so
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
// CPU after each: each time it waits runnable while the crowding processes run their slices, so the
// gap lasts several of the scheduler's slices on the clock.
const xidCrowdedTurns = 4

// xidCrowdingProcesses is how many processes crowd the CPU.
const xidCrowdingProcesses = 8

// crowdedCPU is one CPU that xidCrowdingProcesses processes spin on until stop.
type crowdedCPU struct {
	cpu      unix.CPUSet
	spinners []*exec.Cmd
}

// crowdACPU starts the crowding: a CPU this thread may run on, and xidCrowdingProcesses shells,
// each pinned to that CPU and spinning in a loop of its own builtin. They are processes of their
// own, as a loaded machine's other work is, and not goroutines of this one: a goroutine that spins
// is preempted by the runtime every 10 ms, and each preemption of a goroutine locked to its thread
// is a handoff between threads of this process, which crowding goroutines turned into a storm of
// handoffs the measured thread's own had to wait in (traced at a load of about 300 on a 288-CPU
// machine, 2026-11-20: a measured thread parked 7 ms for a P that one of them, itself waiting for
// the crowded CPU, was to hand back). All of it is done before the measured transaction begins,
// and stop undoes it after, so none of it is inside a window.
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
	t.Cleanup(crowded.stop)
	for range xidCrowdingProcesses {
		spinner := exec.Command("sh", "-c", "while :; do :; done")
		if err := spinner.Start(); err != nil {
			t.Fatalf("start a crowding process: %v", err)
		}
		crowded.spinners = append(crowded.spinners, spinner)
		if err := unix.SchedSetaffinity(spinner.Process.Pid, &crowded.cpu); err != nil {
			t.Fatalf("pin a crowding process: %v", err)
		}
	}
	return crowded
}

// run runs work of this thread's CPU time pinned to the crowded CPU, in xidCrowdedTurns pieces,
// yielding its turn after each as a thread the scheduler preempts gives it up, so it waits
// runnable behind the crowding processes however the scheduler placed it; and gives the thread
// back its own CPUs. The rig locked the calling goroutine to this thread at its transaction's BEGIN.
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

// stop ends the crowding processes; a second call does nothing.
func (crowded *crowdedCPU) stop() {
	for _, spinner := range crowded.spinners {
		_ = spinner.Process.Kill()
		_ = spinner.Wait()
	}
	crowded.spinners = nil
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

// The rig reads a wait for the Go runtime to run the window's goroutine again as the runtime's,
// not as the code's gap (storetest.XidGap.GoRunnable). With one P, the goroutine yields behind a
// goroutine that spins: it is runnable, not running, until the runtime preempts the spinner, which
// it does after the spinner has run 10 ms, and its locked thread sleeps all that time. Read off the
// thread's clocks alone, that sleep is time the code made, over the bound; read off the execution
// trace, it is the runtime's, and the window keeps D6. A receive on a channel the code waits on
// for as long is a wait the code asked for (GoWaiting), and still breaks D6. Each arm first holds
// that the wait happened: its gap, read off the thread's clocks alone, is over the bound.
func TestXidWindowRigReadsTheRuntimesWaitAsTheRuntimes(t *testing.T) {
	skipWhereNoGapIsJudged(t)
	for _, probe := range []struct {
		name   string
		wait   func()
		breaks bool
	}{
		{name: "the runtime runs another goroutine first", wait: yieldBehindASpinner},
		{name: "a channel receive the code waits on", wait: func() { receiveAfter(rigInjectedGap) }, breaks: true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, database, tracer := xidService(t)
			procs := runtime.GOMAXPROCS(1)
			report := tracer.Measure(t, database.Pool, func() {
				runRigTransactionDoing(t, database, probe.wait)
			})
			runtime.GOMAXPROCS(procs)
			if len(report.Problems) > 0 || len(report.Windows) != 1 {
				t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
			}
			window := report.Windows[0]
			if gap := window.Gaps[0]; gap.Clock-gap.Runnable <= storetest.XidMaxGap {
				t.Fatalf("the gap read %s off its thread's clocks alone, not past %s: the wait did not happen, so this run proves nothing\n%s",
					storetest.XidMillis(gap.Clock-gap.Runnable), storetest.XidMillis(storetest.XidMaxGap), window.Detail())
			}
			err := report.Err()
			switch {
			case !probe.breaks && err != nil:
				t.Fatalf("a wait for the runtime to run the goroutine again broke D6: %v", err)
			case probe.breaks && (err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
				!strings.Contains(err.Error(), "follows statement 0: insert into projects")):
				t.Fatalf("a %s wait on a channel: verdict %v, want a D6 violation after the insert\n%s", rigInjectedGap, err, window.Detail())
			}
			t.Logf("%s\n%s", window.Summary(), window.Detail())
		})
	}
}

// yieldBehindASpinner starts a goroutine that spins until told to stop and yields to it until it
// has started: the runtime may run the yielding goroutine first, as it does every 61st schedule,
// when it reads the global run queue before its own, and then the next yield is the one that
// waits. With one P, once the spinner runs it holds the P until the runtime preempts it, at least
// 10 ms on, and only then runs the yielding goroutine again.
func yieldBehindASpinner() {
	var started, stop atomic.Bool
	go func() {
		started.Store(true)
		for !stop.Load() {
		}
	}()
	for !started.Load() {
		runtime.Gosched()
	}
	stop.Store(true)
}

// receiveAfter waits on a channel another goroutine closes after d.
func receiveAfter(d time.Duration) {
	done := make(chan struct{})
	time.AfterFunc(d, func() { close(done) })
	<-done
}
