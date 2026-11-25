package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testkernel"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// startsIgnoring is whether a shell run as wrap has it run - inside a probe attempt's script, or
// bare, as a later child such as the controller's Oh My Pi runs - starts with the signal named sig
// (TSTP, TTOU) ignored. It reads that by behaviour, which every kernel shows: the shell sets a trap
// for the signal and sends the signal to itself. POSIX lets a non-interactive shell set no trap for
// a signal it was started ignoring, so there the signal is discarded and the shell prints only
// "sent"; a shell started with the signal at its default sets the trap, takes the signal, and
// prints "caught" first. Either way the shell does not stop, though both signals stop a process at
// their default. A kill that could not send the signal, or any other answer, fails the row: the
// row then has no answer, not a clean one.
func startsIgnoring(t *testing.T, wrap func(string) string, sig string) bool {
	t.Helper()
	probe := `trap "echo caught" ` + sig + `; kill -s ` + sig + ` $$ || exit 3; echo sent`
	out, err := exec.Command("sh", "-c", wrap("exec sh -c '"+probe+"'")).Output()
	if err != nil {
		t.Fatalf("a child shell's probe of its SIG%s: %v", sig, err)
	}
	switch string(out) {
	case "sent\n":
		return true
	case "caught\nsent\n":
		return false
	}
	t.Fatalf("a child shell's probe of its SIG%s printed %q, want %q or %q", sig, out, "sent\n", "caught\nsent\n")
	return false
}

// requireStartedAtDefault ends t where this test process was started with the signal named sig
// (TSTP, TTOU) ignored, as a child shell started bare shows it (startsIgnoring): the row could then
// not tell an ignore the attempt set from one this process inherited. Either way it first writes
// what it found to stderr, as one line that starts "signal probe:". Where the signal was ignored
// the row follows the daemon's rule for a test this machine cannot run (internal/launcher's
// skipOutsideCI): under GITHUB_ACTIONS it fails, since CI's runner starts go test with every signal
// at its default, so an ignore there is a broken runner or a probe that cannot see a default, never
// a green job with the proof gone from it; anywhere else it skips, naming why.
func requireStartedAtDefault(t *testing.T, sig string) {
	t.Helper()
	ignored := startsIgnoring(t, bare, sig)
	found, verdict := "trapped it", "at its default, so the row runs"
	if ignored {
		found, verdict = "could not trap it", "ignored, so the row cannot run"
	}
	_, _ = fmt.Fprintf(os.Stderr, "signal probe: %s needs this test process started with SIG%s at its default; a child shell started bare %s, so SIG%s is %s\n", t.Name(), sig, found, sig, verdict)
	if !ignored {
		return
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("this test process was started with SIG%s ignored (a child shell started bare could not trap it); CI's runner starts go test with every signal at its default, so this runner, or startsIgnoring, is broken", sig)
	}
	t.Skipf("this test process was started with SIG%s ignored (a child shell started bare could not trap it), so the row cannot tell an ignore the attempt set from one it inherited", sig)
}

// ignoredBy is the SigIgn mask of a process that reads its own /proc/self/status, run as wrap has
// it run (startsIgnoring). A row calls it only once testkernel.RequireSignalMasks has found the
// line, so a status without it fails the row.
func ignoredBy(t *testing.T, wrap func(string) string) uint64 {
	t.Helper()
	out, err := exec.Command("sh", "-c", wrap("exec sed -n 's/^SigIgn:[[:space:]]*//p' /proc/self/status")).Output()
	if err != nil {
		t.Fatalf("read a child's SigIgn: %v", err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		t.Fatal("a child's /proc/self/status has no SigIgn line, though this kernel's probe found one")
	}
	mask, err := strconv.ParseUint(value, 16, 64)
	if err != nil {
		t.Fatalf("parse SigIgn %q: %v", out, err)
	}
	return mask
}

func bit(sig syscall.Signal) uint64 { return 1 << (uint(sig) - 1) }

func bare(command string) string { return command }

// terminalAttempt is a terminal job whose terminal is /dev/null and an attempt for it, enough for
// attach and release to start and end this process's SIGTTOU immunity.
func terminalAttempt(t *testing.T) (*terminalJob, *exec.Cmd) {
	t.Helper()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devNull.Close() })
	attempt := exec.Command("true")
	attempt.SysProcAttr = &syscall.SysProcAttr{}
	return &terminalJob{fd: int(devNull.Fd()), owner: syscall.Getpgrp()}, attempt
}

