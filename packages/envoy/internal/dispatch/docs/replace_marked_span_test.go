package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A replace whose find lies inside a marked span writes its with inside that span, so the span
// stays whole and the text around the find is written as it was. The report is
// dispatch://INFRA-1387/comment/dc3980fd-1ce6-4196-8b5b-ab6cc569a612, with the repro in
// dispatch://OSSV-690/artifact/ossv690-doc-edit-test-md: replaced inside a bold span, `said`
// split the span and wrote its edge characters as entities (`**Sami&#32;**&#97;nswered…`), and
// replaced over a bold span's whole text, `Sami bold` lost the bold. The rows after the repro's
// are the same fault at a span's start and end, in a link, italic, strikethrough and code, and
// the bounds of the rule: a with that writes the span's mark itself, and a find that spells the
// span's markup, over its whole text or across its edge, which leaves the mark to the with. A
// link's markup is spelled with its own URL and title: a titled link's find spelled with its
// title, and one whose title holds a quote and a backslash, which the with writes back so the
// stored markdown reads it as the renderer writes it and the parser reads it; and a find that is
// one link alone, spelled with its own URL, and a titled one with its URL and title, each of
// whose with writes a link to another URL, which is the one the document then holds. Since a with
// that writes a link keeps its own, those two would read the same had the document's link been
// kept, so the next row is the first find under a with of plain text, which then holds no link,
// and the row after it a find inside a link spelled under the link's own URL, whose with is then
// written outside the link. A bare URL and a bare www link are their own markup, so a find that
// covers one whole across its edge spells it; and a find that is a bare link's own text alone,
// matched as written, spells no markup, so its with is written inside the link. A find that
// spells the span's markup over part of the text it covers (`a **b** c` over `**a b c**`) spells
// it over that part alone, so the span keeps its mark. A find that runs past a span's edge
// without spelling its markup, or spells with another URL or title a link its replace would
// remove whole, is refused (TestReplaceAcrossAMarkedSpansEdgeIsRefused). Then a find that matched
// the text as written, whose `*`, `_` or backtick is text the span holds and no markup: `*`, `_`
// and a backtick inside a bold, an italic and a code span, the review's two finds (`2*3*4` in
// italic, `2**3**4` in bold), and `snake_case_name`, whose underscores no reading takes for markup.
// Each row writes one replace and reads it back three ways: the markdown the version stored, the
// document a caller reads, and the text of the paragraph the replace wrote in.
func TestReplaceInsideAMarkedSpanKeepsTheSpanWhole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, markdown, find, with, want, text string }{
		{
			name:     "the repro's find over a bold span's whole text",
			markdown: "Sami wrote [this](https://example.com/Sami) and **Sami bold**.\n",
			find:     "Sami bold",
			with:     "Sami answered bold",
			want:     "Sami wrote [this](https://example.com/Sami) and **Sami answered bold**.\n",
			text:     "Sami wrote this and Sami answered bold.",
		},
		{
			name:     "the repro's find inside a bold span",
			markdown: "Sami wrote [this](https://example.com/Sami) and **Sami said bold** now.\n",
			find:     "said",
			with:     "answered (https://example.com/x)",
			want:     "Sami wrote [this](https://example.com/Sami) and **Sami answered (https://example.com/x) bold** now.\n",
			text:     "Sami wrote this and Sami answered (https://example.com/x) bold now.",
		},
		{
			name:     "a find at a bold span's start",
			markdown: "**Sami said bold** now.\n",
			find:     "Sami",
			with:     "Ann",
			want:     "**Ann said bold** now.\n",
			text:     "Ann said bold now.",
		},
		{
			name:     "a find at a bold span's end",
			markdown: "**Sami said bold** now.\n",
			find:     "bold",
			with:     "loud",
			want:     "**Sami said loud** now.\n",
			text:     "Sami said loud now.",
		},
		{
			name:     "a find inside a link's text",
			markdown: "Sami wrote [this page](https://example.com/Sami) today.\n",
			find:     "page",
			with:     "note",
			want:     "Sami wrote [this note](https://example.com/Sami) today.\n",
			text:     "Sami wrote this note today.",
		},
		{
			name:     "a find over a link's whole text",
			markdown: "Sami wrote [this page](https://example.com/Sami) today.\n",
			find:     "this page",
			with:     "that note",
			want:     "Sami wrote [that note](https://example.com/Sami) today.\n",
			text:     "Sami wrote that note today.",
		},
		{
			name:     "a find inside an italic span",
			markdown: "*Sami said it* now.\n",
			find:     "said",
			with:     "answered",
			want:     "*Sami answered it* now.\n",
			text:     "Sami answered it now.",
		},
		{
			name:     "a find inside a struck span",
			markdown: "~~Sami said it~~ now.\n",
			find:     "said",
			with:     "answered",
			want:     "~~Sami answered it~~ now.\n",
			text:     "Sami answered it now.",
		},
		{
			name:     "a find inside a code span",
			markdown: "Run `make said all` now.\n",
			find:     "said",
			with:     "test",
			want:     "Run `make test all` now.\n",
			text:     "Run make test all now.",
		},
		{
			name:     "a find inside a code span that holds quotes, a caret, a dollar and pipes",
			markdown: "Run `go test ... -run '^(A|B|TestStreamsEnsureCreatesOnlyTheMeasureStream|TestStreamsUpdateChangesOnlyReplicas)$' -count=1 -v` now.\n",
			find:     "TestStreamsEnsureCreatesOnlyTheMeasureStream|TestStreamsUpdateChangesOnlyReplicas",
			with:     "TestStreamsUpdateMakesAndRaisesTheMeasureStream|TestStreamsUpdateChangesOnlyReplicas|TestStreamsUpdateMakesNoCopiedBucket",
			want:     "Run `go test ... -run '^(A|B|TestStreamsUpdateMakesAndRaisesTheMeasureStream|TestStreamsUpdateChangesOnlyReplicas|TestStreamsUpdateMakesNoCopiedBucket)$' -count=1 -v` now.\n",
			text:     "Run go test ... -run '^(A|B|TestStreamsUpdateMakesAndRaisesTheMeasureStream|TestStreamsUpdateChangesOnlyReplicas|TestStreamsUpdateMakesNoCopiedBucket)$' -count=1 -v now.",
		},
		{
			name:     "a with that writes the span's own mark",
			markdown: "**Sami said bold** now.\n",
			find:     "said",
			with:     "**answered**",
			want:     "**Sami answered bold** now.\n",
			text:     "Sami answered bold now.",
		},
		{
			name:     "a find that writes the span's markup leaves the mark to the with",
			markdown: "Sami wrote [this](https://example.com/Sami) and **Sami bold**.\n",
			find:     "**Sami bold**",
			with:     "Sami answered bold",
			want:     "Sami wrote [this](https://example.com/Sami) and Sami answered bold.\n",
			text:     "Sami wrote this and Sami answered bold.",
		},
		{
			name:     "a find that spells a link's markup across its edge leaves the link to the with",
			markdown: "Sami wrote [this](https://example.com/Sami) today.\n",
			find:     "wrote [this](https://example.com/Sami)",
			with:     "wrote that",
			want:     "Sami wrote that today.\n",
			text:     "Sami wrote that today.",
		},
		{
			name:     "a find that spells a titled link's markup with its own title leaves the link to the with",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "wrote [this](https://a.example/1 \"Spec\") today",
			with:     "wrote that today",
			want:     "I wrote that today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find that spells a link's markup with its own title holding a quote and a backslash",
			markdown: "I wrote [this](https://a.example/1 \"say \\\"hi\\\" \\\\ now\") today.\n",
			find:     "wrote [this](https://a.example/1 \"say \\\"hi\\\" \\\\ now\") today",
			with:     "wrote [that](https://a.example/1 \"say \\\"hi\\\" \\\\ now\") today",
			want:     "I wrote [that](https://a.example/1 \"say \\\"hi\\\" \\\\ now\") today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find that is one link alone, spelled with its own URL, holds the link its with writes",
			markdown: "I wrote [this](https://a.example/1) today.\n",
			find:     "[this](https://a.example/1)",
			with:     "[that](https://c.example/3)",
			want:     "I wrote [that](https://c.example/3) today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find that is one link alone, spelled with its own URL and title, holds the link its with writes",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "[this](https://a.example/1 \"Spec\")",
			with:     "[that](https://c.example/3)",
			want:     "I wrote [that](https://c.example/3) today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find that is one link alone, spelled with its own URL, leaves the link to a with of plain text",
			markdown: "I wrote [this](https://a.example/1) today.\n",
			find:     "[this](https://a.example/1)",
			with:     "that",
			want:     "I wrote that today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find inside a link, spelled with the link's own URL, leaves the link of its text to the with",
			markdown: "I wrote [one two](https://a.example/1) today.\n",
			find:     "[one](https://a.example/1)",
			with:     "that",
			want:     "I wrote that[ two](https://a.example/1) today.\n",
			text:     "I wrote that two today.",
		},
		{
			name:     "a find that covers a bare URL whole across its edge leaves the link to the with",
			markdown: "Read the docs at https://example.com/x today.\n",
			find:     "docs at https://example.com/x",
			with:     "docs at https://example.com/y",
			want:     "Read the docs at https://example.com/y today.\n",
			text:     "Read the docs at https://example.com/y today.",
		},
		{
			name:     "a find that covers a bare www link whole across its edge leaves the link to the with",
			markdown: "See www.example.com now.\n",
			find:     "See www.example.com",
			with:     "See www.example.org",
			want:     "See www.example.org now.\n",
			text:     "See www.example.org now.",
		},
		{
			name:     "a find that is a bare link's own text alone writes its with inside the link",
			markdown: "Read https://a.example/1 today.\n",
			find:     "https://a.example/1",
			with:     "that",
			want:     "Read [that](https://a.example/1) today.\n",
			text:     "Read that today.",
		},
		{
			name:     "a find that spells a span's markup over part of the text it covers keeps the span",
			markdown: "**a b c** now.\n",
			find:     "a **b** c",
			with:     "x",
			want:     "**x** now.\n",
			text:     "x now.",
		},
		{
			name:     "a literal * inside a bold span",
			markdown: "**a \\*\\*b\\*\\* c** now.\n",
			find:     "**b**",
			with:     "x",
			want:     "**a x c** now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal _ inside a bold span",
			markdown: "**a \\_\\_b\\_\\_ c** now.\n",
			find:     "__b__",
			with:     "x",
			want:     "**a x c** now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal backtick inside a bold span",
			markdown: "**a \\`b\\` c** now.\n",
			find:     "`b`",
			with:     "x",
			want:     "**a x c** now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal * inside an italic span",
			markdown: "*a \\*b\\* c* now.\n",
			find:     "*b*",
			with:     "x",
			want:     "*a x c* now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal _ inside an italic span",
			markdown: "*a \\_b\\_ c* now.\n",
			find:     "_b_",
			with:     "x",
			want:     "*a x c* now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal backtick inside an italic span",
			markdown: "*a \\`b\\` c* now.\n",
			find:     "`b`",
			with:     "x",
			want:     "*a x c* now.\n",
			text:     "a x c now.",
		},
		{
			name:     "a literal * inside a code span",
			markdown: "Run `make *b* all` now.\n",
			find:     "*b*",
			with:     "x",
			want:     "Run `make x all` now.\n",
			text:     "Run make x all now.",
		},
		{
			name:     "a literal _ inside a code span",
			markdown: "Run `make _b_ all` now.\n",
			find:     "_b_",
			with:     "x",
			want:     "Run `make x all` now.\n",
			text:     "Run make x all now.",
		},
		{
			name:     "a literal backtick inside a code span",
			markdown: "Run ``make `b` all`` now.\n",
			find:     "`b`",
			with:     "x",
			want:     "Run `make x all` now.\n",
			text:     "Run make x all now.",
		},
		{
			name:     "the review's literal asterisks inside an italic span",
			markdown: "*a 2\\*3\\*4 b* now.\n",
			find:     "2*3*4",
			with:     "x",
			want:     "*a x b* now.\n",
			text:     "a x b now.",
		},
		{
			name:     "the review's literal double asterisks inside a bold span",
			markdown: "**a 2\\*\\*3\\*\\*4 b**\n",
			find:     "2**3**4",
			with:     "x",
			want:     "**a x b**\n",
			text:     "a x b",
		},
		{
			name:     "underscores inside a word inside an italic span",
			markdown: "*a snake_case_name b*\n",
			find:     "snake_case_name",
			with:     "x",
			want:     "*a x b*\n",
			text:     "a x b",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
				{Op: "replace", Find: test.find, With: test.with},
			}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
				t.Fatalf("replace %q with %q: %v", test.find, test.with, err)
			}
			requireDocument(t, service, artifactID, test.want, fmt.Sprintf("replace %q with %q in %q", test.find, test.with, test.markdown))
			if text := pmdoc.TextContent(persistedProofTree(t, service.store, artifactID).Children[0]); text != test.text {
				t.Fatalf("replace %q with %q in %q leaves the paragraph's text %q, want %q", test.find, test.with, test.markdown, text, test.text)
			}
		})
	}
}

