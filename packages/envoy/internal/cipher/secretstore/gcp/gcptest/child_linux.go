//go:build linux && (amd64 || arm64)

// A stub gcloud whose child holds its output open, and the reads of /proc that see that child
// running while a gcloud call runs and gone once the call has ended. They build where /proc's stat
// file has the shape procState reads: Linux on amd64 or arm64.

package gcptest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

// GcloudChildFile is the variable that names the file the stub gcloud's child writes its pid to
// (GcloudStubIn). The child inherits it, so it also marks the child's environment as this test's
// (ours).
const GcloudChildFile = "AGENT_SECRETS_TEST_GCLOUD_CHILD"

// GcloudChild is the child of the stub gcloud GcloudStubIn writes, by the file it writes its pid
// to; or, with EnvFile set, another process of that stub (the stub's own, in the CLI's
// TestGcloudEndsWhenAgentSecretsIsKilled), by its pid file and the child's, which its environment
// names (ours).
type GcloudChild struct {
	PidFile string
	EnvFile string
}

// UseGcloudWhoseChildHoldsItsOutput is a stub gcloud that never answers, whatever it is asked: it
// starts a child that holds its standard output open, as gcloud's Python does under a shell
// script that does not exec it, and waits on it. The child writes its own pid to a file before it
// waits, so it names itself even where the stub is killed first. It waits for as long as the
// process that wrote the stub runs, which is no bound of the test's: it only keeps a child the
// kill missed from outliving a test binary that was killed itself. A child still running when t
// ends is killed (CleanUp), and t fails where /proc cannot say whether it still runs. It answers
// the stub's path.
func UseGcloudWhoseChildHoldsItsOutput(t *testing.T) (string, GcloudChild) {
	t.Helper()
	return GcloudStubIn(t, t.TempDir(), "wait")
}

// GcloudStubIn writes UseGcloudWhoseChildHoldsItsOutput's stub gcloud into dir, the file its
// child names itself in beside it, and answers the stub's path. The stub ends with last once it
// has started the child: "wait" waits on the child, never answering; "exit 0" exits at once,
// leaving the child holding the output, as a wrapper that does not wait for what it started does.
func GcloudStubIn(t *testing.T, dir, last string) (string, GcloudChild) {
	t.Helper()
	child := GcloudChild{PidFile: filepath.Join(dir, "child.pid")}
	t.Setenv(GcloudChildFile, child.PidFile)
	script := "#!/bin/sh\n" +
		`/bin/sh -c 'echo $$ >"$` + GcloudChildFile + `"; while kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; do sleep 1; done' &` + "\n" +
		last + "\n"
	path := filepath.Join(dir, "gcloud")
	if err := testexec.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.CleanUp(t) })
	return path, child
}

// CleanUp kills the child where it still runs (Kill). Only a failed run finds no pid written,
// since CallEnds sees the child named: a child that started late, once the stub was killed, still
// names itself, so it is waited for, and t fails where none comes.
func (c GcloudChild) CleanUp(t *testing.T) {
	t.Helper()
	pid := c.Pid()
	if pid == 0 && !heldBeforeGiveUp(t, func() bool { pid = c.Pid(); return pid != 0 }) {
		t.Errorf("the stub gcloud's child never named itself in %s: where it started, it may still run", c.PidFile)
		return
	}
	c.Kill(t, pid)
}

// Pid is the pid the child wrote, 0 while it has written none.
func (c GcloudChild) Pid() int {
	data, err := os.ReadFile(c.PidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// Runs reports whether pid names a running process: not where /proc no longer names it or names
// a dead one (Z, a zombie its reaper has not collected yet, or X). A read of /proc that fails in
// any other way is an error, never an answer that the process is gone. From the child's naming
// itself to the two seconds after the call, pid is the child's: Linux hands pids out in turn, so
// it gives one out again only once its allocator has gone round all the others.
func (c GcloudChild) Runs(pid int) (bool, error) {
	state, err := procState(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state != 'Z' && state != 'X', nil
}

// procState is the state field of /proc/<pid>/stat: the one letter after the parenthesised
// command name, whose own text may hold spaces or parentheses.
func procState(pid int) (byte, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 >= len(data) {
		return 0, fmt.Errorf("/proc/%d/stat has no state after the command name", pid)
	}
	return data[end+2], nil
}

// ours reports whether pid's environment holds this child's pid file (EnvFile where it is set), so
// that no process the kernel has given the pid since is killed for the child. A process that is
// exiting can refuse a read of its environment for a moment (permission denied, seen once in
// twenty runs), so ours asks again until the waits of these tests give up (untilGiveUp) before it
// answers that error.
func (c GcloudChild) ours(t *testing.T, pid int) (bool, error) {
	marker := c.PidFile
	if c.EnvFile != "" {
		marker = c.EnvFile
	}
	var answer bool
	var readErr error
	testwait.Holds(untilGiveUp(t), testwait.Poll, func() bool {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
			answer, readErr = false, nil
		case err != nil:
			readErr = err
			return false
		default:
			answer, readErr = slices.Contains(strings.Split(string(environ), "\x00"), GcloudChildFile+"="+marker), nil
		}
		return true
	})
	return answer, readErr
}

// Kill kills pid where it still runs and is the child (ours). The process handle is taken before
// the check, so where the kernel gives one (a pidfd) the signal reaches the process checked,
// never one the kernel has given the pid since.
func (c GcloudChild) Kill(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Errorf("the stub gcloud's child, pid %d, may still run: %v", pid, err)
		return
	}
	defer process.Release()
	running, err := c.Runs(pid)
	if err == nil && running {
		var mine bool
		if mine, err = c.ours(t, pid); err == nil && mine {
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill the stub gcloud's child, pid %d: %v", pid, err)
			}
		}
	}
	if err != nil {
		t.Errorf("the stub gcloud's child, pid %d, may still run: %v", pid, err)
	}
}