// The attempt's SIGTTOU immunity ends with it: a child started after release - the controller's
// Oh My Pi - is stopped by SIGTTOU again rather than inheriting SIG_IGN, which signal.Reset alone
// leaves in place. The row reads a child's SIGTTOU by behaviour (startsIgnoring), which every
// kernel shows. Its subtest reads a child's whole ignore set from the kernel's record,
// /proc/<pid>/status's SigIgn, and requires the release to leave every signal as it was before the
// attempt; only a kernel that keeps that record can show it (testkernel.RequireSignalMasks), and
// the build cluster's gVisor keeps none.
func TestATerminalAttemptLeavesLaterChildrenSIGTTOUsDefault(t *testing.T) {
	// Not parallel: it changes the whole process's SIGTTOU disposition (terminalJob.attach, release).
	requireStartedAtDefault(t, "TTOU")
	job, attempt := terminalAttempt(t)
	job.attach(attempt)
	during := startsIgnoring(t, bare, "TTOU")
	job.release()
	if !during {
		t.Fatal("while the attempt holds the terminal, a child's shell can trap SIGTTOU: SIGTTOU is not ignored")
	}
	if startsIgnoring(t, bare, "TTOU") {
		t.Fatal("after release a child's shell starts with SIGTTOU ignored, as signal.Reset alone leaves it")
	}
	t.Run("a child's SigIgn after release is as before the attempt", func(t *testing.T) {
		testkernel.RequireSignalMasks(t, "SigIgn")
		before := ignoredBy(t, bare)
		job, attempt := terminalAttempt(t)
		job.attach(attempt)
		during := ignoredBy(t, bare)
		job.release()
		if during&bit(syscall.SIGTTOU) == 0 {
			t.Fatalf("while the attempt holds the terminal, SigIgn = %#x: SIGTTOU is not ignored", during)
		}
		if after := ignoredBy(t, bare); after != before {
			t.Fatalf("after release a child's SigIgn = %#x, want %#x as before the attempt", after, before)
		}
	})
}

// At the terminal the attempt ignores SIGTSTP, so a Ctrl-Z cannot stop the terminal's foreground
// job while this process waits in the background; an attempt run as the daemon's are does not.
// The row reads a child's SIGTSTP by behaviour (startsIgnoring), which every kernel shows, and its
// subtest reads the same from the kernel's record, /proc/<pid>/status's SigIgn, where the kernel
// keeps one (testkernel.RequireSignalMasks).
func TestATerminalAttemptIgnoresSIGTSTP(t *testing.T) {
	t.Parallel()
	requireStartedAtDefault(t, "TSTP")
	job := &terminalJob{}
	if !startsIgnoring(t, job.script, "TSTP") {
		t.Fatal("a terminal attempt's launch can trap SIGTSTP: SIGTSTP is not ignored")
	}
	if startsIgnoring(t, bare, "TSTP") {
		t.Fatal("this process's own SIGTSTP changed: a child's shell starts with it ignored")
	}
	t.Run("a terminal attempt's SigIgn holds SIGTSTP and this process's does not", func(t *testing.T) {
		testkernel.RequireSignalMasks(t, "SigIgn")
		if mask := ignoredBy(t, job.script); mask&bit(syscall.SIGTSTP) == 0 {
			t.Fatalf("a terminal attempt's SigIgn = %#x: SIGTSTP is not ignored", mask)
		}
		if mask := ignoredBy(t, bare); mask&bit(syscall.SIGTSTP) != 0 {
			t.Fatalf("this process's own SIGTSTP changed: a child's SigIgn = %#x", mask)
		}
	})
}

// A terminal attempt the operator's Ctrl-C ended is an interrupt whether it died of SIGINT or, as
// Oh My Pi does, caught it and exited 130; any other end is judged as an answer, and an attempt
// away from a terminal is never an interrupt.
func TestATerminalAttemptEndedByCtrlCIsInterrupted(t *testing.T) {
	t.Parallel()
	ended := func(script string) *os.ProcessState {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		_ = cmd.Run()
		return cmd.ProcessState
	}
	job := &terminalJob{}
	for _, tc := range []struct {
		name   string
		script string
		want   bool
	}{
		{"died of SIGINT", "kill -INT $$", true},
		{"caught it and exited 130", "exit 130", true},
		{"exited 1", "exit 1", false},
		{"died of SIGTERM", "kill -TERM $$", false},
		{"exited 0", "exit 0", false},
	} {
		if got := job.interrupted(ended(tc.script)); got != tc.want {
			t.Errorf("%s: interrupted = %v, want %v", tc.name, got, tc.want)
		}
	}
	var background *terminalJob
	if background.interrupted(ended("exit 130")) {
		t.Error("an attempt away from a terminal that exited 130 counts as interrupted")
	}
}

