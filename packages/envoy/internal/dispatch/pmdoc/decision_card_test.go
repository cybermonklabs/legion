package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

// decisionBlock is a decision block as a writer writes one: a question and two options inside an
// `ask` directive, which the browser reader draws a decision card for.
const decisionBlock = ":::ask{urgency=\"med\"}\n" +
	"Which transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n:::"

// cardInTheTree is where the parse's own tree holds the first decision block: the top-level block
// WalkAskBlocks finds an `ask` in, which is what a caller reading a body the parse stood on reads
// (itemblocks.Check). FindDecisionCard answers the same place, from the tree goldmark read.
func cardInTheTree(t *testing.T, markdown string) (DecisionCard, bool) {
	t.Helper()
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse(%q) = %v, want the tree of a body that parses", markdown, err)
	}
	for index, child := range doc.Children {
		holdsAsk := false
		WalkAskBlocks(child, func(string) { holdsAsk = true })
		if holdsAsk {
			return DecisionCard{Position: index + 1, Type: child.Type}, true
		}
	}
	return DecisionCard{}, false
}

// Over a body the parse reads, FindDecisionCard answers what the parse's own tree says: the first
// top-level block holding an `ask`, by its place and its type, and no card where the tree holds no
// `ask`. So the rule answers one body one way, whether it reads the Proof tree a parse that stood
// left or the goldmark tree a refusal left behind.
func TestFindDecisionCardAnswersWhatTheParsedTreeHolds(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{name: "a decision block at the top", markdown: decisionBlock + "\n"},
		{name: "a decision block in a quote", markdown: "Lead text.\n\n> :::ask{urgency=\"med\"}\n> Ship it?\n> :::\n"},
		{
			name:     "a decision block in a list item",
			markdown: "Lead text.\n\n- Context\n\n  :::ask{urgency=\"med\"}\n  Ship it?\n  :::\n",
		},
		{
			name: "a decision block inside a callout",
			markdown: "Lead text.\n\nMore lead text.\n\n:::callout{kind=\"note\" title=\"Decide\"}\nA note.\n\n" +
				decisionBlock + "\n:::\n",
		},
		{
			name:     "a decision block under front matter",
			markdown: "---\ntitle: A plan\n---\n\n" + decisionBlock + "\n",
		},
		{
			name:     "the first of two decision blocks",
			markdown: "Lead text.\n\n" + decisionBlock + "\n\n:::ask{urgency=\"med\"}\nAnd the version?\n:::\n",
		},
		{name: "a decision block's text in a fenced code block", markdown: "Write it so:\n\n```\n" + decisionBlock + "\n```\n"},
		{name: "no decision block at all", markdown: "Lead text.\n\n- A point\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, wantFound := cardInTheTree(t, test.markdown)
			card, found := FindDecisionCard(test.markdown)
			if card != want || found != wantFound {
				t.Fatalf("FindDecisionCard() = %+v, %v, want the tree's %+v, %v", card, found, want, wantFound)
			}
		})
	}
}

// A body the parse refuses is read all the same: the refusal is a verdict on the tree goldmark
// built, and the reader draws its card from that same tree, so the card is found whether the body
// is refused for a block the schema holds nothing like, for nesting past a bound, for the elements
// it makes or for the cells its tables would be padded with.
func TestFindDecisionCardReadsABodyTheParseRefuses(t *testing.T) {
	for _, test := range []struct {
		name, markdown string
		position       int
		blockType      string
	}{
		{
			name:      "a link reference definition after the decision block",
			markdown:  decisionBlock + "\n\n[a]: http://x\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "a block of HTML after the decision block",
			markdown:  decisionBlock + "\n\n<!-- a comment -->\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "quotes nested past the block bound, the decision block in the first quote",
			markdown:  "> " + strings.ReplaceAll(decisionBlock, "\n", "\n> ") + "\n\n" + strings.Repeat("> ", maxNesting+1) + "deep\n",
			position:  1,
			blockType: "blockquote",
		},
		{
			name:      "headings past the element cap",
			markdown:  decisionBlock + "\n\n" + strings.Repeat("# a\n\n", MaxDocumentElements/4+1),
			position:  1,
			blockType: "ask",
		},
		{
			name: "a table whose short rows would take more padding than one write may add",
			markdown: decisionBlock + "\n\n" + strings.Repeat("| a ", 110) + "|\n" +
				strings.Repeat("| - ", 110) + "|\n" + strings.Repeat("| x |\n", 95),
			position:  1,
			blockType: "ask",
		},
		{
			name: "a callout carrying an attribute its type does not declare, the decision block inside it",
			markdown: "Lead text.\n\n:::callout{kind=\"note\" title=\"T\" priority=\"high\"}\n" +
				decisionBlock + "\n:::\n",
			position:  2,
			blockType: "callout",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.markdown); err == nil {
				t.Fatal("the body parses, so it no longer drives a refused parse")
			}
			card, found := FindDecisionCard(test.markdown)
			if !found || card.Position != test.position || card.Type != test.blockType {
				t.Fatalf("FindDecisionCard() = %+v, %v, want block %d (%s)",
					card, found, test.position, test.blockType)
			}
		})
	}
}

