package admit

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template migratedPool clones its databases from (testpg.Main) and
// fails the run if a NATS server a test started is still running after the last test
// (testnats.Main).
func TestMain(m *testing.M) {
	os.Exit(testpg.Main("legion_admit_test", func() int { return testnats.Main(m) }))
}
