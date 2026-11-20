package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/envoy/heldprocess"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The commands an operator runs from a terminal whose work outlives a kill of the command alone,
// each of which runs under stoppingOnHangupOrQuit: `legion start`, whose boot gate runs in process
// groups of its own; `legion controller start`, whose probe does; and `legion probe-boot`, whose
// probe Sandbox lives in the cluster until its context ends. A command left out of the watch, or
// one added to it, fails this by name. The hidden commands that also start such work are left
// out on purpose, since nothing runs them from a terminal: `legion workspace-init`'s fetch,
// provision and cache are an issue pod's init containers and a cache pod's container
// (internal/runtime/sandbox's podkind.go and repocache.go), and `legion probe-image` is the
// command of the daemon's probe Sandbox and of the image build (internal/runtime/sandbox's
// probe.go); the kubelet stops each with a SIGTERM, which main's context catches.
func TestTheTerminalCommandsWhoseWorkOutlivesThemRunUnderTheHangupAndQuitWatch(t *testing.T) {
	want := []string{"legion controller start", "legion probe-boot", "legion start"}
	var got []string
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Annotations[stopsOnHangupOrQuit] == "true" {
			got = append(got, cmd.CommandPath())
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(newRoot(io.Discard, io.Discard))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the commands under the hang-up and quit watch = %q; want %q", got, want)
	}
}

// hangupWatchHelper is the variable that makes TestHangupOrQuitWatchHelper run: it names the file
// the held stand-in records its pid in, beside the stand-in's sleep in hangupWatchSeconds. Both
// are LEGION_TEST_ names, which TestMain's fixtureenv.MustGuard keeps for the helper.
const (
	hangupWatchHelper  = "LEGION_TEST_HANGUP_WATCH_HELPER"
	hangupWatchSeconds = "LEGION_TEST_HANGUP_WATCH_SECONDS"
)

// TestHangupOrQuitWatchHelper is the process TestAHangupOrQuitStopsTheWorkOfACommandUnderTheWatch
// signals: a command run through stoppingOnHangupOrQuit, as `legion start`, `legion controller
// start` and `legion probe-boot` are, whose body runs a stand-in in a process group of its own
// under the command's context (procgroup.Configure) and waits for it. It exits with the command's
// status, printing nothing.
func TestHangupOrQuitWatchHelper(t *testing.T) {
	pidFile := os.Getenv(hangupWatchHelper)
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	cmd := &cobra.Command{Use: "held", SilenceErrors: true, SilenceUsage: true}
	stoppingOnHangupOrQuit(cmd, func(ctx context.Context, _ []string) int {
		held := exec.CommandContext(ctx, "sh", "-c", heldprocess.Script(pidFile, os.Getenv(hangupWatchSeconds)))
		procgroup.Configure(held)
		if err := held.Run(); err != nil {
			return 1
		}
		return 0
	})
	cmd.SetArgs(nil)
	code := 0
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		var status exitCode
		if !errors.As(err, &status) {
			os.Exit(2)
		}
		code = int(status)
	}
	os.Exit(code)
}

// TestAHangupOrQuitStopsTheWorkOfACommandUnderTheWatch: a SIGHUP, a terminal that closes, or a
// SIGQUIT, its quit key, sent to the terminal's foreground group reaches a command run under
// stoppingOnHangupOrQuit but not the work it runs in a process group of its own; the command
// catches it, stops that work's group, and ends with 128+<signal> having printed nothing. The
// helper is started as the leader of a group of its own and its held stand-in is the work
// (signalWhileHeld). Without the watch the signal's default action killed the command where it
// stood and left the stand-in running, reparented and waited for by nobody.
func TestAHangupOrQuitStopsTheWorkOfACommandUnderTheWatch(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this test reads from it whether the stand-in is still running", err)
	}
	for _, tc := range []struct {
		name    string
		signal  syscall.Signal
		seconds string
	}{
		{"SIGHUP", syscall.SIGHUP, "287"},
		{"SIGQUIT", syscall.SIGQUIT, "286"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "stand-in")
			command := exec.Command(os.Args[0], "-test.run=^TestHangupOrQuitWatchHelper$", "-test.count=1")
			command.Env = append(os.Environ(), hangupWatchHelper+"="+pidFile, hangupWatchSeconds+"="+tc.seconds)
			signalWhileHeld(t, command, "a command under the hang-up and quit watch", pidFile, tc.seconds, "its work", tc.signal)
		})
	}
}

// controllerSignalHelper is the variable that makes TestControllerStartSignalHelper run: it names
// the directory the helper and the test share.
const controllerSignalHelper = "LEGION_TEST_CONTROLLER_SIGNAL_DIR"

