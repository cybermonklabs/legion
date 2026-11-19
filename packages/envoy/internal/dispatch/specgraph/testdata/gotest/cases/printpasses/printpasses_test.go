// Package printpasses has a test that prints go test's kill line at the start of a line of its
// standard output, and then passes.
package printpasses

import (
	"fmt"
	"testing"
)

func TestPrintsTheKillLineThenPasses(t *testing.T) {
	fmt.Println("panic: test timed out after 1s")
}
