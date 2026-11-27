package launcher

//ci:reads packages/daemon/docker/legion-testpg pgDiskRig runs it through the link it puts on PATH, whose target is pgDiskScript (../../docker/legion-testpg) made absolute

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The free bytes and the size a case's disk check reads (testPGDiskSpace): a disk of INFRA's
// 20 GiB, with 512 MiB free under the floor and the floor itself at it.
const (
	pgDiskSize  = 20 << 30
	pgDiskUnder = 512 << 20
)

// pgDiskInitName is where, in the cluster's directory, the script's start makes a new cluster
// before it renames it to the data directory (legion-testpg's init). The launcher never names it:
// only a start, under the script's lock, touches it.
const pgDiskInitName = "data.init"

// pgDiskWait bounds a wait on one reset, whose own bound is testPGResetBound: a reset that waits
// on the lock or on a process in the cluster waits once more for its stop, its initdb and its
// start after that wait ends. It is that bound as the package starts, before any test shortens it.
var pgDiskWait = testPGResetBound

// pgDiskRig is one test's own PostgreSQL, served by the worker image's real legion-testpg from a
// short directory of the test's own (testtmp: the server binds its socket there) on a free
// loopback port of its own. testPGServedDSN points the launcher at that port for the test's life,
// so neither the launcher nor the script ever reaches the machine's shared server.
type pgDiskRig struct {
	cfg       Config
	dir       string // the cluster's directory, testPGDir(cfg.RolesDir)
	dsn       string
	port      int
	superuser string
	bin       string // the real server bin directory the script would pick
	link      string // the script's link on PATH, which every process of a run of the script names
	log       *lockedBuffer
}

// newPGDiskRig serves the test's own cluster with the script's `ensure`, as the launcher's first
// attempt does, and stops it when the test ends. It skips the test, in one line naming what is
// missing, where the machine has no server for the script to run.
func newPGDiskRig(t *testing.T) *pgDiskRig {
	t.Helper()
	path := t.TempDir()
	link := filepath.Join(path, testPGProgram)
	if err := os.Symlink(pgDiskScript(t), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path+string(os.PathListSeparator)+os.Getenv("PATH"))
	var missing []string
	for _, program := range []string{"psql", "pg_isready", "pg_config", "flock"} {
		if _, err := exec.LookPath(program); err != nil {
			missing = append(missing, program)
		}
	}
	if len(missing) > 0 {
		t.Skipf("TestPGDisk needs %s on PATH, for the real %s", strings.Join(missing, ", "), testPGProgram)
	}
	if out, err := exec.Command(testPGProgram, "check").CombinedOutput(); err != nil {
		t.Skipf("TestPGDisk needs a PostgreSQL server under /usr/lib/postgresql/*/bin (or PGBIN) with pgvector: %s check: %v: %s",
			testPGProgram, err, strings.Join(strings.Fields(string(out)), " "))
	}
	superuser, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatalf("id -un: %v", err)
	}
	root := testtmp.Dir(t, filepath.Join("roles", TestPGDirName, "sockets", ".s.PGSQL.65535.lock"))
	r := &pgDiskRig{
		cfg:       Config{RolesDir: filepath.Join(root, "roles"), TestPostgres: true, TestPostgresDisk: true},
		port:      pgDiskFreePort(t),
		superuser: strings.TrimSpace(string(superuser)),
		bin:       pgDiskServerBin(t),
		link:      link,
		log:       &lockedBuffer{},
	}
	r.dir = testPGDir(r.cfg.RolesDir)
	r.dsn = r.databaseDSN("legion_test")
	testPGServedDSN = r.dsn
	t.Cleanup(func() { testPGServedDSN = TestPGDSN })
	t.Cleanup(func() { r.stop(t) })
	if err := makeTestPGLog(r.cfg.RolesDir); err != nil {
		t.Fatal(err)
	}
	if err := runTestPG(context.Background(), r.cfg, r.roleLog(), &pidGuard{}, ensureRun()); err != nil {
		t.Fatalf("the first ensure: %v\n%s", err, r.log)
	}
	return r
}

// pgDiskScript is the worker image's legion-testpg in this repository, at the relative path from
// this package every test that reads packages/daemon/docker takes, made absolute because the
// link to it (newPGDiskRig) is read from a directory of its own.
func pgDiskScript(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "docker", testPGProgram))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the worker image's %s: %v", testPGProgram, err)
	}
	return script
}