// A replace whose find crosses a marked span's edge - it covers text inside the span and text
// outside it - is refused, and the document is left as it was: text written over such a find
// lands outside the span, so a link it covered would go with its URL, and a bold, italic or struck
// span the write cut to end on a space would be written with character references
// (`**Sami said&#32;**loud`). The refusal names the span and its kind (ErrFindCrossesSpan).
// TestARefusedFindNamesTheFindThatKeepsItsRange holds each span's edge with the refusal's whole
// text; the rows here are the shapes it does not. A find that spells a bold span's markup over
// only some of the span's text it covers (`**bo**ld now`; one that spells it over all of that text
// is taken, TestAReplaceFindThatSpellsASpansOwnMarkupOverThePartItCoversIsTaken), and a find past a
// bold span's end at an apostrophe (`'s bold` in `**Sami's** bold`). A find that spells one bold
// span's markup and covers a second bold span whose markup it does not spell, which crosses the
// second span: each span is weighed alone. Then a find that spells a link with another URL or title
// than it has over the link's whole text, which its replace would remove whole: a longer find with
// the link's URL and not its title, as a find copied from an older read after another writer gave
// the link a title; a find over a link's whole text and nothing more that spells only part of it as
// another link; and a longer find spelling part of a link's text as another link. Each names the
// link the document holds, which a find over its whole text spells with the link's own URL and
// title (pmdoc.TextblockAt.SpanOver). A longer find's refusal says it covers text outside the link
// and spells the link with another URL or title; one over the link's whole text and nothing more
// says it spells the link with another URL or title, and nothing of text outside it. A find over
// part of a link that spells it with another URL or title, whose replace removes only that part,
// is taken (TestARefusedFindNamesTheFindThatKeepsItsRange).
func TestReplaceAcrossAMarkedSpansEdgeIsRefused(t *testing.T) {
	t.Parallel()
	// acrossAnotherLink is what the refusal of a longer find spelling a link with another URL or
	// title says: why it crosses the link, not what text written over it would do.
	const acrossAnotherLink = "text outside it, and it spells the link with another URL or title than the one it has"
	for _, test := range []struct {
		name, markdown, find, with, kind, span string
		// markup is the span's markup as the document holds it, where the row reads it.
		markup string
		// says is words the refusal's message holds, and unsaid words it does not hold, where the
		// row reads them.
		says, unsaid string
	}{
		{
			name:     "a find that spells a bold span's markup over only some of the span's text it covers",
			markdown: "**Sami said bold** now.\n",
			find:     "**bo**ld now",
			with:     "loud then",
			kind:     "bold",
			span:     "Sami said bold",
		},
		{
			name:     "a find across a bold span's end at an apostrophe",
			markdown: "**Sami's** bold now.\n",
			find:     "'s bold",
			with:     "'s loud",
			kind:     "bold",
			span:     "Sami's",
		},
		{
			name:     "a find that spells one bold span's markup and covers a second",
			markdown: "**a** and **b** now.\n",
			find:     "**a** and b",
			with:     "x",
			kind:     "bold",
			span:     "b",
		},
		{
			name:     "a find that spells a link's markup without the title it has",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "wrote [this](https://a.example/1) today",
			with:     "wrote that today",
			kind:     "link",
			span:     "this",
			markup:   "[this](https://a.example/1 \"Spec\")",
			says:     acrossAnotherLink,
		},
		{
			name:     "a find over a link's whole text alone that spells part of it as another link",
			markdown: "I wrote [one two](https://a.example/1) today.\n",
			find:     "[one](https://b.example/2) two",
			with:     "that",
			kind:     "link",
			span:     "one two",
			markup:   "[one two](https://a.example/1)",
			says:     "another URL or title",
			unsaid:   "text outside it",
		},
		{
			name:     "a longer find that spells part of a link's text as another link",
			markdown: "I wrote [one two](https://a.example/1) today.\n",
			find:     "wrote [one](https://b.example/2) two today",
			with:     "wrote that today",
			kind:     "link",
			span:     "one two",
			markup:   "[one two](https://a.example/1)",
			says:     acrossAnotherLink,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			before, _ := latestVersionOf(t, service.store, artifactID)
			_, err := joinedApplyOps(service, artifactID, []model.EditOp{
				{Op: "replace", Find: test.find, With: test.with},
			}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
			var crossed *ErrFindCrossesSpan
			if !errors.As(err, &crossed) || crossed.Kind() != test.kind || crossed.Span.Text != test.span || (test.markup != "" && spanMarkup(t, service, artifactID, crossed.Span) != test.markup) {
				t.Fatalf("replace %q with %q in %q = %v, want it refused for crossing the %s %q", test.find, test.with, test.markdown, err, test.kind, test.span)
			}
			if message := err.Error(); (test.says != "" && !strings.Contains(message, test.says)) || (test.unsaid != "" && strings.Contains(message, test.unsaid)) {
				t.Fatalf("refusal = %q, want it to say %q and not %q", message, test.says, test.unsaid)
			}
			requireUnchanged(t, service, artifactID, before, test.markdown, fmt.Sprintf("the refused replace %q", test.find))
		})
	}
}

