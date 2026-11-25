package intake

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// intakeTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel but those whose assertion holds only inside a
// wall-clock margin that other tests' load can eat (each says which in a `Not parallel:` comment),
// which run first, one at a time. Each parallel test clones a database and holds a NATS server of
// testnats' pool, which starts one server for each test that holds one at the same moment, and go
// test's default of GOMAXPROCS would run as many at once as the machine has cores: four on CI's
// runner, hundreds on a dev box, where it would start as many NATS containers. Four is the
// runner's own count, the one the race step's measurements were taken at.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const intakeTestParallelism = 4

// TestMain drops the migrated template this package's tests — the internal ones and the external
// intake_test ones, which share this binary — clone their databases from (testpg.Main), removes
// the directory their Envoy binaries were built into (testbuild.Main), and removes the NATS
// containers they share (testnats.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(intakeTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	os.Exit(testpg.Main("legion_intake_test", func() int {
		return testbuild.Main(func() int { return testnats.Main(m) })
	}))
}
