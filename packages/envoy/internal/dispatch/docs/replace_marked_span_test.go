package docs

import (
	"context"
	"errors"
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
// which the with writes again. A bare URL and a bare www link are their own markup, so a find
// that covers one whole across its edge spells it, where it was refused with that same find as
// the markup to spell. A find that spells the span's markup over part of the text it covers (`a
// **b** c` over `**a b c**`) spells it over that part alone, so the span keeps its mark, where
// the with was written without the bold. A find that runs past a span's edge without spelling
// its markup, or spells a link with another URL or title, is refused
// (TestReplaceAcrossAMarkedSpansEdgeIsRefused). Then a find that matched the text as written,
// whose `*`, `_` or backtick is text the span holds and no markup: `*`, `_` and a backtick inside
// a bold, an italic and a code span, the review's two finds (`2*3*4` in italic, `2**3**4` in
// bold), and `snake_case_name`, whose underscores no reading takes for markup.
// Each row writes one replace and reads it back three ways: the markdown the version stored, the
// document a caller reads, and the text of the paragraph the replace wrote in.
func TestReplaceInsideAMarkedSpanKeepsTheSpanWhole(t *testing.T) {
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
			name:     "a find that is one link alone, spelled with its own URL, leaves the link to the with",
			markdown: "I wrote [this](https://a.example/1) today.\n",
			find:     "[this](https://a.example/1)",
			with:     "[that](https://a.example/1)",
			want:     "I wrote [that](https://a.example/1) today.\n",
			text:     "I wrote that today.",
		},
		{
			name:     "a find that is one link alone, spelled with its own URL and title, leaves the link to the with",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "[this](https://a.example/1 \"Spec\")",
			with:     "[that](https://a.example/1 \"Spec\")",
			want:     "I wrote [that](https://a.example/1 \"Spec\") today.\n",
			text:     "I wrote that today.",
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
			waitForDocumentVersion(t, service.store, artifactID, 2)
			stored, _ := latestVersionOf(t, service.store, artifactID)
			read, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the document: %v", err)
			}
			text := pmdoc.TextContent(persistedProofTree(t, service.store, artifactID).Children[0])
			if stored != test.want || read != test.want || text != test.text {
				t.Fatalf("replace %q with %q in %q:\n stored markdown %q\n read markdown   %q\n text            %q\nwant markdown %q and text %q",
					test.find, test.with, test.markdown, stored, read, text, test.want, test.text)
			}
		})
	}
}