// A replace's find that runs past a span's edge and spells the span's own markup over all of the
// span's text it covers is taken, whatever the span: a link, bold, italic or code each take the
// one rule (pmdoc.TextblockAt.CrossedSpan). The with is written over the text the find covers,
// outside the span, which keeps the rest of its text and its mark, cut inside a word with no
// character reference; where the part a bold or italic span keeps would meet the with at a space,
// which markdown cannot write bold or italic delimiters beside, the find is refused.
// TestARefusedFindNamesTheFindThatKeepsItsRange sends the find its refusal names for each span's
// cut, an untitled link's among them under a with of plain text; the rows here are the links it
// does not. A titled link under a with of plain text leaves the part of the link the find covered
// unlinked, the rest of the link keeping its URL and title. A with that writes the same link, URL
// and title, joins the rest of the link, untitled or titled: the live paragraph holds one text run
// under that link, not two beside each other (requireDocument). And a find that spells a titled
// link with another title over the part it covers, whose replace removes only that part, writes
// the with outside the link, which keeps the document's URL and title.
func TestAReplaceFindThatSpellsASpansOwnMarkupOverThePartItCoversIsTaken(t *testing.T) {
	t.Parallel()
	const (
		plain  = "I wrote [one two](https://a.example/1) today.\n"
		titled = "I wrote [one two](https://a.example/1 \"Spec\") today.\n"
	)
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, test := range []struct {
		name, markdown, find, with string
		// want is the document the replace leaves.
		want string
	}{
		{
			name: "a titled link, with plain text", markdown: titled, find: `wrote [one](https://a.example/1 "Spec")`, with: "NEW",
			want: "I NEW[ two](https://a.example/1 \"Spec\") today.\n",
		},
		{
			name: "a link, with written as the same link", markdown: plain, find: "wrote [one](https://a.example/1)", with: "[NEW](https://a.example/1)",
			want: "I [NEW two](https://a.example/1) today.\n",
		},
		{
			name: "a titled link, with written as the same link", markdown: titled, find: `wrote [one](https://a.example/1 "Spec")`,
			with: `[NEW](https://a.example/1 "Spec")`, want: "I [NEW two](https://a.example/1 \"Spec\") today.\n",
		},
		{
			name: "a titled link spelled with another title, with plain text", markdown: titled, find: `wrote [one](https://a.example/1 "Draft")`, with: "NEW",
			want: "I NEW[ two](https://a.example/1 \"Spec\") today.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: test.find, With: test.with}}, actor, nil); err != nil {
				t.Fatalf("replace %q with %q in %q: %v, want it taken", test.find, test.with, test.markdown, err)
			}
			requireDocument(t, service, artifactID, test.want, fmt.Sprintf("replace %q with %q in %q", test.find, test.with, test.markdown))
		})
	}
}