// An opening line the browser reader opens an `ask` container on is a decision block wherever it
// stands, whatever this parser's own grammar makes of that line: the reader draws its card from
// the container it opened, so the card is found for an opener carrying no attributes, one written
// with four colons, one a space follows, one carrying a label, and each of them inside a quote, a
// list item or a callout. The place the card is named at is the one the reader draws it at, since
// the rewrite keeps every line of the body and the order they stand in.
func TestFindDecisionCardReadsEveryOpenerTheReaderOpensAnAskOn(t *testing.T) {
	const question = "Which transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n"
	for _, test := range []struct {
		name, markdown string
		position       int
		blockType      string
	}{
		{
			name:      "an opener carrying no attributes",
			markdown:  ":::ask\n" + question + ":::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "an opener of four colons",
			markdown:  "::::ask\n" + question + "::::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "a space after the attributes",
			markdown:  ":::ask{#b1} \n" + question + ":::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "a label before the attributes",
			markdown:  ":::ask[Which transport?]{#b1}\n" + question + ":::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "a label and no attributes",
			markdown:  ":::ask[Which transport?]\n" + question + ":::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "attributes this parser's own grammar refuses",
			markdown:  ":::ask{urgency=high}\n" + question + ":::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "no closing fence at the end of the body",
			markdown:  ":::ask\n" + question,
			position:  1,
			blockType: "ask",
		},
		{
			name:      "in a quote",
			markdown:  "Lead text.\n\n> :::ask\n> Ship it?\n> :::\n",
			position:  2,
			blockType: "blockquote",
		},
		{
			name:      "in a list item",
			markdown:  "Lead text.\n\n- Context\n\n  :::ask\n  Ship it?\n  :::\n",
			position:  2,
			blockType: "bullet_list",
		},
		{
			name:      "on a list item's own first line",
			markdown:  "Lead text.\n\n- :::ask\n  Ship it?\n  :::\n",
			position:  2,
			blockType: "bullet_list",
		},
		{
			name:      "inside a callout",
			markdown:  ":::callout{#c1 kind=\"note\"}\nA note.\n\n:::ask\nShip it?\n:::\n:::\n",
			position:  1,
			blockType: "callout",
		},
		{
			name:      "a four-colon opener whose content the ask's own rule refuses",
			markdown:  "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n",
			position:  2,
			blockType: "ask",
		},
		{
			name:      "lines a carriage return ends",
			markdown:  ":::ask\r\nShip it?\r\n:::\r\n",
			position:  1,
			blockType: "ask",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.markdown); err == nil {
				t.Fatal("the body parses, so it no longer drives a refused parse")
			}
			card, found := FindDecisionCard(test.markdown)
			if !found || card.Position != test.position || card.Type != test.blockType {
				t.Fatalf("FindDecisionCard() = %+v, %v, want block %d (%s)",
					card, found, test.position, test.blockType)
			}
		})
	}
}

