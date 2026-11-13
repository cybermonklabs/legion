package shim_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// commandBudget is the budget the requests below carry: long enough that a command still
	// running when the shim is stopped can only be one the stop did not reach, short enough that
	// such a command is gone within the minute rather than outliving the test binary.
	commandBudget = 20 * time.Second
	// stopLimit is how long the shim may take to return once its context ends. The whole stop is
	// the command's SIGTERM to its process group, the moment the group needs to go, the agent's
	// own SIGTERM and its exit: tens of milliseconds, with the rest for a loaded machine.
	stopLimit = 5 * time.Second
)

// A shim command ends with the shim itself. Under the tmux runtime a `kill-pane` gives every
// process of the pane SIGHUP (internal/runtime/tmux's stop), and the jj of a save or an adoption
// used to get it because it shared the pane's process group. Each command has a group of its own
// now, so that signal reaches the shim alone (internal/procgroup), and the shim is what has to
// pass the stop on: every command site builds its context from the one Run was given, so the stop
// ends the command at once instead of leaving it to run against the clone every role of the issue
// shares until its own budget ends it — up to five minutes for a save.
//
// What each case reads is the command's own end: its whole process group — the leader and the
// child it had started — was signalled, and the shim returned without waiting for the command's
// budget. The first is the half that fails without the change: every command is answered in a
// goroutine of its own (fromDaemon), so the shim returns on its stop either way, and a command
// left on context.Background() is simply outlived by the shim that started it, reading and
// writing that shared clone for as much as its whole budget with nobody left to stop it.
//
// The next two cases are the stop the daemon sends rather than the signal this process gets:
// under the tmux runtime a pane is stopped with a `shutdown` frame first, and only what is left
// of it once the stop grace is spent is killed (internal/runtime/tmux's stop). Nothing on that
// path ends the context Run was given, so a command bound to that context alone runs on exactly
// as one bound to context.Background() does; what ends it is the shim's own end, and what lets
// the stop land at all is Run waiting for the command before it returns. The first of them reads
// the record the moment Run returned, with no poll behind it: the command's group has to have
// been signalled while the process whose job that is was still there to do it. The second gives
// that same shutdown an agent that spends its whole grace ignoring SIGTERM, and reads the record
// before the kill that ends that grace, which is a step of the test's own: the command's group is
// signalled while the agent is still running, not once it has been reaped. The shim's loop ends
// only at that reaping, so a command left to the loop alone goes on reading and writing the
// issue's shared clone for the whole of the agent's grace — five seconds with the shipped one —
// after the daemon asked the shim to stop.
//
// The last case is that same shutdown once more, with the agent spending the whole of procgroup's
// stop span inside its grace — two seconds, which a pod's agent spends on nothing unusual. The
// commands are ended when the agent is told to stop, so by the time the drain runs they have been
// ended for longer than the span that bounds it: its bound is 0, and the one command there ever
// was answered and returned while the agent was still in its grace. The drain must then wait for
// nothing at all, which is what the shim's clock reads here — a wait of 0 is still a wait, and it
// is registered on that clock like any other. A drain that took it would be selecting between a
// timer that has already fired and a goroutine that has not yet closed its channel, which picks
// the timer, and the pane's last lines — the ones the daemon quotes when a container ends — would
// say a command the daemon asked for is still running when none is.
func TestTheShimsCommandEndsWithTheShimsStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace func(t *testing.T) string
		request   shimwire.Frame
	}{
		{
			name:      "the working-copy adoption",
			workspace: func(t *testing.T) string { return t.TempDir() },
			request: shimwire.AdoptWorkingCopy{
				ID: "adopt-1", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test",
				TimeoutMs: int(commandBudget.Milliseconds()),
			},
		},
		{
			name:      "the release's save",
			workspace: bundleSkeleton,
			request: shimwire.BundleWorkspace{
				ID: "bundle-1", Issue: "INFRA-3098", MaxBytes: 40 << 20,
				TimeoutMs: int(commandBudget.Milliseconds()),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, record, _ := shimRunningACommand(t, tc.workspace(t), tc.request)

			started := time.Now()
			r.cancel()
			select {
			case <-r.done:
			case <-time.After(stopLimit):
				t.Fatalf("the shim was still running %v after its stop: its jj outlives the stop and ends only at the request's own %v budget; log:\n%s",
					stopLimit, commandBudget, r.log)
			}
			t.Logf("the shim returned %v after its stop; the request's own budget was %v", time.Since(started), commandBudget)
			for _, want := range []string{"the jj caught SIGTERM\n", "its child caught SIGTERM\n"} {
				if !testwait.Holds(stopLimit, testwait.Poll, func() bool { return strings.Contains(recorded(t, record), want) }) {
					t.Errorf("the request's jj recorded\n%swant a line %q: the stop signals the command's whole process group", recorded(t, record), want)
				}
			}
		})
	}

	t.Run("the daemon's shutdown, which reaches the shim as a frame and not as a signal", func(t *testing.T) {
		r, p, record, _ := shimRunningACommand(t, t.TempDir(), shimwire.AdoptWorkingCopy{
			ID: "adopt-2", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test",
			TimeoutMs: int(commandBudget.Milliseconds()),
		})

		started := time.Now()
		p.send(t, shimwire.Shutdown{})
		select {
		case <-r.done:
		case <-time.After(stopLimit):
			t.Fatalf("the shim was still running %v after the daemon's shutdown: its jj outlives that stop and ends only at the request's own %v budget; log:\n%s",
				stopLimit, commandBudget, r.log)
		}
		t.Logf("the shim returned %v after the daemon's shutdown; the request's own budget was %v", time.Since(started), commandBudget)
		for _, want := range []string{"the jj caught SIGTERM\n", "its child caught SIGTERM\n"} {
			if !strings.Contains(recorded(t, record), want) {
				t.Errorf("the request's jj recorded\n%swant a line %q by the time the shim returned: the stop of the command's whole process group is this process's own to send, and after it has returned there is nobody left to send it",
					recorded(t, record), want)
			}
		}
	})

	t.Run("the daemon's shutdown, with an agent that takes its whole grace", func(t *testing.T) {
		// This case's jj ignores its SIGTERM, so the command is still in flight when the drain is
		// reached and the drain has a bound to read: one whose commands have all returned waits
		// for nothing at all and takes no wait on the clock (drainCommands). What ends this jj is
		// procgroup's SIGKILL at the end of the command's own grace, a whole second after the
		// SIGTERM this case reads and well past the step below that reads the bound.
		r, p, record, clock := shimRunningACommand(t, t.TempDir(), shimwire.AdoptWorkingCopy{
			ID: "adopt-3", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test",
			TimeoutMs: int(commandBudget.Milliseconds()),
		}, "FAKE_OMP_IGNORE_SIGTERM=1", "HANGING_JJ_IGNORE_SIGTERM=1")

		started := time.Now()
		p.send(t, shimwire.Shutdown{})
		p.expectRaw(t, "fake_sigterm")
		// terminate ends the commands the daemon asked for before it signals the agent, so every
		// moment from here on is one they have been ended for too: the drain's bound below has to
		// be under what is left of procgroup's stop span by the time it is taken.
		terminated := time.Now()
		// The agent has caught its SIGTERM and is ignoring it; the kill at the end of its grace is
		// the next step of this test, so everything read before it happened while the agent ran.
		expired := clock.next(t, grace)
		for _, want := range []string{"the jj caught SIGTERM\n", "its child caught SIGTERM\n"} {
			if !testwait.Holds(stopLimit, testwait.Poll, func() bool { return strings.Contains(recorded(t, record), want) }) {
				t.Errorf("the request's jj recorded\n%swant a line %q while the agent was still in its grace: the command is ended when the agent is told to stop, not when it has been reaped, and the whole of that grace is time its jj would otherwise spend against the clone every role of the issue shares; log:\n%s",
					recorded(t, record), want, r.log)
			}
		}
		if r.exited() {
			t.Fatalf("the shim returned before the agent's grace ran out; log:\n%s", r.log)
		}
		t.Logf("the jj's record was read %v after the daemon's shutdown, with the agent still in its %v grace and not yet killed", time.Since(started), grace)

		ended := time.Since(terminated)
		expired.elapse()
		// The kill ends the agent, the shim's loop ends with its reaping, and the commands' drain
		// is the shim's next wait. Its bound is procgroup's whole stop span — the SIGTERM to the
		// command's group, the grace before its SIGKILL, and the WaitDelay exec then allows
		// anything still holding the command's pipes — less the time the commands have already
		// been ended for, which on this path is the agent's whole grace. A bound that did not
		// subtract it would spend that span a second time after the grace, and the stop of an
		// agent that ignores its SIGTERM would run past the floor the daemon's configuration
		// refuses a worker_stop_timeout_seconds under (shim.MinStopGrace, which AGENTS.md names)
		// — the point at which the launcher SIGKILLs the shim, leaving the warm-up's lease behind.
		span := procgroup.StopGrace + procgroup.WaitDelay
		drain := clock.nextAny(t, "the drain of the commands the daemon asked for")
		if left := max(span-ended, 0); drain.d >= span || drain.d > left {
			t.Errorf("the shim bounded the commands' drain by %v, want at most %v and under procgroup's whole stop span %v: the commands had been ended for %v by the time the agent was killed, and that is span this wait must not spend over again; log:\n%s",
				drain.d, left, span, ended, r.log)
		}
		drain.elapse()
		select {
		case <-r.done:
		case <-time.After(stopLimit):
			t.Fatalf("the shim was still running %v after the agent was killed at the end of its grace; log:\n%s", stopLimit, r.log)
		}
	})

	t.Run("the daemon's shutdown, with the commands' whole stop span spent inside the agent's grace", func(t *testing.T) {
		r, p, _, clock := shimRunningACommand(t, t.TempDir(), shimwire.AdoptWorkingCopy{
			ID: "adopt-4", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test",
			TimeoutMs: int(commandBudget.Milliseconds()),
		}, "FAKE_OMP_IGNORE_SIGTERM=1")

		p.send(t, shimwire.Shutdown{})
		// terminate ends the commands just before it signals the agent, so the agent's notice of
		// that signal and the adoption's own answer — its jj is ended by that same cancellation —
		// are both on their way, in either order. The answer is what this case waits for: the one
		// command this shim ever answered has returned by the time it is sent, which is the drain
		// this case is about.
		for seen := false; !seen; {
			switch frame := p.next(t).(type) {
			case shimwire.Raw:
				if frame.Type != "fake_sigterm" {
					t.Fatalf("the shim sent a %s frame, want the agent's notice of its SIGTERM or the adoption's answer", frame.Type)
				}
			case shimwire.AdoptWorkingCopyResult:
				if frame.ID != "adopt-4" {
					t.Fatalf("the shim answered %#v, want the result of adopt-4", frame)
				}
				seen = true
			default:
				t.Fatalf("the shim sent %#v, want the agent's notice of its SIGTERM or the adoption's answer", frame)
			}
		}
		answered := time.Now()

		// What the drain subtracts from its bound is read on this process's monotonic clock
		// (sinceCommandsEnded) and never on the shim's own, so it is real time the agent's grace
		// has to hold open here — the `shutdown` whose agent takes two seconds or more of its
		// grace. The commands were ended before the answer just read, so once procgroup's whole
		// stop span has passed since that answer, the drain's bound is 0.
		if left := procgroup.StopGrace + procgroup.WaitDelay - time.Since(answered); left > 0 {
			time.Sleep(left)
		}
		clock.next(t, grace).elapse()
		select {
		case <-r.done:
		case <-time.After(stopLimit):
			t.Fatalf("the shim was still running %v after the agent was killed at the end of its grace; log:\n%s", stopLimit, r.log)
		}

		// Run has returned, so every wait it took is on the clock by now: a drain whose commands
		// have all returned takes none, and a wait of 0 — which is the whole of the bound left
		// here — is a wait like any other.
		clock.idle(t)
		if n := r.log.count("a command the daemon asked for is still running"); n != 0 {
			t.Errorf("the shim logged %d time(s) that a command the daemon asked for is still running; the one command it answered returned while the agent was still in its grace; log:\n%s", n, r.log)
		}
	})
}

