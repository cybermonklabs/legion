package textcheck

import (
	"testing"

	"github.com/sjawhar/envoy/testtmp"
)

// Every test of this module that wants a temporary directory of its own takes it from testtmp, so
// that one place measures a socket path against what Linux holds and one place knows where to fall
// back to (testtmp.CheckPlacements says what it reads). The walk takes in infra, a module of its
// own under this root, whose tests reach testtmp through the replace directive in its go.mod. It
// runs here, in the text-check job, whose filter selects a change to any file under
// packages/envoy, infra's tests included. No file is left unread: testtmp, where both shapes
// belong, holds no test file, and the walk reads test files only.
// packages/daemon/internal/textcheck runs the same check over that module.
func TestNoTestFilePlacesATemporaryDirectoryOfItsOwn(t *testing.T) {
	testtmp.CheckPlacements(t, "../..", nil)
}
