package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// inFlight runs f on a goroutine of its own and answers the channel its answer arrives on, for a
// row that waits, under a bound of its own (testwait.Receive), for a call that may never return.
func inFlight[T any](f func() T) <-chan T {
	answered := make(chan T, 1)
	go func() { answered <- f() }()
	return answered
}

// commandLives keeps a test's command running until a signal ends it. A shell runs a trap when the
// command it is waiting for ends, so the command waits in a loop of short foreground sleeps rather
// than in one long one: a SIGTERM it handles is then acted on within one sleep of its arrival,
// well inside the grace. The sleeps are foreground, and never a loop of `sleep & wait $!`: a
// `wait` returns on the group's own SIGTERM and the loop then forks its next sleep, which that
// signal has already passed by, so a trap that leaves would leave that sleep behind holding the
// command's output pipes. A trap of the empty action on TERM ignores SIGTERM outright, and a
// process started under that disposition inherits it, so an ignoring command's own sleeps and
// helpers ignore SIGTERM too.
//
// It gives up after a minute, which no row here reaches: each row's stop comes once its command has
// reported that it is running — RunCommand's budget passes then (budgetPassedOnceExists), and
// execRunner's git has a Timeout of its own that passes only after a git has reported it whose
// runner kept to main's second and which the machine held for less than a second more
// (stopTimeout) — so the stop under test is what ends each of these commands. It is what a run
// whose stop never came leaves behind — a command that ignores SIGTERM stays until something kills
// it, and a failing row has nothing left to kill it with. Every row reads a timeout, which a
// command that gave up on its own never reports, so no row passes by that end.
const commandLives = `ticks=0; while [ "$ticks" -lt 1200 ]; do sleep 0.05; ticks=$((ticks+1)); done`