// CallEnds is call's error. It starts call, waits until the stub gcloud's child has named itself
// and /proc shows it running, then runs release, which ends the call the way the row's code ends
// it: a fired deadline, or a stub that may now exit. The row holds every deadline of the call past
// the run, so nothing else ends the call before release, the child is always seen running first,
// and its absence afterwards (RequireGone) is read by the same /proc that saw it. The call is timed
// against after, how long the code may take once released (0 for a deadline's kill, the runner's
// wait for output for a gcloud that exited), less the time this process was held up
// (endsByItsDeadline). It fails t where call ends before release (the stub did not hold it), and
// where the child is not seen or call has not ended when the waits of these tests give up. Where
// it fails t before call has returned, t ends by running release, so nothing the row set up still
// holds the call, then killing the child (CleanUp), which lets a call held by the child's open
// output return, and then waits for call, failing where it has not returned when the waits give up.
func (c GcloudChild) CallEnds(t *testing.T, what string, after time.Duration, release func(), call func() error) error {
	t.Helper()
	done, finished := make(chan error, 1), make(chan struct{})
	meter := startStallMeter(t)
	go func() {
		defer close(finished)
		done <- call()
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		release()
		c.CleanUp(t)
		// Past the give-up moment received would wait no time at all, so the released call is
		// given a share of the margin kept for this cleanup to end, and kill its stub, instead.
		select {
		case <-finished:
		case <-time.After(cleanupShare):
			t.Errorf("%s was still waiting on gcloud %s after its release, in its test's cleanup", what, cleanupShare)
		}
	})
	giveUp := time.NewTimer(untilGiveUp(t))
	defer giveUp.Stop()
	poll := time.NewTicker(testwait.Poll)
	defer poll.Stop()
	for seen := false; !seen; {
		select {
		case err := <-done:
			t.Fatalf("%s ended (%v) before it was released, with the stub gcloud's child unseen", what, err)
		case <-giveUp.C:
			t.Fatalf("the stub gcloud's child was never seen running while %s ran, %s before the test binary's deadline; its pid file %s holds %d", what, testwait.Margin, c.PidFile, c.Pid())
		case <-poll.C:
			if pid := c.Pid(); pid != 0 {
				running, err := c.Runs(pid)
				if err != nil {
					t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, err)
				}
				seen = running
			}
		}
	}
	start := time.Now()
	release()
	return endsByItsDeadline(t, meter, what, start, after, done)
}

// RequireGone fails t unless the child, seen running while the call ran (CallEnds), is gone once
// the call has ended: the kill at the deadline reached it, not the stub gcloud alone. The wait's
// one bound is the moment the waits of these tests give up (untilGiveUp): a child the kill missed
// runs for as long as the test process does.
func (c GcloudChild) RequireGone(t *testing.T) {
	t.Helper()
	pid := c.Pid()
	var readErr error
	gone := heldBeforeGiveUp(t, func() bool {
		running, err := c.Runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, readErr)
	}
	if !gone {
		t.Fatalf("the stub gcloud's child, pid %d, still runs after the call that started it ended, %s before the test binary's deadline", pid, testwait.Margin)
	}
}

// cleanupShare is how long a cleanup that runs after a wait gave up may wait for what it has just
// ended (a call it released) before it reports it: a third of testwait.Margin, so two such steps
// and the test's report still fit before go test's alarm. It bounds no verdict of a row that has
// not failed already.
const cleanupShare = testwait.Margin / 3

// untilGiveUp is how long is left before the moment every wait for a fact in these tests gives up:
// the test binary's deadline less testwait.Margin, the clamp testwait.Bounded gives. The day passed
// to it stands for no bound of the wait's own: Bounded answers what is left before that moment when
// it is sooner, and the day itself where there is no deadline (-timeout 0). It is negative once
// that moment has passed.
func untilGiveUp(t *testing.T) time.Duration {
	return testwait.Bounded(t, 24*time.Hour, testwait.Margin)
}

