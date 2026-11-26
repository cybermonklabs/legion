package redeliver_test

import (
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// redeliverTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel: four, the parallelism its build-cluster measurement ran at and go test's
// default on CI's 4-CPU runner. Each parallel test holds a database of its own, so a large
// machine's default would open more of the test server's connections at once than the suite is
// sized for (docs/testing.md). go test parses its flags inside m.Run, so a -parallel on the command
// line still wins.
const redeliverTestParallelism = 4

// TestMain sets the package's default -parallel (redeliverTestParallelism).
//
// It gives the package both harnesses: testnats.Main runs the tests and shuts down the one
// NATS server they share, each test in a JetStream account of its own (openState), and
// storetest.Run then drops the databases they cloned.
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(redeliverTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(storetest.Run(func() int { return testnats.Main(m) }))
}