// At the terminal a Ctrl-C ends the attempt as interrupted even when the launch answers it and
// exits 0, as Oh My Pi does when the SIGINT lands late in `omp models`: the attempt's shell records
// the signal rather than reading it from the launch's status. Without a Ctrl-C the launch's own
// status comes through.
//
// Both ends of the Ctrl-C are facts of the run, not times: the SIGINT is sent once the launch has
// marked that it ignores SIGINT, which it does after the attempt's shell has set its own trap, and
// the launch exits only once the test writes its release, after the SIGINT was sent. A loaded
// machine can hold the attempt's shell past any fixed span before its trap, and a SIGINT that lands
// there ends the shell under the default disposition; it can hold the test past the launch's own
// end, and a SIGINT then finds no one.
func TestATerminalAttemptRecordsACtrlCTheLaunchExitsZeroOn(t *testing.T) {
	t.Parallel()
	job := &terminalJob{}
	run := func(ctrlC bool) *os.ProcessState {
		t.Helper()
		dir := t.TempDir()
		ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
		// The launch ignores SIGINT and finishes normally, exit 0: the case no exit status carries.
		// It runs until the test writes its release, or until this test process is gone. Its paths
		// reach it as arguments, quoted once (shellprefix.Literal).
		launch := fmt.Sprintf(`exec sh -c 'trap "" INT; : >"$1"; while [ ! -e "$2" ] && kill -0 %d 2>/dev/null; do sleep 0.01; done; exit 0' sh %s %s`,
			os.Getpid(), shellprefix.Literal(ready), shellprefix.Literal(release))
		cmd := exec.Command("sh", "-c", job.script(launch))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if !testwait.Holds(testwait.Remaining(t), testwait.Poll, func() bool {
			_, err := os.Stat(ready)
			return err == nil
		}) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatalf("the launch never marked that it ignores SIGINT (%s)", ready)
		}
		if ctrlC {
			// The terminal's Ctrl-C reaches the whole foreground process group.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatalf("release the launch: %v", err)
		}
		_ = cmd.Wait()
		return cmd.ProcessState
	}
	if state := run(true); state.ExitCode() != 130 || !job.interrupted(state) {
		t.Fatalf("a Ctrl-C the launch answered with exit 0: exit %d, interrupted %v; want 130 and interrupted", state.ExitCode(), job.interrupted(state))
	}
	if state := run(false); state.ExitCode() != 0 || job.interrupted(state) {
		t.Fatalf("no Ctrl-C: exit %d, interrupted %v; want the launch's 0 and not interrupted", state.ExitCode(), job.interrupted(state))
	}
}

type chunks []string

func (c *chunks) Write(p []byte) (int, error) {
	*c = append(*c, string(p))
	return len(p), nil
}

// The terminal sees the attempt's stderr as it arrives, without the load probe's report.
func TestTheTerminalCopyDropsTheProbesReportAndShowsAPromptAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		writes []string
		want   []string
	}{
		{"a prompt with no newline", []string{"Enter PIN: "}, []string{"Enter PIN: "}},
		{"the report dropped whole, the rest kept", []string{"warn\nLEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file:///x\ndone\n"}, []string{"warn\ndone\n"}},
		{"a marker split across writes", []string{"LEGION_PLU", "GIN_LOADED=no\n", "ok\n"}, []string{"ok\n"}},
		{"a line that only starts like one", []string{"LEGION_P", "ROFILE=x\n"}, []string{"LEGION_PROFILE=x\n"}},
		{"the report right after a prompt", []string{"Enter PIN: ", "LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file:///x\n"}, []string{"Enter PIN: "}},
		{"a false start inside a marker", []string{"LEGION_PLLEGION_PLUGIN_LOADED=no\nok\n"}, []string{"LEGION_PLok\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got chunks
			filter := &markerFilter{w: &got}
			for _, write := range tc.writes {
				if n, err := filter.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", write, n, err, len(write))
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("the terminal got %q, want %q", got, tc.want)
			}
		})
	}
}