// noteGaveUp writes to stderr, at once, that a wait of t gave up at the binary's deadline less
// testwait.Margin, as testwait's own waits do: without -v a failed subtest's text is printed only
// when its parent test ends, and go test's alarm, should it come first, prints no running test's
// log, so this line is what still names the test whose wait gave up. The caller then fails the
// test with the text that says what it waited for.
func noteGaveUp(t *testing.T) {
	_, _ = fmt.Fprintf(os.Stderr, "testwait: %s: gave up waiting %s before the test binary's deadline\n", t.Name(), testwait.Margin)
}

// heldBeforeGiveUp is testwait.Holds bounded by the moment every wait for a fact in these tests
// gives up (untilGiveUp): whether condition held by then. When it did not, it notes so on stderr
// (noteGaveUp), and the caller fails the test with its own text.
func heldBeforeGiveUp(t *testing.T, condition func() bool) bool {
	if testwait.Holds(untilGiveUp(t), testwait.Poll, condition) {
		return true
	}
	noteGaveUp(t)
	return false
}

// received is the next value ch receives, and true; or, when the moment every wait for a fact in
// these tests gives up passes first, the zero value and false, having noted so on stderr
// (noteGaveUp), and the caller fails the test with its own text. A value ch has by then is still
// taken.
func received[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	giveUp := time.NewTimer(untilGiveUp(t))
	defer giveUp.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-giveUp.C:
		select {
		case v := <-ch:
			return v, true
		default:
			noteGaveUp(t)
			var zero T
			return zero, false
		}
	}
}

// stallTick is how long each sleep of a stallMeter asks for.
const stallTick = 5 * time.Millisecond

// stallMeter adds up how long this process was held up while it runs: a goroutine sleeps
// stallTick at a time and counts how much longer each sleep took than that. A loaded machine, a
// CPU quota or a frozen cgroup holds the whole process up, the code a row times with it, and the
// meter counts that time; code that is late on its own does not move it.
type stallMeter struct {
	mu       sync.Mutex
	held     time.Duration
	lastWake time.Time
}

// startStallMeter starts a stallMeter, which runs until t ends.
func startStallMeter(t *testing.T) *stallMeter {
	t.Helper()
	m := &stallMeter{lastWake: time.Now()}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTimer(stallTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			m.mu.Lock()
			now := time.Now()
			if over := now.Sub(m.lastWake) - stallTick; over > 0 {
				m.held += over
			}
			m.lastWake = now
			m.mu.Unlock()
			tick.Reset(stallTick)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
	return m
}

// heldUp is how long the meter has counted this process held up so far, the sleep it is in now
// included: when a frozen process wakes, the code it timed can end before the meter's goroutine
// has run again.
func (m *stallMeter) heldUp() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.held
	if over := time.Since(m.lastWake) - stallTick; over > 0 {
		held += over
	}
	return held
}

// lateSlack is how much later than its own deadline code in this process may end, beyond the time
// this process was held up meanwhile (stallMeter). What such code still does once its deadline has
// passed takes milliseconds at the machine's own speed, and the meter counts the time the machine
// took from it. Code whose own deadline ends late, by seconds, is past it.
const lateSlack = time.Second

// endsByItsDeadline waits for done, the end of code in this process started at start under a
// deadline of its own, deadline after start, and answers what done received. The wait is bounded
// only by the moment every wait for a fact in these tests gives up (received), so a machine that
// held the code up does not fail the row; the row fails where the code was late on its own
// (requireOnTime). what names the code for the failure's text.
func endsByItsDeadline(t *testing.T, meter *stallMeter, what string, start time.Time, deadline time.Duration, done <-chan error) error {
	t.Helper()
	err, ok := received(t, done)
	if !ok {
		t.Fatalf("%s had not ended %s after it started, past its %s deadline, %s before the test binary's deadline", what, time.Since(start).Round(time.Millisecond), deadline, testwait.Margin)
	}
	requireOnTime(t, meter, what, start, deadline)
	return err
}

// requireOnTime fails t where code in this process, started at start under a deadline of its own,
// deadline after start (0 for code that should not wait at all), and ended just now, ended later
// than that deadline by more than lateSlack plus the time meter, started before start, counted
// this process held up: that is code whose own deadline came late, or that waited for something it
// should not. It logs how late it was. what names the code for the failure's text.
func requireOnTime(t *testing.T, meter *stallMeter, what string, start time.Time, deadline time.Duration) {
	t.Helper()
	took := time.Since(start)
	held := meter.heldUp()
	late := took - deadline - held
	if late > lateSlack {
		t.Fatalf("%s ended %s after it started, past its %s deadline; this process was held up %s of that, so the code was %s late on its own, more than %s",
			what, took.Round(time.Millisecond), deadline, held.Round(time.Millisecond), late.Round(time.Millisecond), lateSlack)
	}
	t.Logf("on time: %s took %s against its %s deadline, %s held up, %s late on its own (slack %s)",
		what, took.Round(time.Millisecond), deadline, held.Round(time.Millisecond), late.Round(time.Millisecond), lateSlack)
}
