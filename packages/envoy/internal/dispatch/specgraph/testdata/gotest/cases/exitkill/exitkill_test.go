// Package exitkill has a test that prints go test's kill line at the start of a line of its
// standard output and then ends its process, so it has no end event, as a test that go test
// killed has none.
package exitkill

import (
	"fmt"
	"os"
	"testing"
)

func TestPrintsTheKillLineThenExits(t *testing.T) {
	fmt.Println("panic: test timed out after 1s")
	os.Exit(3)
}
