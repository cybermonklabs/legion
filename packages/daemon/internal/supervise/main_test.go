package supervise_test

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template the tests that read a claim back from a real store clone
// their databases from (testpg.Main). The package's other tests run on its store double and
// clone nothing.
func TestMain(m *testing.M) {
	os.Exit(testpg.Main("legion_supervise_test", m.Run))
}
