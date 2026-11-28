package race

import (
	"sync"
	"testing"

	"example.com/gotestfixtures/state"
)

// TestRacesOnItsFirstAttempt writes one variable from two goroutines on its first attempt, which
// the race detector reports, and on any later attempt writes it from one, which passes.
func TestRacesOnItsFirstAttempt(t *testing.T) {
	writers := 1
	if state.Attempt(t, "race") == 1 {
		writers = 2
	}
	var shared int
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			shared++
		}()
	}
	group.Wait()
	_ = shared
}
