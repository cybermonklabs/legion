package workspace

//ci:reads packages/daemon/internal/prompts/roles/**/*.md TestTheRolePromptsCheckTheLeaseTheWarmUpHolds reads the four role parts that tell a role to check the warm-up's lease

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stubCodegraph puts a fake `codegraph` executable first on PATH. Its `status --json` reports
// uninitialized until `init` runs, which creates a `.codegraph/` directory in the directory it is
// run from — real codegraph's own side effect, which nextCodegraphStep's dirExists check reads —
// and reports initialized with a complete index from then on. It records every invocation's argv
// to callLog, one call per line, and each invocation's environment, as env prints it, to a file
// of its own named callLog.env.<random> (codegraphEnvironments reads them), so the test can see
// exactly what warmCodegraphIndex ran and with what environment.
func stubCodegraph(t *testing.T, callLog string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + callLog + `'
env >"$(mktemp '` + callLog + `.env.XXXXXX')"
marker=".codegraph/.codegraph-initialized"
case "$1" in
status)
  if [ -f "$marker" ]; then
    echo '{"initialized":true,"index":{"state":"complete"}}'
  else
    echo '{"initialized":false}'
  fi
  ;;
init)
  mkdir -p .codegraph
  touch "$marker"
  ;;
esac
`
	path := filepath.Join(dir, "codegraph")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// codegraphEnvironments returns the variables of each environment stubCodegraph recorded beside
// callLog, one map per invocation, read whole by name (envlisting). It lists callLog's directory
// and keeps the names that start with callLog's own, rather than matching a glob: a temporary
// directory can hold a glob character, and an unclosed [ stops the pattern from compiling, while
// a * or ? would make it match other paths too.
func codegraphEnvironments(t *testing.T, callLog string) []map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(callLog))
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Base(callLog) + ".env."
	var envs []map[string]string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		listing, err := os.ReadFile(filepath.Join(filepath.Dir(callLog), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		envs = append(envs, envlisting.Vars(string(listing)))
	}
	return envs
}

func TestWarmCodegraphIndexBuildsAnIndexWhenTheCliIsOnPath(t *testing.T) {
	// Not parallel: stubCodegraph changes the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	stubCodegraph(t, callLog)
	dir := t.TempDir()

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", ".codegraph-initialized")); err != nil {
		t.Fatalf("workspace left uninitialized after warming: %v", err)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if !strings.Contains(string(calls), "status --json") || !strings.Contains(string(calls), "init") {
		t.Fatalf("codegraph calls = %q, want both status --json and init", calls)
	}

	// Warming the same directory again is a no-op re-index: codegraph runs status again, but
	// never a second init, since the marker already reports initialized.
	before := len(strings.Split(strings.TrimSpace(string(calls)), "\n"))
	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err = os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log after the second warm-up: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != before+1 || lines[len(lines)-1] != "status --json" {
		t.Fatalf("codegraph calls after a second warm-up = %v, want exactly one more status --json call", lines)
	}
}

// TestWarmCodegraphIndexInBackgroundNeverWaitsAndBuildsOncePerWorkspace pins what a launch needs:
// the call returns while `codegraph init` is still running, and a second call for the same
// workspace during that build starts no second one.
func TestWarmCodegraphIndexInBackgroundNeverWaitsAndBuildsOncePerWorkspace(t *testing.T) {
	// Not parallel: gives the process a held codegraph stub through PATH with t.Setenv.
	scratch := t.TempDir()
	callLog := filepath.Join(scratch, "calls.log")
	release := filepath.Join(scratch, "release")
	bin := filepath.Join(scratch, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":false}' ;;
init) while [ ! -f '` + release + `' ] && [ -d '` + scratch + `' ]; do sleep 0.05; done ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub's held `codegraph init` exits once its scratch directory is removed, so no stub
	// loop is left running past this test regardless of how it ends.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()

	// The stub holds its init until the release below, so a call that waited on the build never
	// returns and this wait fails.
	testwait.Receive(t, "WarmCodegraphIndexInBackground to return while codegraph init still runs", inFlight(func() struct{} {
		WarmCodegraphIndexInBackground(procgroup.NoReaper, dir)
		return struct{}{}
	}))
	waitFor(t, func() bool {
		calls, _ := os.ReadFile(callLog)
		return strings.Contains(string(calls), "init")
	}, "codegraph init to start")
	WarmCodegraphIndexInBackground(procgroup.NoReaper, dir)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, busy := warming.Load(dir)
		return !busy
	}, "the background warm-up to finish")
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "init"); got != 1 {
		t.Fatalf("codegraph init ran %d times for one workspace, want 1; calls: %q", got, calls)
	}
}

func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	testwait.Eventually(t, what, done)
}

// TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing proves the acceptance criterion directly:
// warming logs its skip to stderr, so this only has to show nothing panics or leaves an index
// when codegraph is absent from PATH entirely — including on this devbox, which has a real one.
func TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing(t *testing.T) {
	// Not parallel: removes codegraph from the process PATH with t.Setenv.
	dir := t.TempDir()
	// Narrowed to a fresh, empty directory so this test proves the missing-CLI path even on a
	// machine (like this one) that has a real `codegraph` on its ordinary PATH.
	t.Setenv("PATH", t.TempDir())

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", ".codegraph-initialized")); !os.IsNotExist(err) {
		t.Fatalf("stat .codegraph/.codegraph-initialized = %v, want it absent with no codegraph on PATH", err)
	}
}

