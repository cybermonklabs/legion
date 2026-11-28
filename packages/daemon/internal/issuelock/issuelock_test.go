package issuelock

import (
	"testing"
	"time"
)

// Accepting answers true for an issue only while an acceptance of it holds its lock: not while a
// plain writer holds it, not for another issue, and not once the acceptance has released it. A
// plain writer of the issue waits for the acceptance, and a writer of another issue does not.
//
// The writer of LEGION-1 is seen waiting through Waiting, which the set calls only once it found
// the issue's mutex held, so the test times no window: either the writer takes the lock while the
// acceptance holds it, which fails at once, or it is seen blocked on that lock.
func TestAcceptingHoldsOnlyWhileAnAcceptanceHoldsTheIssue(t *testing.T) {
	waiting := make(chan string, 1)
	locks := Locks{Waiting: func(issue string) { waiting <- issue }}
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
	case issue := <-waiting:
		if issue != "LEGION-1" {
			t.Fatalf("the set saw a writer wait on %s, want LEGION-1", issue)
		}
	case <-time.After(time.Minute):
		t.Fatal("a writer of LEGION-1 neither took its lock nor waited on it within a minute")
	}
	if !locks.Accepting("LEGION-1") {
		t.Fatal("Accepting is false for LEGION-1 while a writer waits on its acceptance")
	}
	accepted()
	select {
	case <-took:
	case <-time.After(time.Minute):
		t.Fatal("the writer of LEGION-1 did not take its lock within a minute of the acceptance's release")
	}
	if locks.Accepting("LEGION-1") {
		t.Fatal("Accepting is true for LEGION-1 after its acceptance released it")
	}
	if len(locks.locks) != 0 {
		t.Fatalf("the set keeps %d locks once no writer holds or waits on one, want none", len(locks.locks))
	}
}

// A writer that finds an issue's mutex free takes it without waiting, so Waiting is told of no
// one: the hook reports a writer blocked, never a writer that passed.
func TestAWriterOfAFreeIssueIsNotSeenWaiting(t *testing.T) {
	var told []string
	locks := Locks{Waiting: func(issue string) { told = append(told, issue) }}
	locks.Accept("LEGION-1")()
	locks.Lock("LEGION-1")()
	release := locks.Lock("LEGION-1")
	locks.Lock("LEGION-2")()
	release()
	if len(told) != 0 {
		t.Fatalf("Waiting was told %q with no mutex held, want no one", told)
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
