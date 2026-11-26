package fixtureenv_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// guardFailureChildEnv makes this package's test binary the child of
// TestMustGuardEndsATestProcessWhoseGuardFails: with it set, TestMain runs the half of MustGuard
// that a failed Guard reaches instead of the tests, so the parent can read what that child prints
// and the status it ends on.
const guardFailureChildEnv = "LEGION_FIXTUREENV_GUARD_FAILURE"

// TestMain guards the test process, as the TestMain of every package whose tests run git or jj
// does, after the branch where this binary runs as that helper child. Only TestMain makes that
// child of a copy a test starts: in a build of these tests without it, such a copy runs the tests,
// TestMustGuardEndsATestProcessWhoseGuardFails among them, which would start a copy of its own.
// That test refuses to in such a process (guardChildCopyRefusal), and starts its copy with -h, so
// it fails in such a build by name.
func TestMain(m *testing.M) {
	if os.Getenv(guardFailureChildEnv) == "1" {
		fixtureenv.RunGuardFailureAndExit()
	}
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}

// A TestMain whose Guard fails prints the failure and ends the test process with status 1, so no
// package's tests run on a process Guard did not take the pod's workspace, remote and credential
// out of. That is MustGuard's whole difference from Guard, and every guarding TestMain has it.
//
// The child gets -h, which it does not read. In a build without TestMain, the testing package reads
// -h before any test runs, prints its own flag usage and exits 0, so this test fails there by name
// and its child starts no child of its own.
func TestMustGuardEndsATestProcessWhoseGuardFails(t *testing.T) {
	if err := guardChildCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-h")
	child.Env = append(os.Environ(), guardFailureChildEnv+"=1")
	out, err := child.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("the child ended with %v, want status 1\n%s", err, out)
	}
	if string(out) != fixtureenv.ChildGuardFailureLine {
		t.Fatalf("the child printed %q, want %q", out, fixtureenv.ChildGuardFailureLine)
	}
}

// guardChildCopyRefusal is why this process may start no copy of this test binary, nil when it
// may. A process that has guardFailureChildEnv=1 was started as the guard's failing child, and
// TestMain runs such a process as that child, so one that runs the tests runs in a build without
// TestMain, and a copy started there would run the tests again.
func guardChildCopyRefusal() error {
	if os.Getenv(guardFailureChildEnv) == "1" {
		return fmt.Errorf("this process has %s=1, so a test started it as the guard's failing child, but it runs the tests: this build of the tests has no TestMain that runs the child, and a copy started here would run the tests again", guardFailureChildEnv)
	}
	return nil
}

// guardChildCopyRefusal, which TestMustGuardEndsATestProcessWhoseGuardFails asks before its copy
// starts, refuses a process that has guardFailureChildEnv=1 and no other. While TestMain is in the
// build no copy reaches it, so this test is what holds the refusal.
func TestAProcessStartedAsTheGuardsFailingChildStartsNoCopy(t *testing.T) {
	fixtureenv.Unset(t, guardFailureChildEnv)
	if err := guardChildCopyRefusal(); err != nil {
		t.Fatalf("with %s unset: %v, want no refusal", guardFailureChildEnv, err)
	}
	t.Setenv(guardFailureChildEnv, "1")
	if err := guardChildCopyRefusal(); err == nil || !strings.Contains(err.Error(), guardFailureChildEnv+"=1") {
		t.Fatalf("with %s=1: %v, want a refusal that names it", guardFailureChildEnv, err)
	}
}

