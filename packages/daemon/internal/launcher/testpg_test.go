package launcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		again, err := os.ReadFile(calls)
		if err == nil && strings.Count(string(again), "ensure ") > 1 {
			if got := strings.ReplaceAll(string(again), want, ""); got != "" {
				t.Errorf("a later call is %q, want every call to be %q", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launcher asked once and never again: %q", again)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A log the launcher finds is kept as it is: a launcher that restarts in a pod whose server has
// run leaves that server's lines where every role reads them. And a log that cannot be made is
// logged and nothing else, as a script that fails is: the script still runs, and the pod still
// runs its roles.
func TestATestDatabaseLogIsKeptAndOneThatCannotBeMadeOnlyLogs(t *testing.T) {
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

// A run of the script waits as long as its caller allows, and no longer: at the caller's deadline
// its whole process group is stopped, so a child the script started goes with it, and the error
// names the deadline. The script is told that deadline, in whole seconds since the epoch.
func TestATestDatabaseRunIsStoppedWithItsGroupAtItsCallersDeadline(t *testing.T) {
	dir := t.TempDir()
	told, child := filepath.Join(dir, "told"), filepath.Join(dir, "child")
	script := "#!/bin/sh\necho \"${" + testPGDeadlineVariable + ":-none}\" > " + told + "\nsleep 300 &\n" +
		"echo \"$! $(cut -d' ' -f22 /proc/$!/stat)\" > " + child + "\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		for _, pid := range stillRunning(t, child) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := runTestPG(ctx, Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{}, &pidGuard{}, "ensure")
	returned := time.Now()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), testPGProgram+" ensure did not finish before its caller's deadline (context deadline exceeded), so its process group was stopped") {
		t.Fatalf("runTestPG = %v, want the caller's deadline named", err)
	}
	if returned.Before(deadline) {
		t.Errorf("runTestPG returned %s before its caller's deadline: the run has a bound of its own", deadline.Sub(returned))
	}
	if waited := returned.Sub(deadline); waited > 10*time.Second {
		t.Errorf("runTestPG returned %s after its caller's deadline", waited)
	}
	if recordLines(t, child) != 1 {
		t.Fatal("the script never started its child, so this run proves nothing")
	}
	if running := stillRunning(t, child); len(running) > 0 {
		t.Errorf("the child the script started still runs after the stop: %v", running)
	}
	if got, readErr := os.ReadFile(told); readErr != nil || strings.TrimSpace(string(got)) != strconv.FormatInt(deadline.Unix(), 10) {
		t.Errorf("the script was told %s=%q (%v), want the caller's deadline %d", testPGDeadlineVariable, got, readErr, deadline.Unix())
	}
}

// The launcher's first `ensure` has testPGBootBudget as its deadline, which serveTestPG, its
// caller, gives it: one that has not finished by then is stopped and logged, and serveTestPG
// returns, so the launcher goes on to serve its roles. Every tick after it has no deadline.
func TestTheLaunchersFirstEnsureEndsAtTheBootBudgetAndATickHasNone(t *testing.T) {
	dir := t.TempDir()
	record, first := filepath.Join(dir, "deadlines"), filepath.Join(dir, "first")
	script := "#!/bin/sh\necho \"${" + testPGDeadlineVariable + ":-none}\" >> " + record +
		"\nif [ ! -e " + first + " ]; then : > " + first + "; exec sleep 300; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	testPGBootBudget, testPGInterval = time.Second, 20*time.Millisecond
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	began := time.Now()
	done := serveTestPG(ctx, Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{})
	returned := time.Since(began)
	t.Cleanup(func() {
		cancel()
		<-done
		testPGBootBudget, testPGInterval = 60*time.Second, 15*time.Second
	})
	if returned < time.Second || returned > 15*time.Second {
		t.Errorf("serveTestPG returned %s after it started, over a first ensure that never ends and a 1 s boot budget", returned)
	}
	if !strings.Contains(log.String(), "launcher: test database: "+testPGProgram+" ensure did not finish before its caller's deadline") {
		t.Errorf("the container log holds %q, want the first ensure named as having run out the boot budget", log.String())
	}
	var lines []string
	for wait := time.Now().Add(10 * time.Second); time.Now().Before(wait); time.Sleep(10 * time.Millisecond) {
		data, _ := os.ReadFile(record)
		if lines = strings.Fields(string(data)); len(lines) >= 2 {
			break
		}
	}
	if len(lines) < 2 {
		t.Fatalf("the script was run %d times, want the first ensure and a tick", len(lines))
	}
	told, err := strconv.ParseInt(lines[0], 10, 64)
	if want := began.Add(time.Second).Unix(); err != nil || told < want-1 || told > want+1 {
		t.Errorf("the first ensure was told %s=%q, want the boot budget's end, %d", testPGDeadlineVariable, lines[0], want)
	}
	if lines[1] != "none" {
		t.Errorf("a tick was told %s=%q, want no deadline", testPGDeadlineVariable, lines[1])
	}
}

// The image probe runs what a pod's launcher runs and then proves the DSN every role is told:
// check, ensure, a database made and dropped through it, and the server stopped, so the probe pod
// leaves nothing running. A step that fails is answered with its output, and the server is stopped
// all the same.
func TestCheckTestPostgresRunsEveryStepAndStopsTheServer(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	for _, program := range []string{testPGProgram, "psql"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> " + calls + "\nexit $(cat " + filepath.Join(dir, "exit") + ")\n"
		if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := CheckTestPostgres(context.Background(), filepath.Join(dir, "cluster")); err != nil {
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
	err = CheckTestPostgres(context.Background(), filepath.Join(dir, "cluster"))
	if err == nil || !strings.Contains(err.Error(), "the pod's own test database") {
		t.Fatalf("CheckTestPostgres on a failing step = %v, want the failure named", err)
	}
	if again, readErr := os.ReadFile(calls); readErr != nil || !strings.Contains(string(again), testPGProgram+" stop\n") {
		t.Errorf("a failed probe ran %q (%v), want the server stopped all the same", again, readErr)
	}
}

// probeStandIn puts first on PATH a `legion-testpg` that handles its processes as the worker
// image's script does, and a `psql` that answers at once, each appending what it ran to the file
// calls; each `legion-testpg` also appends its command and the LEGION_TESTPG_DEADLINE it was told
// (`none` for none) to the file deadlines. `ensure` runs its start in flock's child, as the
// script's start-locked runs, so a kill of `ensure` alone leaves that start running. The start
// records itself in the file starters, waits the seconds delay gives, and then starts a server: a
// sleep in a session of its own whose working directory is the cluster's data directory, as
// pg_ctl starts the postmaster and the postmaster changes into its data directory, recorded in the
// cluster's postmaster.pid and in the file servers. `stop` ends the server postmaster.pid names,
// waits until it has exited, as `pg_ctl stop --wait` does, and removes the file, and fails where
// there is none, as pg_ctl does. Three files of the directory change what it answers, as a test
// writes them: psql-fails makes the `create database` step exit 3; stop-fails makes `stop` exit 1
// and leave the server running; stop-hangs makes `stop` wait two minutes and leave the server
// running. A record is a pid and the process's start time, so a pid reused since is never
// signalled; every process a record names is killed when the test ends, so a failing run leaves
// nothing running. It answers the directory those files are in.
func probeStandIn(t *testing.T, delay string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
here='` + dir + `'
printf '%s %s\n' "$(basename "$0")" "$*" >> "$here/calls"
if [ "$(basename "$0")" = psql ]; then
	case "$*" in
	*"strategy file_copy"*) [ ! -e "$here/psql-fails" ] || { echo "psql: the copy was refused" >&2; exit 3; } ;;
	esac
	exit 0
fi
printf '%s %s\n' "$1" "${LEGION_TESTPG_DEADLINE:-none}" >> "$here/deadlines"
cluster=$LEGION_TESTPG_DIR
case $1 in
check) ;;
ensure)
	mkdir -p "$cluster/data"
	flock -o "$cluster/.lock" sh -c '
		echo "$$ $(cut -d" " -f22 /proc/$$/stat)" >> "$1/starters"
		sleep ` + delay + `
		(cd "$2/data" && exec setsid sleep 300 < /dev/null > /dev/null 2>&1) &
		server="$! $(cut -d" " -f22 /proc/$!/stat)"
		echo "$server" > "$2/postmaster.pid"
		echo "$server" >> "$1/servers"
	' start "$here" "$cluster"
	;;
stop)
	[ ! -e "$here/stop-fails" ] || { echo "pg_ctl: could not send stop signal" >&2; exit 1; }
	[ ! -e "$here/stop-hangs" ] || exec sleep 120
	[ -f "$cluster/postmaster.pid" ] || { echo "no server running" >&2; exit 1; }
	read -r pid since < "$cluster/postmaster.pid"
	alive() {
		stat=$(cat "/proc/$pid/stat" 2> /dev/null) || return 1
		[ "$(echo "$stat" | cut -d" " -f22)" = "$since" ] && [ "$(echo "$stat" | cut -d" " -f3)" != Z ]
	}
	if alive; then
		kill "$pid"
		while alive; do sleep 0.05; done
	fi
	rm -f "$cluster/postmaster.pid"
	;;
esac
`
	for _, program := range []string{testPGProgram, "psql"} {
		if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		for _, record := range []string{"starters", "servers"} {
			for _, pid := range stillRunning(t, filepath.Join(dir, record)) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return dir
}

// stillRunning answers the pids of the processes a record file of probeStandIn's names that still
// run: a process that started when its line says and is no zombie. A missing file names none.
func stillRunning(t *testing.T, file string) []int {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var running []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		pidText, since, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			t.Fatalf("%s holds %q, which names no pid", file, line)
		}
		stat, err := os.ReadFile("/proc/" + pidText + "/stat")
		if err != nil {
			continue
		}
		// The fields after the command's closing parenthesis start at the state, field 3, so the
		// start time, field 22, is the twentieth of them.
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		if len(fields) >= 20 && fields[0] != "Z" && fields[19] == since {
			running = append(running, pid)
		}
	}
	return running
}

// recordLines answers how many records a record file of probeStandIn's holds: a step that never
// started its start, or a server that was never started, holds none.
func recordLines(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// deadlinesTold answers what the stand-in's legion-testpg was told as LEGION_TESTPG_DEADLINE on
// each run of command, in order.
func deadlinesTold(t *testing.T, stand, command string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stand, "deadlines"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var told []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok && name == command {
			told = append(told, value)
		}
	}
	return told
}

// Neither caller of the script has a bound of its own: the launcher's `ensure` and the image
// probe's steps each wait as long as their caller's deadline allows. A start that ends 61 s in,
// past the 60 s each of them once cut it at, slow as under gVisor and still working, is waited
// for by both at once with three minutes left. The launcher's server is left serving, the probe's
// is stopped, and nothing of either start is left running. Only a run longer than a bound can see
// that bound, so this one runs past a minute.
func TestTheTestDatabasesCallersWaitForAStartPastAMinute(t *testing.T) {
	stand := probeStandIn(t, "61")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rolesDir := t.TempDir()
	launched := make(chan error, 1)
	go func() {
		launched <- runTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{}, &pidGuard{}, "ensure")
	}()
	if err := CheckTestPostgres(ctx, filepath.Join(stand, "cluster")); err != nil {
		t.Errorf("CheckTestPostgres on a server that starts 61 s in, with 3 minutes left = %v, want it waited for", err)
	}
	if err := <-launched; err != nil {
		t.Errorf("runTestPG ensure on a server that starts 61 s in, with 3 minutes left = %v, want it waited for", err)
	}
	ran, err := os.ReadFile(filepath.Join(stand, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		testPGProgram + " ensure\n",
		"psql -X -q " + TestPGDSN + " -c create database legion_probe_testpg strategy file_copy\n",
		"psql -X -q " + TestPGDSN + " -c drop database legion_probe_testpg with (force)\n",
		testPGProgram + " stop\n",
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the probe ran %q, want a %q among them", ran, want)
		}
	}
	if started := recordLines(t, filepath.Join(stand, "servers")); started != 2 {
		t.Fatalf("the stand-in started %d servers, want the one each ensure starts", started)
	}
	if running := stillRunning(t, filepath.Join(stand, "starters")); len(running) > 0 {
		t.Errorf("after both callers returned, the starts they waited for still run: %v", running)
	}
	if _, err := os.Stat(filepath.Join(stand, "cluster", "postmaster.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the probe's server was not stopped: its postmaster.pid is still there (%v)", err)
	}
	if running := stillRunning(t, filepath.Join(testPGDir(rolesDir), "postmaster.pid")); len(running) != 1 {
		t.Errorf("the launcher's server runs as %v, want the one server its ensure started, serving", running)
	}
}

// The control: a step still running when the caller's deadline passes is named, on one line, and
// stopped with its whole process group, and not before that deadline. The start flock's child
// runs ends with `ensure`, so it cannot start a server after the probe's stop has found none, and
// the stop runs all the same, told a deadline that has passed, so it asks for the shutdown and
// waits for none. The script is told the caller's deadline for each step.
func TestCheckTestPostgresStopsAStepThatOutlivesItsCallersDeadline(t *testing.T) {
	stand := probeStandIn(t, "300")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := CheckTestPostgres(ctx, filepath.Join(stand, "cluster"))
	returned := time.Now()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), testPGProgram+" ensure did not finish before the probe's context ended") {
		t.Fatalf("CheckTestPostgres on a start that never answers = %v, want ensure named as having run out the deadline", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal is %d lines, want one: %q", strings.Count(err.Error(), "\n")+1, err)
	}
	if returned.Before(deadline) {
		t.Errorf("CheckTestPostgres returned %s before its caller's deadline: a step has a bound of its own", deadline.Sub(returned))
	}
	if waited := returned.Sub(deadline); waited > 15*time.Second {
		t.Errorf("CheckTestPostgres returned %s after a 5 s deadline", waited)
	}
	if started := recordLines(t, filepath.Join(stand, "starters")); started != 1 {
		t.Fatalf("the stand-in's start ran %d times, want once: a run that never reached it proves nothing", started)
	}
	if running := stillRunning(t, filepath.Join(stand, "starters")); len(running) > 0 {
		t.Errorf("the start flock's child ran still runs after the deadline: %v", running)
	}
	if started := recordLines(t, filepath.Join(stand, "servers")); started != 0 {
		t.Errorf("the stand-in started %d servers, want none", started)
	}
	if ran, readErr := os.ReadFile(filepath.Join(stand, "calls")); readErr != nil || !strings.Contains(string(ran), testPGProgram+" stop\n") {
		t.Errorf("a probe past its deadline ran %q (%v), want the server stopped all the same", ran, readErr)
	}
	want := strconv.FormatInt(deadline.Unix(), 10)
	for _, command := range []string{"check", "ensure"} {
		if told := deadlinesTold(t, stand, command); len(told) != 1 || told[0] != want {
			t.Errorf("legion-testpg %s was told %s=%v, want the caller's deadline %s", command, testPGDeadlineVariable, told, want)
		}
	}
	told := deadlinesTold(t, stand, "stop")
	if len(told) != 1 {
		t.Fatalf("legion-testpg stop was told %v, want one deadline", told)
	}
	if passed, parseErr := strconv.ParseInt(told[0], 10, 64); parseErr != nil || passed < deadline.Unix() || passed > returned.Unix() {
		t.Errorf("legion-testpg stop was told %s=%q, want a deadline that had passed when it ran", testPGDeadlineVariable, told[0])
	}
}

// recordedPIDs answers every pid a record file of probeStandIn's names, running or not.
func recordedPIDs(t *testing.T, file string) []int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		pidText, _, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			t.Fatalf("%s holds %q, which names no pid", file, line)
		}
		pids = append(pids, pid)
	}
	return pids
}

// otherServer starts a process that stands for a server of another cluster: a sleep in a session
// of its own whose working directory is another data directory, as a postmaster's is. It answers
// the record file that names it, which stillRunning reads; the process is killed when the test
// ends.
func otherServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record")
	cmd := exec.Command("sh", "-c", `(cd "$1" && exec setsid sleep 300 < /dev/null > /dev/null 2>&1) & echo "$! $(cut -d" " -f22 /proc/$!/stat)" > "$2"`, "other", data, record)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start the other cluster's server: %v: %s", err, out)
	}
	t.Cleanup(func() {
		for _, pid := range stillRunning(t, record) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if len(stillRunning(t, record)) != 1 {
		t.Fatal("the other cluster's server is not running, so this run proves nothing")
	}
	return record
}

// A failed step is followed by the stop, a stop that fails is named after the step's own failure
// on the same line, and the probe then reads the server by its data directory: one still running
// is the probe's to end, so it stops it, removes the cluster's data, and names both, so the probe
// pod leaves nothing running. A server of another data directory is left alone.
func TestCheckTestPostgresStopsAndNamesTheServerAFailedStopLeft(t *testing.T) {
	stand := probeStandIn(t, "0")
	for _, knob := range []string{"psql-fails", "stop-fails"} {
		if err := os.WriteFile(filepath.Join(stand, knob), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	other := otherServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cluster := filepath.Join(stand, "cluster")
	err := CheckTestPostgres(ctx, cluster)
	servers := recordedPIDs(t, filepath.Join(stand, "servers"))
	if len(servers) != 1 {
		t.Fatalf("the stand-in started servers %v, want the one ensure starts", servers)
	}
	data := filepath.Join(cluster, "data")
	for _, want := range []string{
		"the pod's own test database: psql -X -q " + TestPGDSN + " -c create database legion_probe_testpg strategy file_copy: exit status 3: psql: the copy was refused",
		"; and the stop after it: the pod's own test database: " + testPGProgram + " stop: exit status 1: pg_ctl: could not send stop signal",
		"; the pod's own test database: the server in " + data + " still ran after the stop: pid " + strconv.Itoa(servers[0]) + " (sleep); the probe stopped it (SIGKILL to its process group and to each pid) and removed " + data,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckTestPostgres = %v, want %q in it", err, want)
		}
	}
	if err != nil && strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal is %d lines, want one: %q", strings.Count(err.Error(), "\n")+1, err)
	}
	if running := stillRunning(t, filepath.Join(stand, "servers")); len(running) > 0 {
		t.Errorf("the server the failed stop left still runs after the probe returned: %v", running)
	}
	if _, statErr := os.Stat(data); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the probe's cluster data %s is still there after the probe returned (%v)", data, statErr)
	}
	if len(stillRunning(t, other)) != 1 {
		t.Error("the probe stopped a server of another data directory")
	}
}

// A stop is waited for as long as the caller's deadline allows, and no longer: one that has not
// ended by then is stopped with its process group and named, and the server it left is stopped by
// the probe and named with it, and the cluster's data removed.
func TestCheckTestPostgresStopsTheServerAStopThatOutlivesItsCallersDeadlineLeft(t *testing.T) {
	stand := probeStandIn(t, "0")
	if err := os.WriteFile(filepath.Join(stand, "stop-hangs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	cluster := filepath.Join(stand, "cluster")
	err := CheckTestPostgres(ctx, cluster)
	returned := time.Now()
	servers := recordedPIDs(t, filepath.Join(stand, "servers"))
	if len(servers) != 1 {
		t.Fatalf("the stand-in started servers %v, want the one ensure starts", servers)
	}
	data := filepath.Join(cluster, "data")
	for _, want := range []string{
		"the pod's own test database: " + testPGProgram + " stop did not finish before the probe's context ended (context deadline exceeded), so its process group was stopped",
		"; the pod's own test database: the server in " + data + " still ran after the stop: pid " + strconv.Itoa(servers[0]) + " (sleep); the probe stopped it (SIGKILL to its process group and to each pid) and removed " + data,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckTestPostgres = %v, want %q in it", err, want)
		}
	}
	if running := stillRunning(t, filepath.Join(stand, "servers")); len(running) > 0 {
		t.Errorf("the server the stop left still runs after the probe returned: %v", running)
	}
	if returned.Before(deadline) || returned.Sub(deadline) > 15*time.Second {
		t.Errorf("CheckTestPostgres returned %s after its caller's 4 s deadline, want at it", returned.Sub(deadline))
	}
}

// The stop the probe runs once its own deadline has passed, because a step ran that deadline out,
// is given testPGLateStopGrace and no more: one that never answers is stopped with its process
// group at the grace and named after the step, so the probe returns within the grace of its
// caller's deadline rather than when the stop ends.
func TestCheckTestPostgresGivesALateStopItsGraceAndNoMore(t *testing.T) {
	stand := probeStandIn(t, "300")
	if err := os.WriteFile(filepath.Join(stand, "stop-hangs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := CheckTestPostgres(ctx, filepath.Join(stand, "cluster"))
	late := time.Since(deadline)
	for _, want := range []string{
		"the pod's own test database: " + testPGProgram + " ensure did not finish before the probe's context ended (context deadline exceeded), so its process group was stopped",
		"; and the stop after it: the pod's own test database: " + testPGProgram + " stop did not finish before the probe's context ended (context deadline exceeded), so its process group was stopped",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckTestPostgres = %v, want %q in it", err, want)
		}
	}
	if late < testPGLateStopGrace || late > testPGLateStopGrace+15*time.Second {
		t.Errorf("CheckTestPostgres returned %s after its caller's deadline, want the late stop's grace, %s, and not much more", late, testPGLateStopGrace)
	}
}

// The server is found by its arguments as well as by its working directory: a postmaster that
// pg_ctl has started and that has not yet changed into its data directory is named by the data
// directory among its arguments, a backend by its working directory, and a process that holds
// neither is not named. A fake /proc, so the argument branch is read on its own.
func TestTheTestDatabasesServerIsFoundByItsArgumentsOrItsWorkingDirectory(t *testing.T) {
	proc := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	entry := func(pid int, cwd, comm string, args ...string) {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"cmdline": strings.Join(args, "\x00") + "\x00",
			"comm":    comm + "\n",
			"stat":    strconv.Itoa(pid) + " (" + comm + ") S 1 " + strconv.Itoa(pid) + " " + strconv.Itoa(pid) + " 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0",
		}
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry(100, elsewhere, "postgres", "/usr/lib/postgresql/18/bin/postgres", "-D", data, "-p", "5433")
	entry(101, data, "postgres", "postgres: checkpointer")
	entry(102, elsewhere, "postgres", "/usr/lib/postgresql/18/bin/postgres", "-D", filepath.Join(elsewhere, "data"))
	running, err := testPGServerProcesses(proc, data)
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, p := range running {
		named = append(named, p.String())
	}
	if got, want := strings.Join(named, ", "), "pid 100 (postgres), pid 101 (postgres)"; got != want {
		t.Errorf("testPGServerProcesses named %q, want %q: the postmaster by its arguments and the backend by its working directory", got, want)
	}
}

// A step that runs out the probe's deadline after printing two lines is refused on one line, with
// what it printed joined in it.
func TestCheckTestPostgresNamesAStepThatRanOutOnOneLine(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase $1 in\nensure)\n\techo \"waiting for the server\"\n\techo \"still waiting\"\n\texec sleep 300\n\t;;\nesac\nexit 0\n"
	for _, program := range []string{testPGProgram, "psql"} {
		if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := CheckTestPostgres(ctx, filepath.Join(dir, "cluster"))
	if err == nil || !strings.Contains(err.Error(), "ensure did not finish before the probe's context ended") ||
		!strings.Contains(err.Error(), "it printed: waiting for the server; still waiting") {
		t.Fatalf("CheckTestPostgres on an ensure that never ends = %v, want the step named with what it printed", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal is %d lines, want one: %q", strings.Count(err.Error(), "\n")+1, err)
	}
}
