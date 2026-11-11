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

// A `::::ask` with no attributes opens no typed directive for either parser, so the reader draws
// its lines as text and there is no card to find: the body of
// packages/dispatch/e2e/unknown-directive.stub.e2e.ts:50, which the parse refuses as well.
func TestFindDecisionCardFindsNoCardWhereNoAskIsOpened(t *testing.T) {
	const body = "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n"
	if _, err := Parse(body); err == nil {
		t.Fatal("the body parses, so it no longer drives a refused parse")
	}
	if card, found := FindDecisionCard(body); found {
		t.Fatalf("FindDecisionCard() = %+v, %v, want no card: neither parser opens an `ask` here", card, found)
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
	pc, count := readContext(false, NewWriteBudget(), 0)
	readSource(blockReader.md.Parser(), []byte(body), pc)
	if count == nil || !count.stopped {
		t.Fatalf("one read of the body reads it whole (stopped=%v), so it no longer drives the guard", count != nil && count.stopped)
	}
	card, found := FindDecisionCard(body)
	if !found || card.Position != paragraphs+1 || card.Type != "ask" {
		t.Fatalf("FindDecisionCard() = %+v, %v, want block %d (ask)", card, found, paragraphs+1)
	}
}
