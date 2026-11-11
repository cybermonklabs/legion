package itemblocks

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// tablePastThePaddingBudget is a table columns wide over rows one-cell body rows: the markdown
// whose short rows the parse would pad with more empty cells than one write may add
// (pmdoc.ErrTablePadding), which is how the measurement wrote it.
func tablePastThePaddingBudget(columns, rows int) string {
	return strings.Repeat("| a ", columns) + "|\n" +
		strings.Repeat("| - ", columns) + "|\n" +
		strings.Repeat("| x |\n", rows)
}

// decisionRefusalRows are the 44 bodies measured for INFRA-1201: every distinct reason
// pmdoc/refusals.go and the parse around it refuse a body for, each with a decision block standing
// in it, and every spelling of a decision block's opening line the browser reader opens an `ask`
// container on where this parser's own grammar reads no typed directive. Each row is named by the
// reason its trigger raises, so a reader of a failure reads the class of body that broke, and
// carries what the browser reader was measured to draw for that body: cards is how many decision
// cards it drew, and position and blockType the top-level block the refusal names.
//
// None of these bodies is stored. Each one holds an opening line the reader opens an `ask`
// container on, which is the whole of the rule: where the parse stands, the rule reads the `ask`
// in the tree that parse left, and where the parse refuses the body, it reads the reader's own
// opener off the body's lines (pmdoc.FindDecisionCard). Three bodies are refused with no card
// drawn, since the reader's container rule is the floor of the refusal and the ask's own content
// rule decides nothing here - a question nobody can answer stands in the body whether or not a
// card is finally drawn for it. A body that is still stored holds no such opener at all: a
// decision block's text inside a fenced code block, or a container directive of another name
// (TestCheckStoresABodyTheReaderDrawsNoCardFor).
//
// A body too long to read as a literal is built here as the measurement built it: the 33 nested
// callouts of the typed-nesting bound, the headings past the element cap, and the 110-column table
// over 95 one-cell rows whose padding passes what one write may add.
var decisionRefusalRows = []struct {
	// reason is the refusal the body's trigger raises, "no refusal" where the parse reads it.
	reason string
	// trigger is what was written into the body beside the decision block.
	trigger string
	body    string
	// cards is how many decision cards the browser reader drew for the body: 1 for each body
	// whose `ask` it draws as a card, 0 for the three it draws in place as text. It is the
	// measurement the rule was built on, and no longer the verdict.
	cards int
	// position and blockType are the top-level block the refusal names, which is the block the
	// card stands in wherever the reader draws one.
	position  int
	blockType string
	// parsed says the server's parse read the body: the control, and the two bodies the browser
	// reader draws no card for though both parses read their decision block. Every other row
	// exists to drive the rule over a body the parse refuses.
	parsed bool
}{
	{
		reason:    "no refusal: the control, a body the parser reads",
		trigger:   "nothing beside the ask block",
		body:      theAsk + "\n\n",
		cards:     1,
		position:  1,
		blockType: "ask",
		parsed:    true,
	},
	{
		reason:    "blockRefusal default: unsupported markdown block <kind>",
		trigger:   "a link reference definition",
		body:      theAsk + "\n\n" + "[a]: http://x\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *ast.HTMLBlock: ErrBlockHTML",
		trigger:   "a block-level HTML comment",
		body:      theAsk + "\n\n" + "<!-- legion-outbox:1 -->\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "htmlFlowRefusal: browserBlockHTML",
		trigger:   "a quoted line whose tab opens block HTML for the browser's parser alone",
		body:      theAsk + "\n\n" + "> </a\t>\n> text\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDirectiveRefusal: unknown typed block",
		trigger:   "a typed block of a type the schema does not declare",
		body:      theAsk + "\n\n" + ":::unknown{#b1}\nBody.\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDirectiveRefusal: undeclaredAttributes",
		trigger:   "a callout carrying an attribute its type does not declare",
		body:      theAsk + "\n\n" + ":::callout{kind=\"note\" title=\"T\" priority=\"high\"}\nBody.\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDirectiveRefusal: parseTypedAttributeValue",
		trigger:   "a callout whose kind is outside its enum",
		body:      theAsk + "\n\n" + ":::callout{kind=\"urgent\" title=\"T\"}\nBody.\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDirectiveRefusal: unclosed at document level",
		trigger:   "a callout left unclosed at document level",
		body:      theAsk + "\n\n" + ":::callout{kind=\"note\" title=\"\"}\nBody.\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDirectiveRefusal: lazyTypedParagraphAttr",
		trigger:   "a lazy line continuing a paragraph inside a quoted callout",
		body:      theAsk + "\n\n" + "> :::callout{kind=\"note\" title=\"\"}\n> Body.\nlazy\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *extensionast.Footnote: nested definition",
		trigger:   "a footnote definition inside another footnote definition",
		body:      theAsk + "\n\n" + "x[^n]\n\n[^n]: a\n\n    [^m]: b\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *extensionast.Footnote: label holds whitespace",
		trigger:   "a footnote definition whose label holds a space",
		body:      theAsk + "\n\n" + "[^x y]: note\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDefinitionRefusal: typedDefinitionInItem",
		trigger:   "a footnote definition inside a callout with a list item around it",
		body:      theAsk + "\n\n" + ":::callout{kind=\"note\" title=\"\"}\n- item\n\n  [^n]: a\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "typedDefinitionRefusal: typedDefinitionHoldingBlock",
		trigger:   "a footnote definition inside a callout holding a list",
		body:      theAsk + "\n\n" + ":::callout{kind=\"note\" title=\"\"}\nText[^n]\n\n[^n]: a\n\n    - b\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *extensionast.Table: lazyRowAttr",
		trigger:   "a quoted table a lazy line would be a row of",
		body:      theAsk + "\n\n" + "> | a |\n> | - |\n> | b |\ntail\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *extensionast.Table: blockRowAttr",
		trigger:   "a table a list item line would be a row of",
		body:      theAsk + "\n\n" + "| a | b |\n| - | - |\n| 1 | 2 |\n2. a\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *extensionast.Table: wideRowAttr",
		trigger:   "a table row holding more cells than its table",
		body:      theAsk + "\n\n" + "| a | b |\n| - | - |\n| `x | y` | z |\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *ast.Heading: underlinedTextAttr",
		trigger:   "a lone - under a table that text stands before",
		body:      theAsk + "\n\n" + "text\n| a | b |\n| - | - |\n| 1 | 2 |\n-\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *ast.FencedCodeBlock: decoded language",
		trigger:   "a fenced code block whose language holds a backslash escape",
		body:      theAsk + "\n\n" + "```a\\*b\nx\n```\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *ast.CodeBlock: held open by a list",
		trigger:   "an indented code block right after a list",
		body:      theAsk + "\n\n" + "100. a\n\n    x\n    y\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "blockRefusal *ast.CodeBlock: held open by a quote",
		trigger:   "an indented code block right after a quote",
		body:      theAsk + "\n\n" + ">\n    a\n    b\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "paragraphDirectiveReason: opening continues a paragraph",
		trigger:   "an indented ask opening continuing a paragraph",
		body:      theAsk + "\n\n" + "Context\n    :::ask{urgency=\"med\"}\nShip it?\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason",
		trigger:   "a Pandoc fenced div",
		body:      theAsk + "\n\n" + "::: {.callout}\nBody.\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: leaf directives",
		trigger:   "a leaf directive",
		body:      theAsk + "\n\n" + "::callout{#b4}\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: text directives",
		trigger:   "a text directive",
		body:      theAsk + "\n\n" + ":callout{#b4}\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "browserListSpacing: emptyItemEndsOuterItem",
		trigger:   "an empty list item before a blank line and a block its outer item holds",
		body:      theAsk + "\n\n" + "- t\n\n  -\n\n  para\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "nestingError: maxNesting blocks",
		trigger:   "101 nested quotes",
		body:      theAsk + "\n\n" + "> > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > > deep\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "nestingError: maxTypedNesting typed blocks",
		trigger:   "33 nested callouts",
		body:      theAsk + "\n\n" + strings.Repeat(":::callout{kind=\"note\" title=\"\"}\n", 33) + "deep\n" + strings.Repeat(":::\n", 33),
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "nestingError: maxInlineNesting marks",
		trigger:   "101 nested strong marks",
		body:      theAsk + "\n\n" + "**********************************************************************************************************************************************************************************************************deep**********************************************************************************************************************************************************************************************************\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "Validate: typed block content",
		trigger:   "a requirement block holding a list where its rule takes paragraphs",
		body:      theAsk + "\n\n" + ":::requirement{}\n- a\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason: "ErrTooManyElements: MaxDocumentElements",
		// A heading and its text weigh four elements, so this many of them pass the cap by one
		// heading, which is the 16,385 the measurement wrote.
		trigger:   "16,385 headings, past the element cap",
		body:      theAsk + "\n\n" + strings.Repeat("# a\n\n", pmdoc.MaxDocumentElements/4+1),
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "ErrTablePadding: maxTablePaddingCells",
		trigger:   "a 110-column table with 95 one-cell rows",
		body:      theAsk + "\n\n" + tablePastThePaddingBudget(110, 95),
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:  "unsupportedDirective: malformedDirectiveReason (a ::::ask with no attributes, whose ask the rewrite opens)",
		trigger: "the four-colon ask of packages/dispatch/e2e/unknown-directive.stub.e2e.ts:50, holding a :::note after its list",
		body:    "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n",
		// The browser draws no card here, and the rule refuses the body all the same: the 0
		// cards come from the ask's content rule, which the drawn `:::note` breaks, and not from
		// an opener that opens nothing. The reader's container rule opens the ask, which is what
		// is refused.
		cards:     0,
		position:  2,
		blockType: "ask",
	},
	{
		reason:  "unsupportedDirective: malformedDirectiveReason (an unknown :::note container)",
		trigger: "an unknown :::note container, the decision block standing inside it",
		body:    "Some **bold** text\n\n:::note\n:::ask{urgency=\"med\"}\nWhich transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n:::\n:::\n",
		cards:   1,
		// The `:::note` line carries no attributes, so this parser opens no container on it and
		// the decision block is a top-level block of its own, the third: the browser's parser
		// opens its note container there and draws the card inside it, at the body's second block.
		// Both draw the one card, which is what the body is refused for; the place they name it at
		// is the place each of them reads the body to hold it.
		position:  3,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an opening line carrying no attributes)",
		trigger:   "a decision block whose opening line carries no attributes",
		body:      ":::ask\nWhich one?\n\n- A: first\n- B: second\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an opening line of four colons)",
		trigger:   "a decision block opened with four colons and no attributes",
		body:      "::::ask\nWhich one?\n\n- A: first\n- B: second\n::::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (the production probe of INFRA-1201)",
		trigger:   "the attribute-less decision block written into production to measure what it does",
		body:      ":::ask\nDoes production refuse this?\n\n- Yes: refused\n- No: stored\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an attribute-less opening line in a quote)",
		trigger:   "a quoted decision block whose opening line carries no attributes",
		body:      "Lead text.\n\n> :::ask\n> Which one?\n>\n> - A: first\n> - B: second\n> :::\n",
		cards:     1,
		position:  2,
		blockType: "blockquote",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an attribute-less opening line in a callout)",
		trigger:   "a decision block inside a callout whose opening line carries no attributes",
		body:      ":::callout{#c1 kind=\"note\"}\nA note.\n\n:::ask\nWhich one?\n\n- A: first\n- B: second\n:::\n:::\n",
		cards:     1,
		position:  1,
		blockType: "callout",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an attribute-less opening line in a list item)",
		trigger:   "a decision block inside a list item whose opening line carries no attributes",
		body:      "Lead text.\n\n- Context for the question\n\n  :::ask\n  Which one?\n  :::\n",
		cards:     1,
		position:  2,
		blockType: "bullet_list",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (an attribute-less opening line with no closing fence)",
		trigger:   "a decision block carrying no attributes and left unclosed at the body's end",
		body:      ":::ask\nWhich one?\n\n- A: first\n- B: second\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (a space after the attributes)",
		trigger:   "a decision block whose opening line ends in a space",
		body:      ":::ask{#b1} \nWhich one?\n\n- A: first\n- B: second\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:    "unsupportedDirective: malformedDirectiveReason (a label before the attributes)",
		trigger:   "a decision block whose opening line carries a directive label",
		body:      ":::ask[Q]{#b1}\nWhich one?\n\n- A: first\n- B: second\n:::\n",
		cards:     1,
		position:  1,
		blockType: "ask",
	},
	{
		reason:  "no refusal: a decision block holding a heading",
		trigger: "a heading inside the decision block, which the browser reader draws no card for",
		body:    ":::ask{#b1}\n# A heading\n\nWhich one?\n:::\n",
		cards:   0,
		// The browser editor's `ask` takes `paragraph+ bullet_list?`, so its reader draws this
		// block's lines in place as text rather than as a card, where the server's parse reads
		// the block and the rule refuses the body for the ask standing in it: the content of an
		// ask the reader opens decides nothing (INFRA-1201).
		position:  1,
		blockType: "ask",
		parsed:    true,
	},
	{
		reason:    "no refusal: a decision block whose paragraph follows its list",
		trigger:   "a paragraph after the decision block's list, which the browser reader draws no card for",
		body:      ":::ask{#b1}\nWhich one?\n\n- A: first\n- B: second\n\nAfter the list\n:::\n",
		cards:     0,
		position:  1,
		blockType: "ask",
		parsed:    true,
	},
}

