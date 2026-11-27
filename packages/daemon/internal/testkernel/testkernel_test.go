package testkernel

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The probe judges the status its shell read of itself, so each kind of kernel is a status here:
// Linux's, which records the shell's SIGUSR1 ignore and SIGUSR2 handler; the build cluster's
// gVisor's (kernel 4.19.0-gvisor), which has no mask line at all; and a kernel that prints the
// lines but not the process, all zero, as that gVisor's /proc/<pid>/stat gives them. Only the
// first has what a mask row needs, and each answer names what it saw.
func TestTheProbeHasTheMasksOnlyWhereTheyRecordItsShell(t *testing.T) {
	const linux = "Name:\tsh\nState:\tS (sleeping)\nSigQ:\t2/1546861\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000000000\n" +
		"SigBlk:\t0000000000000000\nSigIgn:\t0000000000000200\nSigCgt:\t0000000000010802\nCapInh:\t0000000000000000\n"
	const gvisor = "Name:\tsh\nState:\tS (sleeping)\nThreads:\t1\nCapInh:\t0000000000000000\nSeccomp:\t0\n"
	const zero = "Name:\tsh\nSigBlk:\t0000000000000000\nSigIgn:\t0000000000000000\nSigCgt:\t0000000000000000\n"
	both := []string{"SigIgn", "SigCgt"}
	for _, tc := range []struct {
		name, status string
		lines        []string
		has          bool
		found        string
	}{
		{"Linux, both lines", linux, both, true, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200, and showed SigCgt 0000000000010802 with SIGUSR2's bit 0x800"},
		{"Linux, SigIgn alone", linux, []string{"SigIgn"}, true, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200"},
		{"gVisor, no mask line", gvisor, both, false, "showed no SigIgn line, and showed no SigCgt line"},
		{"lines that do not follow the process", zero, both, false, "showed SigIgn 0000000000000000 without SIGUSR1's bit 0x200, and showed SigCgt 0000000000000000 without SIGUSR2's bit 0x800"},
		{"the ignore recorded and the handler not", strings.Replace(linux, "0000000000010802", "0000000000010002", 1), both, false, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200, and showed SigCgt 0000000000010002 without SIGUSR2's bit 0x800"},
		{"a line that is no mask", strings.Replace(linux, "0000000000000200", "-", 1), []string{"SigIgn"}, false, `showed SigIgn "-", which is no mask`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			has, found := judge(tc.status, tc.lines)
			if has != tc.has || found != tc.found {
				t.Fatalf("judge = %t, %q; want %t, %q", has, found, tc.has, tc.found)
			}
		})
	}
}

// A Child tells each way a stop of its group can meet it from its own run. Each case runs the
// command ChildCommand writes as procgroup.Configure runs one, in a process group of its own whose
// output a pipe carries, under a WaitDelay, and does to the group what the case names:
//   - nothing: the child beats on past the run's return, which After reads as no stop.
//   - a SIGTERM once the run has returned, when Wait has closed the pipe's read end: the trap's
//     write meets EPIPE, a stop after the run, at a moment past the run's return.
//   - a SIGTERM while Wait still reads the pipe, before a WaitDelay longer than the case: the write
//     goes through, a stop After cannot judge.
//   - a SIGKILL once the run has returned: the child ends with no record, a stop it cannot judge.
//
// A command that never started the child leaves no ready mark, and a child whose beat never passes
// the return After is given is, once After's bound has passed, a run After cannot tell.
func TestAChildTellsWhatAStopOfItsGroupMetFromItsOwnRun(t *testing.T) {
	until := time.Now().Add(testwait.Bounded(t, time.Minute, 5*time.Second))
	// run runs the command once, does during to its group (its leader's pid) while Wait has not yet
	// begun, and answers the child and the time since boot the run returned at.
	run := func(t *testing.T, waitDelay time.Duration, during func(group int, c *Child)) (*Child, time.Duration, int) {
		t.Helper()
		c := NewChild(t)
		cmd := exec.Command("sh", append([]string{"-c", ChildCommand("echo answered"), "sh"}, c.Args()...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.WaitDelay = waitDelay
		if err := cmd.Start(); err != nil {
			t.Fatalf("start the command: %v", err)
		}
		group := cmd.Process.Pid
		if during != nil {
			during(group, c)
		}
		_ = cmd.Wait()
		return c, testexec.SinceBoot(t), group
	}
	const short = 10 * time.Millisecond

	t.Run("no stop", func(t *testing.T) {
		c, returned, _ := run(t, short, nil)
		if seen := c.After(t, returned, until); seen != (Stop{None: true}) {
			t.Fatalf("After = %+v, want no stop: nothing signalled the group", seen)
		}
	})
	t.Run("a SIGTERM after the run", func(t *testing.T) {
		c, returned, group := run(t, short, nil)
		if err := syscall.Kill(-group, syscall.SIGTERM); err != nil {
			t.Fatalf("stop the group: %v", err)
		}
		if seen := c.After(t, returned, until); !seen.AfterRun || seen.At < returned || seen.None || seen.Unjudged != "" {
			t.Fatalf("After = %+v, want a stop after the run, its trap run at or past the run's return %s", seen, returned)
		}
	})
	t.Run("a SIGTERM while Wait reads the pipe", func(t *testing.T) {
		c, returned, _ := run(t, time.Minute, func(group int, c *Child) {
			if !testwait.Holds(time.Until(until), testwait.Poll, func() bool {
				_, err := os.Stat(c.path("ready"))
				return err == nil
			}) {
				t.Fatalf("the child set no traps by the end of the row's bound")
			}
			if err := syscall.Kill(-group, syscall.SIGTERM); err != nil {
				t.Fatalf("stop the group: %v", err)
			}
		})
		if seen := c.After(t, returned, until); seen != (Stop{Unjudged: stoppedInWait}) {
			t.Fatalf("After = %+v, want the stop inside Wait: %s", seen, stoppedInWait)
		}
	})
	t.Run("a SIGKILL after the run", func(t *testing.T) {
		c, returned, _ := run(t, short, nil)
		pid := pidIn(c.path("pid"))
		if pid <= 0 {
			t.Fatalf("the child wrote no pid")
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("kill the child: %v", err)
		}
		if seen := c.After(t, returned, until); seen != (Stop{Unjudged: killedFirst}) {
			t.Fatalf("After = %+v, want the child ended with no record: %s", seen, killedFirst)
		}
	})
	t.Run("no child", func(t *testing.T) {
		if seen := NewChild(t).After(t, testexec.SinceBoot(t), until); seen != (Stop{Unjudged: stoppedBeforeReady}) {
			t.Fatalf("After = %+v, want no ready mark: %s", seen, stoppedBeforeReady)
		}
	})
	t.Run("no beat past the return by the bound", func(t *testing.T) {
		c, returned, _ := run(t, short, nil)
		if seen := c.After(t, returned+time.Hour, time.Now()); seen != (Stop{Unjudged: notSeen}) {
			t.Fatalf("After = %+v, want a run it cannot tell: %s", seen, notSeen)
		}
	})
}
