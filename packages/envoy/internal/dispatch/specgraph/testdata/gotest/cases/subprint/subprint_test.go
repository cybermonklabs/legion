// Package subprint has two tests, each with a subtest that prints go test's kill line at the start
// of a line of its standard output and passes. Then the first parent fails and the second passes.
package subprint

import (
	"fmt"
	"testing"
)

func TestParentFails(t *testing.T) {
	t.Run("prints", func(t *testing.T) { fmt.Println("panic: test timed out after 1s") })
	t.Error("the parent fails for real")
}

func TestParentPasses(t *testing.T) {
	t.Run("prints", func(t *testing.T) { fmt.Println("panic: test timed out after 1s") })
}
