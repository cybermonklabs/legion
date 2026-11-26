package main

import (
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// cipherdTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Each parallel test has a database of its own on the broker's test server
// (storetest.URL) and starts broker processes with pools of their own on it, so go test's default
// of GOMAXPROCS, four on CI's runner and hundreds on a dev box, would there open more connections
// than the server allows. Four is the runner's own count, the one the package's times were measured
// at (docs/testing.md, "A test in the broker's requests, api or cmd/cipherd package runs beside the
// others").
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const cipherdTestParallelism = 4

// TestMain sets the package's default -parallel (cipherdTestParallelism) and removes the binaries
// the package's tests built (cmdtest.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(cipherdTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(cmdtest.Main(m))
}