// linkDocument is the paragraph whose link each of linkSpellings spells.
const linkDocument = "I wrote [this one](https://a.example/1 \"Spec\") today.\n"

// linkSpellings are the ways a quote spells the link of linkDocument: as the document holds it or
// with another URL or title, alone, inside the link, over part of it and the text before it, or in
// a longer quote, and with no link markup at all. deleted is the document a delete of the quote
// leaves, and "" where the delete is refused: the quote spells the link with another URL or title
// over its whole text, so the delete would remove a link the quote spells otherwise
// (TestARefusedFindNamesTheFindThatKeepsItsRange holds each such shape and its refusal).
var linkSpellings = []struct{ name, quote, deleted string }{
	{name: "the link alone, spelled with another URL", quote: `[this one](https://b.example/2 "Spec")`},
	{name: "the link alone, spelled with another title", quote: `[this one](https://a.example/1 "Draft")`},
	{name: "a longer quote spelling the link with another URL", quote: `wrote [this one](https://b.example/2 "Spec") today`},
	{name: "a longer quote spelling the link with another title", quote: `wrote [this one](https://a.example/1 "Draft") today`},
	{
		name: "a quote inside the link spelling its text as another link", quote: `[this](https://b.example/2 "Spec")`,
		deleted: "I wrote [ one](https://a.example/1 \"Spec\") today.\n",
	},
	{
		name: "a quote over part of the link and the text before it, spelling that part as another link", quote: `wrote [this](https://b.example/2 "Spec")`,
		deleted: "I [ one](https://a.example/1 \"Spec\") today.\n",
	},
	{name: "the link alone, spelled as it is", quote: `[this one](https://a.example/1 "Spec")`, deleted: "I wrote  today.\n"},
	{name: "a longer quote spelling the link as it is", quote: `wrote [this one](https://a.example/1 "Spec") today`, deleted: "I .\n"},
	{
		name: "a quote inside the link under the link's own URL and title", quote: `[this](https://a.example/1 "Spec")`,
		deleted: "I wrote [ one](https://a.example/1 \"Spec\") today.\n",
	},
	{name: "a quote inside the link with no link markup", quote: "this", deleted: "I wrote [ one](https://a.example/1 \"Spec\") today.\n"},
	{
		name: "a quote over part of the link and the text before it, under the link's own URL and title", quote: `wrote [this](https://a.example/1 "Spec")`,
		deleted: "I [ one](https://a.example/1 \"Spec\") today.\n",
	},
	{name: "a quote over part of the link and the text before it, with no link markup", quote: "wrote this", deleted: "I [ one](https://a.example/1 \"Spec\") today.\n"},
}

