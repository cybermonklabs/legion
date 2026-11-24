package launcher

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// namespaceHelperEnv marks the test binary re-executed as the PID 1 launcher below: TestMain
// leaves such a child the environment its test hands it (helperChildVariables).
const namespaceHelperEnv = "LEGION_LAUNCHER_NAMESPACE_HELPER"

// This helper runs the real launcher as PID 1, without a Docker daemon or a model process.
func TestLauncherPIDOneHelper(t *testing.T) {
	if os.Getenv(namespaceHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("launcher namespace helper is not PID 1")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	grace, err := time.ParseDuration(os.Getenv("LEGION_LAUNCHER_TEST_GRACE"))
	if err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Config{
		Connect: os.Getenv("LEGION_LAUNCHER_TEST_ADDRESS"), PrivateDir: os.Getenv("LEGION_LAUNCHER_TEST_PRIVATE"),
		Token: os.Getenv("LEGION_LAUNCHER_TEST_TOKEN"), Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", Stderr: os.Stderr,
		StopGrace: grace,
	})
	if err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

// pidNamespace is the sudo argv prefix that runs a command as PID 1 of a PID namespace of its own,
// with that namespace's own /proc, as this user: the shape of a pod's container, whose /proc lists
// the container's few processes and whose first process adopts every orphan. A machine that
// cannot make one skips the test, and CI, which can, fails it instead.
func pidNamespace(t *testing.T) []string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("PID namespace launcher proof requires Linux")
	}
	prefix := []string{"-n", "unshare", "--pid", "--fork", "--mount-proc", "--kill-child", "--setuid", strconv.Itoa(os.Getuid()), "--setgid", strconv.Itoa(os.Getgid())}
	// The probe is killed 30 s before the test binary's deadline, and a probe killed so fails the
	// test by name, not a skip: it answers in milliseconds where the prerequisite holds.
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 3*365*24*time.Hour, 30*time.Second))
	defer cancel()
	probe := exec.CommandContext(ctx, "sudo", append(append([]string{}, prefix...), "/bin/true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("PID namespace prerequisite: the probe had not answered 30 s before the test binary's deadline: %v: %s", err, output)
		}
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("PID namespace prerequisite: %v: %s", err, output)
		}
		t.Skipf("PID namespace prerequisite: %v: %s", err, output)
	}
	return prefix
}

// pidOneHelperEnv marks the test binary re-executed as PID 1 of a PID namespace of its own by
// runAsPIDOne: TestMain leaves such a child the environment its test hands it
// (helperChildVariables), and each helper it runs returns at once without it.
const pidOneHelperEnv = "LEGION_LAUNCHER_PID_ONE_HELPER"

// runAsPIDOne runs the test binary's helper test as PID 1 of a PID namespace of its own
// (pidNamespace), with only the test's own PATH, where its jj and git are, pidOneHelperEnv and
// env (NAME=value each) in its environment, fails t unless the helper ran and passed, and answers
// what it printed. The helper's own deadline is this test's less helperDeadlineGap, so a helper
// whose wait gives up near its deadline (reaperBound) prints why and ends, and this test then
// still has that gap for its own cleanups before its alarm. When what ran before it left the
// helper less than helperLeastTime (it is past helperStartBy), t fails by name and no helper
// starts: one would reach its own alarm before its bounds could give up. The wait for the helper
// ends 30 s before this test's deadline at the latest, 30 s after the helper's own alarm: sudo is
// then sent SIGTERM, and 10 s later the wait returns whatever sudo does (WaitDelay).
func runAsPIDOne(t *testing.T, helper string, env ...string) string {
	t.Helper()
	prefix := pidNamespace(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, prefix...), "/usr/bin/env", "-i", "PATH="+os.Getenv("PATH"), pidOneHelperEnv+"=1")
	args = append(args, env...)
	// The helper's -test.timeout ends helperDeadlineGap before this test's deadline, not reentry's
	// reentryMargin: this test's own cleanups after a helper that gave up need that gap. The
	// arguments are reentryUntil's, as every re-entry's are.
	var end time.Time
	if deadline, ok := t.Deadline(); ok {
		end = deadline.Add(-helperDeadlineGap)
		if left := time.Until(end); left < helperLeastTime {
			t.Fatalf("%s as PID 1 would have %s before its own deadline, %s before this test's, less than the %s it needs: what ran before it took the rest of the test binary's -timeout", helper, left.Round(time.Millisecond), helperDeadlineGap, helperLeastTime)
		}
	}
	args = append(append(append(args, self), reentryUntil(end, helper)...), "-test.v", "-test.count=1")
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 3*365*24*time.Hour, 30*time.Second))
	defer cancel()
	command := exec.CommandContext(ctx, "sudo", args...)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 10 * time.Second
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s as PID 1 had not ended 30 s before the test binary's deadline, 30 s after its own: %v\n%s", helper, err, output)
	}
	if err != nil || !bytes.Contains(output, []byte("--- PASS: "+helper)) {
		t.Fatalf("%s as PID 1: %v\n%s", helper, err, output)
	}
	return string(output)
}

