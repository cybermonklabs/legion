package workspace

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// git and jj, which run in a managed repository's checkout, start without either raw NATS seed the
// daemon holds. The git's budget is the rest of the test binary's deadline: its environment is the
// row's subject, and a budget a loaded machine could reach before the git printed it would fail
// the row with nothing wrong.
//
// Main's form gave the runner 10 s from before the git started, and that bound stays (runBound),
// on the runner's own time and no stand-in's. The fake git records when the kernel made its process
// (testexec.MarkStart) and the moment just before it exits (testexec.MarkNow), and the span between
// the two is the git's own run. What is left of the call is the runner's: from the call to that
// start, and from that exit to the return.
func TestWorkspaceCommandsGetNoNATSSeed(t *testing.T) {
	const runBound = 10 * time.Second
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	dir := t.TempDir()
	git, mark, exited := filepath.Join(dir, "git"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
	script := "#!/bin/sh\n" + testexec.MarkStart(shellprefix.Literal(mark)) + "\n" + ctlStandInSlow + "env\n" + testexec.MarkNow(shellprefix.Literal(exited)) + "\n"
	ctlLogStandInSlow(t)
	if err := testexec.WriteFile(git, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	timeout := testwait.Remaining(t)
	called := testexec.SinceBoot(t)
	result, err := NewRunner(timeout, map[string]string{"git": git}).Run(context.Background(), Command{Argv: []string{"git", "status"}, Dir: t.TempDir(), Timeout: timeout})
	returned := testexec.SinceBoot(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	vars := envlisting.Vars(result.Stdout)
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("git's environment carries a NATS seed in %s:\n%s", name, result.Stdout)
	}
	if vars["PATH"] == "" {
		t.Errorf("git's environment is not the daemon's (no PATH):\n%s", result.Stdout)
	}
	began, after := testexec.StartedAfter(t, mark, called), returned-exitMark(t, exited)
	if own := began + after; own > runBound {
		t.Errorf("the runner spent %s of its own on the git, past main's %s: %s from the call to the git's start, and %s from the git's exit to the return", own, runBound, began, after)
	}
}