// TestWarmCodegraphIndexGivesCodegraphNoSecretLikeVariable proves the correctness/security finding
// directly: a provisioning caller's own process environment may hold a one-shot GitHub token or
// other secret (GH_TOKEN here, standing in for any of them), and codegraph must never see it —
// only what codegraphEnvironment passes (DO_NOT_TRACK=1, and PATH, HOME and TMPDIR when the caller
// sets them), and on its command line only its subcommand's arguments: any local user can read a
// process's argv in /proc/<pid>/cmdline.
func TestWarmCodegraphIndexGivesCodegraphNoSecretLikeVariable(t *testing.T) {
	// Not parallel: changes the process PATH and plants secret environment variables.
	// The call log sits under a directory whose name holds a glob character, as a machine's
	// temporary directory can, so the recorded environments must be read without a pattern.
	logDir := filepath.Join(t.TempDir(), "tmp-glob[x")
	if err := os.Mkdir(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(logDir, "calls.log")
	stubCodegraph(t, callLog)
	// Unique values, so that only a variable they reached can hold one (envlisting).
	token, grant := envlisting.Unique("ghs_"), envlisting.Unique("grant-")
	t.Setenv("GH_TOKEN", token)
	t.Setenv("LEGION_GRANT", grant)
	dir := t.TempDir()

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("codegraph was never invoked")
	}
	// The call log holds each call's arguments alone ("$*"), no path of the machine's, and the
	// planted values are Unique, so a plain search of it finds only a value that reached argv.
	for _, planted := range []string{token, grant} {
		if strings.Contains(string(calls), planted) {
			t.Fatalf("codegraph's argv carried a secret: %q", calls)
		}
	}
	envs := codegraphEnvironments(t, callLog)
	if len(envs) != len(strings.Split(strings.TrimSpace(string(calls)), "\n")) {
		t.Fatalf("%d environments recorded for the calls %q", len(envs), calls)
	}
	for _, vars := range envs {
		for _, name := range []string{"GH_TOKEN", "LEGION_GRANT"} {
			if _, set := vars[name]; set {
				t.Fatalf("codegraph's environment carried %s: %v", name, vars)
			}
		}
		if name, leaked := envlisting.Carrying(vars, token, grant); leaked {
			t.Fatalf("codegraph's environment carried a secret in %s: %v", name, vars)
		}
		if vars["DO_NOT_TRACK"] != "1" {
			t.Fatalf("codegraph's environment did not carry DO_NOT_TRACK=1: %v", vars)
		}
	}
}

// TestWarmCodegraphIndexRepairsAPartialIndex: an index an earlier warm-up left partial —
// `initialized:true` but `index.state` not `"complete"`, as a daemon exit or the 30-minute kill
// leaves it — is repaired with `codegraph index`, never accepted as built and never re-run
// through `codegraph init` (which would exit 0 printing "Already initialized" without touching
// the partial state).
func TestWarmCodegraphIndexRepairsAPartialIndex(t *testing.T) {
	// Not parallel: puts a codegraph stub on the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status", "index"}) {
		t.Fatalf("codegraph calls = %v, want exactly status then index (never init)", lines)
	}
}

// TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectory: `.codegraph/` can exist
// without a database — CodeGraph's own generated `.codegraph/.gitignore` is `*` then
// `!.gitignore`, so that one file stays tracked, and a fresh workspace of a repository that
// committed it starts with the directory present and `status` reporting `initialized:false`.
// That must still run `init`, not `index` (which refuses: "CodeGraph not initialized"), and must
// not consume the once-per-workspace repair budget.
func TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectory(t *testing.T) {
	// Not parallel: puts a codegraph stub on the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codegraph", ".gitignore"), []byte("*\n!.gitignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":false}' ;;
index) echo 'CodeGraph not initialized' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status", "init"}) {
		t.Fatalf("codegraph calls = %v, want exactly status then init (never index, which refuses on an untracked directory)", lines)
	}
}

// TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectoryAgainstTheRealCli proves the
// same case against the real codegraph CLI (not a stub): `.codegraph/` holding only its own
// generated `.gitignore`, with no database.
func TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectoryAgainstTheRealCli(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("codegraph"); err != nil {
		t.Skip("no codegraph CLI on PATH")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codegraph", ".gitignore"), []byte("*\n!.gitignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("console.log(1);\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())

	codegraphPath, err := exec.LookPath("codegraph")
	if err != nil {
		t.Fatal(err)
	}
	status, err := runCodegraph(context.Background(), clock.Real(), procgroup.NoReaper, codegraphPath, dir, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	initialized, current := codegraphStatus(status.stdout)
	if !initialized || !current {
		t.Fatalf("status after warming = %q, want initialized and current", status.stdout)
	}
}

// TestWarmCodegraphIndexSkipsRepairWhileABuildIsLive: a status of `"indexing"` is also what a
// build still in progress reports, and `.codegraph/codegraph.lock` naming a live process whose
// `/proc/<pid>/cmdline` names codegraph must skip the repair entirely rather than run `codegraph
// index` alongside it.
func TestWarmCodegraphIndexSkipsRepairWhileABuildIsLive(t *testing.T) {
	// Not parallel: puts a codegraph stub on the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A process actually named "codegraph" in its own argv, so codegraphLockHeldByLiveProcess's
	// /proc/<pid>/cmdline check — the PID-reuse hardening — sees what a real builder's PID would.
	builderScript := filepath.Join(binDir, "codegraph-builder")
	if err := os.WriteFile(builderScript, []byte("#!/bin/bash\nexec -a codegraph sleep 86400\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	builder := exec.Command(builderScript)
	if err := builder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Process.Kill() })
	// Start returns once execve has closed the child's close-on-exec pipe, before the kernel has
	// laid out the new argv: /proc/<pid>/cmdline reads empty until it has, and again across the
	// script's own `exec -a`, and an empty cmdline reads as not codegraph. A real builder writes its
	// PID to the lock from its own running code, after its exec has finished, so the lock is written
	// only once the stand-in is there too: argv[0] is codegraph.
	waitFor(t, func() bool {
		cmdline, _ := os.ReadFile("/proc/" + strconv.Itoa(builder.Process.Pid) + "/cmdline")
		argv0, _, _ := strings.Cut(string(cmdline), "\x00")
		return argv0 == "codegraph"
	}, "the stand-in builder to exec as codegraph")
	lock := filepath.Join(dir, ".codegraph", "codegraph.lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(builder.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status"}) {
		t.Fatalf("codegraph calls = %v, want exactly status alone (no index or init while the lock names a live process)", lines)
	}
}

// TestWarmCodegraphIndexJudgesAnEmptyLockByItsAge: CodeGraph 1.6.4's FileLock hard-links into
// place a lock that already holds the builder's PID, but where that link fails it falls back to
// creating `.codegraph/codegraph.lock` before it writes the PID into it, so a build that has only
// just started on that path shows an empty lock. A fresh empty lock is held, and warming skips the
// repair rather than start a second writer beside that build. One older than
// codegraphEmptyLockGrace is what a builder that died between the create and the write leaves
// behind: stale, so the repair runs. So is one dated further than the grace into the future,
// which no starting build wrote.
func TestWarmCodegraphIndexJudgesAnEmptyLockByItsAge(t *testing.T) {
	// Not parallel: each row puts a codegraph stub on the process PATH with t.Setenv.
	for _, tc := range []struct {
		name string
		// age is how old the lock's mtime is when warming reads it; negative is in the future.
		// Zero means the stub's own `status` creates the lock, as a build taking it while warming
		// runs would, so the lock is as young as it can be when warming reads it.
		age  time.Duration
		want []string
	}{
		{"fresh empty lock: a build about to write its PID holds it, no repair", 0, []string{"status"}},
		{"empty lock older than the grace: a dead builder's, repaired", codegraphEmptyLockGrace + time.Minute, []string{"status", "index"}},
		{"empty lock dated past the grace in the future: stale, repaired", -codegraphEmptyLockGrace - time.Minute, []string{"status", "index"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callLog := filepath.Join(t.TempDir(), "calls.log")
			dir := t.TempDir()
			lock := filepath.Join(dir, ".codegraph", "codegraph.lock")
			if err := os.Mkdir(filepath.Dir(lock), 0o700); err != nil {
				t.Fatal(err)
			}
			takeLock := ""
			if tc.age == 0 {
				takeLock = " : > .codegraph/codegraph.lock;"
			} else {
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := time.Now().Add(-tc.age)
				if err := os.Chtimes(lock, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			binDir := t.TempDir()
			script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status)` + takeLock + ` echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
			if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
			calls, err := os.ReadFile(callLog)
			if err != nil {
				t.Fatalf("read codegraph call log: %v", err)
			}
			if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); !slices.Equal(lines, tc.want) {
				t.Fatalf("codegraph calls = %v, want %v", lines, tc.want)
			}
		})
	}
}

// TestWarmCodegraphIndexRepairsOncePerWorkspace: a workspace whose index never reaches
// `"complete"` gets re-indexed at most once per process, not on every call.
func TestWarmCodegraphIndexRepairsOncePerWorkspace(t *testing.T) {
	// Not parallel: puts a codegraph stub on the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if got := strings.Count(string(calls), "index"); got != 1 {
		t.Fatalf("codegraph index ran %d times across two warm-ups of the same workspace, want 1; calls: %q", got, calls)
	}
}

// stubCalls is the argv of every invocation stubCodegraph recorded, in order; nil when it was
// never run.
func stubCalls(t *testing.T, callLog string) []string {
	t.Helper()
	calls, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	var argv []string
	for line := range strings.Lines(strings.TrimSpace(string(calls))) {
		argv = append(argv, strings.TrimSuffix(line, "\n"))
	}
	return argv
}

// TestWarmCodegraphIndexHoldsACrossProcessLease: the six role shims of one issue pod each warm the
// one workspace they share, and only one may build. `.codegraph/legion-warm.lock` settles it across
// processes, where `warming` cannot: a
// fresh lease (its holder's heartbeat within warmLeaseStale) makes a warm-up skip without even
// running `status`, and leaves the file to its holder; one older than warmLeaseStale is a dead
// holder's, taken over and built behind, with nothing of the takeover left in the directory; and a
// warm-up that ran leaves no lease behind for the next to find.
func TestWarmCodegraphIndexHoldsACrossProcessLease(t *testing.T) {
	// Not parallel: stubCodegraph changes the process PATH with t.Setenv.
	leaseOf := func(t *testing.T, dir string, age time.Duration) string {
		t.Helper()
		lease := filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lease, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-age)
		if err := os.Chtimes(lease, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	leaseFiles := func(t *testing.T, dir string) []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, ".codegraph"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), warmLeaseName) {
				names = append(names, entry.Name())
			}
		}
		return names
	}

	t.Run("a fresh lease held by another process skips the warm-up", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		lease := leaseOf(t, dir, 0)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		if calls := stubCalls(t, callLog); calls != nil {
			t.Fatalf("codegraph calls = %v, want none while another process holds the lease", calls)
		}
		if _, err := os.Stat(lease); err != nil {
			t.Fatalf("the holder's lease: %v, want it left in place", err)
		}
	})
	t.Run("a stale lease is taken over and the build runs", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		leaseOf(t, dir, warmLeaseStale+time.Minute)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status --json", "init"}) {
			t.Fatalf("codegraph calls = %v, want status --json then init behind the taken-over lease", calls)
		}
		if left := leaseFiles(t, dir); left != nil {
			t.Fatalf(".codegraph/ holds %v after the warm-up, want neither the lease nor the takeover marker", left)
		}
	})
	t.Run("the lease is released after a run", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status --json", "init"}) {
			t.Fatalf("codegraph calls = %v, want status --json then init", calls)
		}
		if left := leaseFiles(t, dir); left != nil {
			t.Fatalf(".codegraph/ holds %v after the warm-up, want the lease released", left)
		}
	})
}