// decisionRefusalReasons is how many bodies the measurement answers for, counted here so that a
// row dropped from the table fails rather than passes in silence.
const decisionRefusalReasons = 44

// Every one of the measured bodies is refused with a *DecisionBlock naming the top-level block
// the decision block stands in, which is the block the browser reader draws its card in wherever
// it draws one. The parse's own verdict decides nothing, and neither does the ask's content rule:
// a body the reader opens an `ask` container in is refused whatever that ask holds. Each row but
// the control is a body the parse refuses, so the tree the rule reads is the one that refusal
// leaves behind (pmdoc.FindDecisionCard); the row asserts that refusal still stands, since a body
// the parse came to read would stop driving the reason its name carries.
func TestCheckRefusesEveryMeasuredBodyTheReaderOpensAnAskIn(t *testing.T) {
	for _, test := range decisionRefusalRows {
		t.Run(test.reason, func(t *testing.T) {
			if _, doc := split(test.body); (doc != nil) != test.parsed {
				t.Fatalf("the parse reads the body = %v, want %v: the row no longer drives %q (%s)",
					doc != nil, test.parsed, test.reason, test.trigger)
			}
			err := requireRefusal(t, test.reason, test.body,
				decisionRefusal("body", test.position, test.blockType))
			var decision *DecisionBlock
			if !errors.As(err, &decision) {
				t.Fatalf("Check returned %T, want *DecisionBlock", err)
			}
			if decision.Position != test.position || decision.Type != test.blockType {
				t.Fatalf("Check = %+v, want position %d, type %q", decision, test.position, test.blockType)
			}
		})
	}
	if len(decisionRefusalRows) != decisionRefusalReasons {
		t.Fatalf("the table holds %d bodies, want the %d measured", len(decisionRefusalRows), decisionRefusalReasons)
	}
}
