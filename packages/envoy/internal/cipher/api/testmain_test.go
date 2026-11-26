package api

import (
	"flag"
	"os"
	"strconv"
	"testing"
)

// apiTestParallelism is how many of this package's tests run at once when the command line names
// no -parallel. Each parallel test has a database of its own on the broker's test server and a
// pool of pgx's default size on it (storetest.URL), so go test's default of GOMAXPROCS, four on
// CI's runner and hundreds on a dev box, would there open more connections than the server allows.
// Four is the runner's own count, the one the package's times were measured at (docs/testing.md,
// "A test in the broker's requests, api or cmd/cipherd package runs beside the others").
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const apiTestParallelism = 4

func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(apiTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(m.Run())
}
