package ghroute

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/testparallel"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ghrouteTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel as its first statement but those that change
// something the whole process shares, rewrite a fixture tests beside them read
// (TestCaptureGhHelpWalk's saved help), or hold code to a wall-clock bound that tests beside them
// could use up (each says which in a `Not parallel:` comment on its first line); those run first,
// one at a time. TestEveryTestCallsParallelOrSaysWhyNot fails a test that does neither. The tests
// wait more than they compute — on the git and the stand-in gh scripts they start — so they run 8
// at once, as cmd/legion's cmdTestParallelism does, where go test's default of GOMAXPROCS would
// run hundreds at once on a dev box. A -parallel on the command line still wins only because
// TestMain sets this default before testwait.BinaryDeadline parses the command line
// (flag.Parse): the set must stay above that call, or this default would replace a -parallel the
// command line names.
const ghrouteTestParallelism = 8

// TestMain first takes out of the test process every variable that could lead a git to a
// workspace, repository, remote or credential not the test's own (fixtureenv.MustGuard,
// docs/testing.md, "A test that starts jj or git inherits no pod's environment"): this package's
// code reads a working directory's remotes and configuration with git (ReadGitRemotes), and its
// tests run that git in fixtures of their own, so in a Legion issue pod an unguarded run would read
// the pod's live workspace through the GIT_DIR its environment carries. LEGION_GH_PATH is kept: it
// names the real gh TestCaptureGhHelpWalk saves the help of, a binary and no credential, as
// cmd/legion's TestMain keeps it. TestMain then sets the package's default -parallel
// (ghrouteTestParallelism).
//
// Last, it gives every bound registered with programbound, this package's help read's
// (helpTimeout) and git reads' (GitReadBound) among them, what is left of this test binary's
// deadline (testwait.BinaryDeadline) less 30 s when its wait starts, as cmd/legion's TestMain does
// for `legion gh`: the help-read rows run ReadHelp on stand-in gh scripts, and one whose read a
// loaded machine made outlast the product's 30 s would read that stop in place of the one the row
// makes. A program that never answers still fails its row by name before go test's alarm. Under
// -timeout 0, which gives no deadline, the product's bounds hold.
func TestMain(m *testing.M) {
	fixtureenv.MustGuard("LEGION_GH_PATH")
	if err := flag.Set("test.parallel", strconv.Itoa(ghrouteTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	if deadline, ok := testwait.BinaryDeadline(); ok {
		programbound.SetDeadline(deadline)
	}
	os.Exit(m.Run())
}

// Every test of this package calls t.Parallel() as its first statement or says why not
// (ghrouteTestParallelism, testparallel.Require).
func TestEveryTestCallsParallelOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	testparallel.Require(t, ".")
}