// RunCommand's jj and git are stopped the way execRunner's are: the budget's end sends SIGTERM to
// the command's own process group, which is the group procgroup.Configure gives it and which
// RunCommand never had before dispatch://INFRA-3098, waits the grace, and sends SIGKILL only
// after it. The three cases are the three things that stop is: a command that handles SIGTERM
// removes the lock it holds and the run ends well inside the grace; a helper the command forked is
// stopped with it, which only a group can do; and a command that handles SIGTERM and exits 0 at
// the end of its budget is still reported as a timeout rather than as a command that could not be
// run, which is execRunner.Run reading its kill before it propagates the run's own error.
//
// RunCommand's rows pass their budget at a fact of the run, not at a time: once the command has
// reported that it is running, its trap installed and its helper ready (budgetPassedOnceExists). A
// budget counted from before the command started could pass, on a loaded machine, before the
// command had reached that step, and the stop would then find a command with no trap yet, or a
// helper not yet ignoring SIGTERM. RunCommand's report still names the second its caller gave it
// as the budget. execRunner's row hands its git a caller's context that never ends, so the git's
// own Timeout is the only thing that can stop it, as in main's row (stopTimeout).
//
// Each row holds the runner to the second main gave it, from just before the call to the moment
// the command reports that it is running (ownBound, ownSpan), whatever the runner spent that
// second on: before it starts the command, or inside the command's own process before the
// command's first line, such as a prefix that sleeps and then execs it. Of that span the row takes
// off only what the kernel records of the machine: how long it kept the command's processes
// waiting for a CPU on the way (their run-queue wait, markWaited), which a loaded machine
// lengthens and which no runner spends on its own work. A runner that sleeps or works, and a
// command it stops or holds blocked on anything but a CPU, waits for no CPU, so that time stays
// in the span. Of the wait it takes off, the row counts back against the runner as much CPU as
// this test process's own threads used over the call (RUSAGE_SELF, which counts no process it
// started), since the command could have waited behind them: a runner whose own threads keep the
// CPUs busy while its command starts is slow, not loaded. A kernel that keeps no run-queue wait,
// as gVisor's, has nothing taken off, and the row is main's.
func TestRunCommandStopsItsCommandTheSameWay(t *testing.T) {
	environment := []string{"PATH=" + os.Getenv("PATH")}

	t.Run("a command that handles SIGTERM removes its lock", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready, mark := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready"), filepath.Join(dir, "started")
		readyAt := filepath.Join(dir, "ready-at")
		budget := time.Second
		ctx := budgetPassedOnceExists(t, ready)
		// The lock is taken before the command reports that it is running, so a lock that is gone
		// at the end is one the command's own SIGTERM handler removed.
		// DIAG (never merged): the leader writes its pid at $5.pid, and its trap writes, at $5.trap,
		// the uptime and its own schedstat line as it begins and again once its rm is done.
		diag := filepath.Join(dir, "diag")
		script := testexec.MarkStart(`"$3"`) + `; printf '%s\n' "$$" >"$5.pid"; : >"$1"; trap 'read -r tu </proc/uptime; read -r ts </proc/self/schedstat; rm -f "$1"; read -r ou </proc/uptime; read -r os </proc/self/schedstat; printf "%s\n%s\n%s\n%s\n" "$tu" "$ts" "$ou" "$os" >"$5.trap"; exit 0' TERM; ` + markWaited("self", `"$4"`) + `; : >"$2"; ` + commandLives

		// DIAG (never merged): no hold: no busy loop, and the command runs as the row's own does,
		// with no nice and no pin, so the watch sees the row's own stop on this machine.
		h := holdOnCPU0(t, 0, ready)
		w := watchStop(t, ready, diag+".pid", h)
		t.Logf("DIAG pressure before the call: %s", pressure(dir))
		anchorWall, anchorBoot := time.Now(), bootNow()
		call := calling(t)
		started := time.Now()
		_, err := RunCommand(ctx, procgroup.NoReaper, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", lock, ready, mark, readyAt, diag})
		returned := time.Now()
		cpu := processCPU(t)
		elapsed := returned.Sub(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): its budget never passed, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		began := testexec.StartedAfter(t, mark, call.at)
		reached, waits := readWaited(t, readyAt, 1)
		waited, known := runQueueWait(t, waits[0])
		span := ownSince(call, reached, waited, known, cpu)
		if span.own > ownBound {
			t.Errorf("RunCommand spent %s of its own before its command reported that it was running, past main's %s: %s (RunCommand = %v)", span.own, ownBound, span, err)
		}
		t.Logf("RunCommand started its command %s after the call, and spent %s of its own before the command reported that it was running: %s", began, span.own, span)
		passed, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("the command reported that it was running, but its budget had not passed when the run returned after %s (RunCommand = %v)", elapsed, err)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the command held is still there (%v) after %s: the budget's end gave it no SIGTERM to remove it on (RunCommand = %v)", statErr, elapsed, err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v after %s, want \"Command timed out after 1 s …\": the budget ended the command, however cleanly it then left", err, elapsed)
		}
		stopped := returned.Sub(passed)
		if stopped > procgroup.StopGrace/2 {
			t.Errorf("the run returned %s after its budget passed, past half the %s grace: a command that leaves on its SIGTERM must end the grace, not sit out its whole span", stopped, procgroup.StopGrace)
		}
		t.Logf("DIAG the run returned %s after its budget passed", stopped)
		t.Logf("DIAG pressure after the run: %s", pressure(dir))
		timeline(t, anchorBoot+passed.Sub(anchorWall), anchorBoot+returned.Sub(anchorWall), reached, waits[0], diag+".trap", h, w)
	})

	t.Run("a helper the command forked is stopped with it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper-of-this-row")
		trapped, helperWaited, mark := filepath.Join(dir, "helper-trapped"), filepath.Join(dir, "helper-waited"), filepath.Join(dir, "started")
		readyAt := filepath.Join(dir, "ready-at")
		if err := syscall.Mkfifo(trapped, 0o600); err != nil {
			t.Fatalf("make the pipe the helper gives its word on: %v", err)
		}
		budget := time.Second
		ctx := budgetPassedOnceExists(t, ready)
		// Both ignore SIGTERM, so the group's SIGKILL after the grace is what ends them, and the
		// helper carries a path of this row's own temporary directory, which is what names it in
		// /proc. Without a group of its own the command's own kill reaches the leader alone and
		// this helper — a git-remote-https, in a real fetch — keeps running with the pipes open.
		// The helper is forked before the command ignores SIGTERM: the ignore is inherited across
		// fork and exec, and a shell cannot trap a signal that was already ignored when it
		// started, so a helper forked after that line would ignore SIGTERM whatever its own script
		// says, and this row would prove nothing about the helper's own disposition.
		//
		// The fork returns as soon as the helper exists, which is before that helper's own shell
		// has reached its trap line, so the helper gives its word once the ignore is installed, by
		// opening the named pipe trapped, and the leader, which opens the other end, waits there
		// before it ignores SIGTERM itself and reports that it is running: the budget passes once
		// the ready file is there, so its SIGTERM finds the helper under the disposition its
		// script asks for. A stop that raced that line would end the helper with the group's
		// SIGTERM rather than leave it to the SIGKILL, and this row — which reads only that the
		// helper is gone — would pass on the opposite of what it claims. The wait is one open of a
		// pipe, not a loop of sleeps: a sleep is a process of its own, whose run-queue wait no
		// record of the leader's or the helper's holds (waitedPastHelper).
		//
		// The helper holds the command's pipes until this row's directory is gone, which the
		// test's cleanup removes, or until this test process is gone: never for a span of its own,
		// so a helper nothing stopped is still there however long the row waits for it, and a run
		// that waits on its pipes for ever fails the wait below rather than ending with the helper.
		helper := `trap '' TERM; ` + waitInto("waited", "self") + `; printf '%s\n' "$waited" >"$3"; : >"$2"; while [ -d '` + dir + `' ] && kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; do sleep 0.05; done`
		script := testexec.MarkStart(`"$6"`) + `; ` + waitInto("forked", "self") + `; sh -c "$2" sh "$3" "$4" "$5" & read -r word <"$4"; ` + waitInto("heard", "self") + `; trap '' TERM; ` + markWaited("self", `"$7"`, `"$forked"`, `"$heard"`) + `; : >"$1"; ` + commandLives

		returned := make(chan error, 1)
		call := calling(t)
		started := time.Now()
		go func() {
			_, err := RunCommand(ctx, procgroup.NoReaper, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", ready, helper, marker, trapped, helperWaited, mark, readyAt})
			returned <- err
		}()
		var err error
		select {
		case err = <-returned:
		case <-time.After(testwait.Bounded(t, time.Minute, cutRunMargin)):
			t.Fatalf("RunCommand had not returned %s after it started: nothing stopped the helper the command forked, which still holds the command's pipes (%v)", time.Since(started), gitProcessesUnder(t, marker))
		}
		cpu := processCPU(t)
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): its budget never passed, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		began := testexec.StartedAfter(t, mark, call.at)
		reached, waits := readWaited(t, readyAt, 3)
		helperWait, readErr := os.ReadFile(helperWaited)
		if readErr != nil {
			t.Fatalf("read the run-queue wait the helper recorded before it gave its word: %v", readErr)
		}
		waited, known := waitedPastHelper(t, waits[0], waits[1], waits[2], strings.TrimSuffix(string(helperWait), "\n"))
		span := ownSince(call, reached, waited, known, cpu)
		if span.own > ownBound {
			t.Errorf("RunCommand spent %s of its own before its command reported that it was running, its helper ready, past main's %s: %s (RunCommand = %v)", span.own, ownBound, span, err)
		}
		t.Logf("RunCommand started its command %s after the call, and spent %s of its own before the command reported that it was running, its helper ready: %s", began, span.own, span)
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v after %s, want \"Command timed out after 1 s …\": the budget's stop ended a command that ignores SIGTERM", err, elapsed)
		}
		// The stop's SIGKILL reaches the helper with the leader, before the run ends, so five
		// seconds after the run's end is the signal's delivery and nothing else: a helper still
		// running then was signalled late, or not at all.
		helperGone := func() bool { return len(gitProcessesUnder(t, marker)) == 0 }
		if !testwait.Holds(5*time.Second, testwait.Poll, helperGone) {
			t.Errorf("the helper the command forked was still running 5s after the run ended in %s: %v (RunCommand = %v)", elapsed, gitProcessesUnder(t, marker), err)
		}
	})

	t.Run("execRunner reports a command that handled SIGTERM and exited 0 as timed out", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready, mark := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready"), filepath.Join(dir, "started")
		readyAt := filepath.Join(dir, "ready-at")
		fakeGit := filepath.Join(dir, "git")
		script := "#!/bin/sh\n" + testexec.MarkStart(`"`+mark+`"`) + "\n: >\"" + lock + "\"\ntrap 'rm -f \"" + lock + "\"; exit 0' TERM\n" + markWaited("self", `"`+readyAt+`"`) + "\n: >\"" + ready + "\"\n" + commandLives + "\n"
		if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		// The git's own Timeout is the only thing that can stop it: the caller's context never
		// ends, so a Run that bounds the git by its caller's context alone runs it until it gives
		// up on its own (commandLives), exit 0 and not timed out. The Timeout is stopTimeout, so a
		// git whose runner kept to main's second, and which the machine held for less than a
		// second more, has its trap installed when it passes.
		run := NewRunner(stopTimeout, map[string]string{"git": fakeGit})

		call := calling(t)
		started := time.Now()
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: stopTimeout})
		cpu := processCPU(t)
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			startMark, markErr := testexec.ReadStartMark(mark)
			t.Fatalf("the git never reported that it was running (%v) before its own %s Timeout stopped it, so this run proves nothing; a git whose runner keeps to main's %s, and which the machine holds for less than %s more, is running by then (its start mark %+v, %v; Run = %v, exit %d, timed out %t after %s)",
				statErr, stopTimeout, ownBound, stopTimeout-ownBound, startMark, markErr, err, result.ExitCode, result.TimedOut, elapsed)
		}
		began := testexec.StartedAfter(t, mark, call.at)
		reached, waits := readWaited(t, readyAt, 1)
		waited, known := runQueueWait(t, waits[0])
		span := ownSince(call, reached, waited, known, cpu)
		if span.own > ownBound {
			t.Errorf("execRunner.Run spent %s of its own before its git reported that it was running, past main's %s: %s (Run = %v)", span.own, ownBound, span, err)
		}
		t.Logf("execRunner.Run started its git %s after the call, and spent %s of its own before the git reported that it was running: %s", began, span.own, span)
		// exec reports a command whose Cancel succeeded and which then exited cleanly as the
		// context's error, so a Run that propagated that error before reading its own kill would
		// report this git as one it could not run, and transientCloneFailure would read the next
		// such clone as a retryable failure rather than as the timeout it is.
		if err != nil {
			t.Fatalf("Run = %v after %s, want no error: the git exited 0 on the timeout's SIGTERM, which is a timeout and not a failure to run", err, elapsed)
		}
		if !result.TimedOut || result.ExitCode != 0 {
			t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 0 and timed out: the git's own %s Timeout, the only thing here that can stop it, stopped a git that handled its SIGTERM and left", result.ExitCode, result.TimedOut, elapsed, stopTimeout)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the git held is still there (%v) after %s: the git's own %s Timeout gave it no SIGTERM to remove it on", statErr, elapsed, stopTimeout)
		}
	})
}

