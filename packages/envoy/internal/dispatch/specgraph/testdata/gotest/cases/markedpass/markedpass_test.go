// Package markedpass has tests that print a "--- PASS" line with the byte 0x16 before it, the
// marker that the testing package writes before each of its own frames in go test -json. go test's
// converter then writes a pass event for the test that the line names.
package markedpass

import (
	"fmt"
	"testing"
)

// TestFails fails, and runs before TestPrinter.
func TestFails(t *testing.T) { t.Fail() }

// TestPrinter passes, and prints a marked line that says TestFails passed.
func TestPrinter(t *testing.T) {
	fmt.Print("\x16--- PASS: TestFails (0.00s)\n")
}

// TestSelfFails prints a marked line that says it passed, and then fails.
func TestSelfFails(t *testing.T) {
	fmt.Print("\x16--- PASS: TestSelfFails (0.00s)\n")
	t.Fail()
}