// TestTwoContendersThatJudgedOneLeaseStaleCannotBothHoldIt drives the interleavings a scheduler
// rarely produces, through the warmLeaseStep seam: two warm-ups (two roles relaunching
// into one workspace after a SIGKILLed pod) both read the same stale lease. Whichever order they
// then act in, exactly one holds the lease afterwards and the other skips, so one build runs:
// (1) B judges the lease stale, then A takes it over, holds, and starts building before B acts —
// B's takeover must not touch A's fresh lease, which a rename of "whatever is at the path" did;
// (2) B is inside its takeover, the marker held, when A judges the same lease stale — A finds the
// marker and leaves the takeover to B. A dead contender's marker, older than warmLeaseStale, is
// removed and the takeover proceeds; a lease released between the stat and the takeover gives the
// contender its retry rather than a skip.
func TestTwoContendersThatJudgedOneLeaseStaleCannotBothHoldIt(t *testing.T) {
	// Not parallel: replaces the package's warmLeaseStep hook.
	staleLease := func(t *testing.T) (dir, lease string) {
		t.Helper()
		dir = t.TempDir()
		lease = filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lease, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-warmLeaseStale - time.Minute)
		if err := os.Chtimes(lease, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return dir, lease
	}
	// at runs f once, when acquireWarmLease reaches step. The seam is cleared before f runs, so the
	// contender f itself runs — which reaches the same steps — is not caught by it again.
	at := func(t *testing.T, step string, f func()) {
		t.Helper()
		warmLeaseStep = func(s string) {
			if s == step {
				warmLeaseStep = nil
				f()
			}
		}
		t.Cleanup(func() { warmLeaseStep = nil })
	}
	holdsAlone := func(t *testing.T, lease string, releaseA, releaseB func(), heldA, heldB bool) {
		t.Helper()
		if heldA == heldB {
			t.Fatalf("A holds = %v, B holds = %v; want exactly one holder", heldA, heldB)
		}
		if _, err := os.Stat(lease); err != nil {
			t.Fatalf("the lease: %v, want the holder's lease in place", err)
		}
		if _, err := os.Stat(lease + ".takeover"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the takeover marker: %v, want it gone once the takeover ended", err)
		}
		for _, release := range []func(){releaseA, releaseB} {
			if release != nil {
				release()
			}
		}
		if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the lease after the holder's release: %v, want it gone", err)
		}
	}

	t.Run("the first to act holds; the second, judging the same stale lease, finds a fresh one", func(t *testing.T) {
		dir, lease := staleLease(t)
		var releaseA func()
		var heldA bool
		at(t, "judged stale", func() { releaseA, heldA = acquireWarmLease(dir, lease) })
		releaseB, heldB := acquireWarmLease(dir, lease)
		if !heldA || heldB {
			t.Fatalf("A holds = %v, B holds = %v; want A, which acted first, to hold and B to skip", heldA, heldB)
		}
		holdsAlone(t, lease, releaseA, releaseB, heldA, heldB)
	})
	t.Run("a contender inside its takeover holds the marker; the other leaves the takeover to it", func(t *testing.T) {
		dir, lease := staleLease(t)
		var releaseA func()
		var heldA bool
		at(t, "marker held", func() { releaseA, heldA = acquireWarmLease(dir, lease) })
		releaseB, heldB := acquireWarmLease(dir, lease)
		if heldA || !heldB {
			t.Fatalf("A holds = %v, B holds = %v; want B, whose takeover was under way, to hold and A to skip", heldA, heldB)
		}
		holdsAlone(t, lease, releaseA, releaseB, heldA, heldB)
	})
	t.Run("a dead contender's marker is removed and the takeover proceeds", func(t *testing.T) {
		dir, lease := staleLease(t)
		marker := lease + ".takeover"
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-warmLeaseStale - time.Minute)
		if err := os.Chtimes(marker, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		release, held := acquireWarmLease(dir, lease)
		if !held {
			t.Fatal("the contender skipped behind a dead contender's marker, want it to take the lease over")
		}
		holdsAlone(t, lease, release, nil, held, false)
	})
	t.Run("a live contender's marker makes the other skip", func(t *testing.T) {
		dir, lease := staleLease(t)
		if err := os.WriteFile(lease+".takeover", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if release, held := acquireWarmLease(dir, lease); held {
			release()
			t.Fatal("the contender took the lease over behind a live contender's marker")
		}
	})
	t.Run("a lease released between the stat and the takeover is retried, not skipped", func(t *testing.T) {
		dir, lease := staleLease(t)
		at(t, "judged stale", func() {
			if err := os.Remove(lease); err != nil {
				t.Fatal(err)
			}
		})
		release, held := acquireWarmLease(dir, lease)
		if !held {
			t.Fatal("the contender skipped after the holder released, want it to take the free lease")
		}
		holdsAlone(t, lease, release, nil, held, false)
	})
}

// TestATakeoverLeavesAFreshLeaseThatReusedTheStaleOnesInode: the tree volume a pod's workspace
// lives on reuses inode numbers (measured in the LEGION-629 tester pod: v9fs over the issue
// volume gives a file removed and re-created at one path the inode it had; the overlayfs under
// /tmp does not), and os.SameFile compares device and inode alone. So in the interleaving the
// first subtest above drives — B judges the lease stale, A takes it over, holds a fresh lease at
// the same path and starts building, then B acts — B's takeover, finding the path's file to be
// "the one it judged stale" by inode, removes A's live lease, creates its own, and both build: the
// SQLite corruption the lease exists to prevent, on exactly the filesystem it runs on. A fresh
// lease at the path is never the stale one, whatever its inode: the takeover leaves it and B
// skips. The inode is pinned here with a hard link, so the case runs the same on every filesystem
// rather than only where the allocator happens to hand the number back.
func TestATakeoverLeavesAFreshLeaseThatReusedTheStaleOnesInode(t *testing.T) {
	// Not parallel: replaces the package's warmLeaseStep hook.
	dir := t.TempDir()
	lease := filepath.Join(dir, ".codegraph", warmLeaseName)
	if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-warmLeaseStale - time.Minute)
	if err := os.Chtimes(lease, stale, stale); err != nil {
		t.Fatal(err)
	}
	// Once B has judged the lease stale, A's takeover runs to completion: A removes the stale
	// lease and creates its own fresh one at the path, and the filesystem hands the new file the
	// freed inode — kept alive through a hard link for the moment between A's remove and A's
	// create, which is what makes the reuse certain here.
	warmLeaseStep = func(step string) {
		if step != "judged stale" {
			return
		}
		warmLeaseStep = nil
		kept := lease + ".kept"
		for _, err := range []error{os.Link(lease, kept), os.Remove(lease), os.Link(kept, lease), os.Remove(kept)} {
			if err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now()
		if err := os.Chtimes(lease, now, now); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { warmLeaseStep = nil })

	release, held := acquireWarmLease(dir, lease)
	if held {
		release()
		t.Fatal("B took the lease over although A, acting first, holds a fresh lease at the path: two builds would run on one index")
	}
	current, err := os.Stat(lease)
	if err != nil {
		t.Fatalf("A's fresh lease after B's takeover: %v, want it still in place", err)
	}
	if current.ModTime().Before(time.Now().Add(-warmLeaseStale)) {
		t.Fatalf("the lease at the path is stale again after B's takeover (mtime %s), want A's fresh one untouched", current.ModTime())
	}
	if _, err := os.Stat(lease + ".takeover"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the takeover marker: %v, want it gone once the takeover ended", err)
	}
}