// A line the reader opens no `ask` container on opens none here either: another directive's name,
// a name the colons run into, a line the opener does not end, attributes no `}` closes, and an
// opener a fenced code block or an indented line holds, which both parsers read as text.
func TestFindDecisionCardFindsNoCardWhereNoAskOpens(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{name: "another directive's name", markdown: ":::note\nA note.\n:::\n"},
		{name: "a name the colons run into", markdown: ":::asks\nShip it?\n:::\n"},
		{name: "a name ending in a dash", markdown: ":::ask-\nShip it?\n:::\n"},
		{name: "a longer name", markdown: ":::ask-now{#b1}\nShip it?\n:::\n"},
		{name: "two colons", markdown: "::ask\nShip it?\n:::\n"},
		{name: "text after the name", markdown: ":::ask it\nShip it?\n:::\n"},
		{name: "a space before the attributes", markdown: ":::ask {#b1}\nShip it?\n:::\n"},
		{name: "attributes no brace closes", markdown: ":::ask{#b1\nShip it?\n:::\n"},
		{name: "a label no bracket closes", markdown: ":::ask[Which?\nShip it?\n:::\n"},
		{name: "an opener in a fenced code block", markdown: "Write it so:\n\n```\n:::ask\nShip it?\n:::\n```\n"},
		{name: "an opener four columns in", markdown: "Context\n\n    :::ask\n    Ship it?\n    :::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if card, found := FindDecisionCard(test.markdown); found {
				t.Fatalf("FindDecisionCard() = %+v, %v, want no card: the reader opens no `ask` here",
					card, found)
			}
		})
	}
}

// The one opener the rule does not reach: a body whose lines end in a lone carriage return. The
// browser's parser ends a line there and opens the `ask`; goldmark ends none, so it reads the
// body as one line and makes no block of it however the opener is written - the rewrite below
// spells it exactly as this parser's grammar reads an opener, and the tree still holds no `ask`.
// What parts the two is the line ending itself and not the opener, so no spelling closes it.
func TestFindDecisionCardMissesAnOpenerALoneCarriageReturnEnds(t *testing.T) {
	for _, markdown := range []string{
		":::ask\rShip it?\r:::\r",
		":::ask{}\rShip it?\r:::\r",
	} {
		if card, found := FindDecisionCard(markdown); found {
			t.Fatalf("FindDecisionCard(%q) = %+v, %v, want the measured miss", markdown, card, found)
		}
	}
}

// The other opener the rule does not reach: a body whose opening line stands behind a byte-order
// mark. The browser's parser drops one U+FEFF before it reads the line and opens the `ask`;
// goldmark reads the mark as the line's first character, so the line is a paragraph and the tree
// holds no `ask` - and the rewrite opens no line either, since the colon run it looks for does
// not start where the mark stands. What parts the two is the mark itself and not the opener, so
// no spelling closes it; the rule drops the mark in its own copy of the body before it reads
// here (itemblocks.Check).
func TestFindDecisionCardMissesAnOpenerAByteOrderMarkStandsBefore(t *testing.T) {
	for _, markdown := range []string{
		"\ufeff:::ask\nShip it?\n:::\n",
		"\ufeff:::ask{urgency=\"med\"}\nShip it?\n:::\n",
	} {
		if card, found := FindDecisionCard(markdown); found {
			t.Fatalf("FindDecisionCard(%q) = %+v, %v, want the measured miss", markdown, card, found)
		}
	}
}

// A body heavy enough to stop goldmark where it reads it (nodeGuard) is read on from the line it
// stopped at, so a decision block standing past that line is found too, at its own place in the
// body: a mebibyte of markdown, which is what one write may send, carries far more lines than the
// guard reads at once.
func TestFindDecisionCardReadsOnPastTheElementGuard(t *testing.T) {
	// Each paragraph of one line counts about two of the nodes the guard counts, so twice the
	// limit's worth of them passes nodeGuard*MaxDocumentElements, where one read stops.
	const paragraphs = 2 * MaxDocumentElements
	body := strings.Repeat("x\n\n", paragraphs) + decisionBlock + "\n"
	if _, err := Parse(body); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("Parse() = %v, want the body to pass the element cap", err)
	}
	pc, count := readContext(false, NewWriteBudget(), 0, false)
	readSource(blockReader.md.Parser(), []byte(body), pc)
	if count == nil || !count.stopped {
		t.Fatalf("one read of the body reads it whole (stopped=%v), so it no longer drives the guard", count != nil && count.stopped)
	}
	card, found := FindDecisionCard(body)
	if !found || card.Position != paragraphs+1 || card.Type != "ask" {
		t.Fatalf("FindDecisionCard() = %+v, %v, want block %d (ask)", card, found, paragraphs+1)
	}
}
