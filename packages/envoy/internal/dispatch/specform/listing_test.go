package specform

import (
	"slices"
	"strings"
	"testing"
)

// Listing writes the lines that the read route answers and `dispatch spec-form` prints, so the
// client holds no copy of their words. Each finding's line is the line the refusal writes for it
// (the rows read Message's own lines), the head counts the texts as the refusal's head does, and a
// read that lists fewer findings than it counts says how many it lists and counts the texts "or
// more". A document whose approval nothing refuses gets no head.
func TestListingWritesTheLinesOfTheRefusal(t *testing.T) {
	two := breaking(2)
	faults := []Finding{two[0], two[0]}
	faults[1].Detail = "a sentence has 26 words, and a sentence has at most 25"
	// lines is the finding lines of the refusal of findings: its lines but the head.
	lines := func(findings []Finding) []string {
		return strings.Split(Message(ActRequest, findings, len(findings), "").Refusal, "\n")[1:]
	}
	for _, row := range []struct {
		name     string
		refuses  bool
		findings []Finding
		total    int
		want     []string
	}{
		{name: "two texts", refuses: true, findings: two, total: 2, want: append([]string{
			"Dispatch refuses an approval request of spec.md version 12, because 2 texts break the form of The Spec.",
		}, lines(two)...)},
		{name: "one text with two faults", refuses: true, findings: faults, total: 2, want: append([]string{
			"Dispatch refuses an approval request of spec.md version 12, because 1 text breaks the form of The Spec.",
		}, lines(faults)...)},
		{name: "fewer findings than it counts", refuses: true, findings: two, total: 3, want: append(append([]string{
			"Dispatch refuses an approval request of spec.md version 12, because 2 texts or more break the form of The Spec.",
		}, lines(two)...), "Dispatch lists the first 2 of the 3 findings.")},
		{name: "a document that nothing refuses", findings: two, total: 3, want: append(lines(two), "Dispatch lists the first 2 of the 3 findings.")},
		{name: "no finding", refuses: true, want: []string{}},
	} {
		if got := Listing("spec.md", 12, row.refuses, row.findings, row.total); !slices.Equal(got, row.want) || got == nil {
			t.Errorf("%s: Listing = %q, want %q", row.name, got, row.want)
		}
	}
}
