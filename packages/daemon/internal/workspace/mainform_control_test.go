package workspace

// CONTROL ONLY (tmp/load-tests-3-wws1-mainform): main 9abe4e530ad6's two W-ws1 rows, verbatim but
// for their names, so a load run can show whether main's wall-clock form flakes. Never merged.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

func TestMainFormRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: unable to access' >&2\nsleep 30 & echo $! >" + pidFile + "\nexit 128\n"
	if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	timeout := procgroup.WaitDelay - 100*time.Millisecond
	run := NewRunner(timeout, map[string]string{"git": fakeGit})

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run = %v, want the command's own exit", err)
	}
	if elapsed < timeout {
		t.Fatalf("Run returned after %s, inside its %s Timeout: nothing held the pipe past the deadline, so this run proves nothing", elapsed, timeout)
	}
	if result.TimedOut || result.ExitCode != 128 {
		t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own inside its %s Timeout", result.ExitCode, result.TimedOut, elapsed, timeout)
	}
}

func TestMainFormRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		budget  time.Duration
		wantErr string
	}{
		"an exit 3, the pipes held past the budget": {3, procgroup.WaitDelay - 100*time.Millisecond, "Command failed (exit 3)"},
		"an exit 0 inside the budget":               {0, time.Minute, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "lingering.pid")
			t.Cleanup(func() { killPIDFile(t, pidFile) })
			ctx, cancel := context.WithTimeout(context.Background(), testCase.budget)
			defer cancel()
			script := `echo answered; echo refused >&2; sleep 30 & echo $! >"$1"; exit ` + strconv.Itoa(testCase.exit)

			started := time.Now()
			out, err := RunCommand(ctx, procgroup.NoReaper, testCase.budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", pidFile})
			elapsed := time.Since(started)

			if elapsed < procgroup.WaitDelay {
				t.Fatalf("the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if testCase.wantErr == "" {
				if err != nil || out != "answered" {
					t.Errorf("RunCommand = %q, %v after %s; want \"answered\" and no error: the command exited 0 on its own", out, err, elapsed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) || strings.Contains(err.Error(), "timed out") {
				t.Errorf("RunCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own inside its %s budget",
					out, err, elapsed, testCase.wantErr, testCase.budget)
			}
		})
	}
}
