// Package silentlog has two parallel tests that pass. TestP2 logs a line first, so the testing
// package writes a frame that names TestP2, the last frame before TestP1 prints go test's kill line.
// go test's converter gives a line to the test that it named last, so it gives TestP1's line to
// TestP2.
package silentlog

import (
	"fmt"
	"sync"
	"testing"
)

var (
	p2Logged = make(chan struct{})
	p1Done   = make(chan struct{})
	once     sync.Once
)

func TestP1(t *testing.T) {
	t.Parallel()
	<-p2Logged
	fmt.Println("panic: test timed out after 1s")
	close(p1Done)
}

func TestP2(t *testing.T) {
	t.Parallel()
	t.Log("TestP2 logs")
	once.Do(func() { close(p2Logged) })
	<-p1Done
}
