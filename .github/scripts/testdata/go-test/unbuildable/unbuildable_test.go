package unbuildable

import "testing"

// TestDoesNotBuild names a function no file declares, so the package's test binary fails to
// build: the harness's build-failure case.
func TestDoesNotBuild(t *testing.T) {
	declaredNowhere(t)
}