// helperStartBy is the last moment a helper of runAsPIDOne can start, the test binary's deadline
// less helperDeadlineGap and helperLeastTime, and whether the binary has a deadline at all: a test
// bounds what it runs before its helper by it.
func helperStartBy(t *testing.T) (time.Time, bool) {
	deadline, ok := t.Deadline()
	return deadline.Add(-helperDeadlineGap - helperLeastTime), ok
}

// helperDeadlineGap is how much sooner than its test a helper of runAsPIDOne reaches its own
// deadline: the time the test has, once a helper that gave up has ended, for its own cleanups,
// among them the removal of the tree TestPutBacksUnderThePodsReaperKeepTheirExitStatus built for
// its helper. On this dev box that removal took 17.3 s at a one-minute load of about 1,100 on 288
// CPUs (2026-11-22), and more than 15 s in a helper's cleanup at a load of about 760 the day
// before; a minute is more than twice the most seen.
const helperDeadlineGap = time.Minute

// helperLeastTime is the least a helper of runAsPIDOne starts with before its own deadline: its
// waits on the reaper give up 30 s before that deadline (reaperBound), and a wait for a put-back or
// an install 15 s before it, so a helper with less would reach its own alarm before either could
// say why.
const helperLeastTime = 45 * time.Second

func namespaceRig(t *testing.T, grace time.Duration) *rig {
	t.Helper()
	prefix := pidNamespace(t)
	token := ownToken("launcher-token")
	listener := listenForLauncher(t, token)
	g := &rig{t: t, listener: listener, private: t.TempDir(), result: make(chan error, 1)}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, prefix...), "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "GOMAXPROCS=1",
		namespaceHelperEnv+"=1", "LEGION_LAUNCHER_TEST_ADDRESS=tcp://"+listener.Addr().String(), "LEGION_LAUNCHER_TEST_TOKEN="+token,
		"LEGION_LAUNCHER_TEST_PRIVATE="+g.private, "LEGION_LAUNCHER_TEST_GRACE="+grace.String(), self)
	args = append(args, reentry(t, "TestLauncherPIDOneHelper")...)
	command := exec.Command("sudo", args...)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	g.cancel = func() { _ = stdin.Close() }
	go func() {
		err := command.Wait()
		listener.codeReturned(err)
		g.result <- err
		close(g.result)
	}()
	t.Cleanup(func() {
		g.cancel()
		// No bound of the case's own on the launcher's end (reentry): one that does not stop after
		// its context ended is ended by its own -test.timeout, with its alarm's goroutine dump in
		// its output, and its namespace with it. A case that took the launcher's return itself
		// leaves the channel closed (rig.result), with nothing left to read here.
		if err, ok := <-g.result; ok && err != nil {
			t.Errorf("PID 1 launcher exited: %v (one that did not stop after its context ended ends at its own deadline, its panic below)\n%s", err, output.String())
		}
	})
	g.accept()
	return g
}

