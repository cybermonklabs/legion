package textcheck

import (
	"testing"

	"github.com/sjawhar/envoy/testtmp"
)

// placementExceptions are the paths under packages/daemon the placement check leaves unread, with
// the reason each places a temporary directory itself on purpose.
var placementExceptions = map[string]string{
	"internal/shim/socketpath_test.go": "roleTempDir builds a deliberately long directory, as long as the TMPDIR a " +
		"Legion role is told, so that its test proves a socket still binds where a role runs; a short one would take the proof away",
}

// Every test of this module that wants a temporary directory of its own takes it from Envoy's
// testtmp, so that one place measures a socket path against what Linux holds and one place knows
// where to fall back to (testtmp.CheckPlacements says what it reads). The comment on
// internal/daemon/idle_rig_pod_test.go's privateDir spells os.TempDir() in English, which the
// check reads as prose. packages/envoy/internal/textcheck runs the same check over that module.
func TestNoTestFilePlacesATemporaryDirectoryOfItsOwn(t *testing.T) {
	testtmp.CheckPlacements(t, "../..", placementExceptions)
}
