package dispatch

import "testing"

// A planted caller of main's dispatch.New with the token string it took before #4392: the
// merged-tree vet must refuse it, which shows that the vet builds main's side of the merge. It is
// the control of that vet and is never merged.
func TestPlantedCallerOfMainsOldDispatchNew(t *testing.T) {
	_ = New("http://127.0.0.1", "token")
}