// A find names text to change, so a delete's spells a link as a replace's does: a link the delete
// removes whole must be spelled as it is wherever the find spells it, and a delete that would
// remove one the find spells otherwise is refused (TestARefusedFindNamesTheFindThatKeepsItsRange).
// Each other spelling of linkSpellings deletes exactly the text it named: the link as the document
// holds it, alone or in a longer find, and a find over part of the link, inside it or with the
// text before it, whatever URL or title it spells the link with, since that delete removes only
// that part, and the rest of the link keeps the document's URL and title.
func TestADeleteOfAFindThatSpellsALinkDeletesWhatItNamed(t *testing.T) {
	t.Parallel()
	for _, spelling := range linkSpellings {
		if spelling.deleted == "" {
			continue
		}
		t.Run(spelling.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, linkDocument)
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Find: spelling.quote}},
				model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
				t.Fatalf("delete %q in %q: %v, want it taken", spelling.quote, linkDocument, err)
			}
			requireDocument(t, service, artifactID, spelling.deleted, fmt.Sprintf("delete %q in %q", spelling.quote, linkDocument))
		})
	}
}

// An insert's or a move's anchor names a place, not text to change, so it finds its block whatever
// it spells a link with: each of linkSpellings, as the document holds the link or with another URL
// or title, alone, inside the link, over part of it and the text before it, or in a longer quote,
// is taken, the insert and the moved block land after the paragraph, and the link stays as the
// document holds it.
func TestAnAnchorFindsItsBlockWhateverItSpellsALinkWith(t *testing.T) {
	t.Parallel()
	const markdown = "Moved.\n\n" + linkDocument
	for _, op := range []struct {
		op string
		// edit is the operation anchored after quote, and moved the id of the block `Moved.`.
		edit func(quote, moved string) model.EditOp
		// landed is the document the operation leaves.
		landed string
	}{
		{
			op: "insert",
			edit: func(quote, _ string) model.EditOp {
				return model.EditOp{Op: "insert", After: quote, Markdown: "Inserted."}
			},
			landed: markdown + "\nInserted.\n",
		},
		{
			op: "move",
			edit: func(quote, moved string) model.EditOp {
				return model.EditOp{Op: "move", Block: moved, After: quote}
			},
			landed: linkDocument + "\nMoved.\n",
		},
	} {
		for _, spelling := range linkSpellings {
			t.Run(op.op+" after "+spelling.name, func(t *testing.T) {
				service, artifactID := newTestService(t)
				seedServiceText(t, service, artifactID, markdown)
				moved := blockIDOfText(t, liveTree(t, service, artifactID), "Moved.")
				if _, err := joinedApplyOps(service, artifactID, []model.EditOp{op.edit(spelling.quote, moved)},
					model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
					t.Fatalf("%s after %q in %q: %v, want it taken", op.op, spelling.quote, markdown, err)
				}
				requireDocument(t, service, artifactID, op.landed, fmt.Sprintf("%s after %q in %q", op.op, spelling.quote, markdown))
			})
		}
	}
}