// budgetPassedOnceExists is a caller's context whose deadline passes as a budget's does
// (testwait.StandInDeadline: its Err and its cause context.DeadlineExceeded) once path exists,
// which the command under test writes once it has reached the step its stop must find it at: a
// trap installed, a helper ready. A budget counted from before the command started could pass, on
// a loaded machine, before the command had reached that step. Its Deadline, once
// it has passed, is the moment it passed. A path not there within a minute ends the context as a
// cancel, so a run still going then reads no budget passed, and the test's cleanup ends it the
// same way.
func budgetPassedOnceExists(t *testing.T, path string) context.Context {
	t.Helper()
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	bound := testwait.Bounded(t, time.Minute, cutRunMargin)
	go func() {
		there := testwait.Holds(bound, testwait.Poll, func() bool {
			if ctx.Err() != nil {
				return true
			}
			_, err := os.Stat(path)
			return err == nil
		})
		if there && ctx.Err() == nil {
			pass(context.DeadlineExceeded)
			return
		}
		cancel()
	}()
	return ctx
}

// ownBound is main's second: how much of its own time (ownSpan) these rows give the runner from
// just before the call to the moment its command reports that it is running, or, for the real git
// of TestARealGitStoppedAtItsBudgetLeavesNoLock, holds .git/index.lock.
const ownBound = time.Second

