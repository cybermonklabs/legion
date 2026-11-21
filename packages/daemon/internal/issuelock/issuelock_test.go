package issuelock

import (
	"testing"
	"time"
)

// Accepting answers true for an issue only while an acceptance of it holds its lock: not while a
// plain writer holds it, not for another issue, and not once the acceptance has released it. A
// plain writer of the issue waits for the acceptance, and a writer of another issue does not.
func TestAcceptingHoldsOnlyWhileAnAcceptanceHoldsTheIssue(t *testing.T) {
	var locks Locks
	release := locks.Lock("LEGION-1")
	if locks.Accepting("LEGION-1") {
		t.Fatal("Accepting is true while a plain writer holds LEGION-1")
	}
	release()

	accepted := locks.Accept("LEGION-1")
	if !locks.Accepting("LEGION-1") || locks.Accepting("LEGION-2") {
		t.Fatalf("Accepting = %v for LEGION-1 and %v for LEGION-2 under LEGION-1's acceptance, want true and false",
			locks.Accepting("LEGION-1"), locks.Accepting("LEGION-2"))
	}
	locks.Lock("LEGION-2")()
	took := make(chan struct{})
	go func() {
		locks.Lock("LEGION-1")()
		close(took)
	}()
	select {
	case <-took:
		t.Fatal("a writer of LEGION-1 took its lock while its acceptance held it")
	case <-time.After(100 * time.Millisecond):
	}
	accepted()
	<-took
	if locks.Accepting("LEGION-1") {
		t.Fatal("Accepting is true for LEGION-1 after its acceptance released it")
	}
	if len(locks.locks) != 0 {
		t.Fatalf("the set keeps %d locks once no writer holds or waits on one, want none", len(locks.locks))
	}
}

// A nil set serializes nothing and has no acceptance under way.
func TestANilSetSerializesNothing(t *testing.T) {
	var locks *Locks
	first, second := locks.Accept("LEGION-1"), locks.Lock("LEGION-1")
	if locks.Accepting("LEGION-1") {
		t.Fatal("a nil set answers an acceptance under way")
	}
	first()
	second()
}
