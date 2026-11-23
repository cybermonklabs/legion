package citebody

import (
	"regexp"
	"testing"
)

// TestABodyCountsEveryReadAndANilCountNone: a pass counts once, each span counts its bytes and
// the longest is kept, and a Body made with nil reads answers the same and counts nothing.
func TestABodyCountsEveryReadAndANilCountNone(t *testing.T) {
	const text = "ab #12 cd #345"
	re := regexp.MustCompile(`#([0-9]+)`)
	var reads Reads
	counted := New(text, &reads)
	matches := counted.Find(re)
	got := []string{counted.Span(0, 2), counted.Span(matches[0][2], matches[0][3]), counted.Span(matches[1][2], matches[1][3])}
	if want := (Reads{Passes: 1, Spans: 3, Handed: 2 + 2 + 3, Longest: 3}); reads != want {
		t.Errorf("reads %+v, want %+v", reads, want)
	}
	uncounted := New(text, nil)
	if again := uncounted.Find(re); len(again) != len(matches) || uncounted.Span(0, 2) != got[0] {
		t.Errorf("a Body with nil reads answers otherwise: %v and %q", again, uncounted.Span(0, 2))
	}
	if got[1] != "12" || got[2] != "345" {
		t.Errorf("spans %q, want [ab 12 345]", got)
	}
}
