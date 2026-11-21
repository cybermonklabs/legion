package gcp

import "testing"

// SetPrivateModeListenerFloor sets privateModeListenerFloor to floor for t's length, so a test
// renders a private hub's session listener before a listener release that enforces the required
// mode is named. A test that calls it never calls t.Parallel: the floor is the package's.
func SetPrivateModeListenerFloor(t testing.TB, floor string) {
	t.Helper()
	prior := privateModeListenerFloor
	privateModeListenerFloor = floor
	t.Cleanup(func() { privateModeListenerFloor = prior })
}