// stopTimeout is the Timeout execRunner's rows give their git, the only thing that can stop it:
// main's second of the runner's own time (ownBound), and a second more for what the rows take off
// that time, the time the machine kept the git waiting for a CPU. A git that reports it is
// running, or holds its lock, within its runner's second, on a machine that held it for less than
// a second more, has done so when the Timeout passes; one that has not fails its row by name.
const stopTimeout = 2 * ownBound

// aCall is the moment a row called its runner, as a time since boot (testexec.SinceBoot), and the
// CPU time this test process had used by then (processCPU).
type aCall struct{ at, cpu time.Duration }

// calling reads the moment of a call a row is about to make.
func calling(t *testing.T) aCall {
	t.Helper()
	return aCall{at: testexec.SinceBoot(t), cpu: processCPU(t)}
}

// ownSpan is the runner's own time from a call to a moment of its command's run (own): the span
// between the two, less held, the part of the command's run-queue wait on the way (waited) that
// the CPU this test process used over the call (cpu) does not cover. A kernel that keeps no
// run-queue wait (known false) has nothing taken off.
type ownSpan struct {
	own, span, waited, cpu, held time.Duration
	known                        bool
}

// ownSince is the runner's own time from call to reached, a moment of the command's run as a time
// since boot, where the kernel kept the command waiting for a CPU for waited on the way there, or
// keeps no such record (known false). cpuAfter is this test process's CPU time once the call had
// returned (processCPU), which covers every moment of the call's in which the command could have
// waited behind this process.
func ownSince(call aCall, reached, waited time.Duration, known bool, cpuAfter time.Duration) ownSpan {
	s := ownSpan{span: reached - call.at, cpu: cpuAfter - call.cpu, known: known}
	if known {
		s.waited = waited
		s.held = max(0, waited-s.cpu)
	}
	s.own = s.span - s.held
	return s
}

