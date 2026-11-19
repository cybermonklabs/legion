package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A tmux client that exits on its own did not time out, however long pipeDrainDelay then holds
// runTmux open for something still holding its output pipes (a server that took the client's
// stdio over the socket): its exit is its answer. In the first case the timeout is 100 ms short of
// pipeDrainDelay, so the run always returns past it, while the client still has 900 ms to exit
// under load; a runTmux that read a timeout from the deadline having passed would report the
// client's own exit 3 as a timeout. In the second the client exits 0 well inside its timeout; a
// runTmux that took exec.ErrWaitDelay for a failure would report that success as an error.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		timeout time.Duration
	}{
		"an exit 3, the pipes held past the timeout": {3, pipeDrainDelay - 100*time.Millisecond},
		"an exit 0 inside the timeout":               {0, time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "lingering.pid")
			t.Cleanup(func() { killPIDFile(pidFile) })
			client := fakeTmuxClient(t, dir, "echo answered; sleep 30 & echo $! >"+pidFile+"; exit "+strconv.Itoa(testCase.exit))
			r := &Runtime{tmuxPath: client, commandTimeout: testCase.timeout, environ: []string{"PATH=" + os.Getenv("PATH")}}

			started := time.Now()
			res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
			elapsed := time.Since(started)

			if err != nil {
				t.Fatalf("runTmux = %v after %s, want the client's own exit %d", err, elapsed, testCase.exit)
			}
			if elapsed < pipeDrainDelay {
				t.Fatalf("runTmux returned after %s, inside pipeDrainDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if res.timedOut != 0 || res.exitCode != testCase.exit || strings.TrimSpace(res.stdout) != "answered" {
				t.Errorf("runTmux = exit %d, timed out %s, stdout %q after %s; want exit %d, no timeout, and the answer: the client exited on its own inside its %s timeout",
					res.exitCode, res.timedOut, res.stdout, elapsed, testCase.exit, testCase.timeout)
			}
		})
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

// A client a signal ended after it printed its refusal keeps that refusal beneath the signal, as
// a client that exited keeps it (failure): `run tmux list-panes: signal: killed` alone says nothing
// of why.
func TestRunTmuxKeepsWhatAClientASignalEndedPrinted(t *testing.T) {
	client := fakeTmuxClient(t, t.TempDir(), "echo 'no server running on /tmp/tmux-1000/legion-test' >&2; kill -KILL $$")
	r := &Runtime{tmuxPath: client, commandTimeout: time.Minute, environ: []string{"PATH=" + os.Getenv("PATH")}}

	_, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})

	if want := "run tmux list-panes: signal: killed\nno server running on /tmp/tmux-1000/legion-test"; err == nil || err.Error() != want {
		t.Errorf("runTmux = %v, want %q", err, want)
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

// A tmux client a signal ends after new-window or split-window opened its pane leaves no pane the
// daemon does not own. openWindowIn kills the window it opened when its ownership marker's client
// is killed, as it does when the marker exits non-zero, and when the bootstrap window's cleanup
// fails either way after new-window opened the agent's window; splitWindow keeps the pane it split
// when select-layout's client is killed, as it does when select-layout exits non-zero, so the
// launch records it. Read as an error alone, each left an agent pane running beside the claim's
// relaunch.
func TestATmuxClientASignalEndsAfterThePaneOpensLeavesNoUnownedPane(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	if killed == nil {
		t.Fatal("a shell that sent itself SIGKILL exited 0")
	}
	// calls records each tmux command a run makes, the target of a kill-window beside its verb.
	// dies names the command that fails: a verb, or `kill-window <target>`; exits says whether it
	// exits 1 rather than dying of the signal.
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
				return result{}, fmt.Errorf("run tmux %s: %w", command, killed)
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
		if !errors.Is(err, killed) {
			t.Errorf("openWindowIn = %v, want the marker's signal", err)
		}
		if want := []string{"new-window", "set-option", "kill-window @7"}; !slices.Equal(calls, want) {
			t.Errorf("tmux ran %q, want %q: the window the marker could not own is left open", calls, want)
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
// assertions by design (a backgrounded sleep standing in for what holds a client's pipes).
func killPIDFile(pidFile string) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
