//go:build !race

package docs

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// The rig reads the machine's load as the machine's, not as the code's gap (storetest.XidGap):
// the gap after the write is spent giving up its turn on a CPU that other processes crowd, so it
// lasts several times the bound on the clock while the code makes a fraction of it, and the
// window keeps D6. The same crowding with a sleep in the gap still breaks D6: a gap the code
// makes is caught however busy the machine is. Each arm first holds that the crowding did open
// the gap on the clock past the bound, so a run that loaded nothing cannot pass for one that read
// the load away.
func TestXidWindowRigReadsTheMachinesLoadAsTheMachines(t *testing.T) {
	// Not parallel: its subject is the machine's load, which the package's other tests would add to.
	storetest.RequireXidGapJudgment(t)
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
				defer crowded.hold(t)()
				runRigTransactionDoing(t, database, func() {
					crowded.run(t)
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

// xidCrowdedTurns is how many times the crowded gap gives up its turn on the CPU: each time it
// waits runnable while the crowding processes run their slices, so the gap lasts several of the
// scheduler's slices on the clock. The gap does no work of its own beyond its turns. Work there is
// the code's own gap, which rule 2 holds against the statements, and the rig transaction's four
// statements make as little as 0.35 ms on the crowded CPU: a quarter of a millisecond of work read
// 0.385 ms made against statements of 0.347 ms once in 1,000 forced runs, and a millisecond 1.176
// against 1.140 ms once in 1,200, on a 288-CPU machine at a load of about 500, 2026-11-20.
const xidCrowdedTurns = 4

// xidCrowdingProcesses is how many processes crowd the CPU.
const xidCrowdingProcesses = 8

// xidCrowdingStart is the longest crowdACPU waits for its shells to spin on the crowded CPU.
const xidCrowdingStart = 10 * time.Second

// crowdedCPU is one CPU that xidCrowdingProcesses processes spin on until stop.
type crowdedCPU struct {
	cpu      unix.CPUSet
	spinners []*exec.Cmd
}

// crowdACPU starts the crowding: a CPU this thread may run on, and xidCrowdingProcesses shells,
// each pinned to that CPU and spinning in a loop of its own builtins. They are processes of their
// own, as a loaded machine's other work is, and not goroutines of this one: a goroutine that spins
// is preempted by the runtime every 10 ms, and each preemption of a goroutine locked to its thread
// is a handoff between threads of this process, which crowding goroutines turned into a storm of
// handoffs the measured thread's own had to wait in (traced at a load of about 300 on a 288-CPU
// machine, 2026-11-20: a measured thread parked 7 ms for a P that one of them, itself waiting for
// the crowded CPU, was to hand back). Each shell spins only while this test process lives: its
// loop tests the test's pid with kill -0, a builtin, so a test process that dies, killed or
// panicking past its cleanups, leaves no busy loop pinned to a CPU of a shared machine. All of it
// is done before the measured transaction begins, and stop undoes it after, so none of it is
// inside a window. crowdACPU returns only once each shell has run on the crowded CPU since its pin
// and is runnable there (spinningOn), at most xidCrowdingStart: a shell not yet spinning leaves a
// gap that gives up its turn no one to give it to, and in 1,000 forced runs one such gap took
// 15.877 us on the clock, a run that proved nothing.
func crowdACPU(t *testing.T) *crowdedCPU {
	t.Helper()
	var own unix.CPUSet
	if err := unix.SchedGetaffinity(0, &own); err != nil {
		t.Fatalf("read this thread's CPUs: %v", err)
	}
	crowded := &crowdedCPU{}
	cpu := -1
	for c := range len(own) * 64 {
		if own.IsSet(c) {
			cpu = c
			crowded.cpu.Set(c)
			break
		}
	}
	if cpu < 0 {
		t.Fatalf("this thread may run on no CPU of %v", own)
	}
	testcleanup.Add(t, crowded.stop)
	pinned := make([]time.Duration, 0, xidCrowdingProcesses)
	for range xidCrowdingProcesses {
		spinner := exec.Command("sh", "-c", fmt.Sprintf("while kill -0 %d 2>/dev/null; do :; done", os.Getpid()))
		if err := spinner.Start(); err != nil {
			t.Fatalf("start a crowding process: %v", err)
		}
		crowded.spinners = append(crowded.spinners, spinner)
		if err := unix.SchedSetaffinity(spinner.Process.Pid, &crowded.cpu); err != nil {
			t.Fatalf("pin a crowding process: %v", err)
		}
		ran, _, _, _ := procRunState(spinner.Process.Pid)
		pinned = append(pinned, ran)
	}
	deadline := time.Now().Add(xidCrowdingStart)
	for i, spinner := range crowded.spinners {
		for !spinningOn(spinner.Process.Pid, cpu, pinned[i]) {
			if time.Now().After(deadline) {
				ran, state, on, _ := procRunState(spinner.Process.Pid)
				t.Fatalf("crowding process %d is not spinning on CPU %d %s after it was started (it has run %s, is in state %c, on CPU %d), so a gap would not be crowded",
					spinner.Process.Pid, cpu, xidCrowdingStart, ran, state, on)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return crowded
}

// spinningOn reports whether process pid has run more than pinned, the CPU time it had run when it
// was pinned to cpu - so it has run on cpu since - and is runnable on cpu now.
func spinningOn(pid, cpu int, pinned time.Duration) bool {
	ran, state, on, ok := procRunState(pid)
	return ok && ran > pinned && state == 'R' && on == cpu
}

// procRunState is the CPU time process pid has run (the first field of its schedstat), its state
// and the CPU it is on (the third and 39th fields of its stat); ok is false when either is unread.
func procRunState(pid int) (ran time.Duration, state byte, cpu int, ok bool) {
	schedstat, err := os.ReadFile(fmt.Sprintf("/proc/%d/schedstat", pid))
	if err != nil {
		return 0, 0, 0, false
	}
	runNs, _, _ := strings.Cut(string(schedstat), " ")
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, false
	}
	_, rest, found := strings.Cut(string(stat), ") ")
	fields := strings.Fields(rest)
	if !found || len(fields) < 37 || len(fields[0]) != 1 {
		return 0, 0, 0, false
	}
	ns, err1 := strconv.ParseInt(runNs, 10, 64)
	on, err2 := strconv.Atoi(fields[36])
	if err1 != nil || err2 != nil {
		return 0, 0, 0, false
	}
	return time.Duration(ns), fields[0][0], on, true
}

// pin pins this thread to the crowded CPU, and answers what gives it back its own CPUs.
func (crowded *crowdedCPU) pin(t *testing.T) (unpin func()) {
	t.Helper()
	var own unix.CPUSet
	if err := unix.SchedGetaffinity(0, &own); err != nil {
		t.Fatalf("read this thread's CPUs: %v", err)
	}
	if err := crowded.pinThread(); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := unix.SchedSetaffinity(0, &own); err != nil {
			t.Errorf("give this thread back its CPUs: %v", err)
		}
	}
}

// pinThread pins this thread to the crowded CPU for good, for a goroutine other than the test's.
func (crowded *crowdedCPU) pinThread() error {
	if err := unix.SchedSetaffinity(0, &crowded.cpu); err != nil {
		return fmt.Errorf("pin this thread to the crowded CPU: %w", err)
	}
	return nil
}

// hold locks the calling goroutine to its thread and pins the thread to the crowded CPU, and
// answers what lets it go with its own CPUs. Called before a transaction's BEGIN, it puts the
// whole transaction on the crowded CPU, so the rig, which pins a held thread to the CPU it is on at
// BEGIN, counts the softirqs of the CPU the gap's work runs on (storetest.XidGap.Softirqs).
func (crowded *crowdedCPU) hold(t *testing.T) (release func()) {
	t.Helper()
	runtime.LockOSThread()
	unpin := crowded.pin(t)
	return func() {
		unpin()
		runtime.UnlockOSThread()
	}
}

// run gives up this thread's turn on the crowded CPU xidCrowdedTurns times, as a thread the
// scheduler preempts gives it up, so it waits runnable behind the crowding processes however the
// scheduler placed it. The thread is already on the crowded CPU alone: hold pinned it there before
// the transaction's BEGIN, at which the rig locked the calling goroutine to it.
func (crowded *crowdedCPU) run(t *testing.T) {
	t.Helper()
	for range xidCrowdedTurns {
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

// The rig does not read a wait for the Go runtime to run the window's goroutine again as the
// code's gap (storetest.XidGap.GoRunnable), and does not pass it either when it cannot say whose
// it was. With one P, the goroutine yields behind a goroutine that spins: it is runnable, not
// running, until the runtime preempts the spinner, which it does after the spinner has run 10 ms,
// and its locked thread sleeps all that time. Read off the thread's clocks alone, that sleep is
// time the code made, over the bound. Read off the execution trace it is the runtime's wait, but
// whose the rig cannot tell: the spinner is the operation's own goroutine, and no wait on the
// runtime can be laid to the machine (storetest.XidGap.Made), so the window is never judged a
// pass: it keeps D6 only as UNKNOWN, a rig problem, or breaks D6 by the rig's own rules, which
// count as the code's what the rig cannot place - under load, the thread's queue for its CPU
// outside the runtime's wait. The arm takes either, logs which, and fails by name on a pass and on
// any other rig problem, beside a violation too. The decision itself, xidRuntimeWaitProblem, is
// proven with no clock or load by storetest's TestAWindowThatKeepsD6OnlyOnARuntimeWaitIsUnknown
// and TestAWaitOnAPHandedToALockedThreadDecidesNothing; that analyzeXidWindows applies it is shown
// only by this arm's runs that reach the UNKNOWN branch, which log it: envoy-go's step "Test what
// the race build cannot run at all or at full size" runs this file's tests with -v, so its log
// says which branch ran. A receive on a channel the code waits on for as long is a wait the code
// asked for (GoWaiting), and breaks D6. Each arm
// first holds that the wait happened: its gap, read off the thread's clocks alone, is over the
// bound, and in the spinner arm the goroutine waited on the runtime past that 5 ms bound too. The
// one P is set inside the measured operation, since Measure runs its operation with idle Ps beside
// it.
func TestXidWindowRigReadsTheRuntimesWaitAsTheRuntimes(t *testing.T) {
	// Not parallel: it measures with the xid window rig (XidTracer.Measure), which sets the whole
	// process's GOMAXPROCS and collector for its operation and fails a test that called t.Parallel
	// (requireMeasuredAlone) It also sets GOMAXPROCS to 1 inside the operation itself.
	storetest.RequireXidGapJudgment(t)
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
			report := tracer.Measure(t, database.Pool, func() {
				defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
				runRigTransactionDoing(t, database, probe.wait)
			})
			if len(report.Windows) != 1 {
				t.Fatalf("the rig judged %d windows with problems %v, want the one transaction", len(report.Windows), report.Problems)
			}
			window := report.Windows[0]
			gap := window.Gaps[0]
			if gap.Clock-gap.Runnable <= storetest.XidMaxGap {
				t.Fatalf("the gap read %s off its thread's clocks alone, not past %s: the wait did not happen, so this run proves nothing\n%s",
					storetest.XidMillis(gap.Clock-gap.Runnable), storetest.XidMillis(storetest.XidMaxGap), window.Detail())
			}
			if !probe.breaks && gap.GoRunnable <= storetest.XidMaxGap {
				t.Fatalf("the goroutine waited %s runnable on the runtime (traced %t, rig problems %v), not past %s: the spinner did not hold the P, or the trace did not show the gap, so this run proves nothing\n%s",
					storetest.XidMillis(gap.GoRunnable), gap.Traced, report.Problems, storetest.XidMillis(storetest.XidMaxGap), window.Detail())
			}
			err := report.Err()
			switch {
			case !probe.breaks && err == nil:
				t.Fatalf("a wait for the runtime while the operation's own spinner held the one P was judged a pass, want the window UNKNOWN or a D6 violation\n%s", window.Detail())
			case !probe.breaks && strings.Contains(err.Error(), "XID WINDOW VIOLATION") && len(report.Problems) == 0:
				t.Logf("not a pass: the window broke D6 by the rig's rules\n%v", err)
			case !probe.breaks && len(report.Problems) == 1 && strings.Contains(report.Problems[0], "UNKNOWN: backend") &&
				strings.Contains(report.Problems[0], "keeps D6 only if its goroutine's waits on the runtime"):
				t.Logf("not a pass: the window is UNKNOWN")
			case !probe.breaks:
				t.Fatalf("a wait for the runtime while the operation's own spinner held the one P: verdict %v, want the window UNKNOWN or a D6 violation, and no other rig problem\n%s", err, window.Detail())
			case len(report.Problems) > 0 || err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
				!strings.Contains(err.Error(), "follows statement 0: insert into projects"):
				t.Fatalf("a %s wait on a channel: verdict %v, want a D6 violation after the insert\n%s", rigInjectedGap, err, window.Detail())
			}
			t.Logf("%s\n%s", window.Summary(), window.Detail())
		})
	}
}

// xidPlantedSleep is the sleep the code asks for after a handoff, in
// TestXidWindowRigReadsASleepAfterAHandoffAsTheCodes: past the bound, so it breaks D6.
const xidPlantedSleep = 8 * time.Millisecond

// xidHandoffNice is the niceness the window's thread takes beside the crowding processes in
// TestXidWindowRigReadsASleepAfterAHandoffAsTheCodes: a weight low enough that, woken, it cannot
// take the CPU from them, so it waits its turn on the run queue. SCHED_IDLE is lower still, and
// needs no privilege either; niceness 19 is enough, its handoffs waiting 622 to 1,206 ms in 20 of 20
// runs (2026-11-21).
const xidHandoffNice = 19

// A system call the code makes after the runtime hands its goroutine back is the code's time, and
// no wait for a CPU around it is taken out of it. With one P, the window's goroutine yields behind
// a spinning goroutine while its thread is pinned to a CPU other processes crowd, so when the
// runtime hands the goroutine back the woken thread waits its turn on that CPU, at niceness 19
// for a second, all of it runnable on the runtime too; then the goroutine sleeps 8 ms in a system
// call (nanosleep). The gap must read at least the 8 ms made, and break D6. A formula that took a
// run-queue wait out of the gap wherever the clocks put it read one such gap at 0.225 ms made (a
// tester's run, 2026-11-21), a D6 break unseen: the wait the trace tied to no handoff came out of
// the system call. Made takes no wait for a CPU out of the thread's sleep (storetest.XidGap.Made).
// One run, which fails if the thread and the runtime did not both wait 1 ms in the gap, as a run
// that planted no wait proves nothing. The window runs on a goroutine of its own that ends locked
// to its thread, so the runtime ends that thread and none of the process's later work runs at that
// niceness; that goroutine reports its failures to the test's, which alone may stop the test. The
// one P is set inside the measured operation, since Measure runs its operation with idle Ps beside
// it.
func TestXidWindowRigReadsASleepAfterAHandoffAsTheCodes(t *testing.T) {
	// Not parallel: it measures with the xid window rig (XidTracer.Measure), which sets the whole
	// process's GOMAXPROCS and collector for its operation and fails a test that called t.Parallel
	// (requireMeasuredAlone) It also sets GOMAXPROCS to 1 inside the operation itself.
	storetest.RequireXidGapJudgment(t)
	_, database, tracer := xidService(t)
	crowded := crowdACPU(t)
	report := tracer.Measure(t, database.Pool, func() {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		failed := make(chan error, 1)
		go func() {
			// Never unlocked: the goroutine ends locked, and the runtime ends its thread with it.
			runtime.LockOSThread()
			failed <- rigTransactionDoing(database, func() error {
				if err := crowded.pinThread(); err != nil {
					return err
				}
				if err := unix.Setpriority(unix.PRIO_PROCESS, 0, xidHandoffNice); err != nil {
					return fmt.Errorf("lower this thread to niceness %d: %w", xidHandoffNice, err)
				}
				yieldBehindASpinner()
				sleep := unix.NsecToTimespec(int64(xidPlantedSleep))
				if err := unix.Nanosleep(&sleep, nil); err != nil {
					return fmt.Errorf("sleep %s: %w", xidPlantedSleep, err)
				}
				return nil
			})
		}()
		if err := <-failed; err != nil {
			t.Fatal(err)
		}
	})
	crowded.stop()
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	gap := window.Gaps[0]
	if queued := min(gap.GoRunnable, gap.Runnable); queued < time.Millisecond {
		t.Fatalf("the thread waited %s for a CPU and its goroutine %s for the runtime, not 1 ms each: the plant made no long runtime queue, so this run proves nothing\n%s",
			storetest.XidMillis(gap.Runnable), storetest.XidMillis(gap.GoRunnable), window.Detail())
	}
	if made := gap.Made(); made < xidPlantedSleep {
		t.Fatalf("a %s sleep after a handoff read %s made, want at least the sleep\n%s", xidPlantedSleep, storetest.XidMillis(made), window.Detail())
	}
	if err := report.Err(); err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a %s sleep after a handoff: verdict %v, want a D6 violation after the insert\n%s", xidPlantedSleep, err, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// yieldBehindASpinner starts a goroutine that spins until told to stop and yields to it until it
// has started: the runtime may run the yielding goroutine first, as it does every 61st schedule,
// when it reads the global run queue before its own, and then the next yield is the one that
// waits. With one P, once the spinner runs it holds the P until the runtime preempts it, 10 ms
// into its time slice, and only then runs the yielding goroutine again. The spinner must start a
// slice of its own. A goroutine just started is the next to run, and runs in the rest of the
// slice of the goroutine that started it: that slice may already be most of the way to 10 ms old
// on the clock (on a loaded machine, the thread waits on a run queue as it runs), and once the
// spinner was preempted after 2.096 ms, a run that proved nothing. So a second goroutine, which
// does nothing, takes the next place, and the spinner runs after it from the run queue, in a new
// slice.
func yieldBehindASpinner() {
	var started, stop atomic.Bool
	go func() {
		started.Store(true)
		for !stop.Load() {
		}
	}()
	go func() {}()
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
