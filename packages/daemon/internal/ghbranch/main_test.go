package ghbranch

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the process every variable that could lead the fake GitHub's real git
// (githubtest.GitRepository) to a repository, remote or credential not the test's own
// (fixtureenv.MustGuard).
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}
