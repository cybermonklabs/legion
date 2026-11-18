// Package stderr has two tests that print go test's kill line at the start of a line of their
// standard error. The first fails and the second passes.
package stderr

import (
	"fmt"
	"os"
	"testing"
)

func TestPrintsTheKillLineOnStandardErrorThenFails(t *testing.T) {
	fmt.Fprintln(os.Stderr, "panic: test timed out after 1s")
	t.Fatal("fails for real")
}

func TestPrintsTheKillLineOnStandardErrorThenPasses(t *testing.T) {
	fmt.Fprintln(os.Stderr, "panic: test timed out after 1s")
}
