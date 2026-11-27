package a

import "testing"

// TestA is the one test that the selector "example.com/citations/a TestA" picks.
func TestA(t *testing.T) {
	if Answer() != 42 {
		t.Fatal("Answer() is not 42")
	}
}

// helper holds a method named TestA, which go test does not run: it is not a top-level func.
type helper struct{}

// TestA is a method of helper.
func (helper) TestA(t *testing.T) { t.Helper() }
