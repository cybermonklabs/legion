package itemblocks

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// decisionRefusal is the refusal a body is answered with for the decision block its block at
// position, of type blockType, holds: the message spelled out here, so the wording DecisionBlock
// writes is read back against a text this test holds and not against the same format string.
func decisionRefusal(field string, position int, blockType string) string {
	return fmt.Sprintf("%s block %d (%s) holds a decision block, which nobody can answer where it stands: "+
		"ask the question in the issue's own document, as a decision block there, or open an ask with "+
		"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
		field, position, blockType)
}

// theQuestion is the words a decision block holds: a question and two options.
const theQuestion = "Which transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming"

// theAsk is a decision block as a writer writes one: theQuestion inside an `ask` directive.
const theAsk = ":::ask{urgency=\"med\"}\n" + theQuestion + "\n:::"

// A decision block is refused wherever it stands in the body, since the reader draws a card for an
// `ask` node at any depth: at the top, inside a quote, inside a list item and inside a callout.
// The refusal names the top-level block that holds it, counted as the bound's refusals count, and
// that block's own type, which is "ask" only where the decision block stands at the top.
func TestCheckRefusesADecisionBlockWhereverItStands(t *testing.T) {
	for _, test := range []struct {
		name, body, blockType string
		position              int
	}{
		{
			name:      "at the top of the body",
			body:      theAsk + "\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "with a blank option",
			body:      ":::ask{urgency=\"med\"}\nWhich transport should we expose?\n\n- \n- gRPC: streaming\n:::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "in a quote",
			body:      "Lead text.\n\n> :::ask{urgency=\"med\"}\n> Which transport should we expose?\n> :::\n",
			position:  2,
			blockType: "blockquote",
		},
		{
			name:      "in a list item",
			body:      "Lead text.\n\n- Context for the question\n\n  :::ask{urgency=\"med\"}\n  Which transport should we expose?\n  :::\n",
			position:  2,
			blockType: "bullet_list",
		},
		{
			name:      "in a callout at the third block",
			body:      "Lead text.\n\nMore lead text.\n\n:::callout{kind=\"note\" title=\"Decide\"}\nA note.\n\n:::ask{urgency=\"med\"}\nWhich transport should we expose?\n:::\n:::\n",
			position:  3,
			blockType: "callout",
		},
		{
			name:      "written with four colons",
			body:      "::::ask{urgency=\"med\"}\nWhich transport should we expose?\n\n- REST: the platform's own\n::::\n",
			position:  1,
			blockType: "ask",
		},
		{
			name:      "the first of two, which the refusal names",
			body:      "Lead text.\n\n" + theAsk + "\n\n:::ask{urgency=\"med\"}\nAnd which protocol version?\n:::\n",
			position:  2,
			blockType: "ask",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireRefusal(t, test.name, test.body, decisionRefusal("body", test.position, test.blockType))
			var decision *DecisionBlock
			if !errors.As(err, &decision) {
				t.Fatalf("Check returned %T, want *DecisionBlock", err)
			}
			if decision.Field != "body" || decision.Position != test.position || decision.Type != test.blockType {
				t.Fatalf("Check = %+v, want field %q, position %d, type %q",
					decision, "body", test.position, test.blockType)
			}
			// The position the refusal names is the parse tree's top-level index. Where the
			// blocks are the parser's too, as they are in every body here, it is the block's
			// place in the body as well.
			if blocks, doc := split(test.body); doc == nil || len(blocks) != len(doc.Children) {
				t.Fatalf("the body splits into %d blocks and parses into %d top-level nodes, want the two to agree",
					len(blocks), len(doc.Children))
			}
		})
	}
}

// The refusal a reader is answered with, spelled out whole for the body the spec names: a callout
// at the third block of a body whose field is "body".
func TestTheDecisionBlockRefusalReadsAsWritten(t *testing.T) {
	const want = "body block 3 (callout) holds a decision block, which nobody can answer where it stands: " +
		"ask the question in the issue's own document, as a decision block there, or open an ask with " +
		"`dispatch ask`; to show a decision block as an example, put it in a fenced code block"
	err := &DecisionBlock{Field: "body", Position: 3, Type: "callout"}
	if err.Error() != want {
		t.Fatalf("DecisionBlock.Error() = %q, want %q", err.Error(), want)
	}
	body := "Lead text.\n\nMore lead text.\n\n:::callout{kind=\"note\" title=\"Decide\"}\nA note.\n\n" + theAsk + "\n:::\n"
	requireRefusal(t, "a callout holding a decision block at the third block", body, want)
}

