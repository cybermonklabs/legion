package specchange

import (
	"strings"
	"testing"
)

// An outcome line is its text with at most one coverage marker in front: the approved statement
// that requires it, or that it is restored for the conclusion's statement. A line with no marker
// is kept as written, and a marker that names no statement id, or names no outcome after it, is
// refused rather than read as some other outcome.
func TestParseOutcomeReadsEachLineForm(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		line string
		want Outcome
	}{
		{"(requires an-order-is-paid-once) A second payment is refused.", Outcome{Text: "A second payment is refused.", Statement: "an-order-is-paid-once"}},
		{"(restored) A second payment of one order is refused again.", Outcome{Text: "A second payment of one order is refused again.", Restored: true}},
		{"The refusal names the order.", Outcome{Text: "The refusal names the order."}},
		{"  (restored)   A spaced outcome.  ", Outcome{Text: "A spaced outcome.", Restored: true}},
		{"(requires v2.bill_total-1) The total is shown.", Outcome{Text: "The total is shown.", Statement: "v2.bill_total-1"}},
		{"(Optional) A note in parentheses is text.", Outcome{Text: "(Optional) A note in parentheses is text."}},
	} {
		got, err := ParseOutcome(test.line)
		if err != nil {
			t.Errorf("ParseOutcome(%q) refused it: %v", test.line, err)
			continue
		}
		if got != test.want {
			t.Errorf("ParseOutcome(%q) = %+v, want %+v", test.line, got, test.want)
		}
	}
	for _, line := range []string{
		"",
		"   ",
		"(requires) A statement with no id.",
		"(requires Not-An-Id) An id with capitals.",
		"(requires two words) An id with a space.",
		"(requires an-order-is-paid-once)",
		"(requires an-order-is-paid-once)   ",
		"(restored)",
		"(restored)  ",
	} {
		if got, err := ParseOutcome(line); err == nil {
			t.Errorf("ParseOutcome(%q) = %+v, want a refusal", line, got)
		}
	}
}

// The line form of an outcome reads back as the same outcome.
func TestOutcomeLineReadsBackAsTheSameOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []Outcome{
		{Text: "A second payment is refused.", Statement: "an-order-is-paid-once"},
		{Text: "A second payment is refused again.", Restored: true},
		{Text: "The refusal names the order."},
	} {
		line := outcome.Line()
		got, err := ParseOutcome(line)
		if err != nil || got != outcome {
			t.Errorf("%+v reads as the line %q, which reads back as %+v (%v)", outcome, line, got, err)
		}
	}
}

// Two outcomes are one outcome when their texts are equal after the space at each end is trimmed,
// each run of space is one space, one full stop at the end is dropped and every letter is lower
// case; the marker is no part of the key.
func TestOutcomeKeyFoldsSpaceCaseAndOneFullStop(t *testing.T) {
	t.Parallel()
	same := []string{"A second payment is refused.", "  a second   payment\tis REFUSED  ", "A second payment is refused", "a second payment is refused. "}
	for _, text := range same[1:] {
		if OutcomeKey(text) != OutcomeKey(same[0]) {
			t.Errorf("OutcomeKey(%q) = %q, want the key of %q, %q", text, OutcomeKey(text), same[0], OutcomeKey(same[0]))
		}
	}
	if got := OutcomeKey("A second payment is refused.."); got != "a second payment is refused." {
		t.Errorf("OutcomeKey drops %q to %q, want one full stop dropped and the other kept", "A second payment is refused..", got)
	}
	if OutcomeKey("A second payment is refused.") == OutcomeKey("A first payment is refused.") {
		t.Errorf("two different outcomes share the key %q", OutcomeKey("A first payment is refused."))
	}
}

// An outcome of the JSON form is refused when its text is blank or holds a line break, when it is
// both required and restored, when its statement is no statement id, and when its text starts with
// a marker, since its line would then read back as another outcome. A list is refused when two of
// its outcomes have one key.
func TestCheckOutcomesRefusesEachFault(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome Outcome
		says    string
	}{
		{"blank text", Outcome{Text: "  "}, "blank"},
		{"a line break", Outcome{Text: "First line.\nSecond line."}, "line break"},
		{"both markers", Outcome{Text: "Both.", Statement: "an-order-is-paid-once", Restored: true}, "both"},
		{"an id that is no id", Outcome{Text: "Bad id.", Statement: "Not An Id"}, "statement id"},
		{"a marker in the text", Outcome{Text: "(restored) A marker in the text."}, "marker"},
	} {
		err := CheckOutcomes([]Outcome{test.outcome})
		if err == nil || !strings.Contains(err.Error(), test.says) {
			t.Errorf("%s: CheckOutcomes(%+v) = %v, want a refusal that says %q", test.name, test.outcome, err, test.says)
		}
	}
	twice := []Outcome{{Text: "A second payment is refused.", Statement: "an-order-is-paid-once"}, {Text: "a second payment is refused", Restored: true}}
	if err := CheckOutcomes(twice); err == nil || !strings.Contains(err.Error(), "one outcome") {
		t.Errorf("CheckOutcomes(%+v) = %v, want two outcomes of one key refused", twice, err)
	}
	if err := CheckOutcomes(nil); err != nil {
		t.Errorf("CheckOutcomes(nil) = %v, want an empty list taken", err)
	}
	fine := []Outcome{{Text: "  A trimmed outcome.  ", Statement: "an-order-is-paid-once"}, {Text: "Another outcome.", Restored: true}, {Text: "A third."}}
	if err := CheckOutcomes(fine); err != nil {
		t.Errorf("CheckOutcomes(%+v) = %v, want it taken", fine, err)
	}
}
