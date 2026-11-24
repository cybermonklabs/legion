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
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A tmux client that exits on its own did not time out, however long pipeDrainDelay then holds
// runTmux open for something still holding its output pipes (a server that took the client's
// stdio over the socket): its exit is its answer. The timeout is the rest of the test binary's
// deadline, far from the client's own exit on a loaded machine. With exit 3 the run keeps the
// client's own exit and no timeout; with exit 0, a runTmux that took exec.ErrWaitDelay for a
// failure would report that success as an error. What holds the pipes runs until the test's
// cleanup releases it (heldChild), so every run is held past pipeDrainDelay.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipes(t *testing.T) {
	for name, exit := range map[string]int{"an exit 3": 3, "an exit 0": 0} {
		t.Run(name, func(t *testing.T) {
			timeout := testwait.Remaining(t)
			client := fakeTmuxClient(t, t.TempDir(), "echo answered\n"+heldChild(t)+"\nexit "+strconv.Itoa(exit))
			r := &Runtime{tmuxPath: client, commandTimeout: timeout, environ: []string{"PATH=" + os.Getenv("PATH")}}

			started := time.Now()
			res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
			elapsed := time.Since(started)

			if err != nil {
				t.Fatalf("runTmux = %v after %s, want the client's own exit %d", err, elapsed, exit)
			}
			if elapsed < pipeDrainDelay {
				t.Fatalf("runTmux returned after %s, inside pipeDrainDelay: nothing held the pipe, so this run proves nothing", elapsed)
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
// client's own exit 3 as a timeout. The timeout is 100 ms short of pipeDrainDelay, so a run whose
// client exits inside it returns past it.
//
// Only a run whose client was seen reaped before its timeout could pass is judged. Go reaps the
// client in Wait once it has exited, and the deadline comes no sooner than the timeout after the
// row called runTmux. A kill sent to a client Go has reaped finds nothing, and does not count
// (procgroup.KilledByContext), so in such a run the timeout passed while the pipes were held and
// ended nothing. A loaded machine can hold the client past a timeout counted from before the
// client started, and that run, which the timeout's kill ended, shows nothing of this: the row
// runs the client again until a run of the right order comes, within a minute.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipesPastIt(t *testing.T) {
	const timeout = pipeDrainDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, pipeDrainDelay+time.Second)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader := filepath.Join(dir, "leader")
		client := fakeTmuxClient(t, dir, "echo $$ >'"+leader+"'\necho answered\n"+heldChild(t)+"\nexit 3")
		r := &Runtime{tmuxPath: client, commandTimeout: timeout, environ: []string{"PATH=" + os.Getenv("PATH")}}
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

		called := time.Now()
		res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
		elapsed := time.Since(called)
		at, seen := <-reaped

		if !seen {
			t.Fatalf("run %d: the client was not seen reaped within %s (runTmux = exit %d, timed out %s, error %v after %s)",
				runs, within, res.exitCode, res.timedOut, err, elapsed)
		}
		if !at.Before(called.Add(timeout)) {
			if time.Now().Before(until) {
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the client was seen reaped only after its %s timeout could pass", runs, within, timeout)
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

// A client its timeout kills timed out, with that timeout, and is a result, not an error. A
// message about it names the timeout and no exit: the -1 runTmux records is no status the client
// chose, and a message that printed it read `new-session failed (exit -1): timed out after 1s`.
// A client that exited keeps its exit and its stderr in both message forms.
func TestRunTmuxReportsAClientItsTimeoutKilledAsTimedOut(t *testing.T) {
	client := fakeTmuxClient(t, t.TempDir(), "exec sleep 30")
	r := &Runtime{tmuxPath: client, commandTimeout: time.Second, environ: []string{"PATH=" + os.Getenv("PATH")}}

	res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})

	if err != nil || res.timedOut != time.Second || res.exitCode != -1 {
		t.Errorf("runTmux = exit %d, timed out %s, error %v; want exit -1, timed out 1s, no error", res.exitCode, res.timedOut, err)
	}
	if got := "tmux new-session" + failed(res); got != "tmux new-session timed out after 1s" {
		t.Errorf("failed = %q, want the timeout and no exit", got)
	}
	if got := "list-panes" + exited(res); got != "list-panes timed out after 1s" {
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

// A client a signal ended is a failed run, as a client that exited non-zero is, so every caller
// reads it as main read its -1 exit; a message about it names the signal and keeps what the client
// printed (failed, exited), where it said `failed (exit -1)`.
func TestRunTmuxReadsAClientASignalEndedAsAFailedRunNamingTheSignal(t *testing.T) {
	client := fakeTmuxClient(t, t.TempDir(), "echo 'no server running on /tmp/tmux-1000/legion-test' >&2; kill -KILL $$")
	r := &Runtime{tmuxPath: client, commandTimeout: time.Minute, environ: []string{"PATH=" + os.Getenv("PATH")}}

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
	r := &Runtime{tmuxPath: client, socket: "legion-test", commandTimeout: time.Minute, environ: []string{"PATH=" + os.Getenv("PATH")}}
	r.run = r.runTmux

	v, err := r.awaitNotRunning(context.Background(), "%1", incarnation{pid: 4242, ticks: 1}, 10*time.Second)

	if err != nil || v.reason != reasonPaneGone {
		t.Errorf("awaitNotRunning = %+v, %v; want the pane gone and no error", v, err)
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

// fakeTmuxClient writes an executable that runs body under sh in place of a tmux client.
func fakeTmuxClient(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
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
// holds, by the child's own exit.
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
	script := fmt.Sprintf("echo $$ >'%[1]s.tmp' && mv '%[1]s.tmp' '%[1]s'\nwhile [ ! -e '%[2]s' ] && kill -0 %[3]d 2>/dev/null; do sleep 0.05; done\n",
		pidFile, release, os.Getpid())
	if err := os.WriteFile(holder, []byte(script), 0o600); err != nil {
		t.Fatalf("write the held child's script: %v", err)
	}
	return "sh '" + holder + "' &\nwhile [ ! -s '" + pidFile + "' ]; do sleep 0.01; done"
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