// TestControllerStartSignalHelper is the process
// TestASignalToControllerStartWhileOhMyPiRunsEndsOhMyPiAndTheCommand signals: the real command
// line of `legion controller start` (operatorMachine.runUnder), under main's own signal context
// (mainContext), as the binary runs it, whose recording Oh My Pi is held (LEGION_TEST_HOLD),
// ignoring the signals LEGION_TEST_HOLD_IGNORES names and recording any other it gets. It writes
// the recording directory's path to `record` in its directory, its stderr to `stderr` once the
// command ends, and exits with the command's status.
func TestControllerStartSignalHelper(t *testing.T) {
	dir := os.Getenv(controllerSignalHelper)
	if dir == "" {
		t.Skip("subprocess helper")
	}
	c := newControllerStart(t, newControllerDaemon(t), controllerOptions{})
	t.Setenv("LEGION_TEST_HOLD", filepath.Join(dir, "release"))
	if err := os.WriteFile(filepath.Join(dir, "record"), []byte(c.record), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := mainContext()
	code, _, stderr := c.runUnder(ctx)
	stop()
	_ = os.WriteFile(filepath.Join(dir, "stderr"), []byte(stderr), 0o600)
	os.Exit(code)
}

// TestASignalToControllerStartWhileOhMyPiRunsEndsOhMyPiAndTheCommand: a SIGINT, a SIGHUP, a
// SIGQUIT or a SIGTERM sent to `legion controller start`'s own pid while its Oh My Pi runs - a
// `kill` from another terminal, which the terminal's own group, Oh My Pi's, does not get - is sent
// on to Oh My Pi (runOhMyPi), which records the signal it got, and the command ends with
// 128+<signal> once Oh My Pi has: at once where Oh My Pi ends on the signal, and at
// ohMyPiStopGrace, with Oh My Pi killed, where it ignores it. Before, the command caught the
// signal, cancelled a context Oh My Pi is not bound to, and waited for Oh My Pi for as long as the
// session lasted, with Oh My Pi never told. The record is what tells a signal sent on from the kill
// at the grace, which ends Oh My Pi inside the same bound with no signal to end its session on.
func TestASignalToControllerStartWhileOhMyPiRunsEndsOhMyPiAndTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		signal  syscall.Signal
		ignores string
	}{
		{"SIGINT", syscall.SIGINT, ""},
		{"SIGHUP", syscall.SIGHUP, ""},
		{"SIGQUIT", syscall.SIGQUIT, ""},
		{"SIGTERM", syscall.SIGTERM, ""},
		{"SIGHUP to an Oh My Pi that ignores it", syscall.SIGHUP, "HUP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := exec.Command(os.Args[0], "-test.run=^TestControllerStartSignalHelper$", "-test.count=1")
			helper.Env = append(os.Environ(), controllerSignalHelper+"="+dir, "LEGION_TEST_HOLD_IGNORES="+tc.ignores)
			printed, err := os.Create(filepath.Join(dir, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { printed.Close() })
			helper.Stdout, helper.Stderr = printed, printed
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() {
				_ = helper.Wait()
				close(exited)
			}()
			// The held Oh My Pi ends on the release whatever it ignores, and the helper's handle
			// kills only the helper's own pid.
			t.Cleanup(func() {
				_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
				select {
				case <-exited:
				case <-time.After(30 * time.Second):
					_ = helper.Process.Kill()
					<-exited
				}
			})
			var omp int
			testwait.Eventually(t, "the controller's Oh My Pi to be held", func() bool {
				record, err := os.ReadFile(filepath.Join(dir, "record"))
				if err != nil {
					return false
				}
				held, err := os.ReadFile(filepath.Join(string(record), "held"))
				if err != nil {
					return false
				}
				omp, err = strconv.Atoi(strings.TrimSpace(string(held)))
				return err == nil && ohMyPiRuns(omp)
			})
			if err := helper.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			bound := ohMyPiStopGrace + 5*time.Second
			select {
			case <-exited:
			case <-time.After(bound):
				raw, _ := os.ReadFile(printed.Name())
				t.Fatalf("legion controller start still runs %s after a %v to its pid while Oh My Pi (pid %d) ran; want it to send the signal on and end within ohMyPiStopGrace: %s", bound, tc.signal, omp, raw)
			}
			if want := 128 + int(tc.signal); helper.ProcessState.ExitCode() != want {
				stderr, _ := os.ReadFile(filepath.Join(dir, "stderr"))
				t.Errorf("legion controller start after a %v to its pid = %v; want exit %d: %s", tc.signal, helper.ProcessState, want, stderr)
			}
			if ohMyPiRuns(omp) {
				t.Errorf("the controller's Oh My Pi (pid %d) still runs after legion controller start ended on a %v to its pid; want it ended first", omp, tc.signal)
			}
			record, _ := os.ReadFile(filepath.Join(dir, "record"))
			caught, err := os.ReadFile(filepath.Join(string(record), "held-caught"))
			got := strings.TrimSpace(string(caught))
			if want := strings.TrimPrefix(tc.name, "SIG"); tc.ignores == "" && got != want {
				t.Errorf("the controller's Oh My Pi recorded %q (%v) after a %v to legion controller start's pid; want %q: the signal was not sent on to it", got, err, tc.signal, want)
			}
			if tc.ignores != "" && got != "" {
				t.Errorf("the controller's Oh My Pi, ignoring %s, recorded %q; want nothing, and a kill at ohMyPiStopGrace", tc.ignores, got)
			}
		})
	}
}

// ohMyPiRuns says whether pid is still the recording Oh My Pi: a shell running the script named
// omp, read from /proc (heldprocess.CommandLine), so a pid the kernel has given out since does not
// count.
func ohMyPiRuns(pid int) bool {
	argv := heldprocess.CommandLine(pid)
	return len(argv) >= 2 && filepath.Base(argv[1]) == "omp"
}