// pgDiskServerBin is the server bin directory the script picks: PGBIN, or the newest
// /usr/lib/postgresql/<major>/bin that holds a postgres.
func pgDiskServerBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PGBIN"); bin != "" {
		return bin
	}
	dirs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	newest, major := "", -1
	for _, dir := range dirs {
		version, err := strconv.Atoi(filepath.Base(filepath.Dir(dir)))
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "postgres")); err == nil && version > major {
			newest, major = dir, version
		}
	}
	if newest == "" {
		t.Fatal("legion-testpg check passed, yet no /usr/lib/postgresql/*/bin holds a postgres")
	}
	return newest
}

// pgDiskFreePort is a loopback port nothing listens on as the test starts.
func pgDiskFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// stop ends the test's own server wherever it runs, its data directory or a cluster set aside, so
// nothing of the test outlives it. An immediate stop is enough for a cluster deleted next.
func (r *pgDiskRig) stop(t *testing.T) {
	for _, data := range []string{filepath.Join(r.dir, "data"), filepath.Join(r.dir, testPGSetAsideName)} {
		if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
			continue
		}
		out, err := exec.Command(filepath.Join(r.bin, "pg_ctl"), "--pgdata", data, "--mode", "immediate", "--wait", "--timeout", "60", "stop").CombinedOutput()
		if err != nil {
			t.Logf("stopping the test's own server in %s: %v: %s", data, err, out)
		}
	}
}

func (r *pgDiskRig) roleLog() *roleLog { return &roleLog{stdout: r.log, stderr: r.log} }

// pgDiskSpace is a disk reading of free bytes on a disk of pgDiskSize.
func pgDiskSpace(free uint64) func(string) (uint64, uint64, error) {
	return func(string) (uint64, uint64, error) { return free, pgDiskSize, nil }
}

// check runs the launcher's own disk check, the one its loop runs, with the disk reading free and
// the reset's bound testPGResetBound.
func (r *pgDiskRig) check(free uint64) {
	checkTestPGDisk(context.Background(), r.cfg, r.roleLog(), &pidGuard{}, pgDiskSpace(free), testPGResetBound)
}

// checkInBackground runs check in a goroutine of its own and answers a channel closed when it
// returns. The test waits for it before the rig stops the server.
func (r *pgDiskRig) checkInBackground(t *testing.T, free uint64) <-chan struct{} {
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		r.check(free)
	}()
	t.Cleanup(func() { <-checked })
	return checked
}

// script runs one command of the script by hand, as the launcher runs it (runTestPG), and fails
// the test with the container's log where the run fails.
func (r *pgDiskRig) script(t *testing.T, command string) {
	t.Helper()
	if err := runTestPG(context.Background(), r.cfg, r.roleLog(), &pidGuard{}, testPGRun{command: command, bound: testPGBound}); err != nil {
		t.Fatalf("%v\n%s", err, r.log)
	}
}

