package testwait

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// watched is the bounds the test named name took that its watch holds, in the order taken, and
// its latest reading.
func watched(name string) ([]bound, string) {
	value, ok := watches.Load(name)
	if !ok {
		return nil, ""
	}
	w := value.(*watch)
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ends), w.stacks
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
	ends, _ := watched(t.Name())
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

// A test prints one dump at most, its latest reading: the stacks read as a later span ends replace
// those read as an earlier one ended, and each reading names the bound it belongs to, by the line
// that took it. The row waits for each reading to be taken, not for a span of time.
func TestTheLatestReadingReplacesAnEarlierOne(t *testing.T) {
	reads := func(site string) {
		t.Helper()
		Until(t, "the reading of the bound taken at "+site, func() (string, bool) {
			_, stacks := watched(t.Name())
			return stacks, strings.Contains(stacks, " gave at "+site+" at ")
		})
	}
	first := takeASpan(t)
	reads(first)
	second := takeAnotherSpan(t)
	reads(second)
	if _, stacks := watched(t.Name()); strings.Contains(stacks, " gave at "+first+" at ") {
		t.Errorf("the latest reading still names the first bound, %s:\n%.400s", first, stacks)
	}
}

// takeASpan takes a span of 200 ms and returns the line that took it.
func takeASpan(t *testing.T) string {
	Bounded(t, 200*time.Millisecond, 0)
	_, _, line, _ := runtime.Caller(0)
	return fmt.Sprintf("watch_test.go:%d", line-1)
}

// takeAnotherSpan is takeASpan at another line.
func takeAnotherSpan(t *testing.T) string {
	Bounded(t, 200*time.Millisecond, 0)
	_, _, line, _ := runtime.Caller(0)
	return fmt.Sprintf("watch_test.go:%d", line-1)
}
