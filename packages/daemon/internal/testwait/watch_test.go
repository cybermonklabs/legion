package testwait

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// watched is what t's watch holds: the bounds t took, in the order taken, how many readings it
// took, and the end of the last span it finished asking about.
func watched(t testing.TB) ([]bound, int, time.Time) {
	value, ok := watches.Load(t)
	if !ok {
		return nil, 0, time.Time{}
	}
	w := value.(*watch)
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ends), w.reads, w.checked
}

// Every bound this package gives is watched. A bound that ends at the test binary's deadline less
// a spare is watched at that moment, once however often it is taken: Bounded's when its clamp is
// the sooner, Remaining's, and Context's deadline, which is Remaining's moment. A span of the
// caller's own, Bounded's bound when it is the sooner, is watched at its end, and a span taken
// later replaces it.
func TestEveryBoundIsWatched(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("this row needs the test binary's deadline: run it under a -timeout")
	}
	Bounded(t, time.Second, time.Second)
	Bounded(t, 1<<60, 5*time.Second)
	Bounded(t, 1<<60, 5*time.Second)
	Remaining(t)
	Context(t)
	took := time.Now()
	Bounded(t, 2*time.Second, time.Second)
	ends, _, _ := watched(t)
	var deadlines []time.Time
	var spans []bound
	for _, b := range ends {
		if b.span {
			spans = append(spans, b)
		} else {
			deadlines = append(deadlines, b.at)
		}
	}
	want := []time.Time{deadline.Add(-5 * time.Second), deadline.Add(-margin)}
	if !slices.Equal(deadlines, want) {
		t.Errorf("the deadline bounds are watched to end at %v, want %v", deadlines, want)
	}
	if len(spans) != 1 || spans[0].at.Before(took.Add(2*time.Second)) || spans[0].fn != "Bounded" || !strings.HasPrefix(spans[0].site, "watch_test.go:") {
		t.Errorf("the spans watched are %+v, want the one span of 2s taken last, from watch_test.go", spans)
	}
}

// A test that passes reads no stacks for its spans, however its waits end: here ten waits of a 40
// ms span each end at 35 ms, in the last eighth of their span, and the last span ends while the
// test still runs. The row waits until the watch has finished asking about that last span, so a
// reading the watch would take has been taken by then.
func TestAPassingTestReadsNoSpan(t *testing.T) {
	for range 10 {
		Bounded(t, 40*time.Millisecond, 0)
		time.Sleep(35 * time.Millisecond)
	}
	ends, _, _ := watched(t)
	if len(ends) != 1 || !ends[0].span {
		t.Fatalf("the watch holds %+v, want the last span alone", ends)
	}
	Until(t, "the watch to finish asking about the last span", func() (time.Time, bool) {
		_, _, checked := watched(t)
		return checked, checked.Equal(ends[0].at)
	})
	if _, reads, _ := watched(t); reads != 0 {
		t.Errorf("a passing test's watch took %d readings of the goroutine stacks, want none", reads)
	}
}
