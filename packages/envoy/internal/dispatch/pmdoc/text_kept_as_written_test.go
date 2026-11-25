package pmdoc

import (
	"strings"
	"testing"
)

// A `<` that opens no HTML tag and no autolink, and a backtick that no other backtick closes, are
// written as they are: the store adds no backslash before either, so the stored text reads back
// as the writer sent it and a find quoting it as sent matches (INFRA-4345 R11 and R12, from
// production's markdown round trip on 2026-11-23). A `<` that does open one, and a backtick run
// that another closes, are still escaped, and every row reads back alike in Go and in the browser
// editor's engine.
func TestALessThanThatOpensNothingAndALoneBacktickAreWrittenAsTheyAre(t *testing.T) {
	t.Parallel()
	asIs := []string{
		// R11's two escaped shapes, from INFRA-4675's item f233 and the sentence after it.
		"Quote: <at most 2 x measured peak: peak + a margin you name>\n",
		"Set it to <at most 2x> here.\n",
		// R11's five kept shapes, which the round trip kept before this change too.
		"Use <KEY> here.\n",
		"Use <div> here.\n",
		"Use <a b> here.\n",
		"Is a < b here?\n",
		"Assign x<-y here.\n",
		// R12's two sentences.
		"A lone ` backtick in text.\n",
		"He said \"use `a` and ` b\" now.\n",
		// A lone run of two, and a lone run after a code span of another length.
		"Two `` backticks with no partner.\n",
		"Code ``a`b`` then ` alone.\n",
	}
	escaped := []struct{ text, written string }{
		// A text `<` that opens an open tag or an autolink with what follows it, and backtick
		// runs that would pair, as a typed text holds them.
		{"Use <div> here.", "Use \\<div> here.\n"},
		{"Use <span title=x> here.", "Use \\<span title=x> here.\n"},
		{"See <ab-c:z> now.", "See \\<ab-c:z> now.\n"},
		{"literal `not code` here", "literal \\`not code\\` here\n"},
		{"two ``a`` runs", "two \\`\\`a\\`\\` runs\n"},
		// A lone text backtick before a code span whose fence is as long pairs with that fence.
		{"x ` y", ""},
	}
	var markdowns []string
	var docs []*Node
	for _, markdown := range asIs {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
		}
		if rendered != markdown {
			t.Errorf("%q is written back as %q, want it as it is", markdown, rendered)
		}
		markdowns, docs = append(markdowns, rendered), append(docs, doc)
	}
	for _, row := range escaped {
		nodes := []*Node{{Type: "text", Text: row.text}}
		if row.written == "" {
			// The text, then code whose fence is one backtick, as `x ` y` then `` `z` `` reads.
			nodes = append(nodes, &Node{Type: "text", Text: "z", Marks: []Mark{{Type: "inlineCode"}}})
		}
		doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: nodes}}}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(%q) = %v", row.text, err)
		}
		if row.written != "" && rendered != row.written {
			t.Errorf("%q is written %q, want %q", row.text, rendered, row.written)
		}
		if row.written == "" && !strings.HasPrefix(rendered, "x \\` y`") {
			t.Errorf("a lone backtick before a code span is written %q, want it escaped", rendered)
		}
		markdowns, docs = append(markdowns, rendered), append(docs, doc)
	}
	readings := engineReadings(t, markdowns)
	for index, markdown := range markdowns {
		back, err := Parse(markdown)
		if err != nil || !back.Equal(docs[index]) {
			t.Errorf("%q reads back in Go as %s (%v), want %s", markdown, readingSummary(back), err, readingSummary(docs[index]))
		}
		if readings[index] == nil || !readings[index].Equal(docs[index]) {
			t.Errorf("%q reads in the engine as %s, want %s", markdown, readingSummary(readings[index]), readingSummary(docs[index]))
		}
	}
}

// A `<` in a typed block's attribute is written as it is, so the attribute reads back as `<` and
// never as `&lt;` (INFRA-4345 R11: a check's run attribute `a < b` read back as `a &lt; b` on
// 2026-11-23). The parse reads it back as the same value, as the engine does.
func TestALessThanInATypedBlockAttributeIsWrittenAsItIs(t *testing.T) {
	t.Parallel()
	const markdown = ":::check{#c-x run=\"a < b and <at most 2x\" negative_control=\"x<-y\"}\nA check.\n:::\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `run="a < b and <at most 2x"`) || !strings.Contains(rendered, `negative_control="x<-y"`) {
		t.Fatalf("the check is written\n%s\nwant its attributes with `<` as it is", rendered)
	}
	if strings.Contains(rendered, "&lt;") {
		t.Fatalf("the check is written with `&lt;`:\n%s", rendered)
	}
	back, err := Parse(rendered)
	if err != nil || !back.Equal(doc) {
		t.Fatalf("%q reads back as %s (%v), want %s", rendered, readingSummary(back), err, readingSummary(doc))
	}
	readings := engineReadings(t, []string{rendered})
	if readings[0] == nil || !readings[0].Equal(doc) {
		t.Fatalf("%q reads in the engine as %s, want %s", rendered, readingSummary(readings[0]), readingSummary(doc))
	}
}