// Guard takes out what a Legion issue pod hands every process — its workspace, git configuration,
// jj identity, GitHub token and SSH agent — and keeps the tests' own inputs and the names a package
// keeps. A command started with no Env of its own then inherits none of them, and is held to the
// file transport.
func TestGuardTakesOutWhatAPodSetsAndKeepsTheTestsInputs(t *testing.T) {
	t.Cleanup(func() {
		if err := fixtureenv.Guard(); err != nil {
			t.Error(err)
		}
	})
	pod := map[string]string{
		"LEGION_WORKSPACE": "/legion/workspaces/acme/widgets/widgets-42", "GIT_DIR": "/legion/repos/x/.git",
		"GIT_CONFIG_COUNT": "1", "JJ_USER": "a role", "GH_TOKEN": "a pod's token", "GITHUB_TOKEN": "a pod's token",
		"SSH_AUTH_SOCK":            "/run/agent.sock",
		"OMP_SESSION_SQL_DSN_FILE": "/legion/secrets/session-dsn", "OMP_SESSION_STORAGE": "sql",
		"OMP_SESSION_SQL_DSN": "postgres://legion@postgres.legion:5432/sessions",
	}
	inputs := map[string]string{
		"LEGION_TEST_PG_DSN": "postgres://test", "LEGION_E2E_REPO": "acme/widgets", "GITHUB_ACTIONS": "true",
		"LEGION_KEPT_BY_THE_PACKAGE": "1",
	}
	for name, value := range pod {
		t.Setenv(name, value)
	}
	for name, value := range inputs {
		t.Setenv(name, value)
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "https")
	if err := fixtureenv.Guard("LEGION_KEPT_BY_THE_PACKAGE"); err != nil {
		t.Fatal(err)
	}
	child, err := exec.Command("env").Output()
	if err != nil {
		t.Fatal(err)
	}
	inherited := strings.Split(string(child), "\n")
	for name := range pod {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("the process still holds %s after Guard", name)
		}
		if slices.ContainsFunc(inherited, func(line string) bool { return strings.HasPrefix(line, name+"=") }) {
			t.Errorf("a command with no Env inherited %s", name)
		}
	}
	for name, value := range inputs {
		if got := os.Getenv(name); got != value {
			t.Errorf("%s=%q after Guard, want the test input %q kept", name, got, value)
		}
	}
	if got := os.Getenv("GIT_ALLOW_PROTOCOL"); got != "file" {
		t.Errorf("GIT_ALLOW_PROTOCOL=%q after Guard, want file", got)
	}
	if !slices.Contains(inherited, "GIT_ALLOW_PROTOCOL=file") {
		t.Errorf("a command with no Env inherited no GIT_ALLOW_PROTOCOL=file:\n%s", child)
	}
}

// The three names this package takes out are the names that place a session — the two Oh My Pi
// reads, OMP_SESSION_STORAGE and OMP_SESSION_SQL_DSN_FILE, and OMP_SESSION_SQL_DSN, the projected
// file's own name, which carries the same URL by value — read here from the constants the rest of
// the daemon builds on, so that a rename reaching those constants and not this package fails here
// rather than silently leaving a test free to open the deployment's store. Guard, RequireGuarded
// and Isolated all read one predicate, so Isolated's slice holds for the three of them.
// internal/runtime/paneenv_test.go pins its own list the same way, for the same reason.
func TestTheSessionNamesArePinnedToTheirConstants(t *testing.T) {
	for _, name := range []string{ompsessions.StorageVariable, ompsessions.DSNFileVariable, ompsessions.DSNFileName} {
		t.Setenv(name, "a store no test may open")
		if slices.ContainsFunc(fixtureenv.Isolated(t.TempDir()), func(entry string) bool {
			key, _, _ := strings.Cut(entry, "=")
			return key == name
		}) {
			t.Errorf("Isolated holds %s, one of the names that place a session", name)
		}
	}
}

