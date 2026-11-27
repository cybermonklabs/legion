package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A tmux client that exits on its own did not time out, however long pipeDrainDelay then holds
// runTmux open for something still holding its output pipes (a server that took the client's
// stdio over the socket): its exit is its answer. With exit 3 the run keeps the client's own exit
// and no timeout; with exit 0, a runTmux that took exec.ErrWaitDelay for a failure would report
// that success as an error. What holds the pipes runs until the test's cleanup releases it
// (heldChild), so every run is held past pipeDrainDelay.
//
// The timeout is the rest of the test binary's deadline, so a loaded machine that slows the client
// does not cut it off. Main's form gave the exit 0 a minute from before the client started, and
// that bound stays (clientBound), on runTmux's own time and no stand-in's, for both exits: the
// client records when the kernel made its process (testexec.MarkStart) and the moment just before
// it exits (testexec.MarkNow), and what is left of the call is runTmux's, from the call to that
// start and from that exit to the return, less the pipeDrainDelay os/exec holds the pipes after
// the client's reap, which main's timeout, ending at that reap, never counted.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipes(t *testing.T) {
	const clientBound = time.Minute
	for name, exit := range map[string]int{"an exit 3": 3, "an exit 0": 0} {
		t.Run(name, func(t *testing.T) {
			timeout := testwait.Remaining(t)
			dir := t.TempDir()
			mark, exited := filepath.Join(dir, "started"), filepath.Join(dir, "exited")
			client := fakeTmuxClient(t, dir, testexec.MarkStart(shellprefix.Literal(mark))+"\necho answered\n"+heldChild(t)+"\n"+testexec.MarkNow(shellprefix.Literal(exited))+"\nexit "+strconv.Itoa(exit))
			r := &Runtime{tmuxPath: client, commandTimeout: timeout, clock: clock.Real(), environ: []string{"PATH=" + os.Getenv("PATH")}}

			called := testexec.SinceBoot(t)
			started := time.Now()
			res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
			returned := testexec.SinceBoot(t)
			elapsed := time.Since(started)

			if err != nil {
				t.Fatalf("runTmux = %v after %s, want the client's own exit %d", err, elapsed, exit)
			}
			if elapsed < pipeDrainDelay {
				t.Fatalf("runTmux returned after %s, inside pipeDrainDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if began, after := testexec.StartedAfter(t, mark, called), returned-exitMark(t, exited); began+after-pipeDrainDelay > clientBound {
				t.Errorf("runTmux spent %s of its own, past main's %s: %s from the call to the client's start, and %s from the client's exit to the return, less the %s os/exec holds the pipes (pipeDrainDelay)",
					began+after-pipeDrainDelay, clientBound, began, after, pipeDrainDelay)
			}
			if res.timedOut != 0 || res.exitCode != exit || strings.TrimSpace(res.stdout) != "answered" {
				t.Errorf("runTmux = exit %d, timed out %s, stdout %q after %s; want exit %d, no timeout, and the answer: the client exited on its own, its timeout far away",
					res.exitCode, res.timedOut, res.stdout, elapsed, exit)
			}
		})
	}
}

