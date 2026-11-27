// Package testkernel reads whether this machine's kernel has what a test needs, and ends the test
// where it does not, under the daemon's rule for a test this machine cannot run
// (internal/launcher's skipOutsideCI): outside CI the test skips, naming what is missing; under
// GITHUB_ACTIONS it fails, since CI's runners run Linux, which has it, so a runner that lacks it is
// broken, never a green job with the proof gone from it. Either way the test first writes what the
// probe found to stderr, as one line that starts "kernel probe:", the line cmd/cipher's
// requireKernel writes (packages/envoy/cmd/cipher), so a run on any machine says which it was.
// go test shows that line wherever it shows the test binary's output: with -v or -json, for a
// package that fails, and for the binary run directly.
//
// It also holds what a row reads from a process's own run where /proc's signal masks cannot say
// it, as on gVisor: Child, a child a command leaves in its process group, which tells whether a
// stop of that group reached it by the run's return, and ProcessEnded, whether a process is gone.
package testkernel

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// require ends t where this kernel lacks what needs names, as a probe found it: has is the probe's
// answer, and found says what it saw.
func require(t testing.TB, has bool, needs, found string) {
	t.Helper()
	verdict := "has it"
	if !has {
		verdict = "lacks it"
	}
	_, _ = fmt.Fprintf(os.Stderr, "kernel probe: %s needs %s; this kernel %s, so it %s\n", t.Name(), needs, found, verdict)
	if has {
		return
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("this kernel lacks what the test needs, %s: it %s; CI's runners have it, so this runner is broken", needs, found)
	}
	t.Skipf("this kernel lacks what the test needs, %s: it %s", needs, found)
}

// maskProbe is the shell text whose status RequireSignalMasks reads: the shell ignores SIGUSR1 and
// holds a handler for SIGUSR2, and a child of it, cat, prints the shell's own /proc/<pid>/status.
// The cat is not the shell's last command, so the shell forks it rather than exec it, which would
// reset the handler. The shell starts with both signals at their default whatever this process
// was started with: the Go runtime holds a handler of its own for each, which an exec resets.
const maskProbe = `trap '' USR1; trap : USR2; cat "/proc/$$/status" || exit 3; exit 0`

// shows is, for each mask line the probe reads, the signal whose bit maskProbe's shell sets there.
var shows = map[string]struct {
	signal syscall.Signal
	name   string
}{
	"SigIgn": {syscall.SIGUSR1, "SIGUSR1"},
	"SigCgt": {syscall.SIGUSR2, "SIGUSR2"},
}

// probeStatus is the status maskProbe printed, read once per test process.
var probeStatus = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("sh", "-c", maskProbe).Output()
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return "", fmt.Errorf("could not read the /proc/<pid>/status of a shell (%v: %s)", err, strings.TrimSpace(string(exited.Stderr)))
		}
		return "", fmt.Errorf("could not read the /proc/<pid>/status of a shell (%v)", err)
	}
	return string(out), nil
})

// RequireSignalMasks ends t, under the package's rule, where this kernel's /proc/<pid>/status does
// not record what a process does with a signal in each of lines: "SigIgn", the signals the process
// ignores, and "SigCgt", the signals it holds a handler for, each a mask in hex with bit N-1 for
// signal N. A row that reads such a mask calls it first. Linux records both. gVisor's
// release-20260803.0 records neither, and a line that is there need not follow the process: that
// gVisor's /proc/<pid>/stat gives 0 for the masks of a process that ignores a signal. So the probe
// reads what each line means as well as whether it is there: a shell that ignores SIGUSR1 and holds
// a handler for SIGUSR2 reads its own status (maskProbe), which must show SIGUSR1's bit in SigIgn
// and SIGUSR2's in SigCgt. Where /proc cannot be read, as on darwin, the kernel lacks it too.
func RequireSignalMasks(t testing.TB, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if _, ok := shows[line]; !ok {
			t.Fatalf("testkernel: no probe reads the %s line of /proc/<pid>/status", line)
		}
	}
	needs := "/proc/<pid>/status to record what a process does with each signal (its " + strings.Join(lines, " and ") + " line"
	if len(lines) > 1 {
		needs += "s"
	}
	needs += ")"
	has, found := false, ""
	if status, err := probeStatus(); err != nil {
		found = err.Error()
	} else {
		has, found = judge(status, lines)
		found += " in the /proc/<pid>/status of a shell that ignores SIGUSR1 and catches SIGUSR2"
	}
	require(t, has, needs, found)
}

// judge is whether status, the probe shell's own, shows in each of lines the bit of the signal the
// shell set there (shows), and what it showed.
func judge(status string, lines []string) (bool, string) {
	has := true
	found := make([]string, 0, len(lines))
	for _, line := range lines {
		sig := shows[line]
		bit := uint64(1) << (uint(sig.signal) - 1)
		value, present := field(status, line)
		mask, err := strconv.ParseUint(value, 16, 64)
		switch {
		case !present:
			has = false
			found = append(found, "showed no "+line+" line")
		case err != nil:
			has = false
			found = append(found, fmt.Sprintf("showed %s %q, which is no mask", line, value))
		case mask&bit == 0:
			has = false
			found = append(found, fmt.Sprintf("showed %s %s without %s's bit %#x", line, value, sig.name, bit))
		default:
			found = append(found, fmt.Sprintf("showed %s %s with %s's bit %#x", line, value, sig.name, bit))
		}
	}
	return has, strings.Join(found, ", and ")
}

// field is the value of the line named name in a /proc/<pid>/status, and whether it has one.
func field(status, name string) (string, bool) {
	for line := range strings.SplitSeq(status, "\n") {
		if value, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}
