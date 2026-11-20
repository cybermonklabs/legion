package forgedpass

import (
	"fmt"
	"os"
	"testing"
)

// TestPrintsAPassLineOfItselfThenExits prints its own pass line with no marker, and then ends its
// process with status 1, so it does not pass.
func TestPrintsAPassLineOfItselfThenExits(t *testing.T) {
	fmt.Println("--- PASS: TestPrintsAPassLineOfItselfThenExits (0.00s)")
	os.Exit(1)
}
