package shim_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// commandBudget is the budget the requests below carry: long enough that a command still
	// running when the shim is stopped can only be one the stop did not reach, short enough that
	// such a command is gone within the minute rather than outliving the test binary.
	commandBudget = 20 * time.Second
	// stopLimit is how long the shim may take to return once its context ends. The whole stop is
	// the command's SIGTERM to its process group, the moment the group needs to go, the agent's
	// own SIGTERM and its exit: tens of milliseconds, with the rest for a loaded machine.
	stopLimit = 5 * time.Second
)

// A shim command ends with the shim itself. Under the tmux runtime a `kill-pane` gives every
// process of the pane SIGHUP (internal/runtime/tmux's stop), and the jj of a save or an adoption
// used to get it because it shared the pane's process group. Each command has a group of its own
// now, so that signal reaches the shim alone (internal/procgroup), and the shim is what has to
// pass the stop on: every command site builds its context from the one Run was given, so the stop
// ends the command at once instead of leaving it to run against the clone every role of the issue
// shares until its own budget ends it — up to five minutes for a save.
//
// What each case reads is the command's own end: its whole process group — the leader and the
// child it had started — was signalled, and the shim returned without waiting for the command's
// budget. The first is the half that fails without the change: every command is answered in a
// goroutine of its own (fromDaemon), so the shim returns on its stop either way, and a command
// left on context.Background() is simply outlived by the shim that started it, reading and
// writing that shared clone for as much as its whole budget with nobody left to stop it.
func TestTheShimsCommandEndsWithTheShimsStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace func(t *testing.T) string
		request   shimwire.Frame
	}{
		{
			name:      "the working-copy adoption",
			workspace: func(t *testing.T) string { return t.TempDir() },
			request: shimwire.AdoptWorkingCopy{
				ID: "adopt-1", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test",
				TimeoutMs: int(commandBudget.Milliseconds()),
			},
		},
		{
			name:      "the release's save",
			workspace: bundleSkeleton,
			request: shimwire.BundleWorkspace{
				ID: "bundle-1", Issue: "INFRA-3098", MaxBytes: 40 << 20,
				TimeoutMs: int(commandBudget.Milliseconds()),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := hangingJJ(t)
			workspace := tc.workspace(t)
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace, "HANGING_JJ_RECORD="+record)
			r := run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)

			p.send(t, tc.request)
			// The stop is sent once the whole group is up: a jj signalled before it had started
			// its own child would say nothing about the group.
			testwait.Eventually(t, "the jj of the request and its child to be running", func() bool {
				return strings.Count(recorded(t, record), "running\n") == 2
			})

			started := time.Now()
			r.cancel()
			select {
			case <-r.done:
			case <-time.After(stopLimit):
				t.Fatalf("the shim was still running %v after its stop: its jj outlives the stop and ends only at the request's own %v budget; log:\n%s",
					stopLimit, commandBudget, r.log)
			}
			t.Logf("the shim returned %v after its stop; the request's own budget was %v", time.Since(started), commandBudget)
			for _, want := range []string{"the jj caught SIGTERM\n", "its child caught SIGTERM\n"} {
				if !testwait.Holds(stopLimit, testwait.Poll, func() bool { return strings.Contains(recorded(t, record), want) }) {
					t.Errorf("the request's jj recorded\n%swant a line %q: the stop signals the command's whole process group", recorded(t, record), want)
				}
			}
		})
	}
}

// hangingJJ writes a `jj` that outlives any budget a test here gives it, in a directory of its
// own put first on PATH: the shim runs PATH's jj, the one the pane's own shell runs. It starts a
// child of its own, which a command's SIGTERM reaches only because that signal goes to the whole
// process group (internal/procgroup), and both it and the child say in the record when they were
// started and what they caught. Each waits in ticks rather than one long sleep, because a shell
// runs no trap while a foreground command of its own is still going, and each gives up after a
// minute, so a run whose stop never comes leaves nothing behind for long.
func hangingJJ(t *testing.T) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "jj-record")
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--the-jjs-own-child" ]; then
  trap 'printf "its child caught SIGTERM\n" >>"$HANGING_JJ_RECORD"; exit 0' TERM
  printf 'the child is running\n' >>"$HANGING_JJ_RECORD"
else
  trap 'printf "the jj caught SIGTERM\n" >>"$HANGING_JJ_RECORD"; exit 0' TERM
  "$0" --the-jjs-own-child &
  printf 'the jj is running\n' >>"$HANGING_JJ_RECORD"
fi
ticks=0
while [ "$ticks" -lt 1200 ]; do
  sleep 0.05 &
  wait $!
  ticks=$((ticks+1))
done
`
	if err := os.WriteFile(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the hanging jj: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// recorded is what the hanging jj and its child have written so far, which is nothing until the
// first of them runs.
func recorded(t *testing.T, record string) string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the jj's record: %v", err)
	}
	return string(body)
}

// bundleSkeleton is the two files of a jj workspace the save reads before it starts a command
// (workspace.GitDir): the repository directory and the git directory it names. Nothing here runs
// a real jj or git — the save's first command is the hanging jj — so the git directory needs to
// exist and hold nothing.
func bundleSkeleton(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	store := filepath.Join(workspace, ".jj", "repo", "store")
	git := filepath.Join(workspace, ".git")
	for _, dir := range []string{store, git} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store, "git_target"), []byte(git+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}