// A tmux client that exits on its own did not time out, however much of pipeDrainDelay, which
// then holds runTmux open for something still holding the client's pipes, passes after its
// timeout: a runTmux that read a timeout from the deadline having passed would report the
// client's own exit 3 as a timeout. The timeout is main's, 100 ms short of pipeDrainDelay, so a
// run whose client exits inside it returns past it.
//
// Only a run whose client was seen reaped before its timeout could pass shows this. Go reaps the
// client in Wait once it has exited, the deadline comes no sooner than the timeout after the row
// called runTmux, and a kill sent to a client Go has not reaped counts (procgroup.KilledByContext):
// a run reaped past the timeout reports the timeout's kill, as it should. Main's form judged every
// run, and two things bring the reap past the timeout: the client's own run, which a loaded
// machine slows, and runTmux's own time, from the call to the client's start and from the client's
// exit to its reap. The row tells them apart by facts of the run: the client records when the
// kernel made its process (testexec.MarkStart) and the moment just before it exits
// (testexec.MarkNow), and between the two is the client's own run, which nothing runTmux does
// moves. A run reaped inside the timeout is judged as main judged it. A run reaped past it holds
// runTmux's own time to main's timeout: a runTmux that spent that long starting the client or
// reaping it fails, as main's form failed it. Only where runTmux's own time stays inside the
// timeout did the client's own run carry the reap past it: that run is not judged, and the row
// runs the client again until a run is judged, within a minute. So is a run whose client never
// reached its exit mark, which the timeout's kill ended in its own run, once runTmux's time to its
// start is inside the timeout.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipesPastIt(t *testing.T) {
	const timeout = pipeDrainDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, pipeDrainDelay+time.Second)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader, mark, exited := filepath.Join(dir, "leader"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		client := fakeTmuxClient(t, dir, testexec.MarkStart(shellprefix.Literal(mark))+"\necho $$ >"+shellprefix.Literal(leader)+"\necho answered\n"+heldChild(t)+"\n"+testexec.MarkNow(shellprefix.Literal(exited))+"\nexit 3")
		r := &Runtime{tmuxPath: client, commandTimeout: timeout, clock: clock.Real(), environ: []string{"PATH=" + os.Getenv("PATH")}}
		reaped := make(chan time.Time, 1)
		go func() {
			defer close(reaped)
			if testwait.Holds(within, testwait.Poll, func() bool {
				pid := pidIn(leader)
				return pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
			}) {
				reaped <- time.Now()
			}
		}()

		calledBoot := testexec.SinceBoot(t)
		called := time.Now()
		res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
		elapsed := time.Since(called)
		at, seen := <-reaped

		if !seen {
			t.Fatalf("run %d: the client was not seen reaped within %s (runTmux = exit %d, timed out %s, error %v after %s)",
				runs, within, res.exitCode, res.timedOut, err, elapsed)
		}
		if !at.Before(called.Add(timeout)) {
			// runTmux's own time: from the call to the client's start, and from the client's exit
			// to its reap. Between the two is the client's own run.
			began := testexec.StartedAfter(t, mark, calledBoot)
			own, ran := began, "never reached its exit mark: the timeout's kill ended it in its own run"
			if exit, unread := testexec.ReadNowMark(exited); unread == nil {
				own += calledBoot + at.Sub(called) - exit
				ran = fmt.Sprintf("took %s from its start to its exit", exit-calledBoot-began)
			}
			if own >= timeout {
				t.Fatalf("run %d: runTmux spent %s of its own before the client's reap, at or past main's %s timeout: %s from the call to the client's start, and %s from the client's exit to its reap, which the row saw %s after the call (runTmux = exit %d, timed out %s, error %v after %s)",
					runs, own, timeout, began, own-began, at.Sub(called), res.exitCode, res.timedOut, err, elapsed)
			}
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the client was seen reaped %s after the call, past its %s timeout, with runTmux's own time %s inside it: the client's own run %s", runs, at.Sub(called), timeout, own, ran)
				continue
			}
			t.Fatalf("no run judged in %d runs over %s: in each the client was seen reaped past its %s timeout with runTmux's own time inside it; in the last, runTmux's own time was %s, and the client's own run %s", runs, within, timeout, own, ran)
		}
		if err != nil {
			t.Fatalf("run %d: runTmux = %v after %s, want the client's own exit 3", runs, err, elapsed)
		}
		if elapsed < pipeDrainDelay {
			t.Fatalf("run %d: runTmux returned after %s, inside pipeDrainDelay: nothing held the pipe, so this run proves nothing", runs, elapsed)
		}
		if res.timedOut != 0 || res.exitCode != 3 || strings.TrimSpace(res.stdout) != "answered" {
			t.Errorf("run %d: runTmux = exit %d, timed out %s, stdout %q after %s; want exit 3, no timeout, and the answer: the client was reaped before its %s timeout could pass",
				runs, res.exitCode, res.timedOut, res.stdout, elapsed, timeout)
		}
		return
	}
}

