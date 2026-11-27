package store

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// storeTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. A test calls t.Parallel unless its first line says why it does not: each
// opens a database of its own (testdb.Database) and its own pools on it, and none changes
// anything the whole process shares (the environment, the default logger, a package variable,
// flags). A pool holds the pool_max_conns LEGION_TEST_PG_DSN names, or testpool.PoolConns where
// it names none, and each database its admin connection. Four concurrent tests is the CI runner's
// own count and stays below the test server's connection limit on a large dev box.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const storeTestParallelism = 4

// TestMain sets this package's default -parallel (storeTestParallelism).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(storeTestParallelism)); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: set this package's default -parallel: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
