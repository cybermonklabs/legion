package workflow

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// workflowTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel: each clones a database of its own
// (migratedPool) and drives an engine over it, and none changes anything the whole process shares
// (the environment, the default logger, a package variable, flags). Each pool holds up to
// testdb.PoolConns connections, and go test's default of GOMAXPROCS would run as many tests at
// once as the machine has cores: four on CI's runner, hundreds on a dev box, where it would open
// more connections than the test server allows. Four is the runner's own count.
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