// spanMarkup is the markup a find spells span with over its whole text, the document's own
// (pmdoc.TextblockAt.SpanOver), as the stored document holds it.
func spanMarkup(t *testing.T, service *Service, artifactID string, span pmdoc.Span) string {
	t.Helper()
	at, ok := pmdoc.ContainingTextblock(persistedProofTree(t, service.store, artifactID), span.Range.From)
	if !ok {
		t.Fatalf("no textblock of the stored document holds the span %q at %d", span.Text, span.Range.From)
	}
	markup, err := at.SpanOver(span.Mark, span.Range)
	if err != nil {
		t.Fatalf("spell the span %q: %v", span.Text, err)
	}
	return markup
}

// A delete's find and an insert's or a move's anchor write no text over their match, so the
// shapes a replace's find is refused for crossing a span's edge
// (TestReplaceAcrossAMarkedSpansEdgeIsRefused) are taken from them: a quote across a bold, an
// italic or a code span's end, and plain text over a link (`wrote this` over `wrote [this](…)`),
// which spells no link. The delete removes the text it matched, a bold or an italic span it cut
// to end on a space keeping its delimiter with a character reference; the insert and the moved
// block land after the paragraph, which keeps its spans.
func TestADeleteOrAnAnchorAcrossASpansEdgeIsTaken(t *testing.T) {
	t.Parallel()
	shapes := []struct {
		name, paragraph, quote string
		// deleted is the paragraph the delete of quote leaves.
		deleted string
	}{
		{name: "a quote across a bold span's end", paragraph: "**Sami said bold** now.", quote: "bold now", deleted: "**Sami said&#32;**."},
		{name: "a quote across an italic span's end", paragraph: "*Sami said it* now.", quote: "it now", deleted: "*Sami said&#32;*."},
		{name: "a quote across a code span's end", paragraph: "Run `make said all` now.", quote: "all now", deleted: "Run `make said `."},
		{name: "plain text over a link", paragraph: "I wrote [this](https://a.example/1) today.", quote: "wrote this", deleted: "I  today."},
	}
	ops := []struct {
		op string
		// edit is the operation quoting quote, and moved the id of the block `Moved.`.
		edit func(quote, moved string) model.EditOp
		// want is the document the operation leaves of the paragraph, or of what its delete left.
		want func(paragraph, deleted string) string
	}{
		{
			op:   "delete",
			edit: func(quote, _ string) model.EditOp { return model.EditOp{Op: "delete", Find: quote} },
			want: func(_, deleted string) string { return "Moved.\n\n" + deleted + "\n" },
		},
		{
			op: "insert",
			edit: func(quote, _ string) model.EditOp {
				return model.EditOp{Op: "insert", After: quote, Markdown: "Inserted."}
			},
			want: func(paragraph, _ string) string { return "Moved.\n\n" + paragraph + "\n\nInserted.\n" },
		},
		{
			op: "move",
			edit: func(quote, moved string) model.EditOp {
				return model.EditOp{Op: "move", Block: moved, After: quote}
			},
			want: func(paragraph, _ string) string { return paragraph + "\n\nMoved.\n" },
		},
	}
	for _, op := range ops {
		for _, shape := range shapes {
			t.Run(op.op+" of "+shape.name, func(t *testing.T) {
				markdown := "Moved.\n\n" + shape.paragraph + "\n"
				service, artifactID := newTestService(t)
				seedServiceText(t, service, artifactID, markdown)
				moved := blockIDOfText(t, liveTree(t, service, artifactID), "Moved.")
				if _, err := joinedApplyOps(service, artifactID, []model.EditOp{op.edit(shape.quote, moved)},
					model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
					t.Fatalf("%s %q in %q: %v, want it taken", op.op, shape.quote, markdown, err)
				}
				requireDocument(t, service, artifactID, op.want(shape.paragraph, shape.deleted), fmt.Sprintf("%s %q in %q", op.op, shape.quote, markdown))
			})
		}
	}
}

