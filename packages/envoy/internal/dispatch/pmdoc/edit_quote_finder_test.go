package pmdoc

import (
	"errors"
	"testing"
)

// An edit's finder counts only a quote's matches inside one textblock, on each path a document
// edit resolves a quote by: the plain cascade and its markdown reading (FindSpelling), the reading
// as written that a list-marker quote takes first (FindWritten), and the item a list marker selects
// (FindListItem). Each document holds the quote once inside a paragraph and once across the join of
// two list items, which a comment's anchor still counts as a second place (Find); the edit's finder
// resolves the quote to the paragraph's match. A list marker's selection is an item's first
// paragraph from its start, so it never spans blocks and both finders count it the same
// (dispatch://INFRA-4345, R1).
func TestAnEditFinderCountsNoMatchThatSpansTextblocks(t *testing.T) {
	for _, test := range []struct {
		name, markdown, quote string
		find                  func(*QuoteFinder, string) (Range, error)
	}{
		{
			name:     "the plain cascade",
			markdown: "Waits on (the gate on v46). The workflows run.\n\n- Passes (the gate on v46).\n- The errors are read.\n",
			quote:    "v46). The",
			find: func(f *QuoteFinder, quote string) (Range, error) {
				r, _, err := f.FindSpelling(quote, nil, nil)
				return r, err
			},
		},
		{
			name:     "the markdown reading",
			markdown: "Waits on (the gate on v46). **The** workflows run.\n\n- Passes (the gate on v46).\n- **The** errors are read.\n",
			quote:    "v46). **The**",
			find: func(f *QuoteFinder, quote string) (Range, error) {
				r, _, err := f.FindSpelling(quote, nil, nil)
				return r, err
			},
		},
		{
			name:     "the reading as written",
			markdown: "Waits on - v46). The workflows run.\n\n- Passes - v46).\n- The errors are read.\n",
			quote:    "- v46). The",
			find: func(f *QuoteFinder, quote string) (Range, error) {
				return f.FindWritten(quote, nil)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := parseFixture(t, test.markdown)
			var ambiguous *ErrTargetAmbiguous
			if _, err := test.find(NewQuoteFinder(doc), test.quote); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
				t.Fatalf("the anchor finder = %v, want the paragraph and the list's join as two places", err)
			}
			got, err := test.find(validOf(t, doc).EditQuoteFinder(), test.quote)
			if err != nil {
				t.Fatalf("the edit finder = %v, want the paragraph's one match", err)
			}
			if TargetSpansBlocks(doc, got) || got.To > Size(doc.Children[0]) {
				t.Fatalf("the edit finder resolved %v, want a range inside the first paragraph (size %d)", got, Size(doc.Children[0]))
			}
		})
	}
	doc := parseFixture(t, "The release.\n\n- The first passes.\n- The errors are read.\n")
	for name, finder := range map[string]*QuoteFinder{"anchor": NewQuoteFinder(doc), "edit": validOf(t, doc).EditQuoteFinder()} {
		var ambiguous *ErrTargetAmbiguous
		if _, _, err := finder.FindListItem("- The", nil); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
			t.Fatalf("the %s finder's selection of `- The` = %v, want the two items", name, err)
		}
	}
}

// A quote whose only matches span textblocks resolves, for an edit's finder, to the first of them,
// whatever occurrence it names, so the edit refuses it as spanning blocks rather than as ambiguous
// or out of range; and one with several matches inside textblocks is ambiguous among those alone.
func TestAnEditFinderResolvesAQuoteOfSpansAloneToTheFirst(t *testing.T) {
	doc := parseFixture(t, "- Passes (the gate on v46).\n- The errors are read.\n\n- Fails (the gate on v46).\n- The errors are kept.\n")
	finder := validOf(t, doc).EditQuoteFinder()
	var ambiguous *ErrTargetAmbiguous
	if _, err := NewQuoteFinder(doc).Find("v46). The", nil, nil); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("the anchor finder = %v, want both joins as two places", err)
	}
	first, err := finder.Find("v46). The", nil, nil)
	if err != nil || !TargetSpansBlocks(doc, first) {
		t.Fatalf("the edit finder = %v, %v; want the first join, which spans blocks", first, err)
	}
	occurrence := 1
	if got, err := finder.Find("v46). The", &occurrence, nil); err != nil || got != first {
		t.Fatalf("occurrence 1 = %v, %v; want the first join, %v", got, err, first)
	}
	if _, err := finder.Find("(the gate on v46).", nil, nil); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("a quote inside two items = %v, want them as two places", err)
	}
}