// TestAStalledHoldersReleaseLeavesItsSuccessorsLeaseAtTheSameInode: the other half of the inode
// hazard above. A holder stalled past warmLeaseStale is taken over, and its successor's fresh
// lease sits at the same path — on the tree volume (v9fs), with the inode the stalled holder's
// file had (measured in the LEGION-629 tester pod: the successor's lease read os.SameFile with the
// one it replaced, and the pre-fix release removed it from under the successor's build). A holder
// knows its lease by the token it wrote, never by the file's identity: the stalled holder's
// release leaves a lease carrying another token in place, and the successor's own release removes
// it. The successor's lease is built here from the stalled holder's by a hard link, so it has the
// same inode on every filesystem rather than only where the allocator hands the number back.
func TestAStalledHoldersReleaseLeavesItsSuccessorsLeaseAtTheSameInode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lease := filepath.Join(dir, ".codegraph", warmLeaseName)
	if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
		t.Fatal(err)
	}
	releaseA, heldA := acquireWarmLease(dir, lease)
	if !heldA {
		t.Fatal("A did not take the free lease")
	}
	// A stalls past the stale threshold and B takes over: B's fresh lease at the path, another
	// token in it, the inode A's file had.
	kept := lease + ".kept"
	for _, err := range []error{os.Link(lease, kept), os.Remove(lease), os.Link(kept, lease), os.Remove(kept)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(lease, []byte("the-successors-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	successor, err := os.Stat(lease)
	if err != nil {
		t.Fatal(err)
	}

	releaseA()
	current, err := os.Stat(lease)
	if err != nil {
		t.Fatalf("the stalled holder's release removed its successor's live lease: %v", err)
	}
	if !os.SameFile(successor, current) {
		t.Fatal("the lease at the path is not the successor's after the stalled holder's release")
	}
	if body, err := os.ReadFile(lease); err != nil || string(body) != "the-successors-token" {
		t.Fatalf("the successor's lease reads %q (%v) after the stalled holder's release, want its token untouched", body, err)
	}
}

// TestAStalledHoldersHeartbeatLeavesItsSuccessorsLeaseAtTheSameInode: the heartbeat half of the
// inode hazard the release test above covers. A holder stalled past warmLeaseStale is taken over,
// and its successor's fresh lease sits at the same path, with the inode the stalled holder's file
// had on the tree volume (v9fs). Pre-fix, the stalled holder's tick set the mtime of whatever file
// was at the path — the successor's lease — so once the successor itself died, its lease was kept
// fresh forever by a heartbeat that was not its own: a dead holder's lease nothing ever took over.
// The heartbeat knows its lease by the token it wrote, as the release does: the first tick that
// reads the successor's token ends it, and the successor's lease ages as if the stalled holder
// were gone. The control comes first: a heartbeat on its own lease does move the mtime forward.
// The row runs in a synctest bubble, whose clock the heartbeat's ticker and every mtime it writes
// read, and which does no network I/O: each sleep is a known number of ticks, and synctest.Wait
// returns once the heartbeat has handled them. The heartbeat is also released by a cleanup of the
// bubble (sync.OnceFunc, since release closes its stop), which runs inside the bubble before
// synctest.Test returns: a row that fails before its own release ends with its heartbeat stopped,
// not with the runtime's panic for a bubble whose blocked goroutines outlive its main one, which
// would end the package's test binary and every row after this one.
func TestAStalledHoldersHeartbeatLeavesItsSuccessorsLeaseAtTheSameInode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		lease := filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-30 * time.Second)
		if err := os.WriteFile(lease, []byte("A"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lease, past, past); err != nil {
			t.Fatal(err)
		}
		release := sync.OnceFunc(holdWarmLease(lease, "A", warmLeaseHeartbeat))
		t.Cleanup(release)
		time.Sleep(warmLeaseHeartbeat)
		synctest.Wait()
		if info, err := os.Stat(lease); err != nil || !info.ModTime().After(past) {
			t.Fatalf("A's lease after a tick of its own heartbeat: %v (%v), want its mtime past %s", info, err, past)
		}

		// A stalls past the stale threshold and B takes over: B's fresh lease at the path, another
		// token in it, the inode A's file had. Its mtime is then set 30 s back, so the only thing
		// that could bring it forward is A's heartbeat refreshing a lease that is not its own. No
		// tick runs meanwhile: the heartbeat handled its last one before synctest.Wait returned.
		kept := lease + ".kept"
		for _, err := range []error{os.Link(lease, kept), os.Remove(lease), os.Link(kept, lease), os.Remove(kept)} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(lease, []byte("the-successors-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lease, past, past); err != nil {
			t.Fatal(err)
		}
		successor, err := os.Stat(lease)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * warmLeaseHeartbeat)
		synctest.Wait()
		current, err := os.Stat(lease)
		if err != nil {
			t.Fatalf("the successor's lease after ten of the stalled holder's ticks: %v, want it in place", err)
		}
		if !os.SameFile(successor, current) {
			t.Fatal("the lease at the path is not the successor's after the stalled holder's ticks")
		}
		if !current.ModTime().Before(past.Add(time.Second)) {
			t.Fatalf("the stalled holder's heartbeat refreshed its successor's lease (mtime %s), want it left as old as it was, %s", current.ModTime(), past)
		}
		if body, err := os.ReadFile(lease); err != nil || string(body) != "the-successors-token" {
			t.Fatalf("the successor's lease reads %q (%v) after the stalled holder's ticks, want its token untouched", body, err)
		}

		release()
		if body, err := os.ReadFile(lease); err != nil || string(body) != "the-successors-token" {
			t.Fatalf("the successor's lease reads %q (%v) after the stalled holder's release, want it still in place with its token", body, err)
		}
	})
}

// TestALiveHoldersHeartbeatOutlivesAnUnreadableLease: a tick that cannot read the lease must not
// end the heartbeat. A takeover's remove-and-create is two steps, so a tick between them finds no
// file at the path, and the tree volume's 9p can fail a read outright (EIO); the design budgets
// six missed heartbeats before a lease is stale (warmLeaseStale). A heartbeat ended for good at
// the first such tick would let a running build's lease age past the minute, a sibling take it
// over and a second `codegraph init` run on the same workspace. Here the lease is moved away for
// three ticks and moved back 30 s old: a heartbeat still alive brings its mtime forward again at
// its next tick, and the release that follows removes the lease, which carried this holder's
// token throughout. The control comes first: the heartbeat does refresh the lease before it is
// moved away. The row runs in a synctest bubble, and releases its heartbeat in a cleanup of the
// bubble, as the one above does.
func TestALiveHoldersHeartbeatOutlivesAnUnreadableLease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		lease := filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-30 * time.Second)
		if err := os.WriteFile(lease, []byte("A"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lease, past, past); err != nil {
			t.Fatal(err)
		}
		release := sync.OnceFunc(holdWarmLease(lease, "A", warmLeaseHeartbeat))
		t.Cleanup(release)
		refreshedAfterATick := func(what string) {
			t.Helper()
			time.Sleep(warmLeaseHeartbeat)
			synctest.Wait()
			if info, err := os.Stat(lease); err != nil || !info.ModTime().After(past) {
				t.Fatalf("%s: the lease is %v (%v), want its mtime past %s", what, info, err, past)
			}
		}
		refreshedAfterATick("A's heartbeat on its own lease")

		// The lease is unreadable for three ticks — each finds no file at the path — and then back,
		// 30 s old again, so the only thing that could bring its mtime forward is A's heartbeat.
		away := lease + ".away"
		if err := os.Rename(lease, away); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * warmLeaseHeartbeat)
		synctest.Wait()
		if err := os.Rename(away, lease); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lease, past, past); err != nil {
			t.Fatal(err)
		}
		refreshedAfterATick("A's heartbeat after three ticks that could not read the lease")

		release()
		if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat after the release of a lease carrying this holder's token: %v, want it gone", err)
		}
	})
}

