package store

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Every test of this package that takes a database takes one of its own (emptyStore), and the
// drops of those databases run in the background rather than on each test's clock. testdb.Main
// waits for them, so no database outlives this binary and a drop that failed turns the run red.
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m.Run))
}
