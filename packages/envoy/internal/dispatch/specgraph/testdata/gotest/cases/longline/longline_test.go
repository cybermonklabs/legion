// Package longline has a test that prints one line of 1,024 bytes of "x" and then go test's kill
// line. go test's converter cuts an output event at 1,024 bytes, so the kill line starts an event
// of its own. Then the test fails.
package longline

import (
	"fmt"
	"strings"
	"testing"
)

func TestPrintsALongLineThatHoldsTheKillLineThenFails(t *testing.T) {
	fmt.Println(strings.Repeat("x", 1024) + "panic: test timed out after 1s")
	t.Fatal("fails for real")
}
