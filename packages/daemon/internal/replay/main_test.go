package replay

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template seedStore clones its databases from (testpg.Main). A replay
// copies its database from a template of its own (openReplayDatabase), which stays on the server
// for the next run: this does not drop it.
func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_replay_test", m.Run)) }
