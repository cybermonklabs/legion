package launcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
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

	"github.com/sjawhar/legion/daemon/internal/hostproc"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// namespaceHelperEnv marks the test binary re-executed as the PID 1 launcher below: TestMain
// leaves such a child the environment its test hands it (helperChildVariables).
const namespaceHelperEnv = "LEGION_LAUNCHER_NAMESPACE_HELPER"

// This helper runs the real launcher as PID 1, without a Docker daemon or a model process. It first
// writes the wrapper's record and the starting test it was given to the file
// LEGION_LAUNCHER_TEST_ENV_FILE names (pidOneEntries), for the test that started it to read.
func TestLauncherPIDOneHelper(t *testing.T) {
	if os.Getenv(namespaceHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("launcher namespace helper is not PID 1")
	}
	if err := os.WriteFile(os.Getenv("LEGION_LAUNCHER_TEST_ENV_FILE"), []byte(pidOneEntries()), 0o600); err != nil {
		t.Fatal(err)
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
		Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", Stderr: os.Stderr,
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
	probe := exec.Command("sudo", append(append([]string{}, prefix...), "/bin/true")...)
	if output, err := probe.CombinedOutput(); err != nil {
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

// pidOneEntries is what a PID-one helper says of the hostproc entries of its environment, one
// line: the wrapper's record, the starting test, and whether it was marked the binary go ran.
func pidOneEntries() string {
	_, binary := os.LookupEnv(hostproc.BinaryEnv)
	return "pid-one entries: " + hostproc.RecordEnv + "=" + os.Getenv(hostproc.RecordEnv) + " " +
		hostproc.TestEnv + "=" + os.Getenv(hostproc.TestEnv) + " " + hostproc.BinaryEnv + "=" + strconv.FormatBool(binary) + "\n"
}

// TestPIDOneHelperSaysItsEntries is the helper the test below runs as PID 1: it prints its entries.
func TestPIDOneHelperSaysItsEntries(t *testing.T) {
	if os.Getenv(pidOneHelperEnv) == "" {
		return
	}
	fmt.Print(pidOneEntries())
}

// A PID-one helper clears its environment with env -i, so it carries the wrapper's record and the
// test that started it explicitly, and never BinaryEnv: only go's own test binary has it. Each of
// the two places that start one is run here as PID 1 of a namespace of its own, and the helper
// says what it was given: runAsPIDOne, which the drain, reap, put-back and leak-check tests use,
// and namespaceRig, which runs the launcher itself. Run by hand, the test gives itself a record.
func TestPIDOneHelpersCarryThePrivateProcRecordAndStartingTest(t *testing.T) {
	record := os.Getenv(hostproc.RecordEnv)
	if record == "" {
		record = filepath.Join(t.TempDir(), "scans")
		if err := os.WriteFile(record, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(hostproc.RecordEnv, record)
	}
	t.Setenv(hostproc.BinaryEnv, "1")
	want := "pid-one entries: " + hostproc.RecordEnv + "=" + record + " " + hostproc.ChildEnv(t) + " " + hostproc.BinaryEnv + "=false\n"
	t.Run("runAsPIDOne", func(t *testing.T) {
		if got := runAsPIDOne(t, "TestPIDOneHelperSaysItsEntries"); !strings.Contains(got, want) {
			t.Fatalf("the PID-one helper said:\n%s\nwhich does not hold %q", got, want)
		}
	})
	t.Run("namespaceRig", func(t *testing.T) {
		g := namespaceRig(t, time.Second)
		got, err := os.ReadFile(g.entries)
		if err != nil || string(got) != want {
			t.Fatalf("the PID-one launcher said %q (%v), want %q", got, err, want)
		}
	})
}

// pidOneHelperEnvironment is the only environment an env -i PID-one helper receives: the test's
// PATH and marker, plus the wrapper record and starting-test entry where the parent has a record.
// It deliberately takes no entry from os.Environ, so it never passes Hostproc BinaryEnv to a child.
func pidOneHelperEnvironment(t testing.TB, entries ...string) []string {
	environment := append([]string{}, entries...)
	if record := os.Getenv(hostproc.RecordEnv); record != "" {
		environment = append(environment, hostproc.RecordEnv+"="+record)
	}
	return append(environment, hostproc.ChildEnv(t))
}

// runAsPIDOne runs the test binary's helper test as PID 1 of a PID namespace of its own
// (pidNamespace), with only the test's own PATH, where its jj and git are, and pidOneHelperEnv in
// its environment, fails t unless the helper ran and passed, and answers what it printed.
func runAsPIDOne(t *testing.T, helper string) string {
	t.Helper()
	prefix := pidNamespace(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := pidOneHelperEnvironment(t, "PATH="+os.Getenv("PATH"), pidOneHelperEnv+"=1")
	args := append(append(append([]string{}, prefix...), "/usr/bin/env", "-i"), environment...)
	args = append(args, self, "-test.run=^"+helper+"$", "-test.v", "-test.count=1")
	output, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("--- PASS: "+helper)) {
		t.Fatalf("%s as PID 1: %v\n%s", helper, err, output)
	}
	return string(output)
}

func namespaceRig(t *testing.T, grace time.Duration) *rig {
	t.Helper()
	prefix := pidNamespace(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	g := &rig{t: t, listener: listener, private: t.TempDir(), result: make(chan error, 1)}
	g.entries = filepath.Join(t.TempDir(), "pid-one-entries")
	// The launcher is a process of its own here, so the addresses it dials from reach the rig
	// through a file (launcherDials), which acceptRole reads for an empty connection.
	dials := filepath.Join(t.TempDir(), "pid-one-dials")
	launcherDials.watch(t, dials)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := pidOneHelperEnvironment(t, "PATH=/usr/bin:/bin", "GOMAXPROCS=1",
		namespaceHelperEnv+"=1", "LEGION_LAUNCHER_TEST_ADDRESS=tcp://"+listener.Addr().String(),
		"LEGION_LAUNCHER_TEST_PRIVATE="+g.private, "LEGION_LAUNCHER_TEST_GRACE="+grace.String(),
		"LEGION_LAUNCHER_TEST_ENV_FILE="+g.entries, launcherDialsEnv+"="+dials)
	args := append(append(append([]string{}, prefix...), "/usr/bin/env", "-i"), environment...)
	args = append(args, self, "-test.run=^TestLauncherPIDOneHelper$")
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
	go func() { g.result <- command.Wait() }()
	t.Cleanup(func() {
		g.cancel()
		select {
		case err := <-g.result:
			if err != nil {
				t.Errorf("PID 1 launcher exited: %v\n%s", err, output.String())
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-g.result
			t.Error("PID 1 launcher did not stop after its context ended")
		}
	})
	g.accept()
	// A line of the test's own once the PID-one launcher is up, so a run's log shows the body ran
	// where a machine without the namespace would have skipped it.
	if entries, err := os.ReadFile(g.entries); err == nil {
		t.Logf("the PID-one launcher connected; %s", strings.TrimSpace(string(entries)))
	}
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
			case err := <-g.result:
				g.result <- err
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
