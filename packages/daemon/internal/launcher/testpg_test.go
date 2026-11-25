package launcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stubTestPG puts a `legion-testpg` on PATH that appends one line per call — the subcommand, the
// DSN it was given, the directory it was told, the log it was told, and `present` or `absent` for
// whether that log was a file when it ran — to a file, and answers the exit code the file `exit`
// holds. It answers what the worker image's script answers, so the launcher's side is what the
// test reads.
func stubTestPG(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nif [ -f \"$" + testPGLogVariable + "\" ]; then log=present; else log=absent; fi\n" +
		"printf '%s %s %s %s %s\\n' \"$1\" \"$" + TestPGVariable + "\" \"$" + testPGDirVariable + "\" \"$" + testPGLogVariable + "\" \"$log\" >> " + calls +
		"\nexit $(cat " + filepath.Join(dir, "exit") + ")\n"
	if err := testexec.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// toldTestPG starts generation with env as its start command's own Env, answers what
// TestPGVariable and TestPGLogVariable held in the child's environment, in that order and joined
// by "|", and stops the generation. A variable the child did not hold answers "unset": the shell's
// `${name-unset}` answers the value for a name set to anything, including the empty string, and
// "unset" only for a name its environment does not hold.
func (g *rig) toldTestPG(generation uint64, env []string) string {
	g.t.Helper()
	return g.told(generation, "${"+TestPGVariable+"-unset}|${"+TestPGLogVariable+"-unset}", env, "")
}

// The launcher that serves the pod's test database names it to every generation it starts, over
// whatever DSN the daemon's start command carried, and a launcher that serves none takes the
// variable out of the generation's environment. The daemon and the worker image are deployed
// from two separately pinned builds and this launcher comes from the image, so the process that
// creates the database is the one that names it: a role is told a database its own pod's server
// holds whichever of the two builds is older. It reads whether this launcher was told to serve a
// database, never whether a server answers: the first `ensure` runs before any role and its
// failure is logged rather than thrown, so neither rig here runs a server at all. The server's
// log goes with the DSN: a serving launcher's generation is told the path its role-init binds
// the log at, under the fixed root, over whatever its environment carried, and a launcher that
// serves none tells its generation of no log.
func TestEveryGenerationIsToldTheDSNItsOwnLauncherServes(t *testing.T) {
	t.Parallel()
	// What a daemon of another build sends: the database every image's server serves. No daemon
	// sends the log's path; a value a start command carried all the same is replaced by this
	// launcher's own, or dropped, as the DSN is.
	const fromTheDaemon = "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable"
	const fromElsewhere = "/somewhere/else.log"
	carried := []string{TestPGVariable + "=" + fromTheDaemon, TestPGLogVariable + "=" + fromElsewhere}
	// The DSN this launcher serves and the path role-init binds the server's log at.
	want := TestPGDSN + "|" + filepath.Join(roleFixedRoot, RoleTestPGLog)
	serving := newSessionRig(t, Config{TestPostgres: true})
	if got := serving.toldTestPG(1, carried); got != want {
		t.Errorf("a generation whose start command carried %s=%q and %s=%q ran with %s|%s=%q, want this launcher's own %q",
			TestPGVariable, fromTheDaemon, TestPGLogVariable, fromElsewhere, TestPGVariable, TestPGLogVariable, got, want)
	}
	if got := serving.toldTestPG(2, nil); got != want {
		t.Errorf("a generation whose start command named neither ran with %s|%s=%q, want this launcher's own %q",
			TestPGVariable, TestPGLogVariable, got, want)
	}
	none := newSessionRig(t, Config{})
	if got := none.toldTestPG(1, carried); got != "unset|unset" {
		t.Errorf("a generation of a launcher that serves no test database ran with %s|%s=%q, want neither variable",
			TestPGVariable, TestPGLogVariable, got)
	}
}

// The launcher serves the pod's test database before it accepts a role start: serveTestPG's first
// `ensure` has returned when it does, so a role that starts at once finds the server listening.
// Each tick asks again, which is what starts a postmaster that has died, and every call carries
// the DSN the roles are told, the cluster's directory under the roles directory and the server's
// log in it, which is a file before the first `ensure` runs: every role started after it has the
// log to bind, whatever that `ensure` came to.
func TestTheLauncherServesTheTestDatabaseBeforeAnyRoleAndKeepsAsking(t *testing.T) {
	// Not parallel: it writes the package variable testPGInterval, which parallel tests read.
	calls := stubTestPG(t)
	testPGInterval = 20 * time.Millisecond
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	rolesDir := t.TempDir()
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	// The ticker goroutine is waited for before the cleanup above restores the interval, so no
	// read of it outlives this test.
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	first, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("serveTestPG returned before its first ensure ran: %v", err)
	}
	want := "ensure " + TestPGDSN + " " + filepath.Join(rolesDir, TestPGDirName) + " " + testPGLog(rolesDir) + " present\n"
	if string(first) != want {
		t.Errorf("the first call is %q, want %q", first, want)
	}
	// The second call comes on the ticker, every 20 ms; how soon it comes is not the claim, so the
	// wait is for it under the test's hang guard.
	var again []byte
	if !testwait.Holds(beforeDeadline(t), 5*time.Millisecond, func() bool {
		var err error
		again, err = os.ReadFile(calls)
		return err == nil && strings.Count(string(again), "ensure ") > 1
	}) {
		t.Fatalf("the launcher asked once and never again: %q", again)
	}
	if got := strings.ReplaceAll(string(again), want, ""); got != "" {
		t.Errorf("a later call is %q, want every call to be %q", got, want)
	}
}

// A log the launcher finds is kept as it is: a launcher that restarts in a pod whose server has
// run leaves that server's lines where every role reads them. And a log that cannot be made is
// logged and nothing else, as a script that fails is: the script still runs, and the pod still
// runs its roles.
func TestATestDatabaseLogIsKeptAndOneThatCannotBeMadeOnlyLogs(t *testing.T) {
	// Not parallel: it writes the package variable testPGInterval, which parallel tests read.
	calls := stubTestPG(t)
	testPGInterval = time.Hour
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	serve := func(rolesDir string) string {
		t.Helper()
		log := &lockedBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{})
		cancel()
		<-done
		return log.String()
	}
	rolesDir := t.TempDir()
	const earlier = "an earlier server's line\n"
	if err := os.MkdirAll(testPGDir(rolesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testPGLog(rolesDir), []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	if said := serve(rolesDir); strings.Contains(said, "launcher: test database:") {
		t.Errorf("over a log already there the container log holds %q, want no refusal", said)
	}
	if kept, err := os.ReadFile(testPGLog(rolesDir)); err != nil || string(kept) != earlier {
		t.Errorf("the log the launcher found holds %q (%v), want it kept as %q", kept, err, earlier)
	}
	// A roles directory that is a file: nothing can be made under it.
	blocked := filepath.Join(t.TempDir(), "roles")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	if said := serve(blocked); !strings.Contains(said, "launcher: test database: make the server's directory:") {
		t.Errorf("over a log that cannot be made the container log holds %q, want the refusal named", said)
	}
	want := "ensure " + TestPGDSN + " " + testPGDir(blocked) + " " + testPGLog(blocked) + " absent\n"
	if ran, err := os.ReadFile(calls); err != nil || string(ran) != want {
		t.Errorf("after a log that could not be made the script ran %q (%v), want %q", ran, err, want)
	}
}

// A script that fails is logged and nothing else: a pod whose test database cannot start still
// runs its roles, where a launcher that failed its container would cost the issue every role it
// serves.
func TestATestDatabaseThatCannotStartOnlyLogs(t *testing.T) {
	// Not parallel: it writes the package variable testPGInterval, which parallel tests read.
	calls := stubTestPG(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "exit"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testPGInterval = time.Hour
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	// The log is read here while the script's output is still being copied into it: cmd.Wait
	// returns at WaitDelay with that copy goroutine still running, so the buffer the roleLog
	// writes through must be one that holds its own lock against this read.
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveTestPG(ctx, Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	if !strings.Contains(log.String(), "launcher: test database:") {
		t.Errorf("the container log holds %q, want the refusal named", log.String())
	}
}

// The guard holds a running child out of the reaper's pass and forgets it once its own wait has
// read its exit, so nothing of the launcher's own is left in the skip list between calls.
func TestTheGuardForgetsAChildOnceItsWaitHasRead(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (stubTestPG).
	calls := stubTestPG(t)
	guard := &pidGuard{}
	if err := runTestPG(context.Background(), Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{}, guard, "ensure"); err != nil {
		t.Fatalf("runTestPG: %v", err)
	}
	if held := guard.held(); len(held) != 0 {
		t.Errorf("the guard still holds %v", held)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Errorf("the script never ran: %v", err)
	}
}

// One run of the script is bounded, so an `ensure` that hangs — a lock a dead holder still owns —
// costs a pod one wait and a log line, not its whole boot: the launcher's first `ensure` runs
// before any role starts, inside the daemon's boot timeout.
func TestATestDatabaseRunThatHangsIsBounded(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	if err := testexec.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	testPGBound = 200 * time.Millisecond
	t.Cleanup(func() { testPGBound = 60 * time.Second })
	started := time.Now()
	err := runTestPG(context.Background(), Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{}, &pidGuard{}, "ensure")
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("runTestPG = %v, want the bound named", err)
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Errorf("runTestPG waited %s on a script that hangs", waited)
	}
}

// The image probe runs what a pod's launcher runs and then proves the DSN every role is told:
// check, ensure, a database made and dropped through it, and the server stopped, so the probe pod
// leaves nothing running. A step that fails is answered with its output, and the server is stopped
// all the same.
func TestCheckTestPostgresRunsEveryStepAndStopsTheServer(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	for _, program := range []string{testPGProgram, "psql"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> " + calls + "\nexit $(cat " + filepath.Join(dir, "exit") + ")\n"
		if err := testexec.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := CheckTestPostgres(filepath.Join(dir, "cluster")); err != nil {
		t.Fatalf("CheckTestPostgres: %v", err)
	}
	ran, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		testPGProgram + " check\n", testPGProgram + " ensure\n",
		"psql -X -q " + TestPGDSN + " -c create database legion_probe_testpg strategy file_copy\n",
		"psql -X -q " + TestPGDSN + " -c drop database legion_probe_testpg with (force)\n",
		testPGProgram + " stop\n",
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the probe ran %q, want a %q among them", ran, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	err = CheckTestPostgres(filepath.Join(dir, "cluster"))
	if err == nil || !strings.Contains(err.Error(), "the pod's own test database") {
		t.Fatalf("CheckTestPostgres on a failing step = %v, want the failure named", err)
	}
	if again, readErr := os.ReadFile(calls); readErr != nil || !strings.Contains(string(again), testPGProgram+" stop\n") {
		t.Errorf("a failed probe ran %q (%v), want the server stopped all the same", again, readErr)
	}
}
