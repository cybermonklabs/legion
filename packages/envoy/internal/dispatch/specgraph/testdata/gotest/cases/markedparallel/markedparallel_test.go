// Package markedparallel has two parallel tests. While TestRunningFails runs, TestPrinter prints a
// "--- PASS" line for it with the byte 0x16 before it, the marker that the testing package writes
// before each of its own frames in go test -json. Then TestRunningFails fails. So go test's
// converter writes the printed pass event before the real fail event.
package markedparallel

import (
	"fmt"
	"testing"
)

var (
	running = make(chan struct{})
	printed = make(chan struct{})
)

func TestRunningFails(t *testing.T) {
	t.Parallel()
	close(running)
	<-printed
	t.Error("fails for real, after TestPrinter printed that it passed")
}

func TestPrinter(t *testing.T) {
	t.Parallel()
	<-running
	fmt.Print("\x16--- PASS: TestRunningFails (0.00s)\n")
	close(printed)
}
