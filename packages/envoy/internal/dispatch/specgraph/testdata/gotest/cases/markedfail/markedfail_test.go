// Package markedfail has tests that print a "--- FAIL" line with the byte 0x16 before it, the
// marker that the testing package writes before each of its own frames in go test -json. go test's
// converter then writes a fail event for the test that the line names.
package markedfail

import (
	"fmt"
	"testing"
)

// TestPasses passes, and runs before TestPrinter.
func TestPasses(t *testing.T) {}

// TestPrinter passes, and prints a marked line that says TestPasses failed.
func TestPrinter(t *testing.T) {
	fmt.Print("\x16--- FAIL: TestPasses (0.00s)\n")
}

// TestSelfPasses prints a marked line that says it failed, and then passes.
func TestSelfPasses(t *testing.T) {
	fmt.Print("\x16--- FAIL: TestSelfPasses (0.00s)\n")
}
