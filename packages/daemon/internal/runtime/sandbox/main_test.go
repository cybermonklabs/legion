package sandbox

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testparallel"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// sandboxTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel as its first statement but those that change
// something the whole process shares, or that need a step to land inside a window of the wall
// clock (each says what in a `Not parallel:` comment on its first line), which run first, one at a
// time; TestEveryTestCallsParallelOrSaysWhyNot fails a test that does neither. Each test drives
// its own fake API server, launchers and store (newRig), while the runtime's waits run on the wall
// clock, or on a fake one the test moves where it asserts which deadline a wait meets
// (g.r.clock = fc, step), so most of a test's time is a wait rather than CPU: run one at a time,
// the package kept under half of one core busy. So the default is twice CI's four cores, rather
// than go test's GOMAXPROCS, which on a dev box would run hundreds at once. go test parses its
// flags inside m.Run, so a -parallel on the command line still wins.
const sandboxTestParallelism = 8

// TestMain sets the package's default -parallel (sandboxTestParallelism), then takes out of the
// process every variable that could lead the init containers' and the roles' real git, which these
// tests run on this machine, to a repository, remote or credential not the test's own
// (fixtureenv.MustGuard), in the e2e build too, whose untagged tests are these same fixtures. The
// e2e build's live proof takes the process's own environment back for its run (fixtureenv.Live).
// It then drops the migrated template migratedStore clones its databases from (testpg.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(sandboxTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	fixtureenv.MustGuard()
	os.Exit(testpg.Main("legion_sandbox_test", m.Run))
}

// Every test of this package calls t.Parallel() as its first statement or says why not
// (sandboxTestParallelism, testparallel.Require).
func TestEveryTestCallsParallelOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	testparallel.Require(t, ".")
}
