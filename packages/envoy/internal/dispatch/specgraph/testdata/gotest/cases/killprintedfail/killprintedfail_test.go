// Package killprintedfail has a test that prints, with no marker, go test's fail line of its own
// name, and then sleeps past -timeout 1s, so go test kills it.
package killprintedfail

import (
	"fmt"
	"testing"
	"time"
)

func TestPrintsAFailLineOfItselfThenSleeps(t *testing.T) {
	fmt.Println("--- FAIL: TestPrintsAFailLineOfItselfThenSleeps (0.00s)")
	time.Sleep(5 * time.Second)
}
