package launcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The pod's own PostgreSQL for tests, which an issue pod's launcher serves when the deployment
// sets `runtime.kubernetes.test_postgres` (Config.TestPostgres). TestPGVariable is the variable
// this launcher puts into every generation it starts (testPGEnv), and TestPGDSN its value: a
// loopback server with no password, whose user is a `createdb createrole nosuperuser` role of the
// cluster rather than its superuser, so a test that reaches it can make the roles its own suite
// needs and still cannot run programs as the server's process. The database the DSN names is that
// login's own, which `legion-testpg ensure` creates with the login as its owner, so a test may
// create a schema or a trusted extension in it; an untrusted one still needs the superuser, which
// is why `ensure` puts `vector` in template1, where every database made from it carries it.
// TestPGDirName is the directory the server's data and admin socket live in, under the roles
// directory, which every role's namespace covers (role-init), so no role can edit the cluster's
// configuration and have the launcher load it at the next start. The server's log is the one file
// of that directory a role reads: role-init binds it read-only at RoleTestPGLog under the fixed
// root, and TestPGLogVariable is the variable that names that path to every generation, beside
// the DSN.
//
// The launcher owns the server and no role does: a role's generation runs as the init of its own
// PID namespace, and the kernel kills every process in a PID namespace when its init exits, so a
// server a role's setup step started would die with that generation and take every sibling role's
// database with it.
const (
	TestPGVariable    = "LEGION_TEST_PG_DSN"
	TestPGDSN         = "postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable"
	TestPGLogVariable = "LEGION_TEST_PG_LOG"
	TestPGDirName     = "testpg"
	// testPGDirVariable is how the worker image's script is told where the cluster lives.
	testPGDirVariable = "LEGION_TESTPG_DIR"
	// testPGLogVariable is how that script is told which file is the server's log, and
	// testPGLogName is the file's name in the cluster's directory. The launcher names the file
	// rather than leaving the script its default, so the file the script and the server write is
	// the one this launcher made and role-init binds (testPGLog).
	testPGLogVariable = "LEGION_TESTPG_LOG"
	testPGLogName     = "server.log"
	// testPGProgram is that script, on the image's PATH.
	testPGProgram = "legion-testpg"
)

// testPGInterval is how often the launcher asks the script again. `ensure` returns after one
// connection through the DSN while the server serves it, so the poll costs milliseconds, and a
// postmaster that died — killed by the node under memory pressure, or crashed — is replaced
// within it, as is a start that left the server up and its role or database unmade. The
// launcher polls rather than waiting on a child because `pg_ctl` daemonises the postmaster: it is
// reparented to this process, and PID 1's reaper collects it. A test shortens it.
var testPGInterval = 15 * time.Second

// testPGDir is where the server's data and admin socket live for this launcher's roles directory.
func testPGDir(rolesDir string) string { return filepath.Join(rolesDir, TestPGDirName) }

// testPGLog is the server's log for this launcher's roles directory: the file the launcher makes
// (makeTestPGLog), the script and the server write, and role-init binds into every role's
// namespace.
func testPGLog(rolesDir string) string { return filepath.Join(testPGDir(rolesDir), testPGLogName) }

// testPGEnv is what a generation of this launcher is told about the pod's test database: when
// this launcher serves one (serves, from Config.TestPostgres), TestPGVariable is TestPGDSN of
// this binary, over whatever value the daemon's start command carried, and TestPGLogVariable is
// the path the role reads the server's log at, RoleTestPGLog under the fixed root; when it serves
// none both variables are taken out of that environment, so a role is never told of a server its
// pod does not run, nor of a log no server writes. env is the launcher's own environment with the
// start command's values over it (start's mergeEnv), so the removal drops each variable from
// either source. The daemon puts the DSN in a role's start command for an issue pod alone, and
// only while that deployment sets `runtime.kubernetes.test_postgres` (the sandbox package's
// manifest), so from the start command the removal serves a pod started before that key was
// turned on and kept since: its launcher was given no such flag, while the daemon that starts its
// roles now sends the DSN. From the launcher's own environment it serves a launcher that
// inherited the variable, as the idle rig of the daemon package's tests runs one inside a test
// process that holds LEGION_TEST_PG_DSN.
//
// It overrides the start command because the daemon and the worker image are deployed from two
// separately pinned builds, and this launcher, which comes from the image, is the process that
// runs `legion-testpg ensure` and so decides which database the server holds. The process that
// serves the database is therefore the one that names it: a role in a pod whose image predates a
// change of the DSN is told that image's own DSN, rather than a database nothing in its pod
// creates. The log's path is this launcher's alone for the same reason: the daemon sends none,
// and the role-init that binds the file there comes from the same image as this launcher.
//
// It reads whether this launcher was told to serve a database, never whether the server answers:
// the first `ensure` runs before any role starts and a failure of it is logged rather than thrown
// (serveTestPG), so a role whose server is late or dead is still told where that server is, and
// where to read why.
func testPGEnv(env []string, serves bool) []string {
	if !serves {
		return withoutEnv(env, TestPGVariable, TestPGLogVariable)
	}
	return mergeEnv(env, []string{
		TestPGVariable + "=" + TestPGDSN,
		TestPGLogVariable + "=" + filepath.Join(roleFixedRoot, RoleTestPGLog),
	})
}

