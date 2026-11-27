package workspace

// CONTROL ONLY (tmp/load-tests-3-wws1-unjudged): the restored Run row with one change, a stand-in
// git that holds 1.2 s before its exit, so its 900 ms Timeout always passes while the git still
// runs and the stop always reaches the child in its group with the pipe still read. It stands for
// the loaded machine the row's not-judged branch exists for. The row must log each such run as not
// judged and, once its minute is over, fail saying no run was judged — never pass. A pass here
// would mean the row cannot tell a run it never judged from one it judged. Never merged.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestControlTheRestoredRunRowNeverPassesARunItCannotJudge(t *testing.T) {
	const timeout = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		fakeGit, mark, exited := filepath.Join(dir, "git"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		start, child := startGroupChild(t)
		script := "#!/bin/sh\n" + testexec.MarkStart(shellprefix.Literal(mark)) + "\necho 'fatal: unable to access' >&2\n" + start + "\nsleep 1.2\n" + testexec.MarkNow(shellprefix.Literal(exited)) + "\nexit 128\n"
		if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		run := NewRunner(timeout, map[string]string{"git": fakeGit})

		called := testexec.SinceBoot(t)
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		returned := testexec.SinceBoot(t)

		seen := child.stopBy(t, returned, called+timeout-2*testexec.ClockTick, until)
		child.end(t)
		answer := fmt.Sprintf("Run = exit %d, TimedOut %t, error %v", result.ExitCode, result.TimedOut, err)
		if !judgeable(t, runs, seen, called, returned, timeout, within, until, "Run", answer) {
			continue
		}
		t.Fatalf("run %d: the row judged a run whose git was still running when its %s Timeout passed, which no fact of the run can order (%s)", runs, timeout, answer)
	}
}
