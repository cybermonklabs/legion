// Package killprinted has a test that prints, with no marker, go test's pass line of its own name,
// and then sleeps past -timeout 1s, so go test kills it.
package killprinted

import (
	"fmt"
	"testing"
	"time"
)

func TestPrintsAPassLineOfItselfThenSleeps(t *testing.T) {
	fmt.Println("--- PASS: TestPrintsAPassLineOfItselfThenSleeps (0.00s)")
	time.Sleep(5 * time.Second)
}