// A message about a client its timeout killed names the timeout and no exit: the -1 runTmux
// records is no status the client chose, and a message that printed it read `new-session failed
// (exit -1): timed out after 1s`. A client that exited keeps its exit and its stderr in both
// message forms. A live run that its timeout kills, and the result it answers, is the clock row's
// (TestTheCommandTimeoutRunsOnTheConfiguredClockAndOnTheWallClockWhenThereIsNone).
func TestAMessageAboutATimedOutClientNamesTheTimeoutAndNoExit(t *testing.T) {
	timedOut := result{exitCode: -1, timedOut: time.Second}
	if got := "tmux new-session" + failed(timedOut); got != "tmux new-session timed out after 1s" {
		t.Errorf("failed = %q, want the timeout and no exit", got)
	}
	if got := "list-panes" + exited(timedOut); got != "list-panes timed out after 1s" {
		t.Errorf("exited = %q, want the timeout and no exit", got)
	}
	answered := result{exitCode: 1, stderr: "no server running\n"}
	if got := "tmux new-session" + failed(answered); got != "tmux new-session failed (exit 1): no server running" {
		t.Errorf("failed = %q for a client that exited 1", got)
	}
	if got := "list-panes" + exited(answered); got != "list-panes exited 1: no server running" {
		t.Errorf("exited = %q for a client that exited 1", got)
	}
}

// The clock a tmux client's timeout runs on is Options.Clock, and the wall clock where a
// configuration names none, which is every daemon in production (config builds Options without
// one). The row drives both: a runtime with a fake clock holds a client that never exits until
// the row moves the fake past the client's deadline, which no span of the machine's own can
// bring on, and a runtime New built with no clock ends the same client itself. Both answer
// timedOut, as a result and not an error, and the one on the fake answers only after the move,
// so a seam wired to the wall clock in both places would fail this row at its first wait.
//
// The fake half judges a run whose client was running when its deadline came: runTmux arms the
// deadline before it starts the client, so the fake moves only once the client has written its
// marker, and a move before the start, which would give a start error, cannot happen. The wall
// half cannot order its timer after the start, so it judges each run by what the run answered:
// timedOut is the wall clock ending a client it started, whether or not the client's shell got as
// far as anything of its own, and a start error is a deadline that came before the client started,
// which says nothing about who ends a running client, so that run is run again.
func TestTheCommandTimeoutRunsOnTheConfiguredClockAndOnTheWallClockWhenThereIsNone(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	client := fakeTmuxClient(t, dir, "echo $$ >"+started+"; exec sleep 300")
	argv := []string{"tmux", "-L", "legion-test", "list-panes"}
	fake := clocktest.New(time.Now())
	onFake := &Runtime{tmuxPath: client, commandTimeout: time.Minute, clock: fake, environ: []string{"PATH=" + os.Getenv("PATH")}}
	ran := startRunTmux(t, onFake, argv)
	awaitClientStarted(t, started, ran)
	got := passDeadline(t, fake, fake.Now().Add(time.Minute), "the run on the fake clock", ran)
	if got.err != nil || got.res.timedOut != time.Minute || got.res.exitCode != -1 {
		t.Errorf("the run on the fake clock = %v; want exit -1, timed out 1m, no error", got)
	}
	if message := "list-panes" + exited(got.res); message != "list-panes timed out after 1m0s" {
		t.Errorf("exited = %q for the run on the fake clock, want the timeout and no exit", message)
	}

	// No clock: New builds the runtime from Options with none, as config builds every daemon's,
	// and the wall clock ends the client.
	onWall := newFakeServerRuntime(t, staticCredential, nil).Runtime
	if onWall.clock != clock.Real() {
		t.Errorf("a runtime New built with no Options.Clock runs on %T, want clock.Real()", onWall.clock)
	}
	onWall.commandTimeout = 200 * time.Millisecond
	onWall.tmuxPath, onWall.environ = fakeTmuxClient(t, t.TempDir(), "exec sleep 300"), []string{"PATH=" + os.Getenv("PATH")}
	attempt := 0
	got = testwait.Until(t, "a run with no clock whose client started before its 200ms deadline", func() (ranTmux, bool) {
		attempt++
		res, err := onWall.runTmux(testwait.Context(t), argv)
		if err != nil && errors.Is(err, context.DeadlineExceeded) && res == (result{}) {
			t.Logf("run %d with no clock: the 200ms deadline came before the client started (%v); running again", attempt, err)
			return ranTmux{err: fmt.Errorf("start error on run %d, the 200ms deadline before the client started: %w", attempt, err)}, false
		}
		t.Logf("run %d with no clock: the client started and the run answered %v", attempt, ranTmux{res, err})
		return ranTmux{res, err}, true
	})
	if got.err != nil || got.res.timedOut != 200*time.Millisecond || got.res.exitCode != -1 {
		t.Errorf("the run with no clock = %v; want the wall clock's own timeout: exit -1, timed out 200ms, no error", got)
	}
}