// TestRefreshWarmLease table-tests the heartbeat's one tick: a lease carrying this holder's token
// is refreshed to now and the heartbeat goes on (true); one carrying another token — a successor's
// — is left exactly as old as it was and ends the heartbeat (false); one that cannot be read is
// nothing to refresh, and nothing is created, but the heartbeat goes on to read it again at the
// next tick (true).
func TestRefreshWarmLease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		missing bool
		want    bool
	}{
		{"own token: refreshed", "A", false, true},
		{"another token: left as old as it was", "the-successors-token", false, false},
		{"no file: nothing to refresh, the heartbeat goes on", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := filepath.Join(t.TempDir(), warmLeaseName)
			past := time.Now().Add(-30 * time.Second)
			if !tc.missing {
				if err := os.WriteFile(lease, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(lease, past, past); err != nil {
					t.Fatal(err)
				}
			}
			before := time.Now()
			if got := refreshWarmLease(lease, "A"); got != tc.want {
				t.Fatalf("refreshWarmLease = %t, want %t", got, tc.want)
			}
			if tc.missing {
				if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stat after refreshing a missing lease: %v, want it still absent", err)
				}
				return
			}
			info, err := os.Stat(lease)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want && info.ModTime().Before(before.Truncate(time.Second)) {
				t.Fatalf("mtime %s after refreshing this holder's lease, want about now (%s)", info.ModTime(), before)
			}
			if !tc.want && !info.ModTime().Before(time.Now().Add(-20*time.Second)) {
				t.Fatalf("mtime %s after a tick on a lease that is not this holder's, want it left at %s", info.ModTime(), past)
			}
			if body, err := os.ReadFile(lease); err != nil || string(body) != tc.content {
				t.Fatalf("lease reads %q (%v) after the tick, want %q untouched", body, err, tc.content)
			}
		})
	}
}

// TestWarmCodegraphIndexStillInitializesAFreshWorkspaceWhenStatusFails: the lease lives under
// `.codegraph/`, so the warm-up makes that directory before `status` runs, and nextCodegraphStep's
// fallback for a `status` that failed reads an existing directory as an index to repair. A
// workspace that had no `.codegraph/` before the warm-up must still be judged directory-less
// there: `init`, never `index`, which refuses on an uninitialized workspace.
func TestWarmCodegraphIndexStillInitializesAFreshWorkspaceWhenStatusFails(t *testing.T) {
	// Not parallel: puts a codegraph stub on the process PATH with t.Setenv.
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo 'database disk image is malformed' >&2; exit 1 ;;
index) echo 'CodeGraph not initialized' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status", "init"}) {
		t.Fatalf("codegraph calls = %v, want exactly status then init (the lease's directory must not read as an index to repair)", lines)
	}
}

// captureStderr swaps os.Stderr — which has no seam — for the write end of a pipe, drained into a
// buffer, until the returned function is called or the test ends; that function restores
// os.Stderr, closes the pipe and returns everything written meanwhile. No test that calls it runs
// in parallel, and go test starts the package's parallel tests only once every test that is not
// parallel has ended, so the swap races no other test's lines.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&buf, r)
		_ = r.Close()
	}()
	restored := false
	captured := func() string {
		if !restored {
			restored = true
			os.Stderr = saved
			_ = w.Close()
			<-drained
		}
		return buf.String()
	}
	t.Cleanup(func() { captured() })
	return captured
}

// TestWarmCodegraphIndexLogsOneSuccessLine: a warm-up whose build ended well logs exactly one line
// saying so — the subcommand it ran, how long that build took, and how long after the caller's
// launch the index was ready, what a pod's log needs to say about the role's start. Nothing else
// in the decision table says it: an index already complete logs no build, and a build that failed
// keeps its failure line and gets no success line beside it.
func TestWarmCodegraphIndexLogsOneSuccessLine(t *testing.T) {
	// Not parallel: captures os.Stderr and puts a codegraph stub on the process PATH.
	success := regexp.MustCompile(`(?m)^\[legion\] codegraph warm-up for (.*) built the index: codegraph init took .*, done .* after launch$`)
	t.Run("a fresh workspace built: one line", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		captured := captureStderr(t)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		stderr := captured()
		lines := success.FindAllStringSubmatch(stderr, -1)
		if len(lines) != 1 {
			t.Fatalf("success lines = %d, want exactly one in:\n%s", len(lines), stderr)
		}
		if lines[0][1] != dir {
			t.Fatalf("the success line names %q, want the workspace %q:\n%s", lines[0][1], dir, stderr)
		}
	})
	t.Run("an index already complete: no line", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".codegraph", ".codegraph-initialized"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		captured := captureStderr(t)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		if stderr := captured(); success.MatchString(stderr) {
			t.Fatalf("a warm-up that built nothing logged a success line:\n%s", stderr)
		}
		if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status --json"}) {
			t.Fatalf("codegraph calls = %v, want status --json alone on a complete index", calls)
		}
	})
	t.Run("an init that failed: no line", func(t *testing.T) {
		dir := t.TempDir()
		binDir := t.TempDir()
		script := `#!/bin/sh
case "$1" in
status) echo '{"initialized":false}' ;;
init) echo 'no database could be created' >&2; exit 1 ;;
esac
`
		if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		captured := captureStderr(t)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		stderr := captured()
		if success.MatchString(stderr) {
			t.Fatalf("a warm-up whose init failed logged a success line:\n%s", stderr)
		}
		if want := "[legion] codegraph init failed for " + dir + " (exit 1): no database could be created\n"; !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks the failure line %q:\n%s", want, stderr)
		}
	})
}