// An accepted suggestion's text lands inside the marked span the suggestion lay inside, as a
// replace's does (TestReplaceInsideAMarkedSpanKeepsTheSpanWhole): accepting a suggestion over
// `said` in `**Sami said bold**` split the span and wrote its edges as entities, and one over a
// bold span's whole text lost the bold. Each row marks a suggestion on its quote as the margin
// does, accepts it, and reads the document back three ways: the markdown the store holds, the
// document a caller reads, and the text of the paragraph the accept wrote in.
func TestAcceptedSuggestionInsideAMarkedSpanKeepsTheSpanWhole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, markdown, quote, accepted, want, text string }{
		{
			name:     "a suggestion inside a bold span",
			markdown: "**Sami said bold** now.\n",
			quote:    "said",
			accepted: "answered",
			want:     "**Sami answered bold** now.\n",
			text:     "Sami answered bold now.",
		},
		{
			name:     "a suggestion over a bold span's whole text",
			markdown: "Sami wrote [this](https://example.com/Sami) and **Sami bold**.\n",
			quote:    "Sami bold",
			accepted: "Sami answered bold",
			want:     "Sami wrote [this](https://example.com/Sami) and **Sami answered bold**.\n",
			text:     "Sami wrote this and Sami answered bold.",
		},
		{
			name:     "a suggestion inside a link's text",
			markdown: "Sami wrote [this page](https://example.com/Sami) today.\n",
			quote:    "page",
			accepted: "note",
			want:     "Sami wrote [this note](https://example.com/Sami) today.\n",
			text:     "Sami wrote this note today.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
			if _, err := joinedMarkQuote(service, artifactID, spec, test.quote, nil); err != nil {
				t.Fatalf("suggest over %q: %v", test.quote, err)
			}
			if err := joinedAcceptSuggestion(service, artifactID, "s1", test.accepted, model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("accept %q over %q: %v", test.accepted, test.quote, err)
			}
			stored := storedMarkdown(t, service.store, artifactID)
			read, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the document: %v", err)
			}
			text := pmdoc.TextContent(persistedProofTree(t, service.store, artifactID).Children[0])
			if stored != test.want || read != test.want || text != test.text {
				t.Fatalf("accept %q over %q in %q:\n stored markdown %q\n read markdown   %q\n text            %q\nwant markdown %q and text %q",
					test.accepted, test.quote, test.markdown, stored, read, text, test.want, test.text)
			}
		})
	}
}