// ranTmux is what one runTmux answered.
type ranTmux struct {
	res result
	err error
}

// String is the answer as a reader needs it: an error with no result (a start error) says only
// that, and a result names its exit, its timeout and the signal that ended it, so a client a
// signal killed reads apart from one that exited or timed out.
func (r ranTmux) String() string {
	if r.res == (result{}) && r.err != nil {
		return "no result, error: " + r.err.Error()
	}
	return fmt.Sprintf("exit %d, timed out %s, signal %q, error %v", r.res.exitCode, r.res.timedOut, r.res.signal, r.err)
}

// startRunTmux runs r.runTmux(argv) on a goroutine of its own (startCall) and answers the channel
// its one answer arrives on.
func startRunTmux(t *testing.T, r *Runtime, argv []string) <-chan ranTmux {
	t.Helper()
	return startCall(t, func(ctx context.Context) ranTmux {
		res, err := r.runTmux(ctx, argv)
		return ranTmux{res, err}
	})
}

// startCall runs call on a goroutine of its own and answers the channel its one answer arrives
// on. The call is the row's to end on every exit: the row's cleanup cancels the call's context and
// waits for it to return, so a row that fails before it moves a fake clock leaves no call waiting
// on a clock nobody moves, and no client running. That wait is testwait's, bounded by the test
// binary's deadline: a call that blocks outside its context (a read that never returns) fails the
// row by name in its cleanup, rather than holding the binary to its alarm with no FAIL line. After
// an earlier wait of the row gave up, the deadline is spent and the join gives up at once, the
// second give-up line in time order, as testwait's are.
func startCall[T any](t *testing.T, call func(context.Context) T) <-chan T {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	answered, joined := make(chan T, 1), make(chan struct{})
	go func() {
		defer close(joined)
		answered <- call(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the call to return once the row's cleanup cancelled its context", joined)
	})
	return answered
}

// awaitClientStarted waits until a fake tmux client has written marker, the fact that it runs,
// and fails t at once if the run answers first. runTmux arms its deadline before it starts the
// client, so a row that moved a fake clock on the arm alone could end the run's context before
// the client started, and the run would answer a start error, not a timeout.
func awaitClientStarted(t *testing.T, marker string, ran <-chan ranTmux) {
	t.Helper()
	testwait.Eventually(t, "the tmux client running (it writes "+marker+")", func() bool {
		select {
		case got := <-ran:
			t.Fatalf("the run answered %+v, %v before its client started", got.res, got.err)
		default:
		}
		_, err := os.Stat(marker)
		return err == nil
	})
}

