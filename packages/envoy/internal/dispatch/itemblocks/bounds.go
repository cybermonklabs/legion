package itemblocks

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// CapExceeded is one block of a body over contracts.DispatchBlockUnits. Field is the field the
// body arrived in, Position the block's place in the body counted from 1, Type the block's Proof
// node type, Units its length in UTF-16 code units and Limit the bound it passed. The refusal's
// wording lives in Error() and nowhere else; a caller adds only the HTTP status and the code.
type CapExceeded struct {
	Field    string
	Position int
	Type     string
	Units    int
	Limit    int
}

func (e *CapExceeded) Error() string {
	return fmt.Sprintf("%s block %d (%s) is %d characters over the %d-character block limit (%d/%d)",
		e.Field, e.Position, e.Type, e.Units-e.Limit, e.Limit, e.Units, e.Limit)
}

// CountExceeded is a body holding more blocks than contracts.DispatchItemBlocks. Field is the
// field the body arrived in, Blocks how many blocks it holds and Limit the bound it passed. The
// refusal's wording lives in Error() and nowhere else, as CapExceeded's does.
type CountExceeded struct {
	Field  string
	Blocks int
	Limit  int
}

func (e *CountExceeded) Error() string {
	return fmt.Sprintf("%s is %d blocks over the %d-block limit (%d/%d)",
		e.Field, e.Blocks-e.Limit, e.Limit, e.Blocks, e.Limit)
}

// DecisionBlock is a body holding a decision block: an `ask` node, which the reader draws a
// decision card for and which nobody can answer where it stands, since only a document's own
// decision block and an ask carry an answer. Field is the field the body arrived in, Position the
// place, counted from 1, of the top-level block holding the decision block, and Type that block's
// type, which is the decision block's own type only where it stands at the top: the Proof node
// type of the block the parser read, or, in a body the parser refused, what the reader's own block
// of that kind is called (pmdoc.FindDecisionCard). The refusal's wording lives in Error() and
// nowhere else, as CapExceeded's does.
type DecisionBlock struct {
	Field    string
	Position int
	Type     string
}

func (e *DecisionBlock) Error() string {
	return fmt.Sprintf("%s block %d (%s) holds a decision block, which nobody can answer where it stands: "+
		"ask the question in the issue's own document, as a decision block there, or open an ask with "+
		"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
		e.Field, e.Position, e.Type)
}

// Check answers the first rule body breaks, or nil when it breaks none: a *CountExceeded when the
// body holds more than contracts.DispatchItemBlocks blocks, else a *CapExceeded naming the first
// block over contracts.DispatchBlockUnits, else a *DecisionBlock naming the first top-level block
// that holds a decision block. The count is checked first, so a body built to break the indexer is
// refused for how many blocks it holds rather than for one long block, and both bounds are checked
// before the decision block, so a body that also breaks a bound keeps the bound's refusal, its
// wording and its code.
//
// The blocks are Split's, the body as its author wrote it, never the canonical rendering: a table
// the renderer pads and a list it escapes are measured as written, which is what is stored.
//
// The decision block is read from the reader's tree, not from the blocks and not from the parser's
// verdict: what nobody can answer is the card the browser reader draws, and it draws one for each
// `ask` its own parser makes, at any depth and whether or not the server's parse went on to refuse
// the body for something else it holds. So a body the parser read is answered from its tree, where
// the parser opens a typed directive inside a quote, a list item and a callout, and a body the
// parser refused is answered from the tree that refusal left behind (pmdoc.FindDecisionCard),
// which is the one the reader reads. Where the parser read the body but its block starts could not
// be placed, the blocks are the blank-line fallback's while the tree is the parser's, so the
// position named here is the tree's own top-level index, which the fallback's count can differ
// from.
//
// A body holding a lone carriage return is read for a decision block a second time, over a copy
// whose lone carriage returns are line feeds: the browser's reader ends a line at a lone carriage
// return and this parser ends none there, so a body written with them reaches the parse here as
// one line holding no block while the reader reads its lines and draws its card for the decision
// block standing among them. The copy is this rule's own reading and reaches nothing else: the
// bounds above are measured on the body as written, Split's blocks are the body as written, and
// the body a caller goes on to store is the one it passed in, byte for byte. The position named
// from that reading is the reader's own top-level index, which is the one the card stands at.
func Check(field, body string) error {
	blocks, doc := split(body)
	if len(blocks) > contracts.DispatchItemBlocks {
		return &CountExceeded{Field: field, Blocks: len(blocks), Limit: contracts.DispatchItemBlocks}
	}
	for index, block := range blocks {
		if units := Units16(block.Text); units > contracts.DispatchBlockUnits {
			return &CapExceeded{
				Field:    field,
				Position: index + 1,
				Type:     block.Type,
				Units:    units,
				Limit:    contracts.DispatchBlockUnits,
			}
		}
	}
	// A body of only whitespace has no blocks and no tree, and holds nothing to draw.
	if doc == nil && len(blocks) == 0 {
		return nil
	}
	if card, holds := decisionCard(body, doc); holds {
		return &DecisionBlock{Field: field, Position: card.Position, Type: card.Type}
	}
	asTheReaderEndsItsLines, loneCarriageReturn := lineFeedsForLoneCarriageReturns(body)
	if !loneCarriageReturn {
		return nil
	}
	reread, err := pmdoc.Parse(asTheReaderEndsItsLines)
	if err != nil {
		reread = nil
	}
	if card, holds := decisionCard(asTheReaderEndsItsLines, reread); holds {
		return &DecisionBlock{Field: field, Position: card.Position, Type: card.Type}
	}
	return nil
}