// TestWarmCodegraphIndexReadsACodegraphASignalEndedAsOneThatAnsweredNothing: a signal ends a
// CodeGraph 1.6.4 build at once — `codegraph init` killed mid-build by SIGTERM, the warm-up's own,
// exits in 0.03 s (exit 143), and by SIGKILL in 0.02 s (exit 137) — and the kernel's out-of-memory
// kill ends one the same way, so a signal's death is an ordinary end for a codegraph. A `status`
// so ended answered nothing, and the warm-up falls back to the directory as for a `status` that
// exited non-zero: on a workspace whose `.codegraph/` was there, `index`. A build so ended logs the
// signal and what it printed; one the warm-up's own bound ended logs that it timed out, since the
// same build would time out again. Read as a status, each was `exit -1`.
func TestWarmCodegraphIndexReadsACodegraphASignalEndedAsOneThatAnsweredNothing(t *testing.T) {
	// Not parallel: changes the process PATH and captures os.Stderr.
	fake := func(t *testing.T, callLog, cases string) {
		t.Helper()
		binDir := t.TempDir()
		script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + callLog + "'\ncase \"$1\" in\n" + cases + "esac\n"
		if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Run("a status a signal ended: the fallback's repair", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
			t.Fatal(err)
		}
		fake(t, callLog, "status) kill -KILL $$ ;;\nindex) exit 0 ;;\n")

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		calls, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatalf("read codegraph call log: %v", err)
		}
		if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); !slices.Equal(lines, []string{"status", "index"}) {
			t.Fatalf("codegraph calls = %v, want status then index: a status a signal ended answered nothing, as one that exited non-zero", lines)
		}
	})
	t.Run("a build a signal ended: the signal named", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		dir := t.TempDir()
		fake(t, callLog, "status) echo '{\"initialized\":false}' ;;\ninit) echo 'parsing 512 files' >&2; kill -KILL $$ ;;\n")
		captured := captureStderr(t)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		want := "[legion] codegraph init for " + dir + " was ended by signal: killed: parsing 512 files\n"
		if stderr := captured(); !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks the line %q:\n%s", want, stderr)
		}
	})
	// The two rows below hold the build's bound on a fake clock, and the test sets the order of
	// everything the row's verdict reads (boundRow): the stub marks each step it reached, and waits
	// for the test's mark before the next, so no real-clock span decides which of two things came
	// first. Each row's cleanups let the stub's processes go whatever the row did, so a row that
	// fails leaves none running.
	t.Run("a build its own bound ended: the timeout named", func(t *testing.T) {
		r := newBoundRow(t, fake, "echo 'parsing 512 files' >&2; : > '%[1]s/printed'; exec sleep 86400")
		r.reached("the stub's init to print its line", "printed")
		r.endTheBound()
		r.answered("timed out after 1s: parsing 512 files")
	})
	// A build killed by some other signal before its bound, whose child then holds its pipes past
	// the bound, was ended by that signal and not by the bound: the bound's end SIGTERMs a shell
	// that is gone. The stub's init prints, marks "printed" and waits for "go", which the test
	// writes once it sees the init's bound armed; the stub then starts a child that holds its pipes
	// until "release", and kills its own shell, and the child marks "gone" once the shell is
	// reaped. Only then does the test end the bound, and only after that does it write "release".
	// codegraphStopGrace, the real WaitDelay that would close the held pipes on its own, is
	// lengthened to what is left of the test binary's deadline, so it is the release that ends the
	// run, at the moment the test sets.
	t.Run("a build a signal ended before its bound, its pipes held past it: the signal named", func(t *testing.T) {
		grace := codegraphStopGrace
		codegraphStopGrace = testwait.Remaining(t)
		t.Cleanup(func() { codegraphStopGrace = grace })
		r := newBoundRow(t, fake, "echo 'parsing 512 files' >&2; : > '%[1]s/printed'; while [ -d '%[1]s' ] && [ ! -f '%[1]s/go' ]; do sleep 0.01; done; "+
			"( while kill -0 $$ 2>/dev/null; do sleep 0.01; done; : > '%[1]s/gone'; while [ -d '%[1]s' ] && [ ! -f '%[1]s/release' ]; do sleep 0.01; done; : > '%[1]s/left' ) & kill -KILL $$")
		r.reached("the stub's init to print its line", "printed")
		r.c.AwaitArmed(t, "the init's bound, while the build's shell still runs", 1)
		r.mark("go")
		r.reached("the build's shell to be gone with its pipes still held", "gone")
		r.endTheBound()
		r.mark("release")
		r.answered("was ended by signal: killed: parsing 512 files")
	})
}

// boundRow is one row of TestWarmCodegraphIndexReadsACodegraphASignalEndedAsOneThatAnsweredNothing
// whose build runs under a bound on a fake clock (c): the warm-up of dir on its own goroutine
// (done), a stub whose init runs the row's script, a directory for the marks the stub and the
// test leave each other (marks), and the stderr the warm-up wrote (captured).
type boundRow struct {
	t        *testing.T
	c        *clocktest.Fake
	dir      string
	marks    string
	done     chan struct{}
	captured func() string
}

// newBoundRow starts the warm-up of a fresh workspace under a 1 s bound on a fake clock, with a
// stub whose `init` runs script, which names the marks directory as %[1]s. Its cleanup, which runs
// before the marks directory is removed, ends the warm-up's context and writes "release", waits
// for the warm-up to return, and, once the test has written "go" (which lets a stub start a child
// that holds its pipes), waits for that child's "left": the context's end SIGTERMs the build's
// shell, and "release" lets the child go. So a stub the row left waiting is gone when the row is,
// whatever the row did; a stub's loops also end once the marks directory is gone.
func newBoundRow(t *testing.T, fake func(t *testing.T, callLog, cases string), script string) *boundRow {
	t.Helper()
	bound := codegraphTimeout
	codegraphTimeout = time.Second
	t.Cleanup(func() { codegraphTimeout = bound })
	r := &boundRow{t: t, c: clocktest.New(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)), dir: t.TempDir(), marks: t.TempDir(), done: make(chan struct{})}
	fake(t, filepath.Join(t.TempDir(), "calls.log"), "status) echo '{\"initialized\":false}' ;;\ninit) "+fmt.Sprintf(script, r.marks)+" ;;\n")
	r.captured = captureStderr(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(filepath.Join(r.marks, "release"), nil, 0o600)
		testwait.Receive(t, "the warm-up to return once its context ended and its stub was released", r.done)
		if _, err := os.Stat(filepath.Join(r.marks, "go")); err == nil {
			r.reached("the stub's child to leave once released", "left")
		}
	})
	go func() { defer close(r.done); warmCodegraphIndexOn(ctx, r.c, procgroup.NoReaper, r.dir, time.Now()) }()
	return r
}

