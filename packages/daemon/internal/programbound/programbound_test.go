package programbound

import (
	"testing"
	"time"
)

// A registered bound is its own value until a deadline is set; then, while it holds its registered
// value, it is what is left of that deadline less the margin, and a value a test set is kept. An
// unregistered bound is always its own value.
func TestOfGivesARegisteredBoundTheDeadlineUnlessATestSetIt(t *testing.T) {
	saved := deadline
	t.Cleanup(func() { SetDeadline(saved) })
	SetDeadline(time.Time{})
	registeredBound, unregistered := 7*time.Second, 9*time.Second
	Register(&registeredBound)
	t.Cleanup(func() {
		mu.Lock()
		delete(registered, &registeredBound)
		mu.Unlock()
	})
	if got := Of(&registeredBound); got != 7*time.Second {
		t.Fatalf("Of with no deadline = %s, want the bound's own 7s", got)
	}
	end := time.Now().Add(10 * time.Minute)
	SetDeadline(end)
	most := time.Until(end) - margin
	if got := Of(&registeredBound); got > most || got < most-time.Minute {
		t.Fatalf("Of under a deadline 10m away = %s, want what is left of it less %s, at most %s", got, margin, most)
	}
	if got := Of(&unregistered); got != 9*time.Second {
		t.Fatalf("Of of an unregistered bound = %s, want its own 9s", got)
	}
	registeredBound = time.Second
	if got := Of(&registeredBound); got != time.Second {
		t.Fatalf("Of of a bound a test set to 1s = %s, want the test's 1s", got)
	}
}