// serveTestPG starts the pod's test database and keeps it running until ctx ends, and answers a
// channel closed once its ticker goroutine has returned, so a caller that ends ctx can wait for
// it rather than leave it running. The first attempt is this call's, before the launcher serves
// any role, so a role that starts at once finds the server serving its DSN; afterwards each tick
// asks again, which starts a postmaster that has died and finishes a start that left one up.
// A failure is logged and never thrown: a pod whose test database cannot start still runs its
// roles, whose agents then read the refusal in the container's log, where a launcher that failed
// its container would instead cost the issue every role it was serving.
//
// The server's log is made before that first attempt, so every role started after it has a file
// to bind even when the attempt failed before `initdb` wrote one: without it such a role would
// hold the image's empty mount point for its whole generation and see nothing of what the server
// a later tick starts logs. A log that cannot be made is logged as the script's failures are.
func serveTestPG(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard) <-chan struct{} {
	// The interval is read here, in the caller's own goroutine, and not inside the one below: it
	// is a package variable a test shortens, and a goroutine still reading it after the test
	// restored it is a data race the race detector fails the test for.
	interval := testPGInterval
	if err := makeTestPGLog(cfg.RolesDir); err != nil {
		log.linef("launcher: test database: %v", err)
	}
	ensure := func() {
		if err := runTestPG(ctx, cfg, log, guard, "ensure"); err != nil {
			log.linef("launcher: test database: %v", err)
		}
	}
	ensure()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ensure()
			}
		}
	}()
	return done
}

// makeTestPGLog makes the cluster's directory and, where there is none, the server's log in it,
// empty. A log already there is left as it is, never truncated: a launcher that restarts in a pod
// whose server has run finds that server's lines, and they are what a role reads to learn why it
// stopped. The file is the launcher's own and mode 0600: the script and the server it starts run
// as the launcher's user, and so does a role's agent outside its own namespaces (roleUID maps to
// it), which reaches the file through role-init's read-only bind alone.
func makeTestPGLog(rolesDir string) error {
	if err := os.MkdirAll(testPGDir(rolesDir), 0o755); err != nil {
		return fmt.Errorf("make the server's directory: %w", err)
	}
	file, err := os.OpenFile(testPGLog(rolesDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("make the server's log: %w", err)
	}
	return file.Close()
}

// testPGBound is how long one run of the script may take. The first `ensure` runs before the
// launcher serves any role, and the daemon's boot timeout for a pod is 120 s by default, so an
// `ensure` that hangs — a lock a dead holder still owns, a disk that does not answer — must not
// take the pod's whole boot with it. `flock` inside the script waits up to 180 s of its own, which
// this bound cuts short deliberately: a role starting without its test database and a line in the
// log is better than a pod the daemon gives up on. A test shortens it.
var testPGBound = 60 * time.Second

// runTestPG runs one command of the image's script with the DSN it must serve, the directory it
// must serve it from and the file it must log to, writing its output to the container's log
// behind a prefix of its own, so a refusal reads apart from a role's lines. Its process is held
// out of the reaper's pass (pidGuard) until this wait has read its exit. WaitDelay makes the kill
// at testPGBound final: without it a child that keeps the output pipe open — the postmaster
// `pg_ctl` leaves running — would hold this wait past the bound.
func runTestPG(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard, command string) error {
	bounded, cancel := context.WithTimeout(ctx, testPGBound)
	defer cancel()
	out, _ := log.writers(testPGProgram)
	cmd := exec.CommandContext(bounded, testPGProgram, command)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		TestPGVariable+"="+TestPGDSN,
		testPGDirVariable+"="+testPGDir(cfg.RolesDir),
		testPGLogVariable+"="+testPGLog(cfg.RolesDir),
	)
	cmd.Stdout, cmd.Stderr = out, out
	pid, err := guard.start(cmd)
	if err != nil {
		return fmt.Errorf("start %s %s: %w", testPGProgram, command, err)
	}
	defer guard.done(pid)
	if err := cmd.Wait(); err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%s %s did not finish within %s", testPGProgram, command, testPGBound)
		}
		return fmt.Errorf("%s %s: %w", testPGProgram, command, err)
	}
	return nil
}

// CheckTestPostgres proves the pod's own test database where a pod runs it, before any issue pod
// does: `legion probe-image --test-postgres <dir>`, which the daemon's probe Sandbox passes when
// the deployment sets runtime.kubernetes.test_postgres and the image build does not. The probe pod
// runs under the deployment's own runtime (gVisor on the Autopilot cluster), so a runtime the
// server cannot `initdb`, start or connect under refuses the daemon's boot probe rather than every
// pod's first Postgres test. It runs what a pod's launcher runs — check, ensure — then makes and
// drops a database through the DSN every role is told, and stops the server it started, so the
// probe pod leaves nothing running. dir is a directory of the probe's own volume.
func CheckTestPostgres(dir string) error {
	env := append(os.Environ(), TestPGVariable+"="+TestPGDSN, testPGDirVariable+"="+dir)
	run := func(name string, argv ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), testPGBound)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, argv...)
		cmd.WaitDelay = time.Second
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("the pod's own test database: %s %s: %w: %s",
				name, strings.Join(argv, " "), err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	// The database is named for the probe alone, and dropped again, so the cluster the probe
	// leaves behind holds nothing of it; the server is stopped whatever the steps answered. It is a
	// file copy of template1, as every database the module's tests make is (docs/testing.md).
	steps := [][]string{
		{testPGProgram, "check"},
		{testPGProgram, "ensure"},
		{"psql", "-X", "-q", TestPGDSN, "-c", "create database legion_probe_testpg strategy file_copy"},
		{"psql", "-X", "-q", TestPGDSN, "-c", "drop database legion_probe_testpg with (force)"},
	}
	for _, step := range steps {
		if err := run(step[0], step[1:]...); err != nil {
			_ = run(testPGProgram, "stop")
			return err
		}
	}
	return run(testPGProgram, "stop")
}
