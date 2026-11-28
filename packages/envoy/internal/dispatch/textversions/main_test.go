package textversions

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestMain runs the package's tests 4 at once by default (storetest.SetDefaultParallel), each on
// a database of its own, and drops the databases they clone (storetest.Main).
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	os.Exit(storetest.Main(m))
}
