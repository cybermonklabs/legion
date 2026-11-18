package goroutinetest_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/goroutinetest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A test that waits for a goroutine parked on a lock passes only once Traces shows that goroutine
// under the state the runtime gives it, so Traces must show a goroutine parked in Lock with a
// "[sync.Mutex.Lock" header over the function that called Lock, and must stop showing it once it
// has the lock and returns.
func TestTracesShowAGoroutineParkedOnAMutex(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		parkedOnTheTestsMutex(&mu)
	}()
	parked := func() bool {
		for _, trace := range goroutinetest.Traces() {
			if header, _, _ := strings.Cut(trace, "\n"); strings.Contains(header, "[sync.Mutex.Lock") && strings.Contains(trace, ".parkedOnTheTestsMutex(") {
				return true
			}
		}
		return false
	}
	testwait.Eventually(t, "the goroutine parked on the test's mutex to show in the dump", parked)
	mu.Unlock()
	testwait.Receive(t, "the goroutine to take the mutex and return", returned)
	if parked() {
		t.Fatal("the dump still shows a goroutine parked on the test's mutex after it took the mutex and returned")
	}
}

// parkedOnTheTestsMutex takes mu, which the test holds, so it parks in Lock until the test lets go.
//
//go:noinline
func parkedOnTheTestsMutex(mu *sync.Mutex) {
	mu.Lock()
	mu.Unlock()
}
