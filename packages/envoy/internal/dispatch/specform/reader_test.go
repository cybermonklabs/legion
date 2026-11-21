package specform

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// storedMarkdown is markdown as Dispatch stores a version of it: parsed, then rendered again. The
// renderer writes a space at either end of a text as "&#32;", a hard break as a backslash at the
// end of its line, and an id on each typed block.
func storedMarkdown(t *testing.T, markdown string) string {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	stored, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render %q: %v", markdown, err)
	}
	return stored
}

// findingNames is "<kind> <node> <rule>" for each finding, in order.
func findingNames(findings []Finding) []string {
	var names []string
	for _, finding := range findings {
		names = append(names, fmt.Sprintf("%s %s %s", finding.Kind, finding.Node, finding.Rule))
	}
	return names
}

// The rules read the blocks of the tree that the application reads, and the text that a person
// reads in them, from the markdown that Dispatch stores: a character reference is its character,
// a hard break is white space between two words, and a block in a block quote or a list item is a
// block. Each row names the findings, and the text whose body the person reads, so a row with no
// finding still shows that the text was read.
func TestTheRulesReadTheTextThatThePersonReads(t *testing.T) {
	const year = "Dispatch keeps each record for one year."
	for _, test := range []struct {
		name, markdown string
		want           []string
		node           string
		kind           Kind
		body           string
	}{
		{"the control", ":::requirement{#control}\n" + year + "\n:::\n", nil, "control", KindStatement, year},
		{"a space at the end", ":::requirement{#trailing}\n" + year + "&#32;\n:::\n", nil, "trailing", KindStatement, year},
		{"a space at the start", ":::requirement{#leading}\n&#32;" + year + "\n:::\n", nil, "leading", KindStatement, year},
		{"an entry line whose bold text ends in a space", "- **" + year + "&#32;** `trail-entry` · under: root\n", nil, "trail-entry", KindStatement, year},
		{"a claim that ends in a space", ":::check{#c1 run=\"Run the test.\"}\nThe test passes when Dispatch keeps the record.&#32;\n:::\n",
			nil, "c1", KindClaim, "The test passes when Dispatch keeps the record."},
		{"two stars that no strong text takes", ":::requirement{#stars}\nDispatch keeps**&#32;each** record for one year.\n:::\n",
			nil, "stars", KindStatement, "Dispatch keeps** each** record for one year."},
		{"a tilde", ":::requirement{#tilde}\nDispatch answers each read in ~2 seconds.\n:::\n",
			nil, "tilde", KindStatement, "Dispatch answers each read in ~2 seconds."},
		{"two sentences of 16 and 15 words with a hard break between them",
			":::requirement{#hard}\nDispatch keeps each record of each issue for one year in the store of its project.\\\n" +
				"Dispatch shows each record of each issue to the person who follows the issue now.\n:::\n",
			nil, "hard", KindStatement,
			"Dispatch keeps each record of each issue for one year in the store of its project. Dispatch shows each record of each issue to the person who follows the issue now."},
		{"the same two sentences on two soft lines",
			":::requirement{#soft}\nDispatch keeps each record of each issue for one year in the store of its project.\n" +
				"Dispatch shows each record of each issue to the person who follows the issue now.\n:::\n",
			nil, "soft", KindStatement,
			"Dispatch keeps each record of each issue for one year in the store of its project. Dispatch shows each record of each issue to the person who follows the issue now."},
		{"three sentences that hard breaks join",
			":::requirement{#hard3}\nDispatch keeps each record.\\\nDispatch shows each record.\\\nDispatch indexes each record.\n:::\n",
			[]string{"statement hard3 sentences"}, "hard3", KindStatement, "Dispatch keeps each record. Dispatch shows each record. Dispatch indexes each record."},
		{"a requirement in a block quote",
			"> :::requirement{#quoted}\n> Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows each comment.\n> :::\n",
			[]string{"statement quoted sentences"}, "quoted", KindStatement, "Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows each comment."},
		{"a check in a list item", "- An item.\n\n  :::check{#listed}\n  Legion merges each change, as #12 says.\n  :::\n",
			[]string{"claim listed pull-request"}, "listed", KindClaim, "Legion merges each change, as #12 says."},
		{"a requirement of two paragraphs", ":::requirement{#two}\n" + year + "\n\nDispatch shows it.\n:::\n",
			nil, "two", KindStatement, year + "\nDispatch shows it."},
	} {
		stored := storedMarkdown(t, test.markdown)
		extracted, err := ExtractDocument("spec.md", 1, stored)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		at := slices.IndexFunc(extracted.Texts, func(text Text) bool { return text.Node == test.node && text.Kind == test.kind })
		if at < 0 || extracted.Texts[at].Body != test.body {
			t.Errorf("%s: stored %q gives the texts %#v, want the %s %s with the body %q", test.name, stored, extracted.Texts, test.kind, test.node, test.body)
		}
		got := findingNames(Check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts, Proposed: extracted.Terms}))
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: stored %q gives the findings %q, want %q", test.name, stored, got, test.want)
		}
	}
}

// A typed block written with no id is stored with the id that the parser gives it, and the text is
// named by that id, the id that every reader of the stored document reads.
func TestATextIsNamedByTheIDOfTheStoredBlock(t *testing.T) {
	stored := storedMarkdown(t, ":::requirement{}\nLegion lands a change.\n:::\n")
	tree, err := pmdoc.ParseRendering(stored)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	got, err := ExtractDocument("spec.md", 1, stored)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || len(got.Texts) != 1 || got.Texts[0].Node != id {
		t.Fatalf("stored %q gives %#v, want one text named %q", stored, got.Texts, id)
	}
}

// A document that the parser refuses is refused here too, with the document's name: the form
// judges nothing that the application does not read.
func TestADocumentThatTheParserRefusesIsAnError(t *testing.T) {
	if _, err := ExtractDocument("spec.md", 3, "<div>\nblock HTML\n</div>\n"); err == nil {
		t.Fatal("ExtractDocument of block HTML: no error, want the parser's refusal")
	}
}