// psql runs sql under psql's connection arguments connection and answers what it printed: r.dsn,
// as every role's tests reach the server, or superuserSocket, which the DSN's login running out of
// connections leaves open.
func (r *pgDiskRig) psql(sql string, connection ...string) (string, error) {
	args := append([]string{"-X", "-w", "-tAq"}, connection...)
	cmd := exec.Command("psql", append(args, "-c", sql)...)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=10")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql %s -c %q: %w: %s", strings.Join(connection, " "), sql, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// superuserSocket is psql's connection arguments for the cluster's superuser over its socket.
func (r *pgDiskRig) superuserSocket() []string {
	return []string{"-h", filepath.Join(r.dir, "sockets"), "-p", strconv.Itoa(r.port), "-U", r.superuser, "-d", "postgres"}
}

// madeSQL lists the databases a case made, every one named tpg_…, by name.
const madeSQL = `select coalesce(string_agg(datname, ' ' order by datname), '') from pg_database where datname like 'tpg\_%'`

// makeDatabases makes each database through the DSN, as a test harness does, with JIT off as every
// database a test makes (testdb.WithoutJIT, docs/testing.md, "A database a Go test creates").
func (r *pgDiskRig) makeDatabases(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		for _, sql := range []string{"create database " + name + " strategy file_copy", testdb.WithoutJIT(name)} {
			if _, err := r.psql(sql, r.dsn); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// made is the databases a case made that the cluster holds, read through the DSN.
func (r *pgDiskRig) made(t *testing.T) []string {
	t.Helper()
	out, err := r.psql(madeSQL, r.dsn)
	if err != nil {
		t.Fatalf("the DSN does not answer: %v\n%s", err, r.log)
	}
	return strings.Fields(out)
}

// answers makes and drops a database through the DSN, as a test that reaches the server does, with
// JIT off as every database a test makes (testdb.WithoutJIT).
func (r *pgDiskRig) answers(t *testing.T) {
	t.Helper()
	for _, sql := range []string{"create database tpg_after strategy file_copy", testdb.WithoutJIT("tpg_after"), "drop database tpg_after with (force)"} {
		if _, err := r.psql(sql, r.dsn); err != nil {
			t.Fatalf("the DSN does not serve a database: %v\n%s", err, r.log)
		}
	}
}

// listening is whether a server accepts connections on the test's port.
func (r *pgDiskRig) listening() bool {
	return exec.Command("pg_isready", "-q", "-h", "127.0.0.1", "-p", strconv.Itoa(r.port)).Run() == nil
}

// resetLines are the reset lines in the container's log, behind the launcher's prefix.
func (r *pgDiskRig) resetLines() []string {
	var lines []string
	for _, line := range strings.Split(r.log.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, testPGProgram+": "); ok && strings.HasPrefix(rest, testPGProgram+": reset") {
			lines = append(lines, rest)
		}
	}
	return lines
}

// pgDiskExists is whether path is there.
func pgDiskExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// assertReset holds what a reset leaves: none of the databases the case made, the DSN serving a
// database made and dropped through it, nothing set aside, and one reset line, in the container's
// log and in the server's log after the new cluster's own lines. It answers that line.
func (r *pgDiskRig) assertReset(t *testing.T) string {
	t.Helper()
	if made := r.made(t); len(made) != 0 {
		t.Errorf("after the reset the cluster still holds %v, want none of the databases made before it", made)
	}
	r.answers(t)
	if aside := filepath.Join(r.dir, testPGSetAsideName); pgDiskExists(t, aside) {
		t.Errorf("after the reset %s is still there, want the old cluster deleted", aside)
	}
	lines := r.resetLines()
	if len(lines) != 1 {
		t.Fatalf("the container's log holds %d reset lines, want one:\n%s", len(lines), r.log)
	}
	server, err := os.ReadFile(testPGLog(r.cfg.RolesDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(server), lines[0]+"\n") {
		t.Errorf("the server's log holds no line %q:\n%s", lines[0], server)
	}
	if !strings.Contains(lines[0], "512.0 MiB free of 20.0 GiB") || !strings.Contains(lines[0], "floor of 2.0 GiB") {
		t.Errorf("the reset line %q does not give the disk's use, 512.0 MiB free of 20.0 GiB, and the 2.0 GiB floor", lines[0])
	}
	t.Logf("the reset's line: %s", lines[0])
	return lines[0]
}

// With fewer than 2 GiB free on the cluster's own disk, the check resets a cluster whose server
// answers: the databases a harness made and never dropped are gone, the DSN serves a new empty
// cluster, and the reset's line names the databases the old cluster held, largest first, each
// with its owner and its size, so a role reads which harness filled the disk.
func TestPGDiskUnderTheFloorResetsAServerThatAnswersAndNamesWhatItHeld(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	r.check(pgDiskUnder)
	assertHeld(t, r.assertReset(t), "tpg_one", "tpg_two", "legion_test")
}

// assertHeld holds that a reset's line names each database with its owner, legion_test, and its
// size.
func assertHeld(t *testing.T, line string, names ...string) {
	t.Helper()
	for _, name := range names {
		held := regexp.MustCompile(`\b` + name + ` \(owner legion_test, [0-9]+ (bytes|kB|MB|GB|TB)\)`)
		if !held.MatchString(line) {
			t.Errorf("the reset line %q does not name %s with its owner, legion_test, and its size", line, name)
		}
	}
}

// pgDiskNamed is the most databases a reset's line names (legion-testpg's reset_locked, most).
const pgDiskNamed = 20

// databaseDSN is the DSN's login on the test's own server, on the database name.
func (r *pgDiskRig) databaseDSN(name string) string {
	return fmt.Sprintf("postgres://legion_test@127.0.0.1:%d/%s?sslmode=disable", r.port, name)
}

// pgDiskListTimeoutVariable is the variable that sets the time limit of a reset's list of the
// databases (legion-testpg's reset_locked, list_timeout), 60 s where it is unset, as the launcher
// leaves it. Only these tests set it.
const pgDiskListTimeoutVariable = "LEGION_TESTPG_LIST_TIMEOUT"

// A cluster of thousands of databases may take long to list, and a reset that waited on its list
// would let the disk fill on. So the list runs under a time limit of its own, and a list cut by it
// ends as a list that failed: the reset still runs, and its line says the list failed. The case
// holds pg_proc locked in an open transaction of the superuser's, so the list's query waits on
// it, as a catalog read the server is slow to answer would, until its limit, 1 s here, cuts it.
func TestPGDiskAResetWhoseListIsCutByItsTimeLimitStillResetsAndSaysTheListFailed(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	t.Setenv(pgDiskListTimeoutVariable, "1s")
	args := append([]string{"-X", "-w"}, r.superuserSocket()...)
	holder := exec.Command("psql", append(args, "-c", "begin", "-c", "lock table pg_proc in access exclusive mode", "-c", "select pg_sleep(600)")...)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holderLog := &lockedBuffer{}
	holder.Stderr = holderLog
	release := pgDiskHolder(t, holder)
	defer release()
	locked := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if scanner.Text() == "LOCK TABLE" {
				locked <- true
				return
			}
		}
		locked <- false
	}()
	select {
	case ok := <-locked:
		if !ok {
			t.Fatalf("the superuser's psql ended before it locked pg_proc: %s", holderLog)
		}
	case <-time.After(time.Minute):
		t.Fatalf("the superuser's psql had not locked pg_proc after a minute: %s", holderLog)
	}
	r.check(pgDiskUnder)
	line := r.assertReset(t)
	if want := "; the server answered but the list of its databases failed, so it read none"; !strings.HasSuffix(line, want) {
		t.Errorf("the reset line %q, of a list its time limit cut, does not end %q", line, want)
	}
	if want := "canceling statement due to statement timeout"; !strings.Contains(r.log.String(), want) {
		t.Errorf("the container's log holds no %q, so the list failed by another cause than its time limit:\n%s", want, r.log)
	}
}

// A server that does not answer is reset all the same, since waiting on it would let a full disk
// crash it again and again, and the line says it read no databases.
func TestPGDiskUnderTheFloorResetsAServerThatDoesNotAnswer(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	r.script(t, "stop")
	r.check(pgDiskUnder)
	line := r.assertReset(t)
	if !strings.Contains(line, "read no databases") || strings.Contains(line, "tpg_one") {
		t.Errorf("the reset line %q, of a server that did not answer, does not say it read no databases", line)
	}
}

// At 2 GiB free or more the check is the plain `ensure` of today: it resets nothing, whether the
// server answers, is down (and is started again) or refuses the DSN's login for want of
// connections, which the check may log.
func TestPGDiskAtOrOverTheFloorChangesNothing(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	want := []string{"tpg_one", "tpg_two"}
	kept := func(t *testing.T, made []string) {
		t.Helper()
		if !slices.Equal(made, want) {
			t.Errorf("after the check the cluster holds %v, want %v kept", made, want)
		}
		if lines := r.resetLines(); len(lines) != 0 {
			t.Errorf("over the floor the check reset the cluster: %q", lines)
		}
		if aside := filepath.Join(r.dir, testPGSetAsideName); pgDiskExists(t, aside) {
			t.Errorf("over the floor the check set the cluster aside at %s", aside)
		}
	}
	t.Run("up", func(t *testing.T) {
		r.check(testPGResetFloor)
		kept(t, r.made(t))
	})
	t.Run("down", func(t *testing.T) {
		r.script(t, "stop")
		r.check(pgDiskSize)
		kept(t, r.made(t))
	})
	t.Run("out of connections", func(t *testing.T) {
		if _, err := r.psql("alter role legion_test connection limit 0", r.superuserSocket()...); err != nil {
			t.Fatal(err)
		}
		r.check(pgDiskSize)
		out, err := r.psql(madeSQL, r.superuserSocket()...)
		if err != nil {
			t.Fatal(err)
		}
		kept(t, strings.Fields(out))
		if _, err := r.psql("alter role legion_test connection limit -1", r.superuserSocket()...); err != nil {
			t.Fatal(err)
		}
		kept(t, r.made(t))
	})
}

// pgDiskHolder starts cmd in a process group of its own and answers a release that kills the
// group and waits for it, once, however often it is called.
func pgDiskHolder(t *testing.T, cmd *exec.Cmd) (release func()) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
	}
}