// passDeadline waits until deadline is armed on fake, and fails t at once if the call under test,
// which what names, answers on returned first; then it moves fake to deadline and answers what the
// call returns. The release, the resume and the command timeout rows each wait so for the one
// deadline they are about.
func passDeadline[T any](t *testing.T, fake *clocktest.Fake, deadline time.Time, what string, returned <-chan T) T {
	t.Helper()
	at := deadline.Format(time.RFC3339Nano)
	testwait.Until(t, what+": its deadline at "+at+" armed on the fake clock", func() ([]time.Time, bool) {
		select {
		case got := <-returned:
			t.Fatalf("%s answered %+v before its deadline at %s was armed and passed", what, got, at)
		default:
		}
		armed := fake.Armed()
		return armed, slices.Contains(armed, deadline)
	})
	fake.Set(deadline)
	return testwait.Receive(t, what+" once the fake clock passed "+at, returned)
}

// A client a signal ended is a failed run, as a client that exited non-zero is, so every caller
// reads it as main read its -1 exit; a message about it names the signal and keeps what the client
// printed (failed, exited), where it said `failed (exit -1)`.
func TestRunTmuxReadsAClientASignalEndedAsAFailedRunNamingTheSignal(t *testing.T) {
	client := fakeTmuxClient(t, t.TempDir(), "echo 'no server running on /tmp/tmux-1000/legion-test' >&2; kill -KILL $$")
	r := &Runtime{tmuxPath: client, commandTimeout: time.Minute, clock: clock.Real(), environ: []string{"PATH=" + os.Getenv("PATH")}}

	res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})

	if err != nil || res.exitCode != -1 || res.signal != "signal: killed" {
		t.Fatalf("runTmux = %+v, %v; want a result with exit -1 and the signal, and no error", res, err)
	}
	if want := "tmux list-panes was ended by signal: killed: no server running on /tmp/tmux-1000/legion-test"; "tmux list-panes"+failed(res) != want {
		t.Errorf("failed = %q, want %q", "tmux list-panes"+failed(res), want)
	}
	if want := " was ended by signal: killed: no server running on /tmp/tmux-1000/legion-test"; exited(res) != want {
		t.Errorf("exited = %q, want %q", exited(res), want)
	}
}

// A stop's wait for a pane to go keeps waiting through a list-panes client a signal ended, as it
// waits through any listing that proves nothing, and ends once tmux says the pane is gone: the
// stand-in client dies of SIGKILL the first time and answers `can't find pane` after that.
func TestAStopsWaitOutlivesAListingASignalEnded(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	client := fakeTmuxClient(t, dir, "if [ -e '"+seen+"' ]; then echo \"can't find pane: %1\" >&2; exit 1; fi\n: >'"+seen+"'; kill -KILL $$")
	r := &Runtime{tmuxPath: client, socket: "legion-test", commandTimeout: time.Minute, clock: clock.Real(), environ: []string{"PATH=" + os.Getenv("PATH")}}
	r.run = r.runTmux

	v, err := r.awaitNotRunning(context.Background(), "%1", incarnation{pid: 4242, ticks: 1}, 10*time.Second)

	if err != nil || v.reason != reasonPaneGone {
		t.Errorf("awaitNotRunning = %+v, %v; want the pane gone and no error", v, err)
	}
}

