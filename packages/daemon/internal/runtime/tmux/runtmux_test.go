package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ranTmux is what one runTmux answered.
type ranTmux struct {
	res result
	err error
}

// runningTmux runs r.runTmux on a goroutine of its own and answers the channel its answer comes on.
func runningTmux(r *Runtime) <-chan ranTmux {
	ran := make(chan ranTmux, 1)
	go func() {
		res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
		ran <- ranTmux{res, err}
	}()
	return ran
}

// A tmux client that exits on its own did not time out, however long pipeDrainDelay then holds
// runTmux open for something still holding its output pipes (a server that took the client's
// stdio over the socket): its exit is its answer. The timeout is on a fake clock. In the first case
// the row moves it past the timeout once the client has exited and been reaped and while the
// holder keeps the run open; a runTmux that read a timeout from the deadline having passed would
// report the client's own exit 3 as a timeout. In the second the clock never moves, so the client's
// exit 0 is the only end the run has; a runTmux that took exec.ErrWaitDelay for a failure would
// report that success as an error. What proves the pipes were held when the run returned is the
// holder itself, a backgrounded sleep still running then, not a reading of the clock.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit int
		// passes is whether the timeout passes once the client has exited.
		passes bool
	}{
		"an exit 3, the pipes held past the timeout": {3, true},
		"an exit 0 inside the timeout":               {0, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile, clientFile := filepath.Join(dir, "lingering.pid"), filepath.Join(dir, "client.pid")
			t.Cleanup(func() { killPIDFile(pidFile) })
			client := fakeTmuxClient(t, dir, "echo answered; sleep 30 & echo $! >"+pidFile+"; echo $$ >"+clientFile+".part && mv "+
				clientFile+".part "+clientFile+"; exit "+strconv.Itoa(testCase.exit))
			fake := clocktest.New(time.Now())
			r := &Runtime{tmuxPath: client, commandTimeout: defaultCommandTimeout, environ: []string{"PATH=" + os.Getenv("PATH")}, clock: fake}

			ran := runningTmux(r)
			if testCase.passes {
				pid := testwait.Until(t, "the client to write its pid", func() (int, bool) { return pidFromFile(clientFile) })
				testwait.Eventually(t, "the client to exit and be reaped", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
				select {
				case got := <-ran:
					t.Fatalf("runTmux returned %+v as its client exited: nothing held the pipes past that exit, so this run proves nothing", got)
				default:
				}
				fake.Advance(defaultCommandTimeout)
			}
			got := testwait.Receive(t, "runTmux to answer once pipeDrainDelay gave up on the pipes the holder keeps", ran)
			holding := holderRuns(pidFile)

			if got.err != nil {
				t.Fatalf("runTmux = %v, want the client's own exit %d", got.err, testCase.exit)
			}
			if !holding {
				t.Fatalf("runTmux returned with nothing holding the client's pipes: the backgrounded sleep that holds them no longer ran, so this run proves nothing")
			}
			if got.res.timedOut != 0 || got.res.exitCode != testCase.exit || strings.TrimSpace(got.res.stdout) != "answered" {
				t.Errorf("runTmux = exit %d, timed out %s, stdout %q; want exit %d, no timeout, and the answer: the client exited on its own inside its timeout",
					got.res.exitCode, got.res.timedOut, got.res.stdout, testCase.exit)
			}
		})
	}
}

// A client its timeout kills timed out, with that timeout, and is a result, not an error. The
// timeout is on a fake clock, which the row moves to the deadline runTmux armed only once the
// client runs: it writes its pid and then becomes the sleep, so only the timeout ends it.
func TestRunTmuxReportsAClientItsTimeoutKilledAsTimedOut(t *testing.T) {
	dir := t.TempDir()
	clientFile := filepath.Join(dir, "client.pid")
	client := fakeTmuxClient(t, dir, "echo $$ >"+clientFile+".part && mv "+clientFile+".part "+clientFile+"; exec sleep 30")
	fake := clocktest.New(time.Now())
	r := &Runtime{tmuxPath: client, commandTimeout: defaultCommandTimeout, environ: []string{"PATH=" + os.Getenv("PATH")}, clock: fake}

	ran := runningTmux(r)
	testwait.Until(t, "the client to write its pid", func() (int, bool) { return pidFromFile(clientFile) })
	if armed, want := fake.Armed(), fake.Now().Add(defaultCommandTimeout); len(armed) != 1 || !armed[0].Equal(want) {
		t.Fatalf("runTmux armed %v on its clock, want its command timeout alone at %s", armed, want)
	}
	fake.Advance(defaultCommandTimeout)
	got := testwait.Receive(t, "runTmux to answer once its timeout killed the client", ran)

	if got.err != nil || got.res.timedOut != defaultCommandTimeout || got.res.exitCode != -1 {
		t.Errorf("runTmux = exit %d, timed out %s, error %v; want exit -1, timed out %s, no error", got.res.exitCode, got.res.timedOut, got.err, defaultCommandTimeout)
	}
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

// pidFromFile is the PID pidFile holds, and false when it holds none.
func pidFromFile(pidFile string) (int, bool) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil
}

// holderRuns reports whether the process pidFile names still runs: a backgrounded sleep standing
// in for what holds a client's pipes, which holds them for as long as it runs.
func holderRuns(pidFile string) bool {
	pid, ok := pidFromFile(pidFile)
	return ok && syscall.Kill(pid, 0) == nil
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for what holds a client's pipes).
func killPIDFile(pidFile string) {
	if pid, ok := pidFromFile(pidFile); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
