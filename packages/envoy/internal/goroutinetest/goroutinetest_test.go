package goroutinetest_test

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A test that waits for a goroutine to return passes at once, checking nothing, if Live never finds
// a goroutine that is still running, so Live must find one while it runs and lose it once it returns.
func TestLiveFollowsAGoroutineUntilItReturns(t *testing.T) {
	ids := make(chan uint64)
	release := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		ids <- goroutinetest.ID()
		<-release
	}()
	id := <-ids
	if !goroutinetest.Live(id) {
		t.Fatalf("goroutine %d is parked on a channel, and Live reports it returned", id)
	}
	close(release)
	<-returned
	testwait.Eventually(t, fmt.Sprintf("Live to lose goroutine %d, which has returned", id), func() bool {
		return !goroutinetest.Live(id)
	})
}
