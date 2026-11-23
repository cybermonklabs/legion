package pause

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template the package's tests clone their databases from
// (testpg.Main).
func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_pause_test", m.Run)) }
