package testbin

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Oh My Pi's daemon broker leads a session of its own and writes under the HOME it was started
// with as it shuts down, making again each directory a removal took away; the browser it runs
// ends on SIGTERM. This stands in for both with two shells: one that leads its own session and
// writes a record under its HOME without pause, making its directories as it goes, and one that
// takes no SIGTERM. The test that starts them ends, and each must have gone and its HOME been
// removed by then. A third process, under another HOME, must still run.
func TestTheEndOfATestEndsWhatStillRunsUnderItsOMPHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the processes under a HOME are read from /proc")
	}
	stranger := startUnder(t, t, t.TempDir(), "while :; do sleep 1; done")
	var home string
	var writer, deaf *underHome
	passed := t.Run("a HOME a detached writer and a process deaf to SIGTERM still run under", func(sub *testing.T) {
		home = sub.TempDir()
		stopAtEnd(sub, home, time.Second)
		record := filepath.Join(home, "run", "daemons", "scope", "daemons", "browser", "meta.json")
		writer = startUnder(sub, t, home, `while :; do mkdir -p "$(dirname "$RECORD")"; : > "$RECORD"; done`, "RECORD="+record)
		deaf = startUnder(sub, t, home, "trap '' TERM; while :; do sleep 1; done")
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(record); err == nil {
				break
			}
			if time.Now().After(deadline) {
				sub.Fatalf("the writer under %s wrote no %s within 10s", home, record)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if !passed {
		t.Errorf("the test that ran under %s failed at its end", home)
	}
	for _, p := range []*underHome{writer, deaf} {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Errorf("the %s (pid %d) still runs under %s after the test that started it ended", p.name, p.cmd.Process.Pid, home)
		}
	}
	if _, err := os.Stat(home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still there after the test that made it ended (%v), want it removed", home, err)
	}
	select {
	case <-stranger.done:
		t.Errorf("the process under another HOME (pid %d) ended with the test, want it left running", stranger.cmd.Process.Pid)
	default:
	}
}

// OMPHome is what every real-binary test readies its HOME with, so it is the one place the end of
// a test ends what still runs under that HOME: a HOME readied by OMPHome, with a detached writer
// still running under it at the test's end, is left with nothing running and is removed. The
// case above holds stopAtEnd itself; this one holds that OMPHome registers it.
func TestOMPHomeEndsWhatStillRunsUnderTheHomeItReadies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the processes under a HOME are read from /proc")
	}
	omp := OMP(t)
	var home string
	var writer *underHome
	passed := t.Run("a HOME OMPHome readied, a detached writer still running under it", func(sub *testing.T) {
		home = sub.TempDir()
		OMPHome(sub, omp, home)
		record := filepath.Join(home, "run", "daemons", "scope", "meta.json")
		writer = startUnder(sub, t, home, `while :; do mkdir -p "$(dirname "$RECORD")"; : > "$RECORD"; done`, "RECORD="+record)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(record); err == nil {
				break
			}
			if time.Now().After(deadline) {
				sub.Fatalf("the writer under %s wrote no %s within 10s", home, record)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if !passed {
		t.Errorf("the test that ran under %s failed at its end", home)
	}
	if writer == nil {
		t.FailNow() // the subtest failed before it started the writer, and said why
	}
	select {
	case <-writer.done:
	case <-time.After(5 * time.Second):
		t.Errorf("the writer (pid %d) still runs under %s after the test whose HOME OMPHome readied ended", writer.cmd.Process.Pid, home)
	}
	if _, err := os.Stat(home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still there after the test that made it ended (%v), want it removed", home, err)
	}
}

// ProcessStat is the one decoder of /proc/<pid>/stat the tests share: this package's cleanup reads
// it, and so does internal/daemon's local runtime for the live proofs. Its fields are counted from
// after the command name's last parenthesis, since the name may hold spaces and parentheses of its
// own, so a process named "a (b) c" reads the start ticks its record holds. A process that has
// exited and is not yet reaped reads as a zombie, and once reaped its pid reads as gone.
func TestProcessStatDecodesTheRecordWhateverTheCommandIsNamed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the records are read from /proc")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH to name")
	}
	const name = "a (b) c"
	named := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(sleep, named); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(named, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %q: %v", named, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	record, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// The record read by hand, with the name known: field 22 is the 20th field after it.
	after, found := strings.CutPrefix(string(record), strconv.Itoa(pid)+" ("+name+") ")
	if !found {
		t.Fatalf("the record of %s does not name it %q: %s", named, name, record)
	}
	want, err := strconv.ParseUint(strings.Fields(after)[22-3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	state, ticks, gone, err := ProcessStat(pid)
	if err != nil || gone || ticks != want || (state != "S" && state != "R") {
		t.Fatalf("ProcessStat(%d) = %q, %d, gone %t, %v; want sleeping or running, %d, not gone", pid, state, ticks, gone, err, want)
	}

	_ = cmd.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for state != "Z" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		state, _, _, _ = ProcessStat(pid)
	}
	if state != "Z" {
		t.Fatalf("ProcessStat(%d) reads state %q after the kill and before the reap, want Z", pid, state)
	}
	_ = cmd.Wait()
	if _, _, gone, err := ProcessStat(pid); !gone || err != nil {
		t.Fatalf("ProcessStat(%d) after the reap = gone %t, %v; want gone", pid, gone, err)
	}
}

// underHome is one shell a test started under a HOME, and the channel its Wait closes.
type underHome struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
}

// startUnder starts script under sh -c with HOME=home, leading a session of its own, as Oh My Pi
// starts its daemon broker. Its kill is owner's cleanup, which runs after the cleanups of the test
// t that started it, so it never ends a process before the cleanup under test has had its turn,
// and a run whose subject leaves the process running leaves nothing behind.
func startUnder(t, owner *testing.T, home, script string, env ...string) *underHome {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %q under %s: %v", script, home, err)
	}
	p := &underHome{name: "shell " + script, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	owner.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	})
	return p
}
