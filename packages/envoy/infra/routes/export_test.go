package routes

import "testing"

// SetServiceAccountCA points the in-cluster Source at path for the pod's ServiceAccount CA
// certificate until the test ends.
func SetServiceAccountCA(t testing.TB, path string) {
	old := serviceAccountCA
	serviceAccountCA = path
	t.Cleanup(func() { serviceAccountCA = old })
}