func TestPIDOneLauncherCleansDetachedDescendants(t *testing.T) {
	for _, end := range []string{"stop", "natural exit"} {
		t.Run(end, func(t *testing.T) {
			g := namespaceRig(t, 2*time.Second)
			marker := filepath.Join(t.TempDir(), "detached")
			script := `/usr/bin/setsid /bin/sh -c 'printf "%s ready\n" "$$" > "$DETACHED_MARKER"; exec /bin/sleep 600' &
while [ ! -s "$DETACHED_MARKER" ]; do /bin/sleep 0.01; done
`
			if end == "stop" {
				script += "wait\n"
			}
			if result := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: []string{"/bin/sh", "-c", script}, Env: []string{"DETACHED_MARKER=" + marker}}); !result.OK {
				t.Fatalf("start: %+v", result)
			}
			pid, _ := published(t, marker)
			if end == "stop" {
				g.stop(1)
			} else {
				for g.latest.LastExit == nil || g.latest.LastExit.Generation != 1 {
					g.next()
				}
			}
			resultFile := filepath.Join(t.TempDir(), "result")
			inspect := `if kill -0 "$DETACHED_PID" 2>/dev/null; then printf '1 survived\n'; else printf '1 gone\n'; fi > "$RESULT"`
			if result := g.start(shimwire.LauncherStart{ID: "start-2", Generation: 2, Argv: []string{"/bin/sh", "-c", inspect}, Env: []string{"DETACHED_PID=" + strconv.Itoa(pid), "RESULT=" + resultFile}}); !result.OK {
				t.Fatalf("next generation: %+v", result)
			}
			if _, result := published(t, resultFile); strings.TrimSpace(result) != "gone" {
				t.Fatalf("detached process %d %s into the next generation after %s", pid, result, end)
			}
		})
	}
}

func TestPIDOneLauncherReapsOrphansWhileRoleStaysResident(t *testing.T) {
	g := namespaceRig(t, 2*time.Second)
	marker := filepath.Join(t.TempDir(), "orphan")
	resultFile := filepath.Join(t.TempDir(), "reaped")
	script := `/bin/sh -c '/usr/bin/setsid /bin/sh -c '\''/bin/sleep 0.1; printf "%s ready\n" "$$" > "$ORPHAN"; exit 0'\'' &' 
while [ ! -s "$ORPHAN" ]; do /bin/sleep 0.01; done
read pid unused < "$ORPHAN"
for n in $(/usr/bin/seq 1 500); do
  if ! kill -0 "$pid" 2>/dev/null; then printf '1 gone\n' > "$RESULT"; exec /bin/sleep 600; fi
  /bin/sleep 0.01
done
printf '1 zombie\n' > "$RESULT"
exec /bin/sleep 600
`
	if result := g.start(shimwire.LauncherStart{ID: "resident", Generation: 1, Argv: []string{"/bin/sh", "-c", script},
		Env: []string{"ORPHAN=" + marker, "RESULT=" + resultFile}}); !result.OK {
		t.Fatalf("start: %+v", result)
	}
	if _, result := published(t, resultFile); strings.TrimSpace(result) != "gone" {
		t.Fatalf("resident role left an adopted orphan: %s", result)
	}
	g.stop(1)
}

func TestPIDOneLauncherUsesConfiguredShutdownGrace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		grace    time.Duration
		graceful bool
	}{
		{"graceful", 2500 * time.Millisecond, true},
		{"deadline", 50 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := namespaceRig(t, tc.grace)
			ready := filepath.Join(t.TempDir(), "ready")
			finished := filepath.Join(t.TempDir(), "finished")
			script := `trap '/bin/sleep 1.2; printf done > "$FINISHED"; exit 0' TERM
printf '1 ready\n' > "$READY"
while :; do /bin/sleep 600 & wait; done
`
			if result := g.start(shimwire.LauncherStart{ID: "start", Generation: 1, Argv: []string{"/bin/sh", "-c", script},
				Env: []string{"READY=" + ready, "FINISHED=" + finished}}); !result.OK {
				t.Fatalf("start: %+v", result)
			}
			published(t, ready)
			g.cancel()
			select {
			case err, ok := <-g.result:
				if !ok {
					t.Fatal("the launcher's result was taken before this test read it")
				}
				if err != nil {
					t.Fatalf("launcher exit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("launcher exceeded its shutdown budget")
			}
			body, err := os.ReadFile(finished)
			if tc.graceful {
				if err != nil || string(body) != "done" {
					t.Fatalf("configured grace was cut short: %q, %v", body, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("deadline did not stop the slow shutdown: %q, %v", body, err)
			}
		})
	}
}