// decisionCard is where the browser reader draws the first decision card in body, and whether it
// draws one at all: doc is the tree the parser read body as, which the `ask` is looked for in
// where the parser read the body, and nil where the parser refused it, which is read from the
// tree that refusal left behind instead (pmdoc.FindDecisionCard).
func decisionCard(body string, doc *pmdoc.Node) (pmdoc.DecisionCard, bool) {
	if doc == nil {
		// The parse refused the body, which says nothing about the card: the browser reader draws
		// one for the `ask` its parser reads whether or not the server could store what stands
		// beside it, so a refused body is read for that card rather than taken as holding none.
		return pmdoc.FindDecisionCard(body)
	}
	for index, child := range doc.Children {
		if pmdoc.HoldsAskBlock(child) {
			return pmdoc.DecisionCard{Position: index + 1, Type: child.Type}, true
		}
	}
	return pmdoc.DecisionCard{}, false
}

// lineFeedsForLoneCarriageReturns is body with every carriage return that no line feed follows
// written as a line feed, and whether body held such a carriage return at all. A carriage return
// a line feed follows is half of one CR LF line ending and stands as it was: making a line feed
// of it too would end two lines where both parsers end one, splitting a block neither of them
// splits. A body holding no lone carriage return is answered as it came, unread and uncopied, so
// an ordinary body pays one scan for its carriage returns and nothing more.
func lineFeedsForLoneCarriageReturns(body string) (string, bool) {
	first := firstLoneCarriageReturn(body)
	if first < 0 {
		return body, false
	}
	var written strings.Builder
	written.Grow(len(body))
	written.WriteString(body[:first])
	for index := first; index < len(body); index++ {
		if body[index] == '\r' && (index+1 == len(body) || body[index+1] != '\n') {
			written.WriteByte('\n')
			continue
		}
		written.WriteByte(body[index])
	}
	return written.String(), true
}

// firstLoneCarriageReturn is where body's first carriage return that no line feed follows stands,
// and -1 where every carriage return body holds is half of a CR LF pair, the body's end counting
// as no line feed.
func firstLoneCarriageReturn(body string) int {
	for from := 0; from < len(body); {
		index := strings.IndexByte(body[from:], '\r')
		if index < 0 {
			return -1
		}
		at := from + index
		if at+1 == len(body) || body[at+1] != '\n' {
			return at
		}
		from = at + 2
	}
	return -1
}

// Units16 is text's length in UTF-16 code units, how the server counts a text: a rune outside the
// Basic Multilingual Plane counts as the two units its surrogate pair takes. A byte that is not
// valid UTF-8 reads as U+FFFD, one unit, as the JSON encoder writes it.
func Units16(text string) int {
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	return units
}
