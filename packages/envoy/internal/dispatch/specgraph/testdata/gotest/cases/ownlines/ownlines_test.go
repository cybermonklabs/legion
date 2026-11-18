// Package ownlines has tests that print, with no marker, go test's end line of their own name of
// the other kind: a test that prints that it failed, and passes, and a test that prints that it
// passed, and fails.
package ownlines

import (
	"fmt"
	"testing"
)

func TestPrintsAFailLineOfItselfThenPasses(t *testing.T) {
	fmt.Println("--- FAIL: TestPrintsAFailLineOfItselfThenPasses (0.00s)")
}

func TestPrintsAPassLineOfItselfThenFails(t *testing.T) {
	fmt.Println("--- PASS: TestPrintsAPassLineOfItselfThenFails (0.00s)")
	t.Fail()
}