// A reset takes the lock a start takes, so it never runs beside one: while a start holds the
// lock, the reset waits and the databases made before it are still there, served through the DSN,
// and once the lock is released the reset runs.
func TestPGDiskAResetWaitsForTheStartLock(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	lock := filepath.Join(r.dir, ".lock")
	release := pgDiskHolder(t, exec.Command("flock", "--close", lock, "sleep", "600"))
	defer release()
	testwait.Eventually(t, "the start lock to be held", func() bool {
		var exit *exec.ExitError
		err := exec.Command("flock", "--nonblock", "--conflict-exit-code", "99", lock, "true").Run()
		return errors.As(err, &exit) && exit.ExitCode() == 99
	})
	checked := r.checkInBackground(t, pgDiskUnder)
	var changed string
	if testwait.Holds(5*time.Second, 250*time.Millisecond, func() bool {
		select {
		case <-checked:
			changed = "the check returned"
			return true
		default:
		}
		out, err := r.psql(madeSQL, r.dsn)
		if err != nil {
			changed = err.Error()
			return true
		}
		if made := strings.Fields(out); !slices.Equal(made, []string{"tpg_one", "tpg_two"}) {
			changed = fmt.Sprintf("the cluster holds %v", made)
			return true
		}
		return false
	}) {
		t.Fatalf("while a start held the lock the reset went ahead: %s\n%s", changed, r.log)
	}
	release()
	select {
	case <-checked:
	case <-time.After(pgDiskWait):
		t.Fatalf("the reset had not finished %s after the lock was released\n%s", pgDiskWait, r.log)
	}
	r.assertReset(t)
}

