package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A help read that outlives its timeout stops everything gh started, not gh alone. The gh here
// starts a helper of its own, detached from gh's output so nothing holds the read's pipes, and
// then never answers. Once readGhHelp has given up, the helper is gone too. A timeout that killed
// gh alone, as exec.CommandContext's own does, left that helper running, reparented and waited
// for by nobody; under a gh that led back to `legion gh`, every level of the chain was such a
// helper.
func TestAHelpReadThatTimesOutStopsWhatGhStarted(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether gh's helper is still running", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper")
	const helperSeconds = "299"
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nsleep " + helperSeconds + " </dev/null >/dev/null 2>&1 &\necho $! >'" + pidFile + "'\nexec sleep 300\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	timeout := ghHelpTimeout
	ghHelpTimeout = time.Second
	t.Cleanup(func() { ghHelpTimeout = timeout })

	_, err := readGhHelp(context.Background(), gh, []string{"pr"})
	if err == nil || !strings.Contains(err.Error(), "did not answer its help within") {
		t.Fatalf("readGhHelp of a gh that never answers = %v; want it to give up naming the timeout", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("gh recorded no helper: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("gh recorded helper pid %q: %v", raw, err)
	}
	// A helper still running when the row ends is killed, so a failure here leaves nothing behind.
	t.Cleanup(func() {
		if helperRunning(pid, helperSeconds) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	// The group's stop is SIGTERM, then SIGKILL after procgroup.StopGrace; the helper, a sleep,
	// ends on the SIGTERM, and the bound leaves room for a loaded machine to reap it.
	for deadline := time.Now().Add(procgroup.StopGrace + 5*time.Second); helperRunning(pid, helperSeconds); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("gh's helper (pid %d) is still running after the help read gave up; want the read's timeout to stop gh's whole process group", pid)
		}
	}
}

// helperRunning says whether pid is still this row's helper: a process whose command line is
// `sleep <seconds>`, read from /proc so a pid the kernel has since given to another process does
// not count.
func helperRunning(pid int, seconds string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return strings.Join(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), " ") == "sleep "+seconds
}

// A signal the terminal sends `legion gh`'s process group stops everything `legion gh` started
// before gh too, though each of those runs in a process group of its own, which no such signal
// reaches: the resolver's help read, the shared check the leak guard runs, and the guard's read of
// the destination's visibility, which goes back through `legion gh` to the real gh. `legion gh pr
// create -R acme/widgets` is started here as the leader of a group of its own, the way a shell's
// job control starts a command, and in each row one stand-in records its pid and holds that work,
// answering nothing within the row's bounds.
//
// A SIGINT to the group, a Ctrl-C, cancels the command's context (main's signal.NotifyContext),
// which stops the held work's group, and `legion gh` ends with 130 within a few seconds. Before,
// the help read ran under a context of its own, which `legion gh` waited out to ghHelpTimeout, and
// a guard the signal stopped was answered with its setup failure, exit 2. A SIGHUP to the group,
// a pane that closes, is caught until gh would start (hangupCancels) and stops the held work's
// group the same way, and `legion gh` ends with 129. Before, the SIGHUP killed `legion gh` where
// it stood and left that work running, reparented and waited for by nobody.
func TestASignalToLegionGhsGroupStopsWhatItStartedBeforeGh(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether the stand-in is still running", err)
	}
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// holds is the end of a stand-in that holds its work: it records its pid, written whole by a
	// rename, and becomes `sleep <seconds>` under that pid, held far past this row's bounds, with
	// a command line helperRunning tells from a pid the kernel has reused.
	holds := func(pidFile, seconds string) string {
		return "echo $$ >'" + pidFile + ".new'\nmv '" + pidFile + ".new' '" + pidFile + "'\nexec sleep " + seconds + "\n"
	}
	passes := func(string, string) string { return leakguardtest.Passes }
	for _, held := range []struct {
		name string
		// seconds is the holding stand-in's sleep, one per row.
		seconds string
		// gh and check are the stand-in gh and the stand-in shared check, given the file the
		// holding one records its pid in and its sleep.
		gh, check func(pidFile, seconds string) string
	}{
		{
			name: "the help read", seconds: "297", check: passes,
			gh: func(pidFile, seconds string) string {
				return "#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = --help ]; then\n" + holds(pidFile, seconds) + "fi\nexit 0\n"
			},
		},
		{
			name: "the shared check", seconds: "296", check: holds,
			gh: func(string, string) string { return ghStandInPrefix(help) + "exit 0\n" },
		},
		{
			// The stand-in gh answers the resolver's help reads, of the outer `legion gh` and of
			// the one the guard's read goes back through, and holds the read itself.
			name: "the guard's visibility read", seconds: "295", check: passes,
			gh: func(pidFile, seconds string) string {
				return ghStandInHelp(help) +
					"if [ -n \"$" + leakguard.VisibilityReadVar + "\" ]; then\n" + holds(pidFile, seconds) + "fi\nexit 0\n"
			},
		},
	} {
		for _, tc := range []struct {
			name   string
			signal syscall.Signal
		}{
			{"SIGINT", syscall.SIGINT},
			{"SIGHUP", syscall.SIGHUP},
		} {
			t.Run(held.name+" "+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				pidFile := filepath.Join(dir, "stand-in")
				stub := filepath.Join(dir, "gh")
				if err := os.WriteFile(stub, []byte(held.gh(pidFile, held.seconds)), 0o700); err != nil {
					t.Fatal(err)
				}
				check := leakguardtest.Write(t, t.TempDir(), held.check(pidFile, held.seconds))
				standIn := func() (pid int, running bool) {
					raw, err := os.ReadFile(pidFile)
					if err != nil {
						return 0, false
					}
					pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
					return pid, err == nil && helperRunning(pid, held.seconds)
				}
				// A stand-in still running when the row ends is killed, so a failure here leaves
				// nothing behind.
				t.Cleanup(func() {
					if pid, running := standIn(); running {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				})
				command := legionGhCommand(t, t.TempDir(), map[string]string{
					"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
					"PUBLIC_LEAK_CHECK": check,
				}, "pr", "create", "-R", "acme/widgets", "--title", "a title", "--body", "a body")
				// `legion gh` leads a group of its own, as a job a shell starts does, so the
				// signal below goes to that group and never to this test binary's own.
				command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				// Standard output and standard error are a file rather than a pipe, so nothing of
				// the stand-in's can hold the wait open.
				printed, err := os.Create(filepath.Join(dir, "printed"))
				if err != nil {
					t.Fatal(err)
				}
				defer printed.Close()
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
				// A `legion gh` still running when the row ends is killed by its handle, which
				// never signals a pid the kernel has given to another process since.
				t.Cleanup(func() {
					select {
					case <-exited:
					default:
						_ = command.Process.Kill()
						<-exited
					}
				})
				var pid int
				testwait.Eventually(t, "the stand-in to hold "+held.name, func() bool {
					var running bool
					pid, running = standIn()
					return running
				})
				if err := syscall.Kill(-command.Process.Pid, tc.signal); err != nil {
					t.Fatal(err)
				}
				// The held work's group is stopped with a SIGTERM, then a SIGKILL after
				// procgroup.StopGrace. The stand-in, a sleep, ends on the SIGTERM, and the bound
				// leaves room for a loaded machine while staying far short of ghHelpTimeout, which
				// a help read nothing stops runs to.
				bound := procgroup.StopGrace + 5*time.Second
				want := 128 + int(tc.signal)
				select {
				case <-exited:
				case <-time.After(bound):
					t.Fatalf("legion gh is still running %s after a %s to its group while %s was held; want it to stop that work and end with %d: %s", bound, tc.name, held.name, want, readRecord(t, printed.Name()))
				}
				if exit, ok := waited.(*exec.ExitError); !ok || exit.ExitCode() != want {
					t.Errorf("legion gh after a %s to its group while %s was held = %v; want exit %d: %s", tc.name, held.name, waited, want, readRecord(t, printed.Name()))
				}
				for deadline := time.Now().Add(bound); helperRunning(pid, held.seconds); time.Sleep(50 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("the stand-in (pid %d) holding %s is still running %s after a %s to legion gh's group; want its process group stopped", pid, held.name, bound, tc.name)
					}
				}
			})
		}
	}
}
