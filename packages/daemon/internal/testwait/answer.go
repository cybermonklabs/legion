package testwait

import (
	"testing"
	"time"
)

// Answer receives the one value answer carries, the result of a call the test started, and sets
// no span of its own: what bounds the wait is the test binary's deadline less a second, a hang
// guard that fails the test naming what has not answered, so a call that waits for ever fails by
// name before the binary's own panic, while a call that is merely slow on a loaded machine is
// waited for. With no deadline (`go test -timeout 0`) it waits for as long as the call takes. A
// row whose verdict is what the call answers takes it rather than a clock of a few seconds.
func Answer[T any](t *testing.T, what string, answer <-chan T) T {
	t.Helper()
	var guard <-chan time.Time
	if deadline, ok := t.Deadline(); ok {
		guard = time.After(time.Until(deadline) - time.Second)
	}
	select {
	case value := <-answer:
		return value
	case <-guard:
		t.Fatalf("%s has not answered by the test binary's deadline less a second", what)
	}
	var none T
	return none
}
