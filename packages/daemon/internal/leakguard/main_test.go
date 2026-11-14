package leakguard

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the test process every variable that could lead a git to a workspace,
// repository, remote or credential not the test's own (docs/testing.md, "A test that starts jj or
// git inherits no pod's environment"). This package's own code starts git to read a push's
// records, and its fixtures build the repository those records come from, so both sides need the
// guard; in a Legion issue pod an unguarded run would read the pod's live workspace through the
// GIT_DIR its environment carries.
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}
