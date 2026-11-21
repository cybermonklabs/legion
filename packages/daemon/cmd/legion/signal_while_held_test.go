package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// holdsStandIn is the end of a stand-in script that holds its work: it records its pid in pidFile,
// written whole by a rename, and becomes `sleep <seconds>` under that pid, held far past any bound
// of the rows that use it, with a command line ghroutetest.HelperRunning tells from a pid the
// kernel has reused. Each row gives its stand-in a sleep of its own.
func holdsStandIn(pidFile, seconds string) string {
	return "echo $$ >'" + pidFile + ".new'\nmv '" + pidFile + ".new' '" + pidFile + "'\nexec sleep " + seconds + "\n"
}

// signalWhileHeld starts command, the `legion` command name names, as the leader of a process
// group of its own, the way a shell's job control starts a command, waits for the stand-in that
// holds its work (holdsStandIn, under pidFile and seconds) to be running, and sends sig to that
// group, as a terminal does. It fails the test unless command ends with 128+sig within a few
// seconds, having printed nothing, and the stand-in is gone after it; what names the held work in
// a failure. A stand-in or a command still running when the test ends is killed, so a failure
// leaves nothing behind.
func signalWhileHeld(t *testing.T, command *exec.Cmd, name, pidFile, seconds, what string, sig syscall.Signal) {
	t.Helper()
	ghroutetest.KillRecordedHelper(t, pidFile, seconds)
	// The group is the command's own, so the signal below never reaches this test binary's.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Standard output and standard error are a file rather than a pipe, so nothing of the
	// stand-in's can hold the wait open.
	printed, err := os.Create(filepath.Join(t.TempDir(), "printed"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { printed.Close() })
	command.Stdout, command.Stderr = printed, printed
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waited error
	go func() {
		waited = command.Wait()
		close(exited)
	}()
	// A command still running when the test ends is killed by its handle, which never signals a
	// pid the kernel has given to another process since.
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = command.Process.Kill()
			<-exited
		}
	})
	var pid int
	testwait.Eventually(t, "the stand-in to hold "+what, func() bool {
		var running bool
		pid, running = ghroutetest.RecordedHelper(pidFile, seconds)
		return running
	})
	if err := syscall.Kill(-command.Process.Pid, sig); err != nil {
		t.Fatal(err)
	}
	// The held work's group is stopped with a SIGTERM, then a SIGKILL after procgroup.StopGrace.
	// The stand-in, a sleep, ends on the SIGTERM, and the bound leaves room for a loaded machine
	// while staying far short of ghroute's helpTimeout, ReadHelp's bound, which a help read
	// nothing stops runs to.
	bound := procgroup.StopGrace + 5*time.Second
	want := 128 + int(sig)
	select {
	case <-exited:
	case <-time.After(bound):
		t.Fatalf("%s is still running %s after signal %d (%v) to its group while %s was held; want it to stop that work and end with %d: %s", name, bound, int(sig), sig, what, want, readRecord(t, printed.Name()))
	}
	if exit, ok := waited.(*exec.ExitError); !ok || exit.ExitCode() != want {
		t.Errorf("%s after signal %d (%v) to its group while %s was held = %v; want exit %d: %s", name, int(sig), sig, what, waited, want, readRecord(t, printed.Name()))
	}
	// A signal is no answer about the work it stopped, so the command reports none: the setup
	// failure of a check the signal stopped, printed, would read as a broken check.
	if out := readRecord(t, printed.Name()); out != "" {
		t.Errorf("%s after signal %d (%v) to its group while %s was held printed %q; want nothing", name, int(sig), sig, what, out)
	}
	for deadline := time.Now().Add(bound); ghroutetest.HelperRunning(pid, seconds); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in (pid %d) holding %s is still running %s after signal %d (%v) to %s's group; want its process group stopped", pid, what, bound, int(sig), sig, name)
		}
	}
}
