package heldprocess

import (
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestTheReadsOfAProcessSeeWhatItRunsAndIgnores: a real `sleep 283` started under a shell that
// ignores SIGHUP, which sleep inherits, and one started with nothing ignored, the control, are read
// as /proc holds them: the command line, the stand-in check and the ignore of SIGHUP alone. Once the
// process is gone, /proc names it no more and every read says so.
func TestTheReadsOfAProcessSeeWhatItRunsAndIgnores(t *testing.T) {
	for _, row := range []struct {
		name, ignore string
		hup          bool
	}{
		{"started ignoring SIGHUP", "trap '' HUP; ", true},
		{"started ignoring nothing", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			sleep := exec.Command("/bin/sh", "-c", row.ignore+"exec sleep 283")
			if err := sleep.Start(); err != nil {
				t.Fatal(err)
			}
			pid := sleep.Process.Pid
			t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
			// The shell execs sleep in the process it started, so its command line becomes sleep's
			// a moment after the start.
			deadline := time.Now().Add(10 * time.Second)
			for !Running(pid, "283") {
				if time.Now().After(deadline) {
					t.Fatalf("pid %d never ran `sleep 283`: its command line is %q", pid, CommandLine(pid))
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := CommandLine(pid); !slices.Equal(got, []string{"sleep", "283"}) {
				t.Errorf("CommandLine(%d) = %q; want [sleep 283]", pid, got)
			}
			if Running(pid, "284") {
				t.Errorf("Running(%d, 284) = true for `sleep 283`", pid)
			}
			if got := Ignores(pid, syscall.SIGHUP); got != row.hup {
				t.Errorf("Ignores(%d, SIGHUP) = %v; want %v", pid, got, row.hup)
			}
			if Ignores(pid, syscall.SIGTERM) {
				t.Errorf("Ignores(%d, SIGTERM) = true; nothing ignored it", pid)
			}
			_ = sleep.Process.Kill()
			_ = sleep.Wait()
			if got := CommandLine(pid); got != nil || Running(pid, "283") || Ignores(pid, syscall.SIGHUP) {
				t.Errorf("after its end, pid %d reads as running %q, ignoring SIGHUP %v", pid, got, Ignores(pid, syscall.SIGHUP))
			}
		})
	}
}
