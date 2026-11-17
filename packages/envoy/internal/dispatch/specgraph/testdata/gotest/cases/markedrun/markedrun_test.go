// Package markedrun has a test that prints an "=== RUN" line of its own name with the byte 0x16
// before it, the marker of the testing package's own lines in go test -json, and then passes. go
// test's converter then writes a second run event of the test while its first run is open.
package markedrun

import (
	"fmt"
	"testing"
)

func TestPrintsAMarkedRunOfItself(t *testing.T) {
	fmt.Print("\x16=== RUN   TestPrintsAMarkedRunOfItself\n")
}
