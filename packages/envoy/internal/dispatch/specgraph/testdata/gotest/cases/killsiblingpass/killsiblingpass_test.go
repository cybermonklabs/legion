// Package killsiblingpass has two parallel tests. While TestSleeps runs, TestPrinter prints go
// test's pass line of TestSleeps with the byte 0x16 before it, the marker of the testing package's
// own lines, and passes. TestSleeps sleeps past -timeout 1s, so go test kills it. go test's
// converter writes the printed line as a frame of TestSleeps and a pass event of TestSleeps.
package killsiblingpass

import (
	"fmt"
	"testing"
	"time"
)

var sleeping = make(chan struct{})

func TestSleeps(t *testing.T) {
	t.Parallel()
	close(sleeping)
	time.Sleep(5 * time.Second)
}

func TestPrinter(t *testing.T) {
	t.Parallel()
	<-sleeping
	fmt.Print("\x16--- PASS: TestSleeps (0.10s)\n")
}
