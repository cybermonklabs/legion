package pmdoc

import (
	"errors"
	"testing"
)

// A reply's quote is characters of its block's plain text, matched exactly: overlapping matches
// are counted, an occurrence counts them from 0, and neither markdown marks, nor a heading's `#`,
// nor a run of white space the text does not hold reads as anything but the characters it is.
// Find reads all three as a quote of the text they stand for, so each refusal has Find's match
// beside it as its control.
func TestFindPlainMatchesTheTextAloneOverlapsIncluded(t *testing.T) {
	t.Parallel()
	parse := func(markdown string) *Node {
		t.Helper()
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return doc
	}
	occurrence := func(index int) *int { return &index }

	overlapping := NewQuoteFinder(parse("aaa\n"))
	for _, row := range []struct {
		occurrence int
		want       Range
	}{{0, Range{From: 1, To: 3}}, {1, Range{From: 2, To: 4}}} {
		if got, err := overlapping.FindPlain("aa", occurrence(row.occurrence)); err != nil || got != row.want {
			t.Fatalf("aa occurrence %d in aaa = %v, %v; want %v", row.occurrence, got, err, row.want)
		}
	}
	var ambiguous *ErrTargetAmbiguous
	if _, err := overlapping.FindPlain("aa", nil); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("aa in aaa with no occurrence = %v, want *ErrTargetAmbiguous naming the two overlapping matches", err)
	}
	var past *ErrQuoteNotFound
	if _, err := overlapping.FindPlain("aa", occurrence(2)); !errors.As(err, &past) || past.Matches != 2 {
		t.Fatalf("aa occurrence 2 in aaa = %v, want *ErrQuoteNotFound counting the two matches", err)
	}
	if _, err := overlapping.FindPlain("aa", occurrence(-1)); !errors.As(err, &past) {
		t.Fatalf("aa occurrence -1 in aaa = %v, want *ErrQuoteNotFound", err)
	}

	twice := NewQuoteFinder(parse("brown X brown\n"))
	if got, err := twice.FindPlain("brown", occurrence(1)); err != nil || got != (Range{From: 1 + len16("brown X "), To: 1 + len16("brown X brown")}) {
		t.Fatalf("brown occurrence 1 = %v, %v; want the second brown", got, err)
	}
	if _, err := twice.FindPlain("brown", nil); !errors.As(err, &ambiguous) {
		t.Fatalf("brown twice with no occurrence = %v, want *ErrTargetAmbiguous", err)
	}
	if got, err := twice.FindPlain("X", nil); err != nil || got != (Range{From: 1 + len16("brown "), To: 1 + len16("brown X")}) {
		t.Fatalf("X, which the text holds once, with no occurrence = %v, %v; want its one match", got, err)
	}

	for _, row := range []struct {
		name, markdown, plain, marked string
	}{
		{"bold words", "Ship **bold words** now.\n", "bold words", "**bold words**"},
		{"a heading", "# Heading words\n", "Heading words", "# Heading words"},
		{"white space the text does not hold", "alpha beta\n", "alpha beta", "alpha   beta"},
	} {
		t.Run(row.name, func(t *testing.T) {
			doc := parse(row.markdown)
			finder := NewQuoteFinder(doc)
			if _, err := finder.FindPlain(row.plain, nil); err != nil {
				t.Fatalf("the plain text %q of %q: %v", row.plain, row.markdown, err)
			}
			var missing *ErrQuoteNotFound
			if _, err := finder.FindPlain(row.marked, nil); !errors.As(err, &missing) || missing.Matches != 0 {
				t.Fatalf("%q in %q = %v, want *ErrQuoteNotFound: its characters are not the text's", row.marked, row.markdown, err)
			}
			if _, err := FindQuote(doc, row.marked, nil, nil); err != nil {
				t.Fatalf("control: Find reads %q in %q as the text it stands for: %v", row.marked, row.markdown, err)
			}
		})
	}
}
