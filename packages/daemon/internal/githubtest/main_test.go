package githubtest

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the process every variable that could lead the fake's real git to a
// repository, remote or credential not the test's own (fixtureenv.MustGuard).
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}