// A reset moves nothing a process still holds: with a process whose working directory is the
// data directory, as a backend's is, the reset stops the server and then waits, and the cluster
// stays where it is, whole, until that process is gone; then the reset runs.
func TestPGDiskAResetMovesNothingAProcessStillHolds(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	data := filepath.Join(r.dir, "data")
	holder := exec.Command("sleep", "600")
	holder.Dir = data
	release := pgDiskHolder(t, holder)
	defer release()
	checked := r.checkInBackground(t, pgDiskUnder)
	testwait.WithinPolling(t, 2*time.Minute, 100*time.Millisecond, "the reset to stop the server", func() bool { return !r.listening() })
	aside := filepath.Join(r.dir, testPGSetAsideName)
	var moved string
	if testwait.Holds(5*time.Second, 250*time.Millisecond, func() bool {
		select {
		case <-checked:
			moved = "the check returned"
			return true
		default:
		}
		switch {
		case !pgDiskExists(t, filepath.Join(data, "PG_VERSION")):
			moved = data + " holds no PG_VERSION"
		case pgDiskExists(t, aside):
			moved = aside + " is there"
		default:
			return false
		}
		return true
	}) {
		t.Fatalf("with a process in %s the reset moved the cluster: %s\n%s", data, moved, r.log)
	}
	release()
	select {
	case <-checked:
	case <-time.After(pgDiskWait):
		t.Fatalf("the reset had not finished %s after the process left the data directory\n%s", pgDiskWait, r.log)
	}
	r.assertReset(t)
}

