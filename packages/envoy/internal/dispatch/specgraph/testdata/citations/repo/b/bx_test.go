package b_test

import (
	"testing"

	"example.com/citations/b"
)

// TestB of package b_test: the selector "example.com/citations/b TestB" picks it and the TestB of
// package b.
func TestB(t *testing.T) {
	if b.Answer() != 7 {
		t.Fatal("b.Answer() is not 7")
	}
}