// String is what a row's message says of the span: what it counts, and what it takes off.
func (s ownSpan) String() string {
	if !s.known {
		return fmt.Sprintf("%s from the call, with nothing taken off: this kernel keeps no run-queue wait (/proc/<pid>/schedstat)", s.span)
	}
	return fmt.Sprintf("%s from the call, less %s the kernel kept the command waiting for a CPU (its run-queue wait, %s, less the %s of CPU this test process used over the call)", s.span, s.held, s.waited, s.cpu)
}

// processCPU is the CPU time this test process has used so far, user and system, every thread of
// it (getrusage(2), RUSAGE_SELF).
func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("read the CPU time this test process has used: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// waitInto is shell that reads, into the shell variable name, the /proc/<pid>/schedstat line of
// the process proc names (self, the shell's own, or a shell word such as $PPID): its run-queue
// wait so far (runQueueWait). It leaves the variable empty where this kernel keeps no such file,
// as gVisor's. The shell reads it itself (`read` and `[` are builtins), so self is its own
// process.
func waitInto(name, proc string) string {
	return name + `=; [ -r /proc/` + proc + `/schedstat ] && read -r ` + name + ` </proc/` + proc + `/schedstat`
}

// markWaited is shell that records, at the file the shell word at names, a moment of a stand-in's
// run, as testexec.MarkNow does, and, at the file named the same with .waits added, the
// /proc/<pid>/schedstat line of the process proc names (waitInto) as it stood at that moment, then
// the shell words more, one a line (readWaited). The kernel adds a wait to the line when the
// process gets a CPU back, so the line is read just before the moment and again just after it,
// and the pair stands only where both count the same wait: the machine did not send the process
// back to a run queue between the two reads, so the line counts every wait before the moment and
// none after it. A process the machine keeps sending back there is read again, up to 100 times,
// and after that the last line read before its moment stands, which counts less than the wait up
// to that moment, so the row takes less off.
func markWaited(proc, at string, more ...string) string {
	words := `"$waited"`
	for _, word := range more {
		words += " " + word
	}
	schedstat := `/proc/` + proc + `/schedstat`
	return `waited=; tries=0; while [ "$tries" -lt 100 ]; do tries=$((tries+1)); ` +
		`if [ ! -r ` + schedstat + ` ]; then waited=; read -r up </proc/uptime; break; fi; ` +
		`read -r waited <` + schedstat + `; read -r up </proc/uptime; read -r again <` + schedstat + `; ` +
		`w=${waited#* }; a=${again#* }; [ "${w%% *}" = "${a%% *}" ] && break; done; ` +
		`printf '%s\n' "$up" >` + at + `; printf '` + strings.Repeat(`%s\n`, 1+len(more)) + `' ` + words + ` >` + at + `.waits`
}

// readWaited reads what markWaited recorded at path: the moment, as a time since boot, and the
// lines of path.waits, lines of them, which the row fails without.
func readWaited(t *testing.T, path string, lines int) (time.Duration, []string) {
	t.Helper()
	at, err := testexec.ReadNowMark(path)
	if err != nil {
		t.Fatalf("read the moment the command recorded: %v", err)
	}
	raw, err := os.ReadFile(path + ".waits")
	if err != nil {
		t.Fatalf("read the run-queue waits the command recorded: %v", err)
	}
	waits := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(waits) != lines {
		t.Fatalf("the command recorded %d lines of run-queue waits at %s, want %d: %q", len(waits), path+".waits", lines, raw)
	}
	return at, waits
}

// runQueueWait is how long the kernel had kept a process waiting for a CPU by the moment the
// process's /proc/<pid>/schedstat line was read: the line's second field, run_delay, in
// nanoseconds (the kernel's Documentation/scheduler/sched-stats.rst). That is time the process
// was ready to run and the machine ran something else, and nothing else: time it ran, slept, was
// blocked or was stopped is not in it. It answers false for an empty line, which waitInto leaves
// where this kernel keeps no such file.
func runQueueWait(t *testing.T, line string) (time.Duration, bool) {
	t.Helper()
	if line == "" {
		return 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("a /proc/<pid>/schedstat line with no run_delay: %q", line)
	}
	ns, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || ns < 0 {
		t.Fatalf("the run_delay of the /proc/<pid>/schedstat line %q: %v", line, err)
	}
	return time.Duration(ns), true
}

// waitedPastHelper is how long the machine kept a leader waiting for a CPU on its way to its ready
// mark, where on that way it forked a helper and waited for the helper's word, from the
// /proc/<pid>/schedstat lines each read of itself (runQueueWait): the leader's at its mark
// (atMark), just before the fork (forked) and just after the word came (heard), and the helper's
// just before it gave the word (helper). Before the fork and after the word, the leader's own
// waits are the machine's hold of the way. Between the two, the way ran through whichever of the
// leader and the helper came to the word later, and the machine held it there for at least the
// less of the leader's waits over that stretch and the helper's: the helper's all fall before its
// word, so no wait is counted twice, and the less of the two counts none that was off the way. It
// answers false where this kernel keeps no run-queue wait.
func waitedPastHelper(t *testing.T, atMark, forked, heard, helper string) (time.Duration, bool) {
	t.Helper()
	mark, markKnown := runQueueWait(t, atMark)
	before, beforeKnown := runQueueWait(t, forked)
	after, afterKnown := runQueueWait(t, heard)
	helped, helperKnown := runQueueWait(t, helper)
	if !markKnown || !beforeKnown || !afterKnown || !helperKnown {
		return 0, false
	}
	return before + (mark - after) + min(after-before, helped), true
}