// binWithInitdb is a server bin directory of the test's own, for PGBIN: initdb is the script
// given, and every other program a link to the real one (r.bin).
func (r *pgDiskRig) binWithInitdb(t *testing.T, initdb string) string {
	t.Helper()
	bin := t.TempDir()
	programs, err := os.ReadDir(r.bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, program := range programs {
		if program.Name() != "initdb" {
			if err := os.Symlink(filepath.Join(r.bin, program.Name()), filepath.Join(bin, program.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "initdb"), []byte(initdb), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// running is every process whose command line names the cluster's directory or the script's link
// on PATH, by pid, with that command line: what of the test's own still runs, a run of the script
// or its server. A process that ends while it is read, or whose command line is empty, as a
// zombie's is, is not running.
func (r *pgDiskRig) running(t *testing.T) map[int]string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	running := map[int]string{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if strings.Contains(args, r.dir) || strings.Contains(args, r.link) {
			running[pid] = args
		}
	}
	return running
}

// assertCutAfterTheRename holds what a reset cut after its rename, at its initdb, leaves: the old
// cluster set aside whole, no cluster in the data directory, and the reset's one line in the
// container's log, which the reset writes before its rename, so that a reset whose new cluster
// never starts still says what the old one held. It answers that line.
func (r *pgDiskRig) assertCutAfterTheRename(t *testing.T) string {
	t.Helper()
	aside := filepath.Join(r.dir, testPGSetAsideName)
	if !pgDiskExists(t, filepath.Join(aside, "PG_VERSION")) || !pgDiskExists(t, filepath.Join(aside, "base")) {
		t.Fatalf("the cut reset left no whole cluster at %s\n%s", aside, r.log)
	}
	if data := filepath.Join(r.dir, "data"); pgDiskExists(t, filepath.Join(data, "PG_VERSION")) {
		t.Fatalf("the cut reset left a PG_VERSION in %s, past which no later start makes a cluster\n%s", data, r.log)
	}
	lines := r.resetLines()
	if len(lines) != 1 {
		t.Fatalf("the container's log holds %d reset lines after the cut reset, want the one it writes before its rename:\n%s", len(lines), r.log)
	}
	return lines[0]
}

// assertNextCheckServesAnEmptyCluster runs the check that follows a cut reset, over the floor and
// with the real PGBIN, which finds no server and so starts one, and holds what it leaves: the old
// cluster set aside deleted, as the container's log says, no part of a cluster left where a start
// makes one, and an empty cluster served on the DSN.
func (r *pgDiskRig) assertNextCheckServesAnEmptyCluster(t *testing.T) {
	t.Helper()
	t.Setenv("PGBIN", r.bin)
	r.check(pgDiskSize)
	aside := filepath.Join(r.dir, testPGSetAsideName)
	if said := r.log.String(); !strings.Contains(said, "launcher: test database: deleted "+aside+",") {
		t.Errorf("the check that followed the cut does not say it deleted %s:\n%s", aside, said)
	}
	if pgDiskExists(t, aside) {
		t.Errorf("the check that followed the cut left %s", aside)
	}
	if init := filepath.Join(r.dir, pgDiskInitName); pgDiskExists(t, init) {
		t.Errorf("the check that followed the cut left %s", init)
	}
	if made := r.made(t); len(made) != 0 {
		t.Errorf("after the cut and a check the cluster holds %v, want an empty one", made)
	}
	r.answers(t)
}

// A reset cut right after its rename, before its initdb — by its bound, a restart of the roles
// container or a kill — leaves the old cluster set aside whole and no cluster in the data
// directory, and the next check, over the floor or under it, deletes what was set aside and
// serves an empty cluster on the DSN. The cut is an initdb that kills the reset's own shell.
func TestPGDiskACheckAfterAResetCutAtItsRenameDeletesTheOldClusterAndServesAnEmptyOne(t *testing.T) {
	// Not parallel: it sets PATH and PGBIN and writes the variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	initdb := "#!/bin/sh\n# The reset's own shell runs initdb: killing it here cuts the reset right after its rename.\nkill -KILL \"$PPID\"\nexit 1\n"
	t.Setenv("PGBIN", r.binWithInitdb(t, initdb))
	r.check(pgDiskUnder)
	assertHeld(t, r.assertCutAfterTheRename(t), "tpg_one", "tpg_two")
	r.assertNextCheckServesAnEmptyCluster(t)
}

// A reset cut in the middle of its initdb, after initdb wrote PG_VERSION, which it does early, and
// before it made the rest of the cluster, leaves that part in data.init and no cluster in the data
// directory, and the next check that starts a server, here a plain ensure that finds none, deletes
// the part, makes a whole cluster and serves it empty on the DSN. An initdb run in the data
// directory itself would leave there a PG_VERSION that no later start makes a cluster past, and
// the pod's test database down for the pod's life. The cut is an initdb that runs the real one on
// the same arguments, waits until it has written PG_VERSION in the directory --pgdata names, and
// then kills it and the reset's own shell, which runs initdb.
func TestPGDiskAResetCutDuringItsInitdbLeavesNoClusterAndTheNextCheckServesAnEmptyOne(t *testing.T) {
	// Not parallel: it sets PATH and PGBIN and writes the variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	initdb := "#!/bin/sh\n" +
		"pgdata= previous=\n" +
		"for arg in \"$@\"; do\n" +
		"\t[ \"$previous\" != --pgdata ] || pgdata=$arg\n" +
		"\tprevious=$arg\n" +
		"done\n" +
		"'" + filepath.Join(r.bin, "initdb") + "' \"$@\" &\n" +
		"real=$!\n" +
		"while [ ! -f \"$pgdata/PG_VERSION\" ]; do\n" +
		"\tkill -0 \"$real\" 2> /dev/null || exit 1\n" +
		"\tsleep 0.01\n" +
		"done\n" +
		"kill -KILL \"$real\" \"$PPID\"\n" +
		"exit 1\n"
	t.Setenv("PGBIN", r.binWithInitdb(t, initdb))
	r.check(pgDiskUnder)
	assertHeld(t, r.assertCutAfterTheRename(t), "tpg_one", "tpg_two")
	if init := filepath.Join(r.dir, pgDiskInitName); !pgDiskExists(t, filepath.Join(init, "PG_VERSION")) {
		t.Fatalf("the cut initdb left no PG_VERSION in %s, so the cut did not come after initdb wrote it\n%s", init, r.log)
	}
	r.assertNextCheckServesAnEmptyCluster(t)
}

// A reset its bound cuts ends whole. The script runs the reset as `flock … "$0" reset-locked`, so
// a bound that killed the script's own process alone would leave flock and reset-locked holding
// the cluster's lock and working on the cluster after the check returned. Cut by its bound at an
// initdb that outlasts it, the check returns at the bound, no process of the reset is left, the
// lock is free at once, and the next check deletes the old cluster the reset set aside and serves
// an empty one on the DSN.
func TestPGDiskAResetCutByItsBoundLeavesNoProcessAndTheNextCheckServesAnEmptyCluster(t *testing.T) {
	// Not parallel: it sets PATH, PGBIN, testPGServedDSN (newPGDiskRig) and testPGResetBound.
	r := newPGDiskRig(t)
	r.makeDatabases(t, "tpg_one", "tpg_two")
	// The server is stopped first, so the reset reaches its initdb with no stop of its own to wait
	// on, well inside the bound.
	r.script(t, "stop")
	bound := testPGResetBound
	testPGResetBound = 5 * time.Second
	t.Cleanup(func() { testPGResetBound = bound })
	// The initdb sleeps a second at a time for ten minutes, so the process that outlasts the bound
	// is always this shell, whose command line names the cluster's directory, and a kill of it
	// leaves no long sleep behind.
	t.Setenv("PGBIN", r.binWithInitdb(t, "#!/bin/sh\ni=0\nwhile [ \"$i\" -lt 600 ]; do sleep 1; i=$((i + 1)); done\n"))
	started := time.Now()
	r.check(pgDiskUnder)
	took := time.Since(started)
	if cut := fmt.Sprintf("%s reset did not finish within %s", testPGProgram, testPGResetBound); !strings.Contains(r.log.String(), cut) {
		t.Fatalf("the check returned after %s and its log does not say %q\n%s", took, cut, r.log)
	}
	if took > testPGResetBound+10*time.Second {
		t.Errorf("the check returned %s after it started, want it at the reset's bound of %s", took, testPGResetBound)
	}
	// A SIGKILL ends each process as the kernel next runs it, so their exits get a moment; a
	// process the kill missed would go on for the initdb's ten minutes. One that is left is killed
	// here, so a failing run leaves nothing of its own behind.
	var left map[int]string
	if !testwait.Holds(5*time.Second, 100*time.Millisecond, func() bool {
		left = r.running(t)
		return len(left) == 0
	}) {
		for pid := range left {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("after its bound cut the reset, these of its processes still run: %v\n%s", left, r.log)
	}
	lock := filepath.Join(r.dir, ".lock")
	if out, err := exec.Command("flock", "--nonblock", lock, "true").CombinedOutput(); err != nil {
		t.Fatalf("after its bound cut the reset, %s cannot be taken at once: %v: %s", lock, err, out)
	}
	r.assertCutAfterTheRename(t)
	r.assertNextCheckServesAnEmptyCluster(t)
}

// stubPGDisk puts a `legion-testpg` on PATH that appends one line per call — the subcommand and
// the free bytes, the size and the floor it was told, or `unset` — to a file, and answers 0.
func stubPGDisk(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s %s %s %s\\n' \"$1\" \"${" + testPGFreeVariable + "-unset}\" \"${" + testPGSizeVariable + "-unset}\" \"${" +
		testPGFloorVariable + "-unset}\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// callLines are the whole lines of the stub's calls so far.
func callLines(calls string) []string {
	got, _ := os.ReadFile(calls)
	lines := strings.SplitAfter(string(got), "\n")
	return slices.DeleteFunc(lines, func(line string) bool { return !strings.HasSuffix(line, "\n") })
}

// Where the cluster is on a disk of its own, the launcher's first attempt is still the plain
// `ensure` its roles wait on, and the loop then runs the disk check at once and at every tick:
// under the floor each one resets, told the free bytes, the disk's size and the floor, and the
// first deletes a cluster a cut reset set aside.
func TestPGDiskTheLoopChecksTheDiskAtOnceAndAtEveryTick(t *testing.T) {
	// Not parallel: it sets PATH (stubPGDisk) and writes testPGInterval and testPGDiskSpace.
	calls := stubPGDisk(t)
	testPGInterval = 20 * time.Millisecond
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	space := testPGDiskSpace
	testPGDiskSpace = pgDiskSpace(1 << 30)
	t.Cleanup(func() { testPGDiskSpace = space })
	rolesDir := t.TempDir()
	aside := filepath.Join(testPGDir(rolesDir), testPGSetAsideName)
	if err := os.MkdirAll(filepath.Join(aside, "base"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	// The ticker goroutine is waited for before the cleanups above restore the interval and the
	// disk's reader, so no read of either outlives this test.
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true, TestPostgresDisk: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	if first := callLines(calls); len(first) == 0 || first[0] != "ensure unset unset unset\n" {
		t.Fatalf("serveTestPG returned with the calls %q, want the plain ensure first", first)
	}
	reset := fmt.Sprintf("reset %d %d %d\n", 1<<30, pgDiskSize, testPGResetFloor)
	testwait.Eventually(t, "two resets", func() bool { return len(callLines(calls)) >= 3 })
	if later := callLines(calls)[1:]; slices.ContainsFunc(later, func(line string) bool { return line != reset }) {
		t.Errorf("the loop's calls are %q, want every one %q", later, reset)
	}
	if pgDiskExists(t, aside) {
		t.Errorf("the loop's first check left %s", aside)
	}
	if said := log.String(); !strings.Contains(said, "launcher: test database: deleted "+aside+",") {
		t.Errorf("the container log holds %q, want the deletion of %s named", said, aside)
	}
}

// A disk the check cannot read is logged, and that check runs the plain `ensure`: a reading that
// fails never resets a cluster, and never leaves a dead server unstarted.
func TestPGDiskACheckThatCannotReadTheDiskLogsAndEnsures(t *testing.T) {
	// Not parallel: it sets the environment variable PATH (stubPGDisk).
	calls := stubPGDisk(t)
	rolesDir := t.TempDir()
	if err := makeTestPGLog(rolesDir); err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	unread := func(string) (uint64, uint64, error) { return 0, 0, syscall.EIO }
	checkTestPGDisk(context.Background(), Config{RolesDir: rolesDir, TestPostgres: true, TestPostgresDisk: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{}, unread, testPGResetBound)
	if got := callLines(calls); !slices.Equal(got, []string{"ensure unset unset unset\n"}) {
		t.Errorf("the check ran %q, want the plain ensure alone", got)
	}
	if said := log.String(); !strings.Contains(said, "launcher: test database: read the bytes free") || !strings.Contains(said, syscall.EIO.Error()) {
		t.Errorf("the container log holds %q, want the failed reading named", said)
	}
}
