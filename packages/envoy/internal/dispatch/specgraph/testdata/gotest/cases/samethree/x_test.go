package samethree_test

import (
	"fmt"
	"testing"

	"example.com/acme/gotest/samethree"
)

func TestSame(t *testing.T) {
	t.Parallel()
	fmt.Print("\x16=== RUN   TestSame\n")
	close(samethree.Printed)
}
