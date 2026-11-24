package projection

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template the state tests that read a real store clone their
// databases from (testpg.Main).
func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_projection_test", m.Run)) }
