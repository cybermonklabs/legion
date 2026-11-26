//go:build unix

package cmdtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testexec"
	"golang.org/x/sys/unix"
)

const (
	// buildPathsEnv names the file TestBuildChild appends each path it was handed to.
	buildPathsEnv = "CMDTEST_BUILD_PATHS"
	// buildSecondEnvEnv makes TestBuildChild's second build ask for another environment.
	buildSecondEnvEnv = "CMDTEST_BUILD_SECOND_ENV"
	// buildLockProbeEnv makes TestBuildChild first try to take the lock of its process's directory
	// of builds (requireOwnDirLocked).
	buildLockProbeEnv = "CMDTEST_BUILD_LOCK_PROBE"
	// goLogEnv names the file the go stand-in (goStandIn) appends each build it is asked for to,
	// goRealEnv the go it then runs, and goFailOnceEnv a file whose presence makes it fail the next
	// build, once, removing the file.
	goLogEnv      = "CMDTEST_GO_LOG"
	goRealEnv     = "CMDTEST_GO_REAL"
	goFailOnceEnv = "CMDTEST_GO_FAIL_ONCE"
)

// TestBuildChild is the test the build rows' processes run: two subtests, each a test of its own,
// each asking BuildCommand for hello, running it, and appending the path it was handed to
// buildPathsEnv's file. It is skipped where buildChildEnv is unset.
func TestBuildChild(t *testing.T) {
	if os.Getenv(buildChildEnv) == "" {
		t.Skip("subprocess of the build rows")
	}
	if os.Getenv(buildLockProbeEnv) == "1" {
		requireOwnDirLocked(t)
	}
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			c := Command{Name: "hello", Dir: filepath.Join("testdata", "hello")}
			if name == "second" && os.Getenv(buildSecondEnvEnv) == "1" {
				c.Env = append(os.Environ(), "CMDTEST_VARIANT=1")
			}
			binary := BuildCommand(t, c)
			if out, err := exec.Command(binary).Output(); err != nil || string(out) != "hello\n" {
				t.Fatalf("%s printed %q (%v); want hello", binary, out, err)
			}
			f, err := os.OpenFile(os.Getenv(buildPathsEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(binary + "\n"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// requireOwnDirLocked fails t unless the directory of this process's builds is locked: a lock taken
// through another open of it, as another process's sweep takes one, would block (EWOULDBLOCK).
func requireOwnDirLocked(t *testing.T) {
	t.Helper()
	dir := builds
	if dir == "" {
		t.Fatal("the process has no directory of builds: its tests do not run under Main")
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("taking the lock of %s answered %v; want EWOULDBLOCK, the lock its process holds", dir, err)
	}
	t.Log("the process's directory of builds is locked")
}

// buildRun is what one run of a build row's process left: the paths its tests were handed, the
// builds the go stand-in was asked for, the process's pid, exit code and output.
type buildRun struct {
	paths, builds []string
	pid, exit     int
	out           string
}

// runBuildChild runs this test binary's TestBuildChild as a process of its own, with mode as
// buildChildEnv, the go stand-in first on PATH, and env added to its environment.
func runBuildChild(t *testing.T, mode string, env ...string) buildRun {
	t.Helper()
	real, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stand := filepath.Join(dir, "bin")
	if err := os.Mkdir(stand, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := testexec.WriteFile(filepath.Join(stand, "go"), []byte(goStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, log := filepath.Join(dir, "paths"), filepath.Join(dir, "builds")
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildChild$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), buildChildEnv+"="+mode, buildPathsEnv+"="+paths, goLogEnv+"="+log, goRealEnv+"="+real,
		"PATH="+stand+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	run := buildRun{pid: cmd.Process.Pid, out: string(out)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exit = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run the build row's process: %v", err)
	}
	run.paths, run.builds = readLines(t, paths), readLines(t, log)
	return run
}

// goStandIn is a go that appends each build it is asked for to goLogEnv's file, fails it once
// where goFailOnceEnv's file is there, and otherwise runs goRealEnv's go.
const goStandIn = `#!/bin/sh
if [ "$1" = build ]; then
  echo "$*" >>"$` + goLogEnv + `"
  if [ -n "$` + goFailOnceEnv + `" ] && [ -e "$` + goFailOnceEnv + `" ]; then
    rm -f "$` + goFailOnceEnv + `"
    echo "go stand-in: this build fails, once" >&2
    exit 1
  fi
fi
exec "$` + goRealEnv + `" "$@"
`

// readLines is path's lines, none where it does not exist.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.TrimSpace(string(data)); text != "" {
		return strings.Split(text, "\n")
	}
	return nil
}

// requireGone fails t where path is still there.
func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is still there after its process ended (%v)", path, err)
	}
}

// TestATestProcessUnderMainBuildsEachCommandOnce: under Main, two tests that ask for one build are
// handed one binary, which go built once, in a subdirectory of the process's own directory
// (cmdtest-<pid>-*), and that directory is gone once the process has ended.
func TestATestProcessUnderMainBuildsEachCommandOnce(t *testing.T) {
	run := runBuildChild(t, "main")
	if run.exit != 0 || len(run.paths) != 2 {
		t.Fatalf("the process exited %d with paths %q; want 0 and two:\n%s", run.exit, run.paths, run.out)
	}
	if run.paths[0] != run.paths[1] || len(run.builds) != 1 {
		t.Fatalf("two tests were handed %q after %d builds; want one binary, built once", run.paths, len(run.builds))
	}
	processDir := filepath.Dir(filepath.Dir(run.paths[0]))
	if want := processDirPrefix + strconv.Itoa(run.pid) + "-"; !strings.HasPrefix(filepath.Base(processDir), want) {
		t.Fatalf("the binary is under %s; want a directory named %s*", processDir, want)
	}
	requireGone(t, processDir)
}

// TestABuildWithoutMainFailsNamingMain: a test process whose TestMain does not run Main or Run has
// no directory of builds, so a build fails the test that asked, naming cmdtest.Main, and runs no go
// build.
func TestABuildWithoutMainFailsNamingMain(t *testing.T) {
	run := runBuildChild(t, "plain")
	if run.exit != 1 || len(run.paths) != 0 || len(run.builds) != 0 || !strings.Contains(run.out, "does not run its tests through cmdtest.Main or cmdtest.Run") {
		t.Fatalf("the process exited %d with paths %q after %d builds; want 1, no path, no build, and the failure naming cmdtest.Main:\n%s", run.exit, run.paths, len(run.builds), run.out)
	}
}

// TestAnotherEnvironmentIsAnotherBuildUnderMain: under Main, a test that asks for the build with
// another environment is handed another binary, built again, in a directory of its own, so the
// directory of each binary holds that binary alone.
func TestAnotherEnvironmentIsAnotherBuildUnderMain(t *testing.T) {
	run := runBuildChild(t, "main", buildSecondEnvEnv+"=1")
	if run.exit != 0 || len(run.paths) != 2 {
		t.Fatalf("the process exited %d with paths %q; want 0 and two:\n%s", run.exit, run.paths, run.out)
	}
	if filepath.Dir(run.paths[0]) == filepath.Dir(run.paths[1]) || len(run.builds) != 2 {
		t.Fatalf("builds of two environments were handed %q after %d builds; want two binaries in two directories, two builds", run.paths, len(run.builds))
	}
}

// TestAFailedBuildFailsEveryTestThatAsksUnderMain: under Main, a build that fails fails the test
// that asked with go's own output, and every later test that asks for the same build with that
// output too, without building it again.
func TestAFailedBuildFailsEveryTestThatAsksUnderMain(t *testing.T) {
	failOnce := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(failOnce, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := runBuildChild(t, "main", goFailOnceEnv+"="+failOnce)
	if run.exit != 1 || !strings.Contains(run.out, "--- FAIL: TestBuildChild/first") || !strings.Contains(run.out, "--- FAIL: TestBuildChild/second") || strings.Count(run.out, "go stand-in: this build fails, once") != 2 {
		t.Fatalf("the process exited %d; want 1, both tests failing, each with go's output:\n%s", run.exit, run.out)
	}
	if len(run.builds) != 1 || len(run.paths) != 0 {
		t.Fatalf("%d builds, paths %q; want one build and no path", len(run.builds), run.paths)
	}
}

// TestMainRemovesTheBuildsOfEndedProcesses: before the tests, Main removes each directory of
// builds whose lock no process holds and whose pid runs nowhere here, and keeps one whose lock a
// running process holds, whatever pid its name carries, and a directory whose name only looks like
// one. otherNamespace is what a process in another PID namespace sharing this temporary directory
// and user leaves, as each role container of a Legion pod does: its name carries a pid that runs
// nowhere here, and its process holds its lock.
func TestMainRemovesTheBuildsOfEndedProcesses(t *testing.T) {
	tmp := t.TempDir()
	ended := exec.Command("true")
	if err := ended.Run(); err != nil {
		t.Fatal(err)
	}
	gone := strconv.Itoa(ended.Process.Pid)
	dead := filepath.Join(tmp, processDirPrefix+gone+"-left")
	otherNamespace := filepath.Join(tmp, processDirPrefix+gone+"-otherns")
	live := filepath.Join(tmp, processDirPrefix+strconv.Itoa(os.Getpid())+"-running")
	other := filepath.Join(tmp, processDirPrefix+"notapid-x")
	for _, dir := range []string{dead, otherNamespace, live, other} {
		if err := os.MkdirAll(filepath.Join(dir, "build-1"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if processRuns(ended.Process.Pid) {
		t.Skipf("pid %d runs again: another process took it", ended.Process.Pid)
	}
	holdLock(t, otherNamespace)
	holdLock(t, live)
	run := runBuildChild(t, "main", "TMPDIR="+tmp)
	if run.exit != 0 {
		t.Fatalf("the process exited %d:\n%s", run.exit, run.out)
	}
	requireGone(t, dead)
	for _, dir := range []string{otherNamespace, live, other} {
		if _, err := os.Stat(filepath.Join(dir, "build-1")); err != nil {
			t.Fatalf("%s was removed (%v); want it kept", dir, err)
		}
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("TMPDIR holds %d entries after the process; want the three kept, its own directory removed", len(entries))
	}
}

// TestMainKeepsAnUnlockedDirectoryOfAProcessThatRuns: this branch's cmdtest from before the lock
// makes its directory of builds with no lock. Main keeps such a directory while its name's pid runs
// here, so a sweep beside that process does not remove the builds it is still making. (#5123's
// directories, cmdtest-<digits>, are kept by the name check before any of this.)
func TestMainKeepsAnUnlockedDirectoryOfAProcessThatRuns(t *testing.T) {
	tmp := t.TempDir()
	unlocked := filepath.Join(tmp, processDirPrefix+strconv.Itoa(os.Getpid())+"-nolock")
	if err := os.MkdirAll(filepath.Join(unlocked, "build-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := runBuildChild(t, "main", "TMPDIR="+tmp)
	if run.exit != 0 {
		t.Fatalf("the process exited %d:\n%s", run.exit, run.out)
	}
	if _, err := os.Stat(filepath.Join(unlocked, "build-1")); err != nil {
		t.Fatalf("%s, unlocked and named for a pid that runs here, was removed (%v); want it kept", unlocked, err)
	}
}

// TestAProcessHoldsTheLockOfItsDirectoryOfBuilds: under Main, the directory of the process's
// builds is locked while its tests run, so no other process's sweep can take it.
func TestAProcessHoldsTheLockOfItsDirectoryOfBuilds(t *testing.T) {
	run := runBuildChild(t, "main", buildLockProbeEnv+"=1")
	if run.exit != 0 || len(run.paths) != 2 {
		t.Fatalf("the process exited %d with paths %q; want 0 and two:\n%s", run.exit, run.paths, run.out)
	}
	if !strings.Contains(run.out, "the process's directory of builds is locked") {
		t.Fatalf("the process did not find its directory of builds locked while its tests ran:\n%s", run.out)
	}
}

// holdLock holds a flock on dir for the rest of t, as a running process holds its directory of
// builds' (makeProcessDir).
func holdLock(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock %s: %v", dir, err)
	}
}