// reached waits for the stub's mark name.
func (r *boundRow) reached(what, name string) {
	r.t.Helper()
	testwait.Eventually(r.t, what, func() bool { _, err := os.Stat(filepath.Join(r.marks, name)); return err == nil })
}

// mark leaves the stub the mark name.
func (r *boundRow) mark(name string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.marks, name), nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// endTheBound waits for the build's bound to be armed on the fake clock and moves the clock past
// it.
func (r *boundRow) endTheBound() {
	r.t.Helper()
	r.c.AwaitArmed(r.t, "the build's bound", 1)
	r.c.Advance(codegraphTimeout)
}

// answered waits for the warm-up to return and wants its stderr to carry the line of the build's
// end, which ends in end.
func (r *boundRow) answered(end string) {
	r.t.Helper()
	testwait.Receive(r.t, "the warm-up to return", r.done)
	want := "[legion] codegraph init for " + r.dir + " " + end + "\n"
	if stderr := r.captured(); !strings.Contains(stderr, want) {
		r.t.Fatalf("stderr lacks the line %q:\n%s", want, stderr)
	}
}

// TestNextCodegraphStep table-tests the pure decision warmCodegraphIndexOn runs on: a parsed
// status decides outright (its `initialized:false` always means init, even when `.codegraph/`
// exists; an initialized index needs nothing only while it is current, `"complete"` and built by
// the CLI version that reports it or with no builtWithVersion, which a build that indexed no file
// leaves, so a complete one another version built gets an index repair); dirExists is consulted
// only when status failed or didn't parse; and a live lock always wins over an index repair, of
// either kind.
func TestNextCodegraphStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		dirExists, lockLive bool
		statusExitCode      int
		statusStdout        string
		want                codegraphStep
	}{
		{"status valid, not initialized, no directory: init", false, false, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, not initialized, directory exists: init (untracked .codegraph)", true, false, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, complete: none", true, false, 0, `{"initialized":true,"index":{"state":"complete"}}`, codegraphStepNone},
		{"status valid, complete, lock live: none (complete wins, lock irrelevant)", true, true, 0, `{"initialized":true,"index":{"state":"complete"}}`, codegraphStepNone},
		{"status valid, partial, no live lock: index", true, false, 0, `{"initialized":true,"index":{"state":"indexing"}}`, codegraphStepIndex},
		{"status valid, partial, live lock: none", true, true, 0, `{"initialized":true,"index":{"state":"indexing"}}`, codegraphStepNone},
		{"status valid, complete, built by another version, no live lock: index", true, false, 0, `{"initialized":true,"version":"1.6.4","index":{"state":"complete","builtWithVersion":"1.5.0","reindexRecommended":false}}`, codegraphStepIndex},
		{"status valid, complete, built by another version, live lock: none", true, true, 0, `{"initialized":true,"version":"1.6.4","index":{"state":"complete","builtWithVersion":"1.5.0","reindexRecommended":false}}`, codegraphStepNone},
		{"status valid, complete, built by this version: none", true, false, 0, `{"initialized":true,"version":"1.6.4","index":{"state":"complete","builtWithVersion":"1.6.4","reindexRecommended":false}}`, codegraphStepNone},
		{"status valid, complete, builtWithVersion null (a build that indexed no file), version set: none", true, false, 0, `{"initialized":true,"version":"1.6.4","index":{"state":"complete","builtWithVersion":null}}`, codegraphStepNone},
		{"status failed, directory exists, no live lock: index", true, false, 1, ``, codegraphStepIndex},
		{"status failed, directory exists, live lock: none", true, true, 1, ``, codegraphStepNone},
		{"status failed, no directory: init", false, false, 1, ``, codegraphStepInit},
		{"status unparseable, directory exists: index", true, false, 0, `not json`, codegraphStepIndex},
		{"status unparseable, no directory: init", false, false, 0, `not json`, codegraphStepInit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextCodegraphStep(tc.dirExists, tc.lockLive, tc.statusExitCode, tc.statusStdout); got != tc.want {
				t.Fatalf("nextCodegraphStep(%v, %v, %d, %q) = %v, want %v", tc.dirExists, tc.lockLive, tc.statusExitCode, tc.statusStdout, got, tc.want)
			}
		})
	}
}

// TestTheRolePromptsCheckTheLeaseTheWarmUpHolds: a role's `status` during the warm-up's build makes
// that build fail, so each prompt has the role check this lease, by its name and its age, before it
// calls CodeGraph. A renamed lease or a changed stale age would leave the prompts checking the
// wrong thing. The `2>/dev/null` keeps a missing lease silent, since both finds print the path in
// their error.
func TestTheRolePromptsCheckTheLeaseTheWarmUpHolds(t *testing.T) {
	t.Parallel()
	check := ".codegraph/" + warmLeaseName + " -mmin -1 2>/dev/null"
	for _, part := range []string{
		filepath.Join("..", "prompts", "roles", "core", "common.md"),
		filepath.Join("..", "prompts", "roles", "merger.md"),
		filepath.Join("..", "prompts", "roles", "architect.md"),
		filepath.Join("..", "prompts", "roles", "architect-root.md"),
	} {
		body, err := os.ReadFile(part)
		if err != nil {
			t.Fatalf("read %s: %v", part, err)
		}
		text := strings.Join(strings.Fields(string(body)), " ")
		if n := strings.Count(text, check); n != 1 {
			t.Errorf("%s holds %q %d times, want once: the prompt must check the lease the warm-up holds", part, check, n)
		}
	}
	if warmLeaseStale != time.Minute {
		t.Errorf("warmLeaseStale is %s: `find -mmin -1` reads a file as fresh for under one minute, so the prompts' check and the lease's staleness must agree", warmLeaseStale)
	}
	if warmLeaseHeartbeat >= warmLeaseStale {
		t.Errorf("warmLeaseHeartbeat is %s and warmLeaseStale is %s: a holder must refresh its lease before another process reads it as stale", warmLeaseHeartbeat, warmLeaseStale)
	}
}
