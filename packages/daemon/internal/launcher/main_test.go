package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// helperChildVariables are the markers this package's helper children are re-executed with: the
// test binary runs as a launcher's child (TestLauncherChild), as a launcher of its own
// (TestLauncherProcess, TestLauncherPIDOneHelper), as PID 1 of a PID namespace of its own
// (runAsPIDOne's helpers), as a generation that waits for a SIGTERM
// (TestLauncherTermDialer), as role-init in a role's namespaces (TestRoleInitHelper and the
// helpers of roleinit_test.go, which register their own markers here), and as a role's shim,
// agent and agent's child under that role-init (sessionPeerShim, TestSessionPeerStandInHelper).
// Each reads its instructions from a LEGION_ variable its test hands it, and Guard takes every
// LEGION_ variable out, so a process that is one of these children runs no Guard of its own: it
// was started by a guarded test process, whose environment it inherits, so it holds nothing of
// the pod's either.
var helperChildVariables = []string{
	launcherChildEnv, launcherProcessEnv, launcherTermDialerEnv, namespaceHelperEnv, pidOneHelperEnv,
	roleInitEnv, roleProbeEnv, peerPlanEnv, peerStandInEnv,
}

// mainHelpers are this package's helper children that no test can re-enter: one started as the
// controller's shim (`<test binary> worker-shim …`) or as Oh My Pi's eval kernel (`<a copy>
// __omp_worker_js_eval_process`) takes the arguments of the process it stands for, and its first
// argument is no -test flag, so m.Run would refuse it or run every test of the package. Each is
// keyed by its LEGION_ marker, and TestMain runs the one whose marker is set before anything else
// and exits; the file whose helper it is registers it, as roleinit_test.go registers its markers
// in helperChildVariables.
var mainHelpers = map[string]func(){mainHelperProbeEnv: mainHelperProbe}

// mainHelperProbeEnv marks the copy TestACopyStartedAsAMainHelperRunsAsTheHelper starts, which
// TestMain runs as mainHelperProbe; mainHelperProbeLine is all that helper prints.
const (
	mainHelperProbeEnv  = "LEGION_LAUNCHER_TEST_MAIN_HELPER_PROBE"
	mainHelperProbeLine = "the main helper probe ran\n"
)

func mainHelperProbe() { fmt.Print(mainHelperProbeLine) }

// guardKeep is what TestMain's guard keeps beyond fixtureenv's own test inputs: the inputs of a
// gated live test, which a person or a measurement Job sets for that test alone and none of which
// names a workspace, a remote or a credential. Each is a LEGION_ variable, which Guard would
// otherwise take out before the test reads it, so the test would skip wherever it was asked to
// run. The file whose test reads them registers them, as it registers its helpers in mainHelpers
// (memoryguard_live_test.go).
var guardKeep []string

