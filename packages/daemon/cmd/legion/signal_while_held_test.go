package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// recordsChild is a stand-in's line of shell that starts `sleep <seconds>` in the background and
// writes its pid to pidFile whole, by a rename, so a reader polling pidFile
// (ghroutetest.RecordedHelper) sees no pid or the whole one. The child inherits the stand-in's
// descriptors, its output pipe among them. It ends with no separator, so a caller adds its own
// next command after "\n" or "; ". Each path is one literal shell word (shellprefix.Literal).
func recordsChild(pidFile, seconds string) string {
	return "sleep " + seconds + " & echo $! >" + shellprefix.Literal(pidFile+".new") + "; mv " + shellprefix.Literal(pidFile+".new") + " " + shellprefix.Literal(pidFile)
}

// signalWhileHeld starts command, the `legion` command name names, as the leader of a process
// group of its own, the way a shell's job control starts a command, waits for the stand-in that
// holds its work (ghroutetest.HoldsStandIn, under pidFile and seconds) to be running, and sends sig
// to that group, as a terminal does. It fails the test unless command ends with 128+sig, having
// printed nothing, and the stand-in is gone after it; what names the held work in a failure. Each
// of its waits, for the stand-in, the command's end and the stand-in's, has the test binary's
// deadline less testBoundMargin as its only bound, so a step a loaded machine makes slow passes,
// and a stop that never comes fails the row by name. The command itself gets a deadline two days
// off (testDeadlineEnv), so the bounds of the work it holds, its help read's and its guard's
// reads, outlast every wait here: only the signal can end the held work before the row gives up,
// and a command whose signal path let the held work run on to its own bound fails the row. A
// stand-in or a command still running when the test ends is killed, so a failure leaves nothing
// behind.
func signalWhileHeld(t *testing.T, command *exec.Cmd, name, pidFile, seconds, what string, sig syscall.Signal) {
	t.Helper()
	ghroutetest.KillRecordedHelper(t, pidFile, seconds)
	// A later value of a variable in Env wins (exec dedups it), so this replaces the deadline
	// legionCommand handed the command.
	command.Env = append(command.Env, testDeadlineEnv+"="+time.Now().Add(48*time.Hour).Format(time.RFC3339Nano))
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
	// pid the kernel has given to another process since, and what it had printed is logged.
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = command.Process.Kill()
			<-exited
			t.Logf("%s still ran when the test ended, having printed: %s", name, readRecord(t, printed.Name()))
		}
	})
	var pid int
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll, "the stand-in to hold "+what, func() bool {
		var running bool
		pid, running = ghroutetest.RecordedHelper(pidFile, seconds)
		return running
	})
	// The signal must come while the stand-in holds the work: with no running pid recorded, the
	// end check below would pass a signal that came before the work was ever held (a pid of 0 is
	// never running), so the row fails here by name instead.
	if pid <= 0 || !ghroutetest.HelperRunning(pid, seconds) {
		t.Fatalf("the stand-in holding %s had recorded no running pid (%d) in %s when the signal was to be sent: want the signal while %s is held", what, pid, pidFile, what)
	}
	if err := syscall.Kill(-command.Process.Pid, sig); err != nil {
		t.Fatal(err)
	}
	// The held work's group is stopped with a SIGTERM, then a SIGKILL after procgroup.StopGrace,
	// and the stand-in, a sleep, ends on the SIGTERM; nothing else ends it before the row gives up,
	// since the work's own bounds are two days off.
	want := 128 + int(sig)
	testwait.Receive(t, fmt.Sprintf("%s to end after signal %d (%v) to its group while %s was held", name, int(sig), sig, what), exited)
	if exit, ok := waited.(*exec.ExitError); !ok || exit.ExitCode() != want {
		t.Errorf("%s after signal %d (%v) to its group while %s was held = %v; want exit %d: %s", name, int(sig), sig, what, waited, want, readRecord(t, printed.Name()))
	}
	// A signal is no answer about the work it stopped, so the command reports none: the setup
	// failure of a check the signal stopped, printed, would read as a broken check.
	if out := readRecord(t, printed.Name()); out != "" {
		t.Errorf("%s after signal %d (%v) to its group while %s was held printed %q; want nothing", name, int(sig), sig, what, out)
	}
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll,
		fmt.Sprintf("the stand-in (pid %d) holding %s to end after signal %d (%v) to %s's group: want its process group stopped", pid, what, int(sig), sig, name),
		func() bool { return !ghroutetest.HelperRunning(pid, seconds) })
}