// run runs argv in dir under env and returns its trimmed output and error.
func run(dir string, env []string, argv ...string) (string, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = env
	out, err := command.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustRun(t *testing.T, dir string, env []string, argv ...string) string {
	t.Helper()
	out, err := run(dir, env, argv...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return out
}

// A process environment that names another repository (GIT_DIR, GIT_WORK_TREE), a workspace
// (LEGION_WORKSPACE) and a credential (GH_TOKEN), as a Legion issue pod's can, reaches no command
// run under Isolated: a commit in the test's own repository leaves the decoy repository as it was.
func TestIsolatedDropsTheProcesssRepositoryWorkspaceAndCredential(t *testing.T) {
	decoy := t.TempDir()
	clean := fixtureenv.Isolated(t.TempDir())
	mustRun(t, decoy, clean, fixturegit.InitWorkTree("", "main")...)
	mustRun(t, decoy, clean, "git", "commit", "--quiet", "--allow-empty", "-m", "decoy")
	before := mustRun(t, decoy, clean, "git", "rev-parse", "HEAD")

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("LEGION_WORKSPACE", decoy)
	t.Setenv("GH_TOKEN", "a pod's credential")
	t.Setenv("OMP_SESSION_SQL_DSN_FILE", "/legion/secrets/session-dsn")
	t.Setenv("OMP_SESSION_STORAGE", "sql")
	t.Setenv("OMP_SESSION_SQL_DSN", "postgres://legion@postgres.legion:5432/sessions")
	env := fixtureenv.Isolated(t.TempDir())
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains([]string{
			"GIT_DIR", "GIT_WORK_TREE", "LEGION_WORKSPACE", "GH_TOKEN",
			"OMP_SESSION_SQL_DSN_FILE", "OMP_SESSION_STORAGE", "OMP_SESSION_SQL_DSN",
		}, key) {
			t.Errorf("Isolated holds %s, want none of the process's", entry)
		}
	}
	work := t.TempDir()
	mustRun(t, work, env, fixturegit.InitWorkTree("", "main")...)
	mustRun(t, work, env, "git", "commit", "--quiet", "--allow-empty", "-m", "the test's own")
	if after := mustRun(t, decoy, clean, "git", "rev-parse", "HEAD"); after != before {
		t.Errorf("the decoy repository's HEAD moved from %s to %s, want it untouched", before, after)
	}
}

// A state home the process names, as a Legion pod names one for each role, reaches no command run
// under Isolated: Isolated and Home each hold one XDG_STATE_HOME, under the test's home. The leak
// guard caches the repositories it found public there and reads that cache before it asks GitHub,
// so on the runner's state home a test's invented repository can be answered public from what an
// earlier test cached, and a test of a visibility that cannot be read gets no refusal
// (internal/leakguard's TestAVisibilityThatCannotBeReadRefuses failed that way in a pod).
func TestIsolatedAndHomeGiveAStateHomeOfTheTestsOwn(t *testing.T) {
	runner := t.TempDir()
	t.Setenv("XDG_STATE_HOME", runner)
	home := t.TempDir()
	for name, env := range map[string][]string{"Isolated": fixtureenv.Isolated(home), "Home": fixtureenv.Home(home)} {
		var states []string
		for _, entry := range env {
			if key, value, _ := strings.Cut(entry, "="); key == "XDG_STATE_HOME" {
				states = append(states, value)
			}
		}
		if len(states) != 1 || !strings.HasPrefix(states[0], home+string(filepath.Separator)) {
			t.Errorf("%s holds XDG_STATE_HOME %q, want one value under the test's home %s, not the process's %s", name, states, home, runner)
		}
	}
}

// A repository whose own configuration allows http (protocol.http.allow=always, which outranks
// protocol.allow) still reaches no network remote under Isolated: jj git push to a loopback HTTP
// server fails and the server receives no request. Without GIT_ALLOW_PROTOCOL the same push
// reaches the server, so the refusal is the environment's. Every jj is the real jj
// (fixturegit.CompiledJJ): a jj of the machine's own first on PATH, a dev box's shim, can refuse
// the push itself, which would read as the environment's refusal.
func TestIsolatedReachesNoNetworkRemoteWhateverTheRepositoryAllows(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	env := fixtureenv.Isolated(t.TempDir())
	repo := t.TempDir()
	mustRun(t, repo, env, append([]string{jj}, fixturegit.JJInit(repo, true)[1:]...)...)
	mustRun(t, repo, env, jj, "describe", "-m", "work that must not leave the test", "-R", repo)
	mustRun(t, repo, env, jj, "bookmark", "set", "main", "-r", "@", "-R", repo)
	mustRun(t, repo, env, jj, "git", "remote", "add", "origin", server.URL+"/repo.git", "-R", repo)
	mustRun(t, repo, env, "git", "config", "protocol.http.allow", "always")

	push := []string{jj, "git", "push", "--remote", "origin", "--bookmark", "main", "-R", repo}
	if out, err := run(repo, env, push...); err == nil {
		t.Errorf("jj git push under Isolated succeeded, want a refusal:\n%s", out)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the loopback server received %d requests under Isolated, want none", n)
	}

	open := slices.DeleteFunc(slices.Clone(env), func(entry string) bool { return entry == "GIT_ALLOW_PROTOCOL=file" })
	_, _ = run(repo, open, push...)
	if requests.Load() == 0 {
		t.Fatal("the push without GIT_ALLOW_PROTOCOL never reached the loopback server, so this test proves nothing")
	}
}
