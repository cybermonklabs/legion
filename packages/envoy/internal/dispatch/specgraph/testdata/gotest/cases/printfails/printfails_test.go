// Package printfails has a test that prints go test's kill line at the start of a line of its
// standard output, and then fails.
package printfails

import (
	"fmt"
	"testing"
)

func TestPrintsTheKillLineThenFails(t *testing.T) {
	fmt.Println("panic: test timed out after 1s")
	t.Fatal("fails for real")
}
