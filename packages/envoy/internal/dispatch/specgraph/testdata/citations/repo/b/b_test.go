package b

import "testing"

// TestB of package b: the selector "example.com/citations/b TestB" picks it and the TestB of
// package b_test.
func TestB(t *testing.T) {
	if Answer() != 7 {
		t.Fatal("Answer() is not 7")
	}
}
