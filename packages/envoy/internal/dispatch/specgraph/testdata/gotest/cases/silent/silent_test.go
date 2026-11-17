// Package silent has two parallel tests that pass. TestSilent prints nothing. When both run, it
// calls t.Attr, so the testing package writes a frame that names TestSilent, the last frame before
// TestLoud prints go test's kill line. go test's converter gives a line to the test that it named
// last, so it gives TestLoud's line to TestSilent.
package silent

import (
	"fmt"
	"testing"
)

var (
	loudRuns    = make(chan struct{})
	silentNamed = make(chan struct{})
	loudPrinted = make(chan struct{})
)

func TestSilent(t *testing.T) {
	t.Parallel()
	<-loudRuns
	t.Attr("waits-for", "TestLoud")
	close(silentNamed)
	<-loudPrinted
}

func TestLoud(t *testing.T) {
	t.Parallel()
	close(loudRuns)
	<-silentNamed
	fmt.Println("panic: test timed out after 1s")
	close(loudPrinted)
}