// A pane still tmux's fork when the command timeout runs out is given up on, with the error that
// names the timeout, and the launch then kills the pane (killUnrecorded): awaitPaneCommand's bound
// runs on the runtime's clock. The pane here is the server's copy for good, its stat reading the
// server's comm as its parent's does. The bound is a hundred years, which no test binary's
// -timeout reaches, -timeout 0 included, so only a move of the runtime's clock can end the wait:
// the row moves the fake once the wait has read the pane several times, at the machine's pace with
// the fake standing still, and a bound read from the wall clock would leave the wait reading until
// the row gives up by name, or, under -timeout 0, for ever.
func TestAPaneStillTheServersForkIsGivenUpOnWhenTheCommandTimeoutRunsOut(t *testing.T) {
	const bound = 100 * 365 * 24 * time.Hour
	fake := clocktest.New(time.Now())
	var reads atomic.Int64
	r := &Runtime{commandTimeout: bound, clock: fake, readProc: func(path string) ([]byte, error) {
		reads.Add(1)
		switch path {
		case "/proc/4242/stat":
			return []byte("4242 (tmux: server) S 100 4242 4242 0 -1"), nil
		case "/proc/100/stat":
			return []byte("100 (tmux: server) S 1 100 100 0 -1"), nil
		}
		return nil, fmt.Errorf("the row reads no %s", path)
	}}
	waited := startCall(t, func(ctx context.Context) error {
		return r.awaitPaneCommand(ctx, paneReport{window: "@1", pane: "%1", pid: 4242})
	})
	// Each pass of the wait reads the pane's stat and its parent's.
	testwait.Eventually(t, "the wait reading the pane five times while the fake clock stands still", func() bool {
		select {
		case err := <-waited:
			t.Fatalf("awaitPaneCommand answered %v before the clock moved past its %s bound", err, bound)
		default:
		}
		return reads.Load() >= 10
	})
	fake.Advance(bound + time.Nanosecond)
	err := testwait.Receive(t, "awaitPaneCommand once the clock passed its "+bound.String()+" bound", waited)
	if want := "pane %1 pid 4242 did not start its command within " + bound.String(); err == nil || err.Error() != want {
		t.Errorf("awaitPaneCommand = %v, want %q", err, want)
	}
}

// A kill of a pane whose process was never recorded that its timeout ended says so, rather than
// that tmux exited -1 with nothing on stderr.
func TestKillUnrecordedNamesItsTimeoutNotAnExit(t *testing.T) {
	r := &Runtime{socket: "legion-test", run: func(context.Context, []string) (result, error) {
		return result{exitCode: -1, timedOut: 10 * time.Second}, nil
	}}

	err := r.killUnrecorded(context.Background(), paneReport{pane: "%1", pid: 42})

	if want := "kill pane %1, whose process pid 42 was never recorded: tmux timed out after 10s"; err == nil || err.Error() != want {
		t.Errorf("killUnrecorded = %v, want %q", err, want)
	}
}

