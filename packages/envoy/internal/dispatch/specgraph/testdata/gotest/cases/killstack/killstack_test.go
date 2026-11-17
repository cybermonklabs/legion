// Package killstack has two parallel tests. TestSleeps starts 300 goroutines that never end and
// sleeps past -timeout 1s, so go test kills it. TestPasses logs 5,000 bytes and passes first, so go
// test's converter gives it the kill line and the stacks of all goroutines, more than 64 KiB, after
// its own end line and before its pass event. So its end line is more than 4 KiB from the start of
// its output and more than 60 KiB from its end.
package killstack

import (
	"strings"
	"testing"
	"time"
)

var started = make(chan struct{})

func TestSleeps(t *testing.T) {
	t.Parallel()
	for range 300 {
		go func() { select {} }()
	}
	close(started)
	time.Sleep(5 * time.Second)
}

func TestPasses(t *testing.T) {
	t.Parallel()
	<-started
	t.Log(strings.Repeat("x", 5000))
}
