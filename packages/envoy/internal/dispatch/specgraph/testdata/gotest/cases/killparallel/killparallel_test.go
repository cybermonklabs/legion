// Package killparallel has three parallel tests. TestSleeps sleeps past -timeout 1s. The other two
// end only after TestSleeps runs, so the last of them is the test that go test's converter names
// last when the kill line comes, and it gets that line.
package killparallel

import (
	"testing"
	"time"
)

var sleeping = make(chan struct{})

func TestSleeps(t *testing.T) {
	t.Parallel()
	close(sleeping)
	time.Sleep(5 * time.Second)
}

func TestPasses(t *testing.T) {
	t.Parallel()
	<-sleeping
}

func TestFails(t *testing.T) {
	t.Parallel()
	<-sleeping
	t.Error("fails for real")
}
