package workflow

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/testparallel"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// workflowTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel as its first statement: each clones a database
// of its own (migratedPool) and drives an engine over it, and none changes anything the whole
// process shares (the environment, the default logger, a package variable, flags). A test that
// has to change such a thing, or that holds a wall-clock bound the parallel tests' load could make
// it miss, says why in a `Not parallel:` comment on its first line instead, and go test runs it
// alone, before the parallel ones. TestEveryTestCallsParallelOrSaysWhyNot fails a test that does
// neither. Each pool keeps the pool_max_conns LEGION_TEST_PG_DSN names, or testpool.PoolConns where
// it names none. Four concurrent tests is the CI runner's own count and stays below the test
// server's connection limit on a large dev box.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const workflowTestParallelism = 4

// TestMain sets this package's default -parallel (workflowTestParallelism), drops the migrated
// template migratedPool clones its databases from (testpg.Main) and removes the NATS containers
// the package's tests take (testnats.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(workflowTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	os.Exit(testpg.Main("legion_workflow_test", func() int { return testnats.Main(m) }))
}

// Every test of this package calls t.Parallel() as its first statement or says why not
// (workflowTestParallelism, testparallel.Require).
func TestEveryTestCallsParallelOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	testparallel.Require(t, ".")
}