// A body holding the text of a decision block inside a fenced code block is stored: the fence is
// how a writer shows the block as an example, and the reader opens no `ask` on a line of code. So
// is a body the parser refuses that holds no decision block at all, such as a container directive
// no block type claims. A refused parse stores nothing by itself: every refused body holding a
// line the reader opens an `ask` container on is refused for it, whatever that ask holds
// (decisionRefusalRows).
func TestCheckStoresABodyTheReaderDrawsNoCardFor(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{
			name: "a fenced code block holding a decision block's text",
			body: "Write it so:\n\n```\n" + theAsk + "\n```\n",
		},
		{
			name: "an unknown container directive",
			body: "Some **bold** text\n\n:::note\nA note with **inner** words\n:::\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirePasses(t, test.name, test.body)
		})
	}
}

// A body that holds a decision block and breaks a bound as well is refused for the bound, in the
// bound's own words and under the bound's own code: the decision block is read after both bounds.
func TestCheckRefusesABodyOverABoundForTheBound(t *testing.T) {
	overTheBlock := theAsk + "\n\n" + strings.Repeat("x", contracts.DispatchBlockUnits+1)
	err := requireRefusal(t, "a decision block beside a block one character over", overTheBlock,
		"body block 2 (paragraph) is 1 characters over the 32000-character block limit (32001/32000)")
	var capExceeded *CapExceeded
	if !errors.As(err, &capExceeded) {
		t.Fatalf("Check returned %T, want *CapExceeded", err)
	}
	if _, doc := split(overTheBlock); doc == nil {
		t.Fatal("the body over the block bound did not parse, so its refusal shows no order")
	}

	// The parser weighs a block at three of the 65,536 elements it reads, so no body of more
	// blocks than a body may hold parses at all: its blocks are the blank-line fallback's, where
	// the one-line ask is one block. The decision block it holds is one the reader draws a card
	// for, and the count is still what the body is refused for, since both bounds are answered
	// before the card is looked for at all.
	overTheCount := ":::ask{urgency=\"med\"}\nWhich transport should we expose?\n:::\n\n" +
		paragraphBody(contracts.DispatchItemBlocks, 1)
	err = requireRefusal(t, "a decision block beside one block too many", overTheCount,
		"body is 1 blocks over the 32768-block limit (32769/32768)")
	var countExceeded *CountExceeded
	if !errors.As(err, &countExceeded) {
		t.Fatalf("Check returned %T, want *CountExceeded", err)
	}
}

// benchmarkBody is a body of the shape a writer sends: lead text, a quote, a list and a callout,
// the callout holding what inner holds.
func benchmarkBody(inner string) string {
	return "Lead text for the body.\n\n" +
		"> A quote of what was said before.\n\n" +
		"- The first point\n- The second point\n\n" +
		"A paragraph between them.\n\n" +
		":::callout{kind=\"note\" title=\"Context\"}\n" + inner + "\n:::\n"
}

// BenchmarkCheck shows that Check parses the body once: over a body holding a decision block and
// over a clean body alike it costs about what the one parse beneath it costs, the parse each pair
// measures beside it, rather than twice that, because Split and Check share the package's single
// parse (split). The clean body carries the decision block's text as a question and a list, so
// the two bodies give the parser the same words to read.
func BenchmarkCheck(b *testing.B) {
	for _, body := range []struct{ name, text string }{
		{name: "a body holding a decision block", text: benchmarkBody(theAsk)},
		{name: "a clean body", text: benchmarkBody(theQuestion)},
	} {
		b.Run(body.name+"/one parse", func(b *testing.B) {
			for b.Loop() {
				if _, _, err := pmdoc.ParseWithSourceStarts(body.text); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(body.name+"/Check", func(b *testing.B) {
			for b.Loop() {
				Check("body", body.text)
			}
		})
	}
	if err := Check("body", benchmarkBody(theAsk)); err == nil {
		b.Fatal("the body holding a decision block passed")
	}
	if err := Check("body", benchmarkBody(theQuestion)); err != nil {
		b.Fatalf("the clean body was refused: %v", err)
	}
}