// A tmux client that fails after new-window or split-window opened its pane, by its exit or by a
// signal ending it, leaves no pane the daemon does not own. openWindowIn kills the window it opened
// when its ownership marker fails, and when the bootstrap window's cleanup fails after new-window
// opened the agent's window, where it left that window running; splitWindow keeps the pane it
// split when select-layout fails, so the launch records it.
func TestATmuxClientThatFailsAfterThePaneOpensLeavesNoUnownedPane(t *testing.T) {
	// calls records each tmux command a run makes, the target of a kill-window beside its verb.
	// dies names the command that fails: a verb, or `kill-window <target>`; exits says whether it
	// exits 1 rather than dying of a signal.
	runner := func(dies string, exits bool, calls *[]string) func(context.Context, []string) (result, error) {
		return func(_ context.Context, argv []string) (result, error) {
			command := verb(argv)
			if command == "kill-window" {
				command += " " + argv[len(argv)-1]
			}
			*calls = append(*calls, command)
			switch {
			case command == dies && exits:
				return result{exitCode: 1, stderr: "can't find window"}, nil
			case command == dies:
				return result{exitCode: -1, signal: "signal: killed", stderr: "lost server"}, nil
			case command == "new-window":
				return result{stdout: "@7 %9 4242\n"}, nil
			case command == "split-window":
				return result{stdout: "%9 4242\n"}, nil
			}
			return result{}, nil
		}
	}
	bootstrap := "kill-window legion-test:" + bootstrapWindow
	for _, exits := range []bool{false, true} {
		t.Run(fmt.Sprintf("the bootstrap window's cleanup, exiting %t", exits), func(t *testing.T) {
			var calls []string
			r := &Runtime{socket: "legion-test", run: runner(bootstrap, exits, &calls)}
			if _, err := r.openWindowIn(context.Background(), "tree", []string{"true"}, true); err == nil {
				t.Error("openWindowIn = nil, want the cleanup's failure")
			}
			if want := []string{"new-window", bootstrap, "kill-window @7"}; !slices.Equal(calls, want) {
				t.Errorf("tmux ran %q, want %q: the window new-window opened is left open", calls, want)
			}
		})
	}
	t.Run("the window ownership marker", func(t *testing.T) {
		var calls []string
		r := &Runtime{socket: "legion-test", run: runner("set-option", false, &calls)}
		_, err := r.openWindowIn(context.Background(), "tree", []string{"true"}, false)
		if want := "tmux window ownership marker was ended by signal: killed: lost server"; err == nil || err.Error() != want {
			t.Errorf("openWindowIn = %v, want %q", err, want)
		}
		if want := []string{"new-window", "set-option", "kill-window @7"}; !slices.Equal(calls, want) {
			t.Errorf("tmux ran %q, want %q: the window the marker could not own is left open", calls, want)
		}
	})
	// The launch's context ends during the marker, and the kill of the window it opened is then
	// ended by a signal: the kill still runs, and the launch error says the window may still run,
	// through openWindow too, which tries no second look for the session once the context ended.
	t.Run("a kill of the opened window a signal ended, its launch's context gone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls []string
		r := &Runtime{socket: "legion-test", run: func(ctx context.Context, argv []string) (result, error) {
			if ctx.Err() != nil {
				return result{}, ctx.Err()
			}
			command := verb(argv)
			calls = append(calls, command)
			switch command {
			case "has-session":
				return result{}, nil
			case "new-window":
				return result{stdout: "@7 %9 4242\n"}, nil
			case "set-option":
				cancel()
				return result{}, context.Canceled
			}
			return result{exitCode: -1, signal: "signal: killed", stderr: "lost server"}, nil
		}}
		_, err := r.openWindow(ctx, "tree", []string{"true"})
		if want := "context canceled\nkill window @7, which the failed launch opened: tmux was ended by signal: killed: lost server"; err == nil || err.Error() != want {
			t.Errorf("openWindow = %v, want it to say %q", err, want)
		}
		if want := []string{"has-session", "new-window", "set-option", "kill-window"}; !slices.Equal(calls, want) {
			t.Errorf("tmux ran %q, want %q: the kill must run once the launch's context ended", calls, want)
		}
	})
	// With the launch's context alive, a window whose marker failed and whose kill a signal ended
	// sends openWindow to look for the session again; when that look fails too, the launch error
	// keeps the first failure, kill report and all, beside the second.
	t.Run("a kill of the opened window a signal ended, its second session check failing", func(t *testing.T) {
		var calls []string
		sessions := 0
		r := &Runtime{socket: "legion-test", run: func(_ context.Context, argv []string) (result, error) {
			command := verb(argv)
			calls = append(calls, command)
			switch command {
			case "has-session":
				sessions++
				if sessions > 1 {
					return result{exitCode: 1, stderr: "can't find session"}, nil
				}
				return result{}, nil
			case "new-window":
				return result{stdout: "@7 %9 4242\n"}, nil
			case "set-option":
				return result{exitCode: 1, stderr: "invalid option"}, nil
			case "new-session":
				return result{exitCode: 1, stderr: "create window failed"}, nil
			}
			return result{exitCode: -1, signal: "signal: killed", stderr: "lost server"}, nil
		}}
		_, err := r.openWindow(context.Background(), "tree", []string{"true"})
		want := "tmux window ownership marker failed (exit 1): invalid option\n" +
			"kill window @7, which the failed launch opened: tmux was ended by signal: killed: lost server\n" +
			"tmux new-session failed (exit 1): create window failed"
		if err == nil || err.Error() != want {
			t.Errorf("openWindow = %v, want %q", err, want)
		}
		if want := []string{"has-session", "new-window", "set-option", "kill-window", "has-session", "new-session"}; !slices.Equal(calls, want) {
			t.Errorf("tmux ran %q, want %q", calls, want)
		}
	})
	t.Run("select-layout after a split", func(t *testing.T) {
		var calls []string
		var logs strings.Builder
		r := &Runtime{socket: "legion-test", run: runner("select-layout", false, &calls), log: slog.New(slog.NewTextHandler(&logs, nil))}
		report, err := r.splitWindow(context.Background(), "@7", []string{"true"})
		if err != nil || report != (paneReport{window: "@7", pane: "%9", pid: 4242}) {
			t.Errorf("splitWindow = %+v, %v; want the split pane and no error: an error leaves the pane unowned", report, err)
		}
		if !strings.Contains(logs.String(), "select-layout failed after a split") || !strings.Contains(logs.String(), "signal: killed") {
			t.Errorf("logged %q, want the select-layout failure with its signal", logs.String())
		}
	})
	t.Run("select-layout that cannot run after a split", func(t *testing.T) {
		var logs strings.Builder
		r := &Runtime{socket: "legion-test", log: slog.New(slog.NewTextHandler(&logs, nil)), run: func(_ context.Context, argv []string) (result, error) {
			if verb(argv) == "select-layout" {
				return result{}, context.DeadlineExceeded
			}
			return result{stdout: "%9 4242\n"}, nil
		}}
		report, err := r.splitWindow(context.Background(), "@7", []string{"true"})
		if err != nil || report != (paneReport{window: "@7", pane: "%9", pid: 4242}) {
			t.Errorf("splitWindow = %+v, %v; want the split pane and no error: an error leaves the pane unowned", report, err)
		}
		if !strings.Contains(logs.String(), "context deadline exceeded") {
			t.Errorf("logged %q, want the select-layout failure", logs.String())
		}
	})
}

