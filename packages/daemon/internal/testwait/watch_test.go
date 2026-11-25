package testwait

import (
	"slices"
	"testing"
	"time"
)

// watchedEnds is when the bounds the test named name took are watched to end, in the order they
// were taken.
func watchedEnds(name string) []time.Time {
	value, ok := watches.Load(name)
	if !ok {
		return nil
	}
	w := value.(*watch)
	w.mu.Lock()
	defer w.mu.Unlock()
	ends := make([]time.Time, 0, len(w.ends))
	for _, b := range w.ends {
		ends = append(ends, b.at)
	}
	return ends
}

// Every bound that ends at the test binary's deadline less a spare is watched at that moment:
// Bounded's when its clamp is the sooner, Remaining's, and Context's deadline, which is Remaining's
// moment and so watched once. A bound of the caller's own that ends sooner, Bounded's bound when
// that is the sooner, is not watched, and a bound taken again is watched once.
func TestTheBoundsThatEndAtTheDeadlineAreWatched(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("this row needs the test binary's deadline: run it under a -timeout")
	}
	Bounded(t, time.Second, time.Second)
	if ends := watchedEnds(t.Name()); len(ends) != 0 {
		t.Fatalf("a bound of a second, sooner than the deadline, is watched to end at %v, want no watch", ends)
	}
	Bounded(t, 1<<60, 5*time.Second)
	Bounded(t, 1<<60, 5*time.Second)
	Remaining(t)
	Context(t)
	want := []time.Time{deadline.Add(-5 * time.Second), deadline.Add(-margin)}
	if ends := watchedEnds(t.Name()); !slices.Equal(ends, want) {
		t.Errorf("the bounds are watched to end at %v, want %v", ends, want)
	}
}