// TestMain takes out of the process every variable that could lead git or jj to a workspace,
// remote or credential not the test's own (fixtureenv.Guard). The tests here run both, and the
// launcher starts every generation under the process's own environment merged with the start's
// (mergeEnv), which is where the pod's git lock sweep reads the workspace whose clone it sweeps
// (envValue(env, "LEGION_WORKSPACE") in manager.start, gitLockSweep.learn). In a Legion issue pod
// that variable names the pod's live workspace, so without this a test whose start named no
// workspace of its own would have the sweep remove git locks from the clone the pod's own roles
// work in, under the first pass's rule, which removes every lock no process it can see holds,
// whatever its age (docs/testing.md, "A test that starts jj or git inherits no pod's
// environment"). It keeps guardKeep, the gated live tests' own inputs.
//
// It takes out the two variables that name a session store as well, which Guard leaves: Guard
// takes out the LEGION_, GIT_, JJ_, GH_ and GITHUB_ variables and not these. A start's
// environment is mergeEnv(os.Environ(), command.Env) and a resume looks its session up through
// newSessionDial on that environment, so in a Legion issue pod, where OMP_SESSION_SQL_DSN_FILE
// and OMP_SESSION_STORAGE name the deployment's shared sessions database, a test of this package
// would connect to and query the store every agent of the deployment reads and writes. Each test
// here that needs a session database names its own (testdb.DSNFile).
//
// The shim stand-in of the session peer test is this binary run as `<binary> worker-shim`, the
// argv a shim has (roleShim). The test framework would read that argument as no test flag and run
// every test, so TestMain runs the stand-in before anything else, and only in a process that
// carries the test's marker (sessionPeerShim).
//
// Only TestMain makes a helper of a copy a test starts, a mainHelpers helper or the shim stand-in:
// in a build of these tests without it, such a copy runs the tests, each of which starts copies
// that run them again. Each test that starts such a copy asks helperCopyRefusal first, and
// TestACopyStartedAsAMainHelperRunsAsTheHelper fails in such a build by name.
func TestMain(m *testing.M) {
	for name, helper := range mainHelpers {
		if os.Getenv(name) != "" {
			helper()
			os.Exit(0)
		}
	}
	if startedAsThePeerShim() {
		os.Exit(sessionPeerShim())
	}
	for _, name := range helperChildVariables {
		if os.Getenv(name) != "" {
			os.Exit(m.Run())
		}
	}
	fixtureenv.MustGuard(guardKeep...)
	for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// startedAsThePeerShim says whether this process was started as the session peer test's shim
// stand-in: `worker-shim` as its first argument, with peerPlanEnv set.
func startedAsThePeerShim() bool {
	return len(os.Args) > 1 && os.Args[1] == shimArgument && os.Getenv(peerPlanEnv) != ""
}

// helperCopyRefusal is why this process may not start a copy that TestMain runs as one of its
// helpers, a mainHelpers helper or the session peer's shim stand-in, nil when it may. TestMain runs
// a process started as such a helper as that helper, so a test that runs in one runs in a build
// without TestMain, and a copy started there would run the tests again. It answers every helper's
// marker, not only the one the copy gets, since a copy of one kind could start a copy of another.
func helperCopyRefusal() error {
	for name := range mainHelpers {
		if os.Getenv(name) != "" {
			return fmt.Errorf("this process has %s set, so a test started it as one of TestMain's helpers, but it runs the tests: this build of the tests has no TestMain that runs the helper, and a copy started here would run the tests again", name)
		}
	}
	if startedAsThePeerShim() {
		return fmt.Errorf("this process is `%s` with %s set, so a test started it as the session peer's shim stand-in, but it runs the tests: this build of the tests has no TestMain that runs the stand-in, and a copy started here would run the tests again", shimArgument, peerPlanEnv)
	}
	return nil
}

// helperCopyRefusal, which each start of a helper's copy asks first, refuses a process that has a
// main helper's marker, and one started as `worker-shim` with peerPlanEnv set, and no other. While
// TestMain is in the build no copy reaches it, so this test is what holds the refusal. It sets
// os.Args, so it runs alone (no t.Parallel) and puts them back when it ends.
func TestAProcessStartedAsAHelperStartsNoCopy(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	fixtureenv.Unset(t, mainHelperProbeEnv, peerPlanEnv)
	if err := helperCopyRefusal(); err != nil {
		t.Fatalf("with no helper's marker: %v, want no refusal", err)
	}
	t.Setenv(mainHelperProbeEnv, "1")
	if err := helperCopyRefusal(); err == nil || !strings.Contains(err.Error(), mainHelperProbeEnv) {
		t.Fatalf("with %s=1: %v, want a refusal that names it", mainHelperProbeEnv, err)
	}
	fixtureenv.Unset(t, mainHelperProbeEnv)
	os.Args = []string{args[0], shimArgument}
	if err := helperCopyRefusal(); err != nil {
		t.Fatalf("as `%s` with no %s: %v, want no refusal", shimArgument, peerPlanEnv, err)
	}
	t.Setenv(peerPlanEnv, "{}")
	if err := helperCopyRefusal(); err == nil || !strings.Contains(err.Error(), peerPlanEnv) {
		t.Fatalf("as `%s` with %s set: %v, want a refusal that names it", shimArgument, peerPlanEnv, err)
	}
	os.Args = []string{args[0], "-test.run=^TestRoleInitAgentHelper$"}
	if err := helperCopyRefusal(); err != nil {
		t.Fatalf("with %s set and no `%s` argument: %v, want no refusal", peerPlanEnv, shimArgument, err)
	}
}

// A copy started with a main helper's marker runs as that helper: mainHelperProbe prints its line.
// The probe does not read -h. In a build without TestMain, the testing package reads -h before any
// test runs, prints its own flag usage and exits, so this test fails there by name and its copy
// starts no copy of its own. The session peer's shim stand-in takes `worker-shim` as its first
// argument, ahead of any -h, so no copy of that kind can be checked this way.
func TestACopyStartedAsAMainHelperRunsAsTheHelper(t *testing.T) {
	t.Parallel()
	if err := helperCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-h")
	cmd.Env = append(os.Environ(), mainHelperProbeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != mainHelperProbeLine {
		t.Fatalf("a copy started as a main helper with -h: %v, printed %q, want exit 0 and %q", err, out, mainHelperProbeLine)
	}
}

// reentry is the arguments that run this test binary's helper test alone, in a process of its own
// the test starts: the -test.run of that one test, and a -test.timeout that ends reentryMargin
// before this process's own deadline. The child's own waits end 30 s before that (beforeDeadline)
// and its cleanups 10 s before (cleanupBeforeDeadline), so a child whose fact never comes fails
// with its own text, and its parent prints that text, before the parent's own timeout. Under
// -timeout 0 this process has no deadline, and the child is given none either.
//
// So a test waits for a child it re-entered to end with no bound of its own (cmd.Wait,
// CombinedOutput), or with one that ends after the child's own deadline (runAsPIDOne's ends 30 s
// after its helper's): the child's -test.timeout ends it, its alarm's goroutine dump in its output,
// and each child of its own that is re-entered ends before it, so its failure, its cleanups and its
// alarm all reach the test while the test still waits. A bound of the test's own there,
// beforeDeadline say, would give up on the child 25 s before the child's own end and kill it
// without its report. A wait for something the child does short of its end is an ordinary hang
// guard: the child's own waits end before it, and the case's cleanup ends the child. What the child
// starts that is not re-entered ends with it only where the child sees to that: a PID namespace the
// child is PID 1 of, or a parent-death signal, as the role stage's sleep outside the role has.
//
// A child started with too little time is still started, with what is left (reentryUntil), and
// fails at its first wait bounded by beforeDeadline, on a line that names its deadline; a child
// with no such wait that runs out of time ends at its own alarm, whose panic names its timeout. So
// no starter checks the time up front: what a child with little time does depends on whether it
// waits at all.
func reentry(t *testing.T, helper string) []string {
	return reentryUntil(reentryEnd(t), helper)
}

// reentryMargin is how much of its deadline a test keeps back from a child it re-enters: the time
// it needs to read the child's exit and print what the child said. It is a few seconds rather than
// beforeDeadline's 30 s, because the child's own waits and cleanups already end before its deadline,
// so each level of nesting costs 5 s, and a case of a few seconds whose children re-enter twice
// still runs under a -timeout of a minute or so.
const reentryMargin = 5 * time.Second

// reentryEnd is when a child this test starts now must end (reentry): reentryMargin before this
// process's deadline, or the zero time under -timeout 0. A child that the launcher starts by an
// argv the test does not write, and that has no test of its own to read it from, is handed it in
// its plan (peerPlan.End).
func reentryEnd(t *testing.T) time.Time {
	deadline, ok := t.Deadline()
	if !ok {
		return time.Time{}
	}
	return deadline.Add(-reentryMargin)
}

// reentryUntil is reentry for a child that must end by end, the zero time for no end. The timeout
// is whole seconds, so no command line of the package's gains digits a test reads /proc for (the
// role stage's detached sleep), and one second at least, so a child started at or after end ends
// at once rather than with no deadline, which a -test.timeout of 0 would be. The session peer's
// shim calls it with the end its plan carries.
func reentryUntil(end time.Time, helper string) []string {
	args := []string{"-test.run=^" + helper + "$"}
	if end.IsZero() {
		return args
	}
	return append(args, "-test.timeout="+max(time.Until(end).Truncate(time.Second), time.Second).String())
}

// exitHelper ends a helper test that runs as a process of its own: with os.Exit(0) when it passed,
// so the test framework writes nothing more on an output a starter may read as a protocol (the
// deleter's replies), and through t.FailNow when it failed. A re-entered helper runs without
// -test.v, so the framework keeps its t.Error lines until the test ends, and os.Exit would end the
// process with exit 1 and drop them; FailNow ends the test, and the framework writes its
// `--- FAIL` and those lines on the helper's output, which its starter prints with the rest.
func exitHelper(t *testing.T) {
	if t.Failed() {
		t.FailNow()
	}
	os.Exit(0)
}

// No test of this package can reach the workspace of the pod it runs in. The first two cases fail
// without a Guard in a Legion issue pod, where LEGION_WORKSPACE names the pod's live workspace,
// and name the workspace whose clone a git lock sweep of this package's tests would have swept.
// The third holds whether or not a Guard ran: it checks only that fixtureenv.Isolated, which the
// git and jj fixtures of the sweep's tests build their environment from (fixturegit.Isolated),
// drops a workspace the process holds.
func TestNoTestOfThisPackageCanReachThePodsWorkspace(t *testing.T) {
	// The test process: Guard ran, and nothing it takes out is left — no LEGION_, no GIT_ but
	// GIT_ALLOW_PROTOCOL=file, no JJ_ (fixtureenv.RequireGuarded), so neither this package's code
	// under test nor a child it starts with no environment of its own can read the pod's.
	t.Run("the test process holds no workspace of the pod's", func(t *testing.T) {
		fixtureenv.RequireGuarded(t)
		if dir := os.Getenv("LEGION_WORKSPACE"); dir != "" {
			t.Errorf("the test process holds LEGION_WORKSPACE=%s, the workspace a sweep would work", dir)
		}
	})
	// The environment a generation is started under, which is where the sweep reads the workspace
	// it works (gitLockSweep.learn): a start that names no workspace of its own must reach a
	// sweep that has none at all. RequireGuarded holds the process's variables; this holds the
	// launcher's own merge of them with the start's.
	t.Run("a generation a start names no workspace in reaches none", func(t *testing.T) {
		started := mergeEnv(os.Environ(), []string{"TOLD=" + t.TempDir()})
		if dir := envValue(started, "LEGION_WORKSPACE"); dir != "" {
			t.Errorf("a generation started by a test here would have the sweep work %s, the workspace of the process that runs the tests", dir)
		}
	})
	// fixtureenv.Isolated, which fixturegit.Isolated builds its environment from, drops a
	// workspace the process holds whether or not a Guard ran. This reads Isolated itself. The
	// process is handed a workspace of this case's own to read back, so what passes is Isolated's
	// doing and not an environment that held none anyway.
	t.Run("a fixture's environment carries no workspace the process holds", func(t *testing.T) {
		decoy := filepath.Join(t.TempDir(), "not-the-pods-workspace")
		t.Setenv("LEGION_WORKSPACE", decoy)
		if got := envValue(os.Environ(), "LEGION_WORKSPACE"); got != decoy {
			t.Fatalf("this case holds LEGION_WORKSPACE=%q, want the decoy %s it set: it proves nothing of Isolated", got, decoy)
		}
		if dir := envValue(fixtureenv.Isolated(t.TempDir()), "LEGION_WORKSPACE"); dir != "" {
			t.Errorf("a fixture of this package would run its git and jj under LEGION_WORKSPACE=%s", dir)
		}
	})
}

// No test of this package can reach the session store of the pod it runs in. Both cases fail
// without TestMain's unset in a Legion issue pod, where OMP_SESSION_SQL_DSN_FILE and
// OMP_SESSION_STORAGE name the deployment's shared sessions database, and name the store a
// resume of this package's tests would have queried. A test that wants a session database names
// its own with testdb.DSNFile, in the start's own environment.
func TestNoTestOfThisPackageCanReachThePodsSessionStore(t *testing.T) {
	// The test process: TestMain unset both, so neither this package's code under test nor a
	// child it starts with no environment of its own can read the pod's store.
	t.Run("the test process holds no session store of the pod's", func(t *testing.T) {
		for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
			if value := os.Getenv(name); value != "" {
				t.Errorf("the test process holds %s=%s, the session store a resume would query", name, value)
			}
		}
	})
	// The environment a generation is started under, which is where a resume reads the store it
	// looks its session up in (newSessionDial): a start that names no store of its own must reach
	// a lookup that has none at all.
	t.Run("a generation a start names no session store in reaches none", func(t *testing.T) {
		started := mergeEnv(os.Environ(), []string{"TOLD=" + t.TempDir()})
		for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
			if value := envValue(started, name); value != "" {
				t.Errorf("a generation started by a test here would resume against %s=%s, the session store of the deployment the test process runs in", name, value)
			}
		}
	})
}