// shimRunningACommand is a shim the daemon has asked for one command, with that command's whole
// process group up: the jj and the child the jj started are both running (hangingJJ), so a stop
// that reaches the group reaches both. env is added to the environment the shim passes on, the
// agent's and the command's jj's alike, for a case that needs one of them in a particular temper.
// It answers the shim, the daemon's side of its stream, the record the jj and its child write,
// and the clock the shim's waits are taken from, which is a case's way of holding the agent
// inside its grace.
func shimRunningACommand(t *testing.T, workspace string, request shimwire.Frame, env ...string) (*running, *peer, string, *fakeClock) {
	t.Helper()
	record := hangingJJ(t)
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace, "HANGING_JJ_RECORD="+record)
	cfg.Env = append(cfg.Env, env...)
	clock := newClock()
	r := run(t, cfg, clock)
	p := daemon.accept(t)
	p.open(t)

	p.send(t, request)
	// The stop is sent once the whole group is up: a jj signalled before it had started its own
	// child would say nothing about the group.
	testwait.Eventually(t, "the jj of the request and its child to be running", func() bool {
		return strings.Count(recorded(t, record), "running\n") == 2
	})
	return r, p, record, clock
}

// hangingJJ writes a `jj` that outlives any budget a test here gives it, in a directory of its
// own put first on PATH: the shim runs PATH's jj, the one the pane's own shell runs. It starts a
// child of its own, which a command's SIGTERM reaches only because that signal goes to the whole
// process group (internal/procgroup), and both it and the child say in the record when they were
// started and what they caught, each after its own trap is installed: a stop that landed before
// that line would find the process under the default disposition and end it with nothing
// recorded, which is the opposite of what every case here reads.
//
// Each spends its life in a loop of short foreground sleeps, and never in a loop of `sleep &
// wait $!`: the stop's SIGTERM reaches the whole group, so it ends the sleep the shell is
// waiting for, and the shell then runs its pending trap at the loop's own next command — before
// it forks anything. A `wait $!` returns on that same signal and the loop forks its next sleep
// first, which the group's SIGTERM has already passed by, so the trap's exit leaves that sleep
// behind holding the command's output pipes until it gives up on its own (measured on this
// machine with the stop these rows send: 3 of 60 stops of this shape left one behind, and 0 of
// 60 of the loop below). A trap is then acted on within one sleep of its arrival, which is
// milliseconds against the seconds every bound here allows, and each gives up after a minute, so
// a run whose stop never comes leaves nothing behind for long. HANGING_JJ_IGNORE_SIGTERM=1 keeps
// both of them running once they have recorded that signal, for the one case that needs its
// command still in flight when the shim drains: procgroup's SIGKILL at the end of the command's
// own grace is then what ends them.
func hangingJJ(t *testing.T) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "jj-record")
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--the-jjs-own-child" ]; then
  trap 'printf "its child caught SIGTERM\n" >>"$HANGING_JJ_RECORD"; [ "$HANGING_JJ_IGNORE_SIGTERM" = 1 ] || exit 0' TERM
  printf 'the child is running\n' >>"$HANGING_JJ_RECORD"
else
  trap 'printf "the jj caught SIGTERM\n" >>"$HANGING_JJ_RECORD"; [ "$HANGING_JJ_IGNORE_SIGTERM" = 1 ] || exit 0' TERM
  "$0" --the-jjs-own-child &
  printf 'the jj is running\n' >>"$HANGING_JJ_RECORD"
fi
ticks=0
while [ "$ticks" -lt 1200 ]; do
  sleep 0.05
  ticks=$((ticks+1))
done
`
	if err := os.WriteFile(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the hanging jj: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// recorded is what the hanging jj and its child have written so far, which is nothing until the
// first of them runs.
func recorded(t *testing.T, record string) string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the jj's record: %v", err)
	}
	return string(body)
}

// bundleSkeleton is the two files of a jj workspace the save reads before it starts a command
// (workspace.GitDir): the repository directory and the git directory it names. Nothing here runs
// a real jj or git — the save's first command is the hanging jj — so the git directory needs to
// exist and hold nothing.
func bundleSkeleton(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	store := filepath.Join(workspace, ".jj", "repo", "store")
	git := filepath.Join(workspace, ".git")
	for _, dir := range []string{store, git} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store, "git_target"), []byte(git+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}
