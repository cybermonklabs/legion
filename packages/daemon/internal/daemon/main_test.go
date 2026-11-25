package daemon

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/ciphertest"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// daemonTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel but those that cannot run beside the others,
// each of which says why in a `Not parallel:` comment: it changes something the whole process
// shares, it is a helper process that exits the binary, or it holds a wall-clock bound the
// parallel tests' load could make it miss (docs/testing.md says which). Those run first, one at a
// time.
// Each parallel test clones a database and boots daemons whose pools hold up to four connections
// each, and go test's default of GOMAXPROCS would run as many at once as the machine has cores:
// four on CI's runner, hundreds on a dev box, where it would open more connections than a test
// server allows (the shared one allows 483). Four is the runner's own count, the one the race
// step's measurements were taken at.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const daemonTestParallelism = 4

// processDSN is LEGION_TEST_PG_DSN as this test process started with it, after the guard: the
// server whose template testDSN clones each test's database from. A test that sets the variable to
// a server of its own differs from it, and testDSN hands that test what it set.
var processDSN string

// TestMain takes out of the process every variable that could lead the tests' real git and jj to
// a workspace, remote or credential not their own (fixtureenv.MustGuard), keeping the three the
// kept-decision property test reads: LEGION_KEPT_PROPERTY_SEED, which replays one run, and
// LEGION_KEPT_PROPERTY_RUNS and LEGION_KEPT_PROPERTY_STEPS, its two counts. CI's pull-request
// lane reduces the runs (.github/workflows/ci-job-daemon-go.yaml) and leaves the steps at the
// test's own default; the guard keeps all three, so a developer may set any of them by hand. It
// keeps the news record's model test's four the same way (TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold):
// LEGION_NEWS_MODEL_SEED, LEGION_NEWS_MODEL_RUNS, LEGION_NEWS_MODEL_STEPS and
// LEGION_NEWS_MODEL_TRACE, which prints each seed's steps. It
// drops the migrated template testDSN and isolatedOutboxPool clone their databases from
// (testpg.Main), drops the database of this binary's own that every secrets-broker rig makes its
// schema in (ciphertest.Run), removes the directory the tests' Envoy binaries were built into
// (testbuild.Main), and removes the NATS container the package's tests share (testnats.Main).
//
// It names one gh for every daemon the tests boot, LEGION_GH_PATH, this test binary: a boot
// resolves gh from the process's environment (resolveTools), and the first gh on a machine's PATH
// may be a script, which a boot refuses (RefuseScriptGH). Each test that booted a daemon on a
// workflow configuration named this same binary itself (workflowConfigOn); named once here, those
// tests can run beside each other, which a t.Setenv refuses. The tests of the resolution itself
// hand resolveTools an environment of their own (tools_test.go).
//
// The guard runs for the test process alone. This binary is also the rig's role process, which its
// launcher re-enters as TestIdleRigRoleProcess (idle_rig_pod_test.go) with the environment a role
// of an issue pod is given: LEGION_IDLE_RIG_URL names the rig to serve, and LEGION_SANDBOX,
// LEGION_ISSUE, LEGION_ROLE and LEGION_DAEMON_URL say which role of which issue it is. Guard takes
// every LEGION_* variable out, so guarding that run would leave the child naming no rig at all: it
// would return from its test, print PASS and exit 0, and the daemon would read every one of its
// roles as a process that died at once. So the child's run is told apart by that variable and
// skips the guard, keeping what its launcher handed it, which is what Guard's own doc asks for a
// helper child ("that child keeps the variables its test hands it"); the git and jj it runs on a
// rig's workspace run under the environment the rig hands it with that workspace
// (idleRigWorkspaces.env), never under its own, and it goes on into m.Run as any other run of
// this binary does, where the one test it names serves the rig. That child serves one rig and
// clones no database and builds nothing, so it takes neither template nor build directory, and
// it is told of no database either: the rig's launchers run without TestPostgres, so testPGEnv
// takes LEGION_TEST_PG_DSN out of the child.
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(daemonTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	if os.Getenv(idleRigURLVariable) != "" {
		os.Exit(testnats.Main(m))
	}
	fixtureenv.MustGuard("LEGION_KEPT_PROPERTY_SEED", "LEGION_KEPT_PROPERTY_RUNS", "LEGION_KEPT_PROPERTY_STEPS",
		"LEGION_NEWS_MODEL_SEED", "LEGION_NEWS_MODEL_RUNS", "LEGION_NEWS_MODEL_STEPS", "LEGION_NEWS_MODEL_TRACE")
	processDSN = os.Getenv("LEGION_TEST_PG_DSN")
	executable, err := os.Executable()
	if err == nil {
		err = os.Setenv("LEGION_GH_PATH", executable)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: name this test binary as every daemon's gh (LEGION_GH_PATH): %v\n", err)
		os.Exit(1)
	}
	// The idle rig's launchers read their database password from PGPASSWORD alone
	// (idleRigSessionPassword), and every rig's database is a clone on LEGION_TEST_PG_DSN's
	// server and login, so the password is named once here rather than by each rig.
	if processDSN != "" {
		cfg, err := pgconn.ParseConfig(processDSN)
		if err == nil && cfg.Password != "" {
			err = os.Setenv("PGPASSWORD", cfg.Password)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: name LEGION_TEST_PG_DSN's password as PGPASSWORD for the idle rig's launchers: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(testpg.Main("legion_daemon_test", func() int {
		return ciphertest.Run(func() int {
			return testbuild.Main(func() int { return testnats.Main(m) })
		})
	}))
}