// fakeTmuxClient writes an executable that runs body under sh in place of a tmux client, under the
// fork lock (testexec.WriteFile).
func fakeTmuxClient(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "tmux")
	if err := testexec.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write the fake tmux client: %v", err)
	}
	return path
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (heldChild's child, standing in for what holds a client's pipes).
func killPIDFile(pidFile string) {
	if pid := pidIn(pidFile); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// heldChild is lines of a fake client's shell script that start a child holding the client's
// output pipes, as a tmux server that took the client's stdio can, and go on once the child has
// written its pid. The child runs until the test's cleanup writes its release, and kills it, or
// until this test process is gone: never for a span of its own, so no run ends, and no assertion
// holds, by the child's own exit. The child's paths reach it as arguments, quoted once
// (shellprefix.Literal).
func heldChild(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pidFile, release, holder := filepath.Join(dir, "pid"), filepath.Join(dir, "release"), filepath.Join(dir, "hold")
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the held child: %v", err)
		}
		killPIDFile(pidFile)
	})
	script := fmt.Sprintf("echo $$ >\"$1.tmp\" && mv \"$1.tmp\" \"$1\"\nwhile [ ! -e \"$2\" ] && kill -0 %d 2>/dev/null; do sleep 0.05; done\n", os.Getpid())
	if err := os.WriteFile(holder, []byte(script), 0o600); err != nil {
		t.Fatalf("write the held child's script: %v", err)
	}
	return "sh " + shellprefix.Literal(holder) + " " + shellprefix.Literal(pidFile) + " " + shellprefix.Literal(release) + " &\nwhile [ ! -s " + shellprefix.Literal(pidFile) + " ]; do sleep 0.01; done"
}

// pidIn is the pid a client's shell wrote to path, once the whole line is there, and 0 before.
func pidIn(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "\n") {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// exitMark is the moment, as a time since boot, a fake client recorded at path just before it
// exited (testexec.MarkNow). A client that wrote none never reached its exit, and the row fails
// saying so.
func exitMark(t *testing.T, path string) time.Duration {
	t.Helper()
	at, err := testexec.ReadNowMark(path)
	if err != nil {
		t.Fatalf("read the moment the client exited, which it records just before its exit: %v", err)
	}
	return at
}