// A replace whose find crosses a marked span's edge - it covers text inside the span and text
// outside it - is refused, and the document is left as it was. Text written over such a find
// lands outside the span: over the tester's `wrote this`, where only `this` is the link, the link
// and its URL were gone with no warning, and a bold, italic or struck span the write cut to end
// on a space was written with character references (`**Sami said&#32;**loud`). The refusal names
// the span and its kind (ErrFindCrossesSpan). The rows are the link, bold and italic edges, a
// strikethrough's and a code span's, a find that spells only the part of a bold span's markup it
// covers, and the find past a bold span's end (`'s bold` in `**Sami's** bold`) that
// TestReplaceInsideAMarkedSpanKeepsTheSpanWhole once wrote outside the span, as `**Sami**'s loud`.
// The next two are a find that spells one span's markup and covers a second span of that kind
// whose markup it does not spell: the second link lost its URL, and the second bold its bold,
// since the find was read as spelling every span of the kind once it spelled one. The next two are
// a find that spells a link's markup with another URL, and one that spells it with the link's URL
// and not its title, as finds copied from an older read after another writer changed the URL or
// gave the link a title: each was written, and the URL or the title the document held went with
// no word. The last two are a find that is that one link alone, spelled with another URL and with
// another title: it lay inside the link, so it was written there under the URL and title the
// document held, where the same spelling in a longer find was refused. Each refusal gives the
// link's markup as the document holds it. The longer find's refusal says it covers text outside
// the link; the find that is the link alone covers none, so its refusal says it spells the link
// with another URL or title, and nothing of text outside it.
func TestReplaceAcrossAMarkedSpansEdgeIsRefused(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with, kind, span string
		// markup is the span's markup the refusal says to spell, where the row reads it.
		markup string
		// says is words the refusal's message holds, and unsaid words it does not hold, where the
		// row reads them.
		says, unsaid string
	}{
		{
			name:     "a find across a link's start",
			markdown: "Sami wrote [this](https://example.com/Sami) today.\n",
			find:     "wrote this",
			with:     "wrote that",
			kind:     "link",
			span:     "this",
		},
		{
			name:     "a find across a bold span's end",
			markdown: "**Sami said bold** now.\n",
			find:     "bold now",
			with:     "loud then",
			kind:     "bold",
			span:     "Sami said bold",
		},
		{
			name:     "a find across an italic span's end",
			markdown: "*Sami said it* now.\n",
			find:     "it now",
			with:     "that then",
			kind:     "italic",
			span:     "Sami said it",
		},
		{
			name:     "a find across a struck span's end",
			markdown: "~~Sami said it~~ now.\n",
			find:     "it now",
			with:     "that then",
			kind:     "strikethrough",
			span:     "Sami said it",
		},
		{
			name:     "a find across a code span's end",
			markdown: "Run `make said all` now.\n",
			find:     "all now",
			with:     "test then",
			kind:     "code",
			span:     "make said all",
		},
		{
			name:     "a find that spells only the part of a bold span it covers",
			markdown: "**Sami said bold** now.\n",
			find:     "**bold** now",
			with:     "loud then",
			kind:     "bold",
			span:     "Sami said bold",
		},
		{
			name:     "a find across a bold span's end that once wrote outside the span",
			markdown: "**Sami's** bold now.\n",
			find:     "'s bold",
			with:     "'s loud",
			kind:     "bold",
			span:     "Sami's",
		},
		{
			name:     "a find that spells one link's markup and covers a second link",
			markdown: "I wrote [this](https://a.example/1) and [that](https://b.example/2) today.\n",
			find:     "wrote [this](https://a.example/1) and that",
			with:     "wrote it",
			kind:     "link",
			span:     "that",
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
			name:     "a find that spells a link's markup with another URL",
			markdown: "I wrote [this](https://a.example/1) today.\n",
			find:     "wrote [this](https://wrong.example/9) today",
			with:     "wrote that today",
			kind:     "link",
			span:     "this",
			markup:   "[this](https://a.example/1)",
			says:     "text outside it",
		},
		{
			name:     "a find that spells a link's markup without the title it has",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "wrote [this](https://a.example/1) today",
			with:     "wrote that today",
			kind:     "link",
			span:     "this",
			markup:   "[this](https://a.example/1 \"Spec\")",
		},
		{
			name:     "a find that is one link alone, spelled with another URL",
			markdown: "I wrote [this](https://a.example/1) today.\n",
			find:     "[this](https://wrong.example/9)",
			with:     "that",
			kind:     "link",
			span:     "this",
			markup:   "[this](https://a.example/1)",
			says:     "another URL or title",
			unsaid:   "text outside it",
		},
		{
			name:     "a find that is one link alone, spelled with another title",
			markdown: "I wrote [this](https://a.example/1 \"Spec\") today.\n",
			find:     "[this](https://a.example/1 \"Draft\")",
			with:     "that",
			kind:     "link",
			span:     "this",
			markup:   "[this](https://a.example/1 \"Spec\")",
			says:     "another URL or title",
			unsaid:   "text outside it",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			before, _ := latestVersionOf(t, service.store, artifactID)
			_, err := joinedApplyOps(service, artifactID, []model.EditOp{
				{Op: "replace", Find: test.find, With: test.with},
			}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
			read, readErr := service.Text(context.Background(), artifactID)
			if readErr != nil {
				t.Fatalf("read the document: %v", readErr)
			}
			var crossed *ErrFindCrossesSpan
			if !errors.As(err, &crossed) || crossed.Kind() != test.kind || crossed.Span.Text != test.span || (test.markup != "" && crossed.Span.Markdown != test.markup) {
				t.Fatalf("replace %q with %q in %q = %v, want it refused for crossing the %s %q; the document reads %q",
					test.find, test.with, test.markdown, err, test.kind, test.span, read)
			}
			if message := err.Error(); (test.says != "" && !strings.Contains(message, test.says)) || (test.unsaid != "" && strings.Contains(message, test.unsaid)) {
				t.Fatalf("refusal = %q, want it to say %q and not %q", message, test.says, test.unsaid)
			}
			// A refused replace stores no version: the latest is the one before the seed, or the
			// seed's own if a settlement has stored it since.
			stored, _ := latestVersionOf(t, service.store, artifactID)
			if (stored != before && stored != test.markdown) || read != test.markdown {
				t.Fatalf("after the refused replace %q in %q:\n stored markdown %q, was %q\n read markdown   %q\nwant both unchanged",
					test.find, test.markdown, stored, before, read)
			}
		})
	}
}

// An accepted suggestion's text lands inside the marked span the suggestion lay inside, as a
// replace's does (TestReplaceInsideAMarkedSpanKeepsTheSpanWhole): accepting a suggestion over
// `said` in `**Sami said bold**` split the span and wrote its edges as entities, and one over a
// bold span's whole text lost the bold. Each row marks a suggestion on its quote as the margin
// does, accepts it, and reads the document back three ways: the markdown the store holds, the
// document a caller reads, and the text of the paragraph the accept wrote in.
func TestAcceptedSuggestionInsideAMarkedSpanKeepsTheSpanWhole(t *testing.T) {
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
