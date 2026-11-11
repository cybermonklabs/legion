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

// decisionRefusalRows are the 33 bodies measured for INFRA-1201: every distinct reason
// pmdoc/refusals.go and the parse around it refuse a body for, each with a decision block standing
// in it, and the two bodies whose decision block neither parser reads as a typed directive. Each
// row is named by the reason its trigger raises, so a reader of a failure reads the class of body
// that broke, and carries what the browser reader was measured to draw for that body: cards is how
// many decision cards it drew, and position and blockType the top-level block the card stands in.
//
// A body too long to read as a literal is built here as the measurement built it: the 33 nested
// callouts of the typed-nesting bound, the headings past the element cap, and the 110-column table
// over 95 one-cell rows whose padding passes what one write may add.
var decisionRefusalRows = []struct {
	// reason is the refusal the body's trigger raises, "no refusal" for the control.
	reason string
	// trigger is what was written into the body beside the decision block.
	trigger string
	body    string
	// cards is how many decision cards the browser reader drew for the body: 1 for every body
	// holding an `ask` its parser makes a node for, 0 for the two it reads as text.
	cards int
	// position and blockType are the top-level block the card stands in, which the refusal names;
	// both are zero where no card is drawn.
	position  int
	blockType string
	// parsed says the server's parse read the body: true for the control alone, since every other
	// row exists to drive the rule over a body the parse refuses.
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
		reason:  "unsupportedDirective: malformedDirectiveReason (an ask whose content also breaks paragraph+ bullet_list?)",
		trigger: "the four-colon ask of packages/dispatch/e2e/unknown-directive.stub.e2e.ts:50, holding a :::note after its list",
		body:    "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n",
		cards:   0,
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
}

// decisionRefusalReasons is how many bodies the measurement answers for, counted here so that a
// row dropped from the table fails rather than passes in silence.
const decisionRefusalReasons = 33

// Every one of the measured bodies is answered as the browser reader draws it: one the reader
// draws a decision card for is refused with a *DecisionBlock naming the top-level block the card
// stands in, and one it draws no card for is stored. The parse's own verdict decides nothing. Each
// row but the control is a body the parse refuses, so the tree the rule reads is the one that
// refusal leaves behind (pmdoc.FindDecisionCard); the row asserts that refusal still stands, since
// a body the parse came to read would stop driving the reason its name carries.
func TestCheckAnswersEveryMeasuredBodyAsTheReaderDrawsIt(t *testing.T) {
	for _, test := range decisionRefusalRows {
		t.Run(test.reason, func(t *testing.T) {
			if _, doc := split(test.body); (doc != nil) != test.parsed {
				t.Fatalf("the parse reads the body = %v, want %v: the row no longer drives %q (%s)",
					doc != nil, test.parsed, test.reason, test.trigger)
			}
			if test.cards == 0 {
				requirePasses(t, test.reason, test.body)
				return
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
