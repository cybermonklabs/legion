package textcheck

import (
	"testing"

	"github.com/sjawhar/envoy/testtmp"
)

// placementExceptions are the paths under packages/envoy the placement check leaves unread, with
// the reason each places a temporary directory itself on purpose.
var placementExceptions = map[string]string{
	"testtmp": "the package itself, where both shapes belong: Dir reads os.TempDir and makes the directory " +
		"itself, naming /tmp when it falls back, so that no other test has to",
}

// Every test of this module that wants a temporary directory of its own takes it from testtmp, so
// that one place measures a socket path against what Linux holds and one place knows where to fall
// back to (testtmp.CheckPlacements says what it reads). The walk takes in infra, a module of its
// own under this root, whose tests reach testtmp through the replace directive in its go.mod. It
// runs here, in the text-check job, whose filter selects a change to any file under
// packages/envoy, infra's tests included. packages/daemon/internal/textcheck runs the same check
// over that module.
func TestNoTestFilePlacesATemporaryDirectoryOfItsOwn(t *testing.T) {
	testtmp.CheckPlacements(t, "../..", placementExceptions)
}
