package baseline

import "testing"

func TestEachPatternMatchesItsPhraseAndNotAPlainSentence(t *testing.T) {
	t.Parallel()
	if len(Patterns) != 6 {
		t.Fatalf("%d patterns, want six", len(Patterns))
	}
	for i, text := range []string{
		"We landed the PR before lunch.",
		"This unblocks the release.",
		"It names the owner.",
		"The cache pays for itself.",
		"Carry the finding to review.",
		"The graph SHOWS the drop.",
	} {
		if !Patterns[i].MatchString(text) {
			t.Errorf("pattern %d (%s) does not match %q", i, Patterns[i], text)
		}
		if !Matches(text) {
			t.Errorf("Matches(%q) is false", text)
		}
	}
	for _, text := range []string{
		"The landing page loads in 2 seconds.",
		"The record names Alice as the holder.",
		"Mia carries the pager this week.",
		"The table lists 42 rows.",
	} {
		if Matches(text) {
			t.Errorf("Matches(%q) is true, want false", text)
		}
	}
}
