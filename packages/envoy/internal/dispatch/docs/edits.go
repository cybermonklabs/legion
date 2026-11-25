package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// opIndex is the batch position of the operation an error came from. The API renders these
// errors from their own text rather than the wrapped chain, so each carries its own index; an
// error raised outside a batch leaves it nil and reads exactly as it did before.
type opIndex struct{ operation *int }

func (o *opIndex) setOperation(index int) { o.operation = &index }

func (o opIndex) prefix() string {
	if o.operation == nil {
		return ""
	}
	return fmt.Sprintf("operation %d: ", *o.operation)
}

// operationStamped is every error that renders its own operation index, so a new one joins by
// embedding opIndex rather than by being listed here.
type operationStamped interface {
	error
	setOperation(int)
}

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	opIndex
	Field  string
	Reason string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("%sinvalid document operation field %q: %s", e.prefix(), e.Field, e.Reason)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}

type ErrQuoteNotFound struct {
	opIndex
	Quote   string
	Nearest []string
	// Matches is how many places the quote did match, nonzero only when an occurrence was out
	// of range: the quote is right and the occurrence is not.
	Matches int
	// Cells is the text of each table cell a quote written as a row's cells names, one per part,
	// when its pipes are the boundaries between one row's cells rather than text a cell holds
	// (tableRowQuoted), and Field the operation field the quote was written in.
	Cells []string
	Field string
}

func (e *ErrQuoteNotFound) Error() string {
	if e.Matches > 0 {
		places := "places"
		if e.Matches == 1 {
			places = "place"
		}
		return fmt.Sprintf(
			"%squote %q matches %d %s, so that occurrence is out of range; occurrence is zero-based; nearest blocks: %s",
			e.prefix(), e.Quote, e.Matches, places, pmdoc.QuoteBlocks(e.Nearest),
		)
	}
	if len(e.Cells) > 0 {
		return fmt.Sprintf(
			`%squote %q not found: it runs across table cells, and " | " is the boundary markdown writes between two cells, not text either cell holds; a quote matches inside one table cell; %s`,
			e.prefix(), e.Quote, oneCellAdvice(e.Field, e.Cells),
		)
	}
	if item := pmdoc.ParseListItemQuote(e.Quote); strings.TrimSpace(item.Text) != "" {
		// The marker, and a task box after it, are no text the item holds: they select an item of
		// their kind whose text starts with the rest, so a miss is the rest's, the kind's or the
		// box's, and the rest alone is the quote that matches the text anywhere.
		list := "a bullet list"
		if item.Kind == pmdoc.MarkerOrdered {
			list = "an ordered list"
		}
		selects := "an item of " + list
		if item.Checked != nil {
			selects = "an unchecked task item of " + list
			if *item.Checked {
				selects = "a checked task item of " + list
			}
		}
		return fmt.Sprintf(
			"%squote %q not found; a leading list marker is no text the item holds: it selects %s, its number not matched, whose text starts with the rest; quote the item's text without the marker (%q) to match it anywhere; nearest blocks: %s",
			e.prefix(), e.Quote, selects, strings.TrimSpace(item.Text), pmdoc.QuoteBlocks(e.Nearest),
		)
	}
	return fmt.Sprintf(`%squote %q not found; quotes match the block text as rendered (inline markdown is tolerated; use "heading:<title>", "block:<id>", "start", or "end" as insert and move anchors); nearest blocks: %s`, e.prefix(), e.Quote, pmdoc.QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return pmdoc.ErrTargetNotFound }

// ErrBlockNotFound names a block id an operation wrote that the document does not carry. A block
// id is read from the document rather than written by the caller, and a later version of the
// document can hold the same words under another id, so the refusal names the quote form of the
// field the id was written in, which the document's own words answer, and the id the block whose
// text the operation quotes carries now.
type ErrBlockNotFound struct {
	opIndex
	// Block is the id the operation wrote, Op the operation that wrote it, and Field the
	// operation field it stood in: "after", "before" or "block".
	Block string
	Op    string
	Field string
	// Quote is the text the operation carries that identifies a block - a replace's find, a
	// delete's quote - and Current the id the block holding that text carries now. Both are empty
	// where the operation carries no such text, and where two or more blocks hold it, since
	// neither is the block the caller meant.
	Quote   string
	Current string
}

func (e *ErrBlockNotFound) Error() string {
	current := ""
	if e.Current != "" {
		current = fmt.Sprintf(" The block whose text is %q now has the id %q.", e.Quote, e.Current)
	}
	return fmt.Sprintf("%sblock %q is not in the document.%s%s", e.prefix(), e.Block, current, e.quoteForm())
}

// quoteForm is the quote form of the field the missing id stood in: an insert's or a move's anchor
// takes the anchored block's own text in place of `block:<id>`, and a delete takes the removed
// block's text in find. The block a move, a retype or a table operation names is addressed by id
// alone, so that refusal names no quote form.
func (e *ErrBlockNotFound) quoteForm() string {
	switch {
	case e.Field == "after" || e.Field == "before":
		return fmt.Sprintf(" Anchor by quote instead: write the text of that block in %s, in place of \"block:%s\".", e.Field, e.Block)
	case e.Op == "delete":
		return " Name the block by its text instead: write that text in find."
	}
	return ""
}

func (e *ErrBlockNotFound) Unwrap() error { return pmdoc.ErrTargetNotFound }

// blockNotFound dresses a refusal of blockID, written in field, which the tree does not carry: the
// pmdoc call reports it as a target miss, and the caller reads ErrBlockNotFound's words instead.
// Any other error is the caller's to read as it is.
func blockNotFound(tree *batchTree, op model.EditOp, field, blockID string, err error) error {
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		return err
	}
	missing := &ErrBlockNotFound{Block: blockID, Op: op.Op, Field: field}
	// find is the one field of an operation that identifies a block by its text: a replace's
	// find, a delete's quote.
	if current, named := blockHoldingText(tree, op.Find); named {
		missing.Quote, missing.Current = op.Find, current
	}
	return missing
}

// blockHoldingText is the id of the one block whose text quote matches. It names none where the
// quote matches nothing, where it matches in two or more blocks, since neither is the block the
// caller meant, and where the tree cannot be read at all, which the operation itself reports.
func blockHoldingText(tree *batchTree, quote string) (string, bool) {
	if quote == "" {
		return "", false
	}
	finder, err := tree.finder()
	if err != nil {
		return "", false
	}
	matches, err := finder.NearestFirst(quote, 0)
	if err != nil {
		return "", false
	}
	current := ""
	for _, match := range matches {
		blockID, err := pmdoc.BlockIDForRange(tree.node, match)
		if err != nil || blockID == "" {
			return "", false
		}
		if current != "" && current != blockID {
			return "", false
		}
		current = blockID
	}
	return current, current != ""
}

// ErrAnchorAmbiguous names a quote or a `heading:` anchor that resolved in several places, with
// the candidates the API hands back so the caller can pick one.
type ErrAnchorAmbiguous struct {
	opIndex
	// Kind is the noun the caller wrote: "quote" or "heading anchor".
	Kind       string
	Target     string
	Candidates []pmdoc.Candidate
}

func (e *ErrAnchorAmbiguous) Error() string {
	// A heading anchor matches whole headings, so there is no surrounding text to quote more of.
	advice := "pass a zero-based occurrence to choose one"
	if e.Kind == "quote" {
		advice = "pass a zero-based occurrence, or quote more of the surrounding text"
	}
	return fmt.Sprintf("%s%s %q matches %d places; %s", e.prefix(), e.Kind, e.Target, len(e.Candidates), advice)
}

// ErrQuoteSpansBlocks names a quote that reached across a block boundary. A quote-anchored edit
// stays inside one textblock, so the fix is to quote less, or to address the blocks by id.
type ErrQuoteSpansBlocks struct {
	opIndex
	Quote string
	// Cells is the text of the table cell the quote starts in and the one it ends in, when both
	// are table cells, and Field the operation field the quote was written in.
	Cells []string
	Field string
}

func (e *ErrQuoteSpansBlocks) Error() string {
	if len(e.Cells) > 0 {
		return fmt.Sprintf(
			"%squote %q spans more than one block, reaching from one table cell into another; a quote matches inside one table cell; %s",
			e.prefix(), e.Quote, oneCellAdvice(e.Field, e.Cells),
		)
	}
	return fmt.Sprintf(
		"%squote %q spans more than one block; a quote-anchored edit stays inside one block, so quote text from a single block, or address whole blocks by id",
		e.prefix(), e.Quote,
	)
}

// oneCellAdvice says what to quote in place of a quote that reached across the named cells: an
// insert or move anchor (`after`, `before`) takes one cell's text, and a find takes one cell's
// text in each operation of the batch. The cells are written `"a", "b"`, not with the ` | `
// QuoteBlocks writes between nearest blocks, since a cell's text can hold a pipe.
func oneCellAdvice(field string, cells []string) string {
	quoted := make([]string, len(cells))
	for index, cell := range cells {
		quoted[index] = strconv.Quote(cell)
	}
	list := strings.Join(quoted, ", ")
	if field == "after" || field == "before" {
		return fmt.Sprintf("anchor on one cell's own text, one of %s", list)
	}
	return fmt.Sprintf("quote one cell's own text in each operation, one operation per cell in the same batch; the cells are %s", list)
}

func (e *ErrQuoteSpansBlocks) Unwrap() error { return pmdoc.ErrTargetSpansBlocks }

// ErrFindCrossesSpan names a document edit's find, a replace's or a delete's, that crosses a
// marked span - bold, italic, strikethrough, a link, code (pmdoc.TextblockAt.CrossedSpan). A link
// the edit removes whole must be spelled as it is wherever the find spells it, so a find that
// spells such a link with another URL or title crosses it: the edit would remove a link another
// writer may have changed since the find was read, its URL and title with it, with no word of the
// change. That is a find over the link's whole text, and one over part of it whose edit removes
// the rest too: a delete's whose list marker selected the item holding the link, since that
// delete removes the whole item (removedText), and a replace's whose with is blocks, whose write
// drops the whitespace beside the find, where that is all the link holds beyond it
// (blockReplacementRemoves). An edit that removes only part of a link changes only the text its
// find names, and the rest of the link keeps the document's URL and title. A replace's find also
// crosses the edge of every span it covers text inside and text outside of, unless it spells the
// span's own markup over all of the span's text it covers: its with written over both lands
// outside the span, so a link it covered would go with its URL, and a span it cut to end on a
// space would be written with character references. Even then it crosses a bold or italic span's
// edge where the part of the span it leaves would meet the written text at a space, which markdown
// cannot write the span's delimiters beside (pmdoc.LeavesEdgeOnSpace). An insert's or a move's
// anchor names a place, not text to change, so it crosses no span (quoteInOneTextblock).
//
// The refusal names the find that covers the very text the find named and selects as it does,
// so the edit removes what the refused one would have, with the occurrence at which it matches
// that text (findKeepingRange): the text it matched written with each span the document holds
// over it, a link under its own URL and title, after the list marker that selected the find's
// item where one did (Own). Where it names none, it gives the advice of the span that stops every
// such find (Blocked), the span the find crosses or another whose edge the find crosses too: for
// a struck span, whose markup no find spells, to replace the text inside it and the text outside
// it in two operations; for a bold or italic span the find cuts so that the part it leaves would
// meet the written text at a space, to move that space out of the span (Kept); and otherwise that
// no find written from the document's text covers that text, and to read the document again. A
// replace's find that spells a span's markup replaces that markup too, so with decides that
// formatting, and one that is a bare link's own text writes with inside the link.
type ErrFindCrossesSpan struct {
	opIndex
	Quote string
	Span  pmdoc.Span
	// Crossing is why the find crosses the span, which the refusal says.
	Crossing pmdoc.Crossing
	// Own is the find to send in place of this one: the list marker, and the task box after it,
	// that the find opens with where they selected the item its match opens (resolveEditQuote's
	// byMarker), then the text the find matched with each span the document holds over it written
	// with its markup, the span the find crosses and a link under its own URL and title
	// (pmdoc.TextblockAt.SpanOver). It is zero where no match of it is that text, selected as the
	// find selected it, or it would be refused there itself (findKeepingRange).
	Own NamedFind
	// OwnIsText says SpanOver wrote the span the find crosses as its bare text, as it writes a bare
	// link, whose markup is that text: a replace's with is then written inside the link.
	OwnIsText bool
	// Blocked is the span that stops every find where Own is zero: the span the find written from
	// the document's text crosses where it matches that text and is refused there, and Span
	// otherwise.
	Blocked pmdoc.Span
	// Kept is the part of Blocked, a bold or italic span, that the find leaves where Own is zero
	// and that part would meet the written text at a space (pmdoc.TextblockAt.KeptOnSpace), and ""
	// otherwise.
	Kept string
	// Writes says the find is a replace's, whose with is written over its match.
	Writes bool
}

// NamedFind is a find a refusal names in place of the refused one: the quote, and the occurrence
// to send it with, nil where it is the quote's only match and the refused find named none.
type NamedFind struct {
	Quote      string
	Occurrence *int
}

// written is the find as a refusal writes it: the quote in parentheses, and the occurrence to send
// it with where it needs one.
func (n NamedFind) written() string {
	if n.Occurrence == nil {
		return fmt.Sprintf(" (%q)", n.Quote)
	}
	return fmt.Sprintf(" (%q) with occurrence %d", n.Quote, *n.Occurrence)
}

// Kind is what the span the find crosses is, as the refusal names it: "link", "bold", "italic",
// "strikethrough" or "code".
func (e *ErrFindCrossesSpan) Kind() string {
	return spanKind(e.Span.Mark.Type)
}

// spanKind is what a span of markType is, as a refusal names it.
func spanKind(markType string) string {
	switch markType {
	case "strong":
		return "bold"
	case "emphasis":
		return "italic"
	case "strike_through":
		return "strikethrough"
	case "inlineCode":
		return "code"
	}
	return markType
}

// spanNoun is a span of markType as a refusal's sentence names it: "link", or its kind's span.
func spanNoun(markType string) string {
	if markType == "link" {
		return "link"
	}
	return spanKind(markType) + " span"
}

// What a replace's with does over a find: one that spells a span's markup replaces that markup
// too, and one that is a bare link's own text, which a find matches as written, spells none, so
// the with is written inside the link (pmdoc.WithoutQuotedMarkup).
const (
	replacesMarkup   = ", which then replaces the markup too, so with decides that formatting"
	writesInsideLink = ", which writes with inside the link"
)

// narrowInsideStrikethrough is the advice where a struck span stops every find over the text a
// find named: no find spells strikethrough's markup, so the text inside the span and the text
// outside it are replaced in two operations.
const narrowInsideStrikethrough = "narrow find to the text inside the strikethrough span, and replace the text outside it in an operation of its own, since a find cannot spell strikethrough's markup"

// leavesOnSpace is why no find keeps the part, kept, of a bold or italic span (noun) that a find
// cutting the span leaves where that part would meet the text written over the find at a space,
// and the advice: markdown cannot write the span's delimiters there
// (pmdoc.TextblockAt.KeptOnSpace).
func leavesOnSpace(noun, kept string) string {
	return fmt.Sprintf("the part of the %s it leaves, %q, would meet the text written over it at a space, "+
		"where markdown cannot write the %s's delimiters; move that space out of the %s", noun, kept, noun, noun)
}

func (e *ErrFindCrossesSpan) Error() string {
	noun := spanNoun(e.Span.Mark.Type)
	// crossesEdge opens the refusal of a find that covers text inside the span and text outside
	// it, and spellsAnother that of one over the link's whole text and nothing more that spells
	// it as another.
	crossesEdge := fmt.Sprintf("%sfind %q crosses the edge of the %s %q: it covers text inside the %s and text outside it, and ",
		e.prefix(), e.Quote, noun, e.Span.Text, noun)
	spellsAnother := fmt.Sprintf("%sfind %q spells the link %q with another URL or title than the one it has; ", e.prefix(), e.Quote, e.Span.Text)
	switch e.Crossing {
	// A find, a delete's or a replace's, over a whole link it spells with another URL or title.
	case pmdoc.SpellsAnotherLinkAlone:
		// A bare link's markup is its own text, so a replace's find that is that text, after the
		// marker that selected its item, spells no markup, and its with is written inside the link.
		if e.OwnIsText {
			return spellsAnother + e.named("write the link as it is in find", writesInsideLink)
		}
		return spellsAnother + e.named("write the link as it is in find", replacesMarkup)
	case pmdoc.SpellsAnotherLink:
		return crossesEdge + "it spells the link with another URL or title than the one it has; " +
			e.named("write the whole link as it is in find", replacesMarkup)
	// A find over part of a link whose edit removes the rest of the link too: a delete's whose
	// marker selected the item holding the link, which the delete removes whole, or a replace's
	// whose with is blocks, whose write drops the whitespace beside the find, all the link holds
	// beyond it.
	case pmdoc.RemovesAnotherLink:
		removes := fmt.Sprintf("%sfind %q selects a list item, which the delete removes whole with the link %q", e.prefix(), e.Quote, e.Span.Text)
		if e.Writes {
			removes = fmt.Sprintf("%sfind %q covers part of the link %q, whose rest is whitespace beside find that a with of blocks drops, so the replace removes the link whole", e.prefix(), e.Quote, e.Span.Text)
		}
		return removes + ", and find spells that link with another URL or title than the one it has; " +
			e.named("write the link as it is in find", replacesMarkup)
	// pmdoc.LeavesEdgeOnSpace and pmdoc.CrossesEdge, which CrossedSpan answers only for a find text
	// is written over, a replace's: a delete's find crosses a span only by spelling another link
	// the delete removes whole, above.
	case pmdoc.LeavesEdgeOnSpace:
		return crossesEdge + leavesOnSpace(noun, e.Kept)
	}
	return crossesEdge + "text written over both would not keep the " + noun + " whole; " +
		e.named("to keep the text find names, spell the "+noun+"'s markup over the part of it find covers", replacesMarkup)
}

// named is the fix the refusal gives where a find keeps the text the find named: fix, then that
// find, Own, with the occurrence that matches that text where it needs one and what a replace's
// with does over it (ownWrites). Where there is none, it is the advice of the span that stops
// every find (Blocked), naming that span where it is another than the one the refusal names: for
// a struck span, two operations; for a bold or italic span whose part the find leaves would meet
// the written text at a space, to move that space; otherwise that no find written from the
// document's text covers that text, and to read the document again.
func (e *ErrFindCrossesSpan) named(fix, ownWrites string) string {
	if e.Own.Quote != "" {
		named := fix + e.Own.written()
		if e.Writes {
			named += ownWrites
		}
		return named
	}
	noun := spanNoun(e.Blocked.Mark.Type)
	// another names the span that stops every find where it is another than the one the refusal
	// names, which the find crosses too.
	another := ""
	if e.Blocked.Range != e.Span.Range {
		another = fmt.Sprintf("it crosses the edge of the %s %q too", noun, e.Blocked.Text)
	}
	switch {
	case e.Blocked.Mark.Type == "strike_through" && another == "":
		return narrowInsideStrikethrough
	case e.Blocked.Mark.Type == "strike_through":
		return "no find keeps the text find names, since " + another + ": " + narrowInsideStrikethrough
	case e.Kept != "" && another == "":
		return "no find keeps the text find names, since " + leavesOnSpace(noun, e.Kept)
	case e.Kept != "":
		return "no find keeps the text find names, since " + another + ", and " + leavesOnSpace(noun, e.Kept)
	}
	return "no find written from the document's text covers the text find names and is taken there: read the document again and quote what it holds"
}

// ErrInsertDoesNotFit names an insert whose markdown the block it lands in cannot hold where the
// anchor puts it. An insert anchored on a block lands beside that block, inside its own parent:
// the innermost typed block, list item or blockquote holding it (pmdoc.ValidDocument.BlockBoundary).
// That parent's content rule decides what the insert may write there: a requirement holds
// paragraphs alone, an ask no paragraph after its options, and a list item no block before its
// text. Such an insert was written after the typed block, at the document's own level, or beside
// the whole list, and refused nothing, which left a requirement's words outside every
// requirement, where no check's serves reached them (dispatch://INFRA-3142). The fix is to write
// what the rule holds there, or to anchor the insert on that parent itself, which writes the
// markdown outside it.
type ErrInsertDoesNotFit struct {
	opIndex
	// Field is the anchor's field, "after" or "before", and Anchor the anchor it names.
	Field, Anchor string
	// HolderType and HolderID are the type and id of the block the insert lands in, Rule its
	// content rule, and Broken what of the markdown breaks that rule there (pmdoc.ContentBreak).
	HolderType, HolderID string
	Rule                 pmdoc.BlockContentRule
	Broken               string
}

func (e *ErrInsertDoesNotFit) Error() string {
	holder, outside := strings.ReplaceAll(e.HolderType, "_", " ")+" block", "beside it"
	switch e.HolderType {
	case "list_item":
		// A list holds items alone, so markdown anchored on the item's own block lands beside the
		// list holding it.
		holder, outside = "list item", e.Field+" the list holding it"
	case "blockquote":
		holder = "blockquote"
	}
	beside := "anchor the insert on the " + holder + " itself"
	if e.HolderID != "" {
		beside += fmt.Sprintf(" (%s: %q)", e.Field, "block:"+e.HolderID)
	}
	return fmt.Sprintf(
		"%sinsert %s %q lands inside the %s %q, whose content rule, %s, cannot hold %s there; write what that rule holds there, or %s to write the markdown %s",
		e.prefix(), e.Field, e.Anchor, holder, e.HolderID, e.Rule, e.Broken, beside, outside,
	)
}

// insertDoesNotFit is the refusal of an insert of fragment's blocks at boundary, among the
// children of the block the insert lands in, whose content rule cannot hold them there, and nil
// where the rule holds them or the block holds the insert by no rule: the document, or a list
// that list content joins (pmdoc.ValidDocument.ListBoundary). A block that already breaks its
// rule, an ask a browser edit left so, is no reason to refuse this insert: the batch's ask check
// weighs the ask it leaves (validateEditedAskBlocks).
func insertDoesNotFit(boundary pmdoc.Boundary, fragment *pmdoc.Node, field, anchor string) error {
	holder := boundary.Holder
	if holder == nil {
		return nil
	}
	if rule, broken := pmdoc.ContentBreak(holder, holder.Children); rule == "" || broken != "" {
		return nil
	}
	children := make([]*pmdoc.Node, 0, len(holder.Children)+len(fragment.Children))
	children = append(children, holder.Children[:boundary.Index]...)
	children = append(children, fragment.Children...)
	children = append(children, holder.Children[boundary.Index:]...)
	rule, broken := pmdoc.ContentBreak(holder, children)
	if broken == "" {
		return nil
	}
	return &ErrInsertDoesNotFit{
		Field: field, Anchor: anchor, HolderType: holder.Type, HolderID: blockID(holder), Rule: rule, Broken: broken,
	}
}

// stampOperation gives index to an error that renders its own operation, and falls back to
// wrapping everything else with the same prefix.
func stampOperation(index int, err error) error {
	var stamped operationStamped
	if errors.As(err, &stamped) {
		stamped.setOperation(index)
		return err
	}
	return fmt.Errorf("operation %d: %w", index, err)
}

// isEditRefusal reports an error the caller wrote the batch wrong, whose own text is what the
// route serves. Wrapping one in the service's internal prose would bury the operation index and
// the anchor the reader needs. A batch whose markdown, or whose result, is too large to store is
// one: its refusal says what to shorten. So is a check block the batch leaves breaking a rule of
// a check CI proves, which keeps its own code rather than becoming an invalid operation.
func isEditRefusal(err error) bool {
	var invalid *ErrInvalidOp
	var ambiguous *ErrAnchorAmbiguous
	var ciCheck *ErrInvalidCICheck
	var crossed *ErrFindCrossesSpan
	var doesNotFit *ErrInsertDoesNotFit
	return errors.Is(err, pmdoc.ErrTargetNotFound) ||
		errors.Is(err, pmdoc.ErrTargetSpansBlocks) ||
		errors.Is(err, pmdoc.ErrTableWidth) ||
		IsTooLarge(err) ||
		errors.As(err, &invalid) ||
		errors.As(err, &ambiguous) ||
		errors.As(err, &ciCheck) ||
		errors.As(err, &crossed) ||
		errors.As(err, &doesNotFit)
}

// ErrInvalidPrecondition identifies a malformed optimistic-concurrency guard.
type ErrInvalidPrecondition struct {
	Reason string
}

func (e *ErrInvalidPrecondition) Error() string {
	return "invalid document edit precondition: " + e.Reason
}

// ErrPreconditionBusy reports that a room's bounded conditional-edit queue is full.
var ErrPreconditionBusy = errors.New("document conditional edit queue is full")

// ErrPreconditionUnavailable reports a whole-document upload repairing a document whose live
// tree is outside the Proof schema and naming a token for the version it replaces. Such a tree
// renders no markdown and has no token at all, so the caller named a token this document has
// never had: the upload is refused rather than taken with its guard dropped, which would tell
// the caller its write was conditional when nothing had checked it. The write read the live tree
// to decide it and left it untouched. Its caller knows the document by name and words the
// refusal for its reader (api's upload).
var ErrPreconditionUnavailable = errors.New("an upload repairing a document outside the Proof schema must name no token, since such a document has none")

// ErrPreconditionFailed reports a stale document or block token, or an edit under a block guard
// that removes a block the guard does not cover, without changing the live tree. CurrentDocument
// is the token the live tree carries now, which is what the write read. How far behind the client
// is, and what it should do next, are the caller's: the write reads no version rows and knows no
// command its reader runs, so the route that answers the client words both (api's
// writeUploadDocumentError).
type ErrPreconditionFailed struct {
	CurrentDocument string
	Mismatches      []model.EditPreconditionMismatch
	// Advice is the way back from a block the document no longer holds, which a whole-document
	// upload mints a fresh id for whenever it changed that block: the quote anchor form, and the
	// id that block carries now where the batch's own text names exactly one
	// (adviseRemovedPreconditionBlock). It is empty for every other mismatch.
	Advice string
	// Unguarded is each block an edit under a block guard removes that the guard does not cover,
	// with the token it carries now (requirePreconditionCoverage). That token is for the caller
	// to compare with the token its own read listed, never to send: where the two match, a guard
	// naming the block at the token read takes the edit; where they differ, or the read listed no
	// token for the block, another writer changed or added it since that read, and the caller
	// reads the document again. It is empty for a stale token.
	Unguarded []model.EditBlockPrecondition
}

func (e *ErrPreconditionFailed) Error() string {
	reasons := make([]string, 0, len(e.Mismatches))
	for _, mismatch := range e.Mismatches {
		if mismatch.Scope == "document" {
			reasons = append(reasons, "document changed")
			continue
		}
		if mismatch.Current == nil {
			reasons = append(reasons, fmt.Sprintf("block %q was removed", mismatch.BlockID))
			continue
		}
		reasons = append(reasons, fmt.Sprintf("block %q changed", mismatch.BlockID))
	}
	if len(e.Unguarded) > 0 {
		reasons = append(reasons, unguardedReason(e.Unguarded))
	}
	return "document edit precondition failed: " + strings.Join(reasons, ", ") + e.Advice
}

// unguardedReason is why an edit under a block guard is refused for the blocks it removes that
// the guard does not cover (ErrPreconditionFailed.Unguarded), naming each by its id, and how the
// caller answers it: by comparing the token the refusal gives each, the one it carries now, with
// the token its own read listed, sending the edit again only where they match, and reading the
// document again where they differ or the read listed no token for a block.
func unguardedReason(blocks []model.EditBlockPrecondition) string {
	ids := make([]string, len(blocks))
	for index, block := range blocks {
		ids[index] = strconv.Quote(block.ID)
	}
	if len(ids) == 1 {
		return fmt.Sprintf("the edit removes block %s, which the block guard does not cover; "+
			"compare the token this refusal gives it with the token your own read listed for it: "+
			"if they match, name it in the guard at the token you read and send the edit again; "+
			"if they differ, or your read listed no token for it, "+
			"another writer changed or added it since your read, "+
			"so read the document again before you remove it", ids[0])
	}
	return fmt.Sprintf("the edit removes blocks %s, which the block guard does not cover; "+
		"compare the token this refusal gives each with the token your own read listed for it: "+
		"if all match, name them in the guard at the tokens you read and send the edit again; "+
		"if one differs, or your read listed no token for one, "+
		"another writer changed or added that block since your read, "+
		"so read the document again before you remove it", strings.Join(ids, ", "))
}

// adviseRemovedPreconditionBlock adds that way back to a precondition refused for a block the
// document no longer holds. A batch prepared from an earlier listing fails here on exactly the
// blocks another session changed, and the refusal alone leaves the caller with an id that names
// nothing: so it names the quote anchor form, and the id the block carries now where the batch's
// own text identifies one block (blockHoldingText, which names none where two or more hold it).
func adviseRemovedPreconditionBlock(tree *batchTree, ops []model.EditOp, err error) error {
	var failed *ErrPreconditionFailed
	if !errors.As(err, &failed) || failed.Advice != "" {
		return err
	}
	removed := false
	for _, mismatch := range failed.Mismatches {
		removed = removed || (mismatch.Scope == "block" && mismatch.Current == nil)
	}
	if !removed {
		return err
	}
	failed.Advice = " A block the document no longer holds took a fresh id when another session" +
		" changed it: read the document's blocks again, and name each block by its text."
	for _, op := range ops {
		current, named := blockHoldingText(tree, op.Find)
		if !named {
			continue
		}
		failed.Advice += fmt.Sprintf(" The block whose text is %q now has the id %q.", op.Find, current)
		break
	}
	return failed
}

// nodeToken hashes a canonical semantic Proof representation, including inline marks but excluding
// stable block identity and server-owned typed attributes: the SHA-256 of pmdoc.EncodeTokenJSON's
// encoding.
func nodeToken(node *pmdoc.Node) (string, error) {
	encoded, err := node.TokenJSON()
	if err != nil {
		return "", err
	}
	return tokenHash(encoded), nil
}

// validToken is the document token of a tree its holder has already validated, from one encoding
// of it (pmdoc.ValidDocument.TokenJSON): nodeToken for a validated tree.
func validToken(tree pmdoc.ValidDocument) (string, error) {
	encoded, err := tree.TokenJSON(nil)
	if err != nil {
		return "", err
	}
	return tokenHash(encoded), nil
}

// tokenHash is the token of an encoded token representation: "sha256:" and the lowercase hex of
// its SHA-256.
func tokenHash(encoded []byte) string {
	const prefix = "sha256:"
	sum := sha256.Sum256(encoded)
	var buf [len(prefix) + 2*sha256.Size]byte
	copy(buf[:], prefix)
	hex.Encode(buf[len(prefix):], sum[:])
	return string(buf[:])
}

// encodedTokens is tree's document token and the token of each block in it, from one encoding of
// the tree into one buffer this call allocates and drops (spanTokens). It validates tree first
// (pmdoc.EncodeTokenJSON), because its caller, checkEditPrecondition, holds a plain *pmdoc.Node
// rather than a pmdoc.ValidDocument. An error is the tree's, as nodeToken(tree) returns it.
func encodedTokens(tree *pmdoc.Node) (document string, blocks map[string]string, err error) {
	var spans []pmdoc.TokenSpan
	encoded, err := pmdoc.EncodeTokenJSON(tree, &spans)
	if err != nil {
		return "", nil, err
	}
	document, blocks = spanTokens(encoded, spans)
	return document, blocks, nil
}

// spanTokens is the document token and the block tokens of encoded, one encoding of a tree, with
// its block spans (pmdoc.EncodeTokenJSON): the document's is the hash of the whole buffer and each
// block's the hash of its own span. Where several nodes carry one block id, the last of them in
// pre-order holds it, as an edit addressing the id reads.
func spanTokens(encoded []byte, spans []pmdoc.TokenSpan) (document string, blocks map[string]string) {
	blocks = make(map[string]string, len(spans))
	for _, span := range spans {
		blocks[span.ID] = tokenHash(encoded[span.Start:span.End])
	}
	return tokenHash(encoded), blocks
}

// treeTokens is a tree's document token and its block tokens, each beside its own error: a block's
// token refuses only a value inside that block, while the document's refuses one anywhere in the
// tree, so a tree can have every block token and no document token.
type treeTokens struct {
	document    string
	documentErr error
	blocks      map[string]string
	blocksErr   error
}

// documentTokens is tree's tokens from one encoding of the tree, which it does not validate again
// (pmdoc.ValidDocument.TokenJSON, spanTokens); when the whole tree cannot be encoded, the blocks
// are encoded each on its own (nodeToken), in pre-order, and the first that cannot be is blocksErr.
// Only a tree holding an unencodable value pays for that second pass.
func documentTokens(tree pmdoc.ValidDocument) treeTokens {
	var spans []pmdoc.TokenSpan
	encoded, err := tree.TokenJSON(&spans)
	if err == nil {
		document, blocks := spanTokens(encoded, spans)
		return treeTokens{document: document, blocks: blocks}
	}
	tokens := treeTokens{documentErr: err, blocks: make(map[string]string)}
	pmdoc.Walk(tree.Node(), func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		token, err := nodeToken(node)
		if err != nil {
			tokens.blocks, tokens.blocksErr = nil, err
			return false
		}
		tokens.blocks[id] = token
		return true
	})
	return tokens
}

func validateEditPrecondition(precondition model.EditPrecondition) error {
	hasDocument := precondition.Document != ""
	hasBlocks := len(precondition.Blocks) > 0
	if hasDocument == hasBlocks {
		return &ErrInvalidPrecondition{Reason: "provide exactly one of document or blocks"}
	}
	if hasDocument {
		return nil
	}
	seen := make(map[string]struct{}, len(precondition.Blocks))
	for _, block := range precondition.Blocks {
		if block.ID == "" || block.Token == "" {
			return &ErrInvalidPrecondition{Reason: "each block needs id and token"}
		}
		if _, duplicate := seen[block.ID]; duplicate {
			return &ErrInvalidPrecondition{Reason: fmt.Sprintf("block %q appears more than once", block.ID)}
		}
		seen[block.ID] = struct{}{}
	}
	return nil
}

// ValidateUploadPrecondition holds a whole-document upload's precondition to what an upload can
// mean. The upload replaces the whole document, so only the document token says which text it
// believes it is replacing: a blocks precondition guards part of a tree the upload rewrites
// whole, and a precondition naming nothing guards nothing at all. Each is refused in words
// true of an upload rather than in the doc-edit rule about selecting document or blocks, which
// offers a choice an upload does not have.
func ValidateUploadPrecondition(precondition model.EditPrecondition) error {
	if len(precondition.Blocks) > 0 {
		return &ErrInvalidPrecondition{Reason: "an upload's precondition names the document token and nothing else"}
	}
	if precondition.Document == "" {
		return &ErrInvalidPrecondition{Reason: "an upload's precondition names the token of the version it replaces as document"}
	}
	return nil
}

// checkEditPrecondition refuses an edit whose precondition the document no longer meets, and
// answers the document's own token, which it encodes to check either kind of precondition: the
// caller's batch takes it as the token of the tree it starts from (applyBatch), so a conditional
// edit encodes its tree once rather than once a run.
func checkEditPrecondition(tree *pmdoc.Node, precondition model.EditPrecondition) (string, error) {
	if err := validateEditPrecondition(precondition); err != nil {
		return "", err
	}
	if precondition.Document != "" {
		currentDocument, err := nodeToken(tree)
		if err != nil {
			return "", err
		}
		if precondition.Document == currentDocument {
			return currentDocument, nil
		}
		return "", &ErrPreconditionFailed{
			CurrentDocument: currentDocument,
			Mismatches: []model.EditPreconditionMismatch{{
				Scope: "document", Expected: precondition.Document, Current: &currentDocument,
			}},
		}
	}

	// A tree with a block that cannot be encoded cannot be encoded whole either, so the document's
	// error covers every refusal of a block's token.
	currentDocument, current, err := encodedTokens(tree)
	if err != nil {
		return "", err
	}
	mismatches := make([]model.EditPreconditionMismatch, 0)
	for _, expected := range precondition.Blocks {
		token, found := current[expected.ID]
		if !found {
			mismatches = append(mismatches, model.EditPreconditionMismatch{
				Scope: "block", BlockID: expected.ID, Expected: expected.Token,
			})
			continue
		}
		if expected.Token != token {
			currentToken := token
			mismatches = append(mismatches, model.EditPreconditionMismatch{
				Scope: "block", BlockID: expected.ID, Expected: expected.Token, Current: &currentToken,
			})
		}
	}
	if len(mismatches) == 0 {
		return currentDocument, nil
	}
	return "", &ErrPreconditionFailed{CurrentDocument: currentDocument, Mismatches: mismatches}
}

// requirePreconditionCoverage refuses a block guard that leaves out a block the batch requires
// (operationPreconditionBlocks): the block an operation names or whose text it matched, and a
// move's block and its anchor's - what a block guard has always required. A guarded removal is
// refused when it takes a block that neither the guard names nor a named block contains
// (pmdoc.ValidDocument.UncoveredBlocks): it is refused as a stale token is
// (ErrPreconditionFailed.Unguarded), with nothing written, and the refusal names each such block
// with the token it carries now. The caller compares that token with the token its own read
// listed: where they match, it adds the block to its guard at the token it read and sends the edit
// again; where they differ, or its read listed no token for the block, another writer changed or
// added the block since that read, and the caller reads the document again before it removes it. A
// list, a list item or a quote the removal empties of blocks the guard covers holds nothing of its
// own but its attributes, which no listed token covers, so it is taken. A guard that names a block
// is held to its token as to any other block (checkEditPrecondition). A typed block a replace's
// with rewrites in place under its own id is not refused: the edit answers each such block the
// guard does not name, with the token it carried before the edit rewrote it (unguarded), for the
// caller to compare with the token its own read listed, and naming them in the guard refuses the
// edit when one of them changed. A block the tree the batch starts from does not hold - one an
// earlier insert of the batch wrote - has no token a caller read, so the guard is not refused for
// it and none is answered for it.
func requirePreconditionCoverage(tree *batchTree, ops []model.EditOp, precondition model.EditPrecondition, before string) (unguarded []model.EditBlockPrecondition, err error) {
	if precondition.Document != "" {
		return nil, nil
	}
	covered := make(map[string]struct{}, len(precondition.Blocks))
	for _, block := range precondition.Blocks {
		covered[block.ID] = struct{}{}
	}
	required, uncovered, rewritten := make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})
	if _, err := applyBatch(tree, ops, func(current *batchTree, op model.EditOp) error {
		names, removed, rewrites, err := operationPreconditionBlocks(current, op)
		if err != nil {
			return err
		}
		for _, id := range names {
			required[id] = struct{}{}
		}
		// Only a removal is read for coverage, over the validated tree its path was read from.
		if len(removed) > 0 {
			valid, err := current.validated("UncoveredBlocks")
			if err != nil {
				return err
			}
			for _, block := range valid.UncoveredBlocks(removed, covered) {
				uncovered[block] = struct{}{}
			}
		}
		for _, id := range rewrites {
			rewritten[id] = struct{}{}
		}
		return nil
	}, before); err != nil {
		return nil, err
	}
	if missing := unnamedBlocks(required, covered); len(missing) > 0 {
		return nil, &ErrInvalidPrecondition{Reason: "blocks must include tokens for resolved blocks: " + strings.Join(missing, ", ")}
	}
	unremoved, unrewritten := unnamedBlocks(uncovered, covered), unnamedBlocks(rewritten, covered)
	if len(unremoved) == 0 && len(unrewritten) == 0 {
		return nil, nil
	}
	// Only an edit whose guard leaves such a block unnamed encodes the tree again for its tokens.
	valid, err := tree.validated("EncodeTokenJSON")
	if err != nil {
		return nil, err
	}
	tokens := documentTokens(valid)
	if tokens.blocksErr != nil {
		return nil, tokens.blocksErr
	}
	if refused := readBlockTokens(unremoved, tokens.blocks); len(refused) > 0 {
		return nil, &ErrPreconditionFailed{CurrentDocument: before, Mismatches: []model.EditPreconditionMismatch{}, Unguarded: refused}
	}
	return readBlockTokens(unrewritten, tokens.blocks), nil
}

// unnamedBlocks is the ids of blocks that a guard naming covered leaves out, sorted.
func unnamedBlocks(blocks, covered map[string]struct{}) []string {
	unnamed := make([]string, 0, len(blocks))
	for id := range blocks {
		if _, found := covered[id]; !found {
			unnamed = append(unnamed, id)
		}
	}
	sort.Strings(unnamed)
	return unnamed
}

// readBlockTokens is each of ids with its token among tokens, the tree the batch starts from,
// leaving out a block that tree does not hold: one an earlier insert of the batch wrote.
func readBlockTokens(ids []string, tokens map[string]string) []model.EditBlockPrecondition {
	var read []model.EditBlockPrecondition
	for _, id := range ids {
		if token, found := tokens[id]; found {
			read = append(read, model.EditBlockPrecondition{ID: id, Token: token})
		}
	}
	return read
}

// operationPreconditionBlocks is, for op read over the tree it applies to, the blocks a block
// guard must name (required), the path of what op removes (removed), which the guard must cover
// (pmdoc.ValidDocument.UncoveredBlocks), and the typed block op rewrites in place (rewritten),
// which the edit's answer names where the guard does not (requirePreconditionCoverage). An
// operation must name the block it names or the block holding the text it matched, and a move the
// block it moves and its anchor's block. One that removes a block holding others removes the whole
// of it (INFRA-341): a delete what its removal takes (deletePreconditionBlocks), and a move the
// list or quote its block's leaving empties (anchoredPreconditionBlocks). A replace removes no
// block: it rewrites the typed block its with names under that block's own id
// (rewrittenTypedBlock).
func operationPreconditionBlocks(tree *batchTree, op model.EditOp) (required []string, removed []int, rewritten []string, err error) {
	switch op.Op {
	case "replace":
		r, at, _, byMarker, err := findEditQuote(tree, op)
		if err != nil {
			return nil, nil, nil, err
		}
		if required, err = rangePreconditionBlock(tree, r); err != nil {
			return nil, nil, nil, err
		}
		if typed := rewrittenTypedBlock(tree, at, r, op, byMarker); typed != nil && blockID(typed) != "" {
			rewritten = []string{blockID(typed)}
		}
		return required, nil, rewritten, nil
	case "delete":
		target, err := resolveDelete(tree, op)
		if err != nil {
			return nil, nil, nil, err
		}
		if op.Block != "" {
			required = []string{op.Block}
		} else if required, err = rangePreconditionBlock(tree, target.text); err != nil {
			return nil, nil, nil, err
		}
		removed, err = deletePreconditionBlocks(tree, target)
		return required, removed, nil, err
	case "retype", "delete_row", "delete_column":
		if err := requireBlock(tree, op, "block", op.Block); err != nil {
			return nil, nil, nil, err
		}
		return []string{op.Block}, nil, nil, nil
	case "insert", "move":
		required, removed, err = anchoredPreconditionBlocks(tree, op)
		return required, removed, nil, err
	default:
		return nil, nil, nil, nil
	}
}

// removalPreconditionBlocks is what a block guard must cover for a removal that starts at the
// block carrying id and removes held (deleteTarget.held), or for a move taking that block from its
// place (held nil): the path of the outermost node the removal takes
// (pmdoc.ValidDocument.BlockRemoval), which the guard must cover as the listing names its blocks,
// whose tokens cover every block inside it (pmdoc.ValidDocument.UncoveredBlocks). That is the list
// or quote the removal empties, where it empties one, and otherwise the removed block. A list
// item, which a find's marker selects, is no block the listing names, so its delete asks for the
// item's paragraph and the blocks after it - its nested lists, its typed blocks - whose tokens
// cover every block the item holds. The known limit is the item's own attributes, which lie on the
// list_item and in no listed token: a guard on those blocks does not see a task item's checked
// mark another writer set.
func removalPreconditionBlocks(tree *batchTree, id string, held map[string]struct{}) ([]int, error) {
	valid, err := tree.validated("BlockRemoval")
	if err != nil {
		return nil, err
	}
	return valid.BlockRemoval(id, held)
}

// deletePreconditionBlocks is the path of what a block guard must cover for the delete of target
// (resolveDelete): for a block, the whole of what its removal takes (removalPreconditionBlocks),
// which for a list item a find's marker selected is the item with the items nested in it; where a
// find covers a textblock's whole text, the outermost node that removal takes
// (pmdoc.ValidDocument.TextblockRemoval) - the textblock, which is all a list item whose one
// nested list takes its place loses, the list item whose text it is, or the list or quote that
// empties. A find that covers part of a textblock's text removes no block, so its path is nil.
func deletePreconditionBlocks(tree *batchTree, target deleteTarget) ([]int, error) {
	if target.block != "" {
		return removalPreconditionBlocks(tree, target.block, target.held)
	}
	valid, err := tree.validated("TextblockRemoval")
	if err != nil {
		return nil, invalidSchemaOp("find", err)
	}
	removed, err := valid.TextblockRemoval(target.text)
	if err != nil {
		return nil, invalidSchemaOp("find", err)
	}
	return removed, nil
}

// rewrittenTypedBlock is the outermost typed block a replace's with rewrites in place under its
// own id, and nil where it rewrites none. The fit climbs out of each typed block around the
// match's paragraph that the with holds exactly one block of under that block's type and id
// (pmdoc.RewritesBlock, pmdoc's fitReplacement), up to the first that it does not, and writes the
// with where the outermost of them stood. The with is the one the write reads
// (readReplacementWith), in the line feeds applyBatch gives an operation before its guard and its
// write read it (withLineFeeds); one that reading refuses rewrites nothing here, and the write
// refuses it.
// at is the textblock holding the match, as findEditQuote found it.
func rewrittenTypedBlock(tree *batchTree, at pmdoc.TextblockAt, r pmdoc.Range, op model.EditOp, byMarker bool) *pmdoc.Node {
	if at.Node.Type != "paragraph" || len(at.Ancestors) == 0 || !pmdoc.IsTypedBlock(at.Ancestors[0].Type) {
		return nil
	}
	with, err := readReplacementWith(tree, at, r, op, byMarker)
	if err != nil || !with.blocks {
		return nil
	}
	var rewritten *pmdoc.Node
	for _, ancestor := range at.Ancestors {
		if !pmdoc.IsTypedBlock(ancestor.Type) || !pmdoc.RewritesBlock(with.parsed.Children, ancestor) {
			break
		}
		rewritten = ancestor
	}
	return rewritten
}

// anchoredPreconditionBlocks is what a block guard must name for an insert or a move (required):
// the block the anchor resolves to - the one a `block:<id>` anchor names, or the one holding the
// text a quote anchor matched - and, for a move, the block it relocates. taken is, for a move, the
// path of the outermost node that block's leaving takes (removalPreconditionBlocks): the list or
// quote it empties. The document's edges and a `heading:` anchor resolve to no block of their own,
// so an operation naming none is refused and the batch takes the document token instead.
func anchoredPreconditionBlocks(tree *batchTree, op model.EditOp) (required []string, taken []int, err error) {
	required = make([]string, 0, 2)
	if op.Op == "move" && op.Block != "" {
		if err := requireBlock(tree, op, "block", op.Block); err != nil {
			return nil, nil, err
		}
		required = append(required, op.Block)
		if taken, err = removalPreconditionBlocks(tree, op.Block, nil); err != nil {
			return nil, nil, err
		}
	}
	anchor, after, err := anchorOf(op)
	if err != nil {
		return nil, nil, err
	}
	field := anchorField(after)
	if blockID, ok := strings.CutPrefix(anchor, "block:"); ok {
		if blockID == "" {
			return nil, nil, invalidOp("block")
		}
		if err := requireBlock(tree, op, field, blockID); err != nil {
			return nil, nil, err
		}
		required = append(required, blockID)
	} else if anchor != "start" && anchor != "end" && !strings.HasPrefix(anchor, "heading:") {
		anchored, err := quotePreconditionBlock(tree, field, anchor, op.Occurrence)
		if err != nil {
			return nil, nil, err
		}
		required = append(required, anchored...)
	}
	if len(required) == 0 {
		return nil, nil, &ErrInvalidPrecondition{Reason: fmt.Sprintf(
			"%s anchored on %q names no block to guard. The document token guards that operation.", op.Op, anchor,
		)}
	}
	return required, taken, nil
}

// quotePreconditionBlock is the block holding the text quote matched, written in field of an
// insert or a move: its anchor (quoteInOneTextblock).
func quotePreconditionBlock(tree *batchTree, field, quote string, occurrence *int) ([]string, error) {
	range_, _, _, err := quoteInOneTextblock(tree, field, quote, occurrence)
	if err != nil {
		return nil, err
	}
	return rangePreconditionBlock(tree, range_)
}

// rangePreconditionBlock is the lowest block holding all of r that carries an id.
func rangePreconditionBlock(tree *batchTree, r pmdoc.Range) ([]string, error) {
	blockID, err := pmdoc.BlockIDForRange(tree.node, r)
	if err != nil {
		return nil, err
	}
	if blockID == "" {
		return nil, &ErrInvalidPrecondition{Reason: "document token is required for a quote spanning blocks"}
	}
	return []string{blockID}, nil
}

// requireBlock refuses a precondition naming a block the tree does not carry, as
// pmdoc.ValidDocument.BlockRange refuses it, over the batch's one validation of that tree. The
// refusal is the one an operation naming that block gets (blockNotFound): field is the operation
// field the id was written in.
func requireBlock(tree *batchTree, op model.EditOp, field, blockID string) error {
	valid, err := tree.validated("BlockRange")
	if err != nil {
		return err
	}
	if _, err = valid.BlockRange(blockID); err != nil {
		return blockNotFound(tree, op, field, blockID, err)
	}
	return nil
}

type batchRemoval struct {
	operation int
	parent    string
	cascaded  bool
}

type operationValidator func(*batchTree, model.EditOp) error

func removedBlockError(blockID string, removal batchRemoval) error {
	reason := fmt.Sprintf(`block %q was removed by operation %d with delete {block:%q}`, blockID, removal.operation, removal.parent)
	if removal.cascaded {
		reason = fmt.Sprintf(`block %q was removed by operation %d as a cascade of delete {block:%q}`, blockID, removal.operation, removal.parent)
	}
	return &ErrInvalidOp{Field: "block", Reason: reason + "; remove it from the atomic batch"}
}

// editBatch is what a resolved batch of operations produced: the tree to write, the token of the
// document before the batch, every operation that left the tree as it was, and what each
// operation wrote (see writes).
type editBatch struct {
	tree       *pmdoc.Node
	beforeTree *pmdoc.Node
	before     string
	unchanged  []int
	// operations is how many operations the batch resolved, which is what tells the one-operation
	// batch - every insertion is that operation's, with nothing to diff - from the rest.
	operations int
	written    []operationWrite
}

// operationWrite is one operation's W: the blocks whose own inline content it wrote. A block the
// batch created without an id of its own is not one: EnsureBlockIDs stamps the tree before the
// write, so every block the update reaches carries an id, and a block that still has none holds
// no text a caller could address (editBatch.writes attributes it to the batch).
type operationWrite struct {
	ids []string
}

// writes resolves each operation's W: which of the blocks the write actually inserted text into
// (candidates, read from the Yjs update the batch made) each operation wrote. It runs after the
// caller's EnsureBlockIDs, which is where a block an operation created without an id gets one.
//
// A one-operation batch needs no diff at all: every run that operation's own update inserted is
// that operation's, whichever block it landed in. That is the case the uncontended path pays for,
// so the batch does not walk the tree for it (applyBatch).
//
// Every candidate is attributed, including the ones no operation's own text change claims. A
// batch writes one Yjs update, and pmdoc.Update rewrites a block it passes over in place when an
// operation inserts or removes a block above it - that untouched paragraph's text is re-inserted
// under the same block id, and a concurrent deletion can take it exactly as it takes an
// operation's own. Nothing in the batch can say which operation caused a collateral rewrite, and
// the batch is atomic, so each such block belongs to every operation: dropping it instead would
// let the paragraph vanish while the edit answered success, which is the report this whole check
// exists to prevent.
func (b editBatch) writes(candidates map[string]struct{}) []map[string]struct{} {
	if b.operations == 1 {
		return []map[string]struct{}{candidates}
	}
	written := make([]map[string]struct{}, len(b.written))
	claimed := map[string]struct{}{}
	for index, write := range b.written {
		written[index] = make(map[string]struct{}, len(write.ids))
		for _, id := range write.ids {
			if _, wanted := candidates[id]; !wanted {
				continue
			}
			written[index][id] = struct{}{}
			claimed[id] = struct{}{}
		}
	}
	for id := range candidates {
		if _, named := claimed[id]; named {
			continue
		}
		for index := range written {
			written[index][id] = struct{}{}
		}
	}
	return written
}

// outcome is the batch's verdict, taken once the caller has stamped the block ids the write
// carries, beside the validated tree it took the verdict's token from, which the caller hands to
// pmdoc.UpdateDocument: nothing changes that tree between the two. It compares the document token,
// the same semantic identity the precondition machinery uses:
// canonical markdown renders no anchor mark, so a batch that orphans a human's comment anchor
// while leaving the words alone is a change here. Whether it becomes a version is the caller's:
// with a summary the edit route names one through NamedVersion, which versions unconditionally,
// so such a batch writes a version whose markdown equals the previous one's, which carries an
// approval pinned to that one to it while its blocks are as their rows have them (carryApproval);
// without a summary SnapshotVersion compares renderings and writes nothing. LEGION-260's
// follow-up makes the route version on the rendered markdown and keep this verdict as its report
// to the agent. That same post-batch token is the verdict's Token, the document precondition the
// caller's next edit passes.
func (b editBatch) outcome(applied int) (EditOutcome, pmdoc.ValidDocument, error) {
	valid, err := pmdoc.ValidateDocument(b.tree, "EncodeTokenJSON")
	if err != nil {
		return EditOutcome{}, pmdoc.ValidDocument{}, err
	}
	after, err := validToken(valid)
	if err != nil {
		return EditOutcome{}, pmdoc.ValidDocument{}, err
	}
	return EditOutcome{
		Applied:        applied,
		Changed:        after != b.before,
		Unchanged:      b.unchanged,
		Token:          after,
		AddedAskBlocks: addedAskBlocks(b.beforeTree, b.tree),
	}, valid, nil
}

// editReadBackBound is how many bytes of UTF-8 an edit's read-back may hold. The bound is the
// server's, so every caller of the edits route reads a bounded field, rather than each command
// bounding what it prints for itself.
const editReadBackBound = 2000

// readBack is what the batch wrote, for the person or agent who reads the edit's answer: the
// markdown of each of the document's own blocks that the written tree holds and the tree before
// the batch did not, or holds with other content or in another place, in document order, and one
// line for each block the batch removed or emptied. A batch that left the document as it was
// reads back as nothing.
//
// written is the validated tree the batch's verdict was taken from (editBatch.outcome), which the
// caller hands to pmdoc.UpdateDocument: it is rendered once here, for the markdown and for each
// block's byte range in that markdown together (pmdoc.ValidDocument.RenderWithBlockOffsets). The
// tree before the batch is read as a tree and never rendered, so a batch pays for one render.
//
// The blocks are the document's own, its root's children: a change inside a list or a table reads
// back as the list or the table the edit left, which is the markdown a caller would read the
// document for. Every block of both trees carries an id by then - the edit stamps the tree it
// starts from (stampAskBlockIDs) and the caller stamps the tree it wrote (pmdoc.EnsureBlockIDs) -
// so the two sides are matched by id.
func (b editBatch) readBack(written pmdoc.ValidDocument) (string, error) {
	// The render reads the document the batch wrote, so a tree only the renderer refuses is the
	// document's own refusal (ErrDocOutsideSchema), which the route answers 409 DOC_SCHEMA, as
	// every other render of a document names it (documentMarkdownValidWithOffsets).
	markdown, offsets, err := written.RenderWithBlockOffsets()
	if err != nil {
		return "", documentSchemaError(err)
	}
	spans := make(map[string]pmdoc.BlockOffset, len(offsets))
	for _, offset := range offsets {
		if _, held := spans[offset.ID]; !held {
			spans[offset.ID] = offset
		}
	}
	before, after := documentBlockNodes(b.beforeTree), documentBlockNodes(written.Node())
	held := make(map[string]*pmdoc.Node, len(before))
	order := make(map[string]int, len(before))
	for index, block := range before {
		held[block.id], order[block.id] = block.node, index
	}
	standing := make(map[string]struct{}, len(after))
	for _, block := range after {
		standing[block.id] = struct{}{}
	}
	first, removed := removedDocumentBlocks(before, standing)
	moved := movedDocumentBlocks(after, order)
	entries := make([]readBackEntry, 0, len(after)+len(first))
	for _, block := range first {
		entries = append(entries, block.named("removed"))
	}
	previous := ""
	for _, block := range after {
		prior, kept := held[block.id]
		switch {
		case !kept || !block.node.Equal(prior):
			text := strings.TrimRight(markdown[spans[block.id].From:spans[block.id].To], "\n")
			if strings.TrimSpace(text) != "" {
				entries = append(entries, readBackEntry{text: text})
				break
			}
			// A block with nothing left in it renders as nothing, so it is named rather than
			// printed: emptied where the document held it before, and empty where the batch
			// wrote it with no content of its own.
			if kept {
				entries = append(entries, block.named("emptied"))
				break
			}
			entries = append(entries, block.named("empty"))
		case moved[block.id]:
			if previous == "" {
				entries = append(entries, block.named("moved to the start of the document"))
				break
			}
			entries = append(entries, block.named("moved after "+previous))
		}
		for _, gone := range removed[block.id] {
			entries = append(entries, gone.named("removed"))
		}
		previous = block.id
	}
	return boundedReadBack(entries), nil
}

// documentBlock is one of a document's own blocks: the node and the id it carries.
type documentBlock struct {
	id   string
	node *pmdoc.Node
}

// named is the one line a read-back gives a block it prints no markdown for: the block, its type
// and what the batch did to it.
func (b documentBlock) named(did string) readBackEntry {
	return readBackEntry{text: b.id + " " + b.node.Type + ": " + did, named: true}
}

// readBackEntry is one entry of a read-back: a block's markdown, which the document holds, or,
// named, the line naming a block the batch removed, left empty or moved, which is no text of it.
type readBackEntry struct {
	text  string
	named bool
}

// documentBlockNodes are the document's own blocks, its root's children, in document order. A
// child carrying no id is left out: nothing can match it against the other tree.
func documentBlockNodes(tree *pmdoc.Node) []documentBlock {
	if tree == nil {
		return nil
	}
	blocks := make([]documentBlock, 0, len(tree.Children))
	for _, child := range tree.Children {
		if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			blocks = append(blocks, documentBlock{id: id, node: child})
		}
	}
	return blocks
}

// removedDocumentBlocks are the blocks of before that standing, the written tree's block ids, no
// longer holds, placed where they stood: first is the run that stood before any block the write
// kept, and after holds, for each kept block, the run that stood behind it. A read-back names them
// there, so it reads in document order.
func removedDocumentBlocks(before []documentBlock, standing map[string]struct{}) (first []documentBlock, after map[string][]documentBlock) {
	after = map[string][]documentBlock{}
	anchor := ""
	for _, block := range before {
		if _, stands := standing[block.id]; stands {
			anchor = block.id
			continue
		}
		if anchor == "" {
			first = append(first, block)
			continue
		}
		after[anchor] = append(after[anchor], block)
	}
	return first, after
}

// movedDocumentBlocks are the blocks of after, the written tree's own, that stand in another
// place than the tree before the batch had them: the ones left over once the longest run of them
// that is still in the document's old order is taken out (longestRunInOrder). Taking that run as
// the blocks that stayed is what makes one block moved past two others one move and not two.
func movedDocumentBlocks(after []documentBlock, order map[string]int) map[string]bool {
	places := make([]int, 0, len(after))
	ids := make([]string, 0, len(after))
	for _, block := range after {
		place, kept := order[block.id]
		if !kept {
			continue
		}
		places = append(places, place)
		ids = append(ids, block.id)
	}
	stayed := longestRunInOrder(places)
	moved := map[string]bool{}
	for position, id := range ids {
		if !stayed[position] {
			moved[id] = true
		}
	}
	return moved
}

// longestRunInOrder is which positions of places belong to one longest strictly rising run of it,
// found by patience sorting: tails holds, for each length of run, the position of the smallest
// value ending such a run, and back the position each one follows.
func longestRunInOrder(places []int) map[int]bool {
	var tails, back []int
	for position, place := range places {
		back = append(back, -1)
		length := sort.Search(len(tails), func(index int) bool { return places[tails[index]] >= place })
		if length > 0 {
			back[position] = tails[length-1]
		}
		if length == len(tails) {
			tails = append(tails, position)
			continue
		}
		tails[length] = position
	}
	run := make(map[int]bool, len(tails))
	if len(tails) == 0 {
		return run
	}
	for position := tails[len(tails)-1]; position >= 0; position = back[position] {
		run[position] = true
	}
	return run
}

// boundedReadBack joins entries, one blank line between them, within editReadBackBound bytes: it
// writes them in document order until the bound, shortens the markdown the bound falls in at a
// rune boundary, and ends with the line saying what the read-back leaves unshown
// (ReadBackCutLine). A line naming a block the bound falls in is left out whole, since a part of
// it names no block. The room that closing line needs is taken off the bound first, measured at
// the largest counts it could name, so the whole field holds the bound whatever the counts turn
// out to be.
func boundedReadBack(entries []readBackEntry) string {
	bytes, characters, blocks := 0, 0, 0
	for index, entry := range entries {
		if index > 0 {
			bytes += 2
		}
		bytes += len(entry.text)
		if !entry.named {
			blocks++
			characters += utf8.RuneCountInString(entry.text)
		}
	}
	lines := len(entries) - blocks
	if bytes <= editReadBackBound {
		var out strings.Builder
		out.Grow(bytes)
		for index, entry := range entries {
			if index > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(entry.text)
		}
		return out.String()
	}
	budget := editReadBackBound - 2 - len(ReadBackCutLine(characters, blocks, lines))
	kept, used := make([]string, 0, len(entries)+1), 0
	for index, entry := range entries {
		separator := 0
		if index > 0 {
			separator = 2
		}
		if used+separator+len(entry.text) <= budget {
			kept = append(kept, entry.text)
			used += separator + len(entry.text)
			if entry.named {
				lines--
			} else {
				blocks--
				characters -= utf8.RuneCountInString(entry.text)
			}
			continue
		}
		if room := runeBoundaryBelow(entry.text, budget-used-separator); room > 0 && !entry.named {
			kept = append(kept, entry.text[:room])
			characters -= utf8.RuneCountInString(entry.text[:room])
		}
		break
	}
	return strings.Join(append(kept, ReadBackCutLine(characters, blocks, lines)), "\n\n")
}

// ReadBackCutLine closes a read-back that reached editReadBackBound. It names the read-back and
// never the document: the read-back is only the answer's echo of what the edit wrote, so a line
// saying the edit cut text read as text the edit lost, and an agent reverted a correct edit over
// it (dispatch://INFRA-4749). characters and blocks count the markdown the read-back leaves
// unshown - the rest of the block the bound falls in and every block after it - which the
// document holds. lines counts the lines naming a block the edit removed, left empty or moved
// that it leaves unshown, which it names apart: the document holds no removed block, and an
// agent told it holds every block left unshown would read its delete as not done. It is exported
// so a caller's test reads the line this package writes rather than a copy of its wording.
func ReadBackCutLine(characters, blocks, lines int) string {
	// A part is left out where it counts nothing, and one block or one line is named in the
	// singular, which is shorter, so boundedReadBack's room for the line, measured at the most it
	// could name, still holds.
	opening := fmt.Sprintf("the read-back shows at most %d bytes; ", editReadBackBound)
	named := fmt.Sprintf("%d %s naming blocks the edit removed, left empty or moved", lines, plural(lines, "line"))
	if blocks == 0 {
		return opening + named + " are not shown here"
	}
	markdown := fmt.Sprintf("%d characters of %d %s are not shown here, and the document holds them", characters, blocks, plural(blocks, "block"))
	if lines == 0 {
		return opening + markdown
	}
	return opening + markdown + "; " + named + " are not shown either"
}

// plural is noun for count, with an s for any count but one.
func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

// runeBoundaryBelow is the largest offset into text at or below bound that opens a rune, so a cut
// there never splits one.
func runeBoundaryBelow(text string, bound int) int {
	if bound >= len(text) {
		return len(text)
	}
	for bound > 0 && !utf8.RuneStart(text[bound]) {
		bound--
	}
	if bound < 0 {
		return 0
	}
	return bound
}

// batchTree is a tree an edit batch works on, beside the one validation it pays for: the first
// step that needs a validated tree validates it, and every later step over that same tree takes
// the held result, its refusal included, so an edit validates each of its trees once rather than
// once a call (pmdoc.ValidDocument). Nothing changes a tree while a batchTree holds it: the
// operations copy, so each operation's output is a new tree and a new batchTree, and the stamp an
// edit makes (stampAskBlockIDs) runs before the batch starts.
//
// It also holds the quote finder over that tree, which flattens the tree's text once for every
// quote the batch resolves against it (pmdoc.QuoteFinder).
type batchTree struct {
	node   *pmdoc.Node
	valid  pmdoc.ValidDocument
	err    error
	done   bool
	quotes *pmdoc.QuoteFinder
	// wrote is the ids of the blocks the batch's inserts and moves wrote before this tree, nil for
	// none: a delete of a block keeps those inside it where it stood (resolveDelete).
	wrote map[string]struct{}
}

func newBatchTree(node *pmdoc.Node) *batchTree {
	return &batchTree{node: node}
}

// next is the batch tree of node, an operation's output over t, in t's batch. ids is the blocks
// that operation wrote, where it is an insert or a move (applyBatch). t's own set is never
// changed, since every tree of a batch keeps what was written before it.
func (t *batchTree) next(node *pmdoc.Node, ids []string) *batchTree {
	wrote := t.wrote
	if len(ids) > 0 {
		wrote = make(map[string]struct{}, len(t.wrote)+len(ids))
		for id := range t.wrote {
			wrote[id] = struct{}{}
		}
		for _, id := range ids {
			wrote[id] = struct{}{}
		}
	}
	return &batchTree{node: node, wrote: wrote}
}

// validatedBatchTree is a batch tree whose validation its caller already paid for: the fork read
// applyLive hands its mutate (readForkTree, editTree).
func validatedBatchTree(valid pmdoc.ValidDocument) *batchTree {
	return &batchTree{node: valid.Node(), valid: valid, done: true}
}

// validated is the tree as a validated document. operation names the step that first needed it,
// which pmdoc.ValidateDocument's refusal of a tree that is no document names; every edit path
// hands this a document read off the live fork, so only a caller that does not refuses.
func (t *batchTree) validated(operation string) (pmdoc.ValidDocument, error) {
	if !t.done {
		t.done = true
		t.valid, t.err = pmdoc.ValidateDocument(t.node, operation)
	}
	return t.valid, t.err
}

// finder is the quote finder over the tree, made on first use: a batch resolving several quotes
// against one tree flattens that tree's text once. It is an edit's finder
// (pmdoc.ValidDocument.EditQuoteFinder): a match that spans two or more textblocks is none an
// edit can take, so it counts toward neither a quote's matches nor its ambiguity, and a quote with
// one match inside one textblock resolves to it whatever spans it also has. A quote matched once
// inside a paragraph and once across the join of two list items was refused TARGET_AMBIGUOUS,
// "matches 2 places" (dispatch://INFRA-4345, R1).
func (t *batchTree) finder() (*pmdoc.QuoteFinder, error) {
	if t.quotes == nil {
		valid, err := t.validated("FindQuote")
		if err != nil {
			return nil, err
		}
		t.quotes = valid.EditQuoteFinder()
	}
	return t.quotes, nil
}

// applyBatch applies ops to start's tree, each operation to its predecessor's output, running
// validate before each. before is the token of start's tree when the caller has already encoded
// that tree (checkEditPrecondition), and "" when it has not, where the batch encodes it itself:
// the token leaves out block identity, so the stamp between the two (stampAskBlockIDs) cannot
// change it, and a conditional edit encodes its tree once for the check and both runs of its
// batch.
//
// The batch is one caller write, so its operations' markdown and table rows spend one table-padding
// budget; each run of a batch - a conditional one runs to check its anchors and preconditions as
// well as to apply - takes its own, so no run charges the batch's padding twice. What the batch
// leaves is weighed against what one upload may hold where it is written (refuseGrowth).
//
// Each operation reads the document as the operations before it left it, and none removes or
// changes a block it does not name: a delete of a block removes all it holds but the blocks an
// earlier insert of the batch wrote into it, which stay where it stood (resolveDelete). An insert
// of a list after a list's own block joined that list, and a delete of the list after it took the
// inserted items with it (dispatch://INFRA-4517). An operation whose block an earlier `delete
// {block:…}` removed, that block or one inside it, is refused naming that delete
// (removedBlockError); one whose block, quote or heading is gone otherwise is refused as a target
// the document does not hold.
func applyBatch(start *batchTree, ops []model.EditOp, validate operationValidator, before string) (editBatch, error) {
	tree := start
	beforeTree := start.node
	budget := pmdoc.NewWriteBudget()
	if before == "" {
		valid, err := tree.validated("EncodeTokenJSON")
		if err != nil {
			return editBatch{}, err
		}
		if before, err = validToken(valid); err != nil {
			return editBatch{}, err
		}
	}
	removed := make(map[string]batchRemoval)
	var unchanged []int
	written := make([]operationWrite, len(ops))
	// Which blocks each operation wrote, told apart only when there is more than one operation to
	// tell apart: blockText is every block's own inline content, carried forward operation by
	// operation so each is compared with the tree its predecessor left rather than with the
	// batch's input. A walk of a thousand-block tree per operation is real work on the
	// uncontended path, and a one-operation batch needs none of it (editBatch.writes).
	tellOperationsApart := len(ops) > 1
	var blockText map[string]string
	if tellOperationsApart {
		blockText = pmdoc.BlockText(tree.node)
	}
	for index, op := range ops {
		// validate and the write read one operation: the one whose text is in line feeds alone.
		op = withLineFeeds(op)
		if removal, alreadyRemoved := removed[op.Block]; op.Block != "" && alreadyRemoved {
			return editBatch{}, stampOperation(index, removedBlockError(op.Block, removal))
		}
		if validate != nil {
			if err := validate(tree, op); err != nil {
				return editBatch{}, stampOperation(index, err)
			}
		}
		// A delete is resolved once, and both the write and the blocks this batch records as
		// removed are read from that one resolution (deleteOperation).
		var deleted deleteTarget
		var next *pmdoc.Node
		var err error
		if op.Op == "delete" {
			deleted, next, err = deleteOperation(tree, op)
		} else {
			next, err = applyOperation(tree, op, budget)
		}
		if err != nil {
			return editBatch{}, stampOperation(index, err)
		}
		if next.Equal(tree.node) {
			unchanged = append(unchanged, index)
		}
		var wrote []string
		if tellOperationsApart {
			prior := blockText
			written[index].ids, blockText = pmdoc.BlocksGainingText(blockText, next)
			// The blocks an insert or a move wrote, which a later delete of a block holding them
			// keeps (resolveDelete): an insert's are the ones its output holds and its input did
			// not, read from the two block-text maps this comparison already walks for, and a
			// move's the block it moved and every block inside it. Only a later operation reads
			// them.
			switch {
			case index+1 == len(ops):
			case op.Op == "insert":
				for id := range blockText {
					if _, held := prior[id]; !held {
						wrote = append(wrote, id)
					}
				}
			case op.Op == "move":
				valid, err := tree.validated("BlockDescendantIDs")
				if err == nil {
					wrote, err = valid.BlockDescendantIDs(op.Block)
				}
				if err != nil {
					return editBatch{}, stampOperation(index, err)
				}
			}
		}
		// A delete by block records every block it removed, so a later operation naming one is
		// refused naming that delete; the blocks it kept are none of them.
		if op.Op == "delete" && op.Block != "" {
			for blockID := range deleted.held {
				removed[blockID] = batchRemoval{
					operation: index,
					parent:    op.Block,
					cascaded:  blockID != op.Block,
				}
			}
		}
		tree = tree.next(next, wrote)
	}
	return editBatch{
		tree:       tree.node,
		beforeTree: beforeTree,
		before:     before,
		unchanged:  unchanged,
		operations: len(ops),
		written:    written,
	}, nil
}

func hasTableAnchorMutation(ops []model.EditOp) bool {
	for _, op := range ops {
		if op.Op == "delete_row" || op.Op == "delete_column" {
			return true
		}
	}
	return false
}

type tableAnchorSnapshot struct {
	signature string
	axis      string
	index     int
	markIDs   []string
}

// tableRowMarks and tableColumnMarks are the marks a row or column deletion would remove, read
// over the batch's one validation of its tree (batchTree.validated): the pmdoc functions they
// replaced validated that tree for themselves, and refuse a tree that is no document with the
// same words this validation gives it.
func tableRowMarks(tree *batchTree, blockID string, index int) ([]pmdoc.MarkRef, error) {
	valid, err := tree.validated("TableRowMarks")
	if err != nil {
		return nil, err
	}
	return valid.TableRowMarks(blockID, index)
}

func tableColumnMarks(tree *batchTree, blockID string, index int) ([]pmdoc.MarkRef, error) {
	valid, err := tree.validated("TableColumnMarks")
	if err != nil {
		return nil, err
	}
	return valid.TableColumnMarks(blockID, index)
}

func tableAnchorCheck(tree *batchTree, op model.EditOp) (tableAnchorSnapshot, []pmdoc.MarkRef, bool, error) {
	var (
		axis  string
		index int
		marks []pmdoc.MarkRef
		err   error
	)
	switch op.Op {
	case "delete_row":
		if op.Block == "" {
			return tableAnchorSnapshot{}, nil, false, nil
		}
		index, err = tableIndex(tree.node, op.Block, "row", op.Index)
		if err == nil {
			marks, err = tableRowMarks(tree, op.Block, index)
		}
		axis = "row"
	case "delete_column":
		if op.Block == "" {
			return tableAnchorSnapshot{}, nil, false, nil
		}
		index, err = tableIndex(tree.node, op.Block, "column", op.Index)
		if err == nil {
			marks, err = tableColumnMarks(tree, op.Block, index)
		}
		axis = "column"
	default:
		return tableAnchorSnapshot{}, nil, false, nil
	}
	if err != nil {
		return tableAnchorSnapshot{}, nil, true, invalidTableIndexOp(err)
	}
	refs := make([]string, len(marks))
	markIDs := make([]string, 0, len(marks))
	for index, mark := range marks {
		refs[index] = mark.Type + ":" + mark.ID
		switch mark.Type {
		case string(MarkAsk), string(MarkComment), string(MarkSuggestion):
			markIDs = append(markIDs, mark.ID)
		}
	}
	sort.Strings(refs)
	sort.Strings(markIDs)
	return tableAnchorSnapshot{
		signature: axis + ":" + op.Block + ":" + strconv.Itoa(index) + ":" + strings.Join(refs, ", "),
		axis:      axis,
		index:     index,
		markIDs:   markIDs,
	}, marks, true, nil
}

func (s *Service) prevalidateOperations(ctx context.Context, artifactID string, tree *pmdoc.Node, ops []model.EditOp) ([]tableAnchorSnapshot, error) {
	snapshots := make([]tableAnchorSnapshot, 0)
	_, err := applyBatch(newBatchTree(tree), ops, func(current *batchTree, op model.EditOp) error {
		snapshot, marks, table, err := tableAnchorCheck(current, op)
		if err != nil || !table {
			return err
		}
		if err := s.rejectLiveTableAnchors(ctx, artifactID, snapshot.axis, snapshot.index, marks); err != nil {
			return err
		}
		if err := s.rejectUnindexedTableMarks(ctx, artifactID, snapshot.axis, snapshot.index, marks); err != nil {
			return err
		}
		snapshots = append(snapshots, snapshot)
		return nil
	}, "")
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

// tableAnchorRecordsQuery lists the mark ids that the asks and comments anchored on the document
// bound to $1 record, among the marks bound to $2. Each arm finds its table's rows through that
// table's anchor index (anchorTarget.anchoredOn), so neither arm scans a table that holds every
// ask or comment of the instance. The text is built once, at start: it carries no value of its
// own, and pgx caches a prepared plan per statement text.
var tableAnchorRecordsQuery = `
		select anchor->>'mark_id' from asks
		where ` + asksAnchorTarget.anchoredOn() + ` and anchor->>'mark_id' = any($2::text[])
		union
		select anchor->>'mark_id' from comments
		where ` + commentsAnchorTarget.anchoredOn() + ` and anchor->>'mark_id' = any($2::text[])
	`

// tableAnchorMarkIDs are the ids of the marks a table anchor can record: an ask's, a comment's
// and a suggestion's. A mark of any other type is recorded on no row, so it is left out and the
// two readers below answer at once when nothing is left.
func tableAnchorMarkIDs(marks []pmdoc.MarkRef) []string {
	markIDs := make([]string, 0, len(marks))
	for _, mark := range marks {
		switch mark.Type {
		case string(MarkAsk), string(MarkComment), string(MarkSuggestion):
			markIDs = append(markIDs, mark.ID)
		}
	}
	return markIDs
}

func (s *Service) rejectUnindexedTableMarks(ctx context.Context, artifactID, axis string, index int, marks []pmdoc.MarkRef) error {
	markIDs := tableAnchorMarkIDs(marks)
	if len(markIDs) == 0 {
		return nil
	}
	rows, err := s.queryFrom(ctx).Query(ctx, tableAnchorRecordsQuery, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("list table anchor records: %w", err)
	}
	defer rows.Close()
	indexed := make(map[string]struct{}, len(markIDs))
	for rows.Next() {
		var markID string
		if err := rows.Scan(&markID); err != nil {
			return fmt.Errorf("scan table anchor record: %w", err)
		}
		indexed[markID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table anchor records: %w", err)
	}
	var missing []string
	for _, markID := range markIDs {
		if _, found := indexed[markID]; !found {
			missing = append(missing, markID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d has unindexed anchor marks: %s", axis, index, strings.Join(missing, ", "))}
}

func verifyTableAnchorSnapshots(tree *pmdoc.Node, ops []model.EditOp, snapshots []tableAnchorSnapshot, before string) error {
	index := 0
	_, err := applyBatch(newBatchTree(tree), ops, func(current *batchTree, op model.EditOp) error {
		snapshot, _, table, err := tableAnchorCheck(current, op)
		if err != nil || !table {
			return err
		}
		if index >= len(snapshots) || snapshots[index].signature != snapshot.signature {
			return &ErrInvalidOp{Field: "index", Reason: "table anchors changed during validation; retry"}
		}
		index++
		return nil
	}, before)
	if err != nil {
		return err
	}
	if index != len(snapshots) {
		return &ErrInvalidOp{Field: "index", Reason: "table anchors changed during validation; retry"}
	}
	return nil
}

func (s *Service) applyOperations(ctx context.Context, artifactID string, tree *batchTree, ops []model.EditOp) (editBatch, error) {
	return applyBatch(tree, ops, func(tree *batchTree, op model.EditOp) error {
		return s.validateTableEditAnchors(ctx, artifactID, tree, op)
	}, "")
}

func (s *Service) validateTableEditAnchors(ctx context.Context, artifactID string, tree *batchTree, op model.EditOp) error {
	var (
		axis  string
		index int
		marks []pmdoc.MarkRef
		err   error
	)
	switch op.Op {
	case "delete_row":
		if op.Block == "" {
			return nil
		}
		index, err = tableIndex(tree.node, op.Block, "row", op.Index)
		if err == nil {
			marks, err = tableRowMarks(tree, op.Block, index)
		}
		axis = "row"
	case "delete_column":
		if op.Block == "" {
			return nil
		}
		index, err = tableIndex(tree.node, op.Block, "column", op.Index)
		if err == nil {
			marks, err = tableColumnMarks(tree, op.Block, index)
		}
		axis = "column"
	default:
		return nil
	}
	if err != nil {
		return invalidTableIndexOp(err)
	}
	return s.rejectLiveTableAnchors(ctx, artifactID, axis, index, marks)
}

// activeTableAnchorsQuery names every open ask and unresolved comment that is anchored on the
// document bound to $1 and records one of the marks bound to $2 - the anchors an edit would
// take with the table row it removes. Each arm finds its table's rows through that table's
// anchor index (anchorTarget.anchoredOn) and holds the row open by that table's own predicate
// (anchorTarget.open), and the text is built once, as tableAnchorRecordsQuery's is.
var activeTableAnchorsQuery = `
		select 'ask', id::text, anchor->>'mark_id'
		from asks
		where ` + asksAnchorTarget.anchoredOn() + ` and ` + asksAnchorTarget.open + ` and anchor->>'mark_id' = any($2::text[])
		union all
		select 'comment', id::text, anchor->>'mark_id'
		from comments
		where ` + commentsAnchorTarget.anchoredOn() + ` and ` + commentsAnchorTarget.open + ` and anchor->>'mark_id' = any($2::text[])
		order by 1, 2
	`

func (s *Service) rejectLiveTableAnchors(ctx context.Context, artifactID, axis string, index int, marks []pmdoc.MarkRef) error {
	markIDs := tableAnchorMarkIDs(marks)
	if len(markIDs) == 0 {
		return nil
	}
	rows, err := s.queryFrom(ctx).Query(ctx, activeTableAnchorsQuery, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("list active table anchors: %w", err)
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var kind, id, markID string
		if err := rows.Scan(&kind, &id, &markID); err != nil {
			return fmt.Errorf("scan active table anchor: %w", err)
		}
		anchors = append(anchors, fmt.Sprintf("%s %s (anchor %s)", kind, id, markID))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active table anchors: %w", err)
	}
	if len(anchors) == 0 {
		return nil
	}
	return &ErrInvalidOp{
		Field:  "index",
		Reason: fmt.Sprintf("%s index %d would remove active anchors: %s", axis, index, strings.Join(anchors, ", ")),
	}
}

// withLineFeeds is op with every text it writes - a replace's with, an insert's markdown, whether
// it becomes blocks or table rows, and a retype's attributes - in line feeds alone
// (pmdoc.LineFeeds), as it reaches the document. applyBatch gives each operation them before
// anything reads it, so an operation's guard and its write read the same text.
func withLineFeeds(op model.EditOp) model.EditOp {
	op.With = pmdoc.LineFeeds(op.With)
	op.Markdown = pmdoc.LineFeeds(op.Markdown)
	op.Attributes = pmdoc.LineFeedAttrs(op.Attributes)
	return op
}

// applyOperation applies op, its text in line feeds alone (withLineFeeds), to tree, padding the
// tables and table rows it writes on budget, its batch's. A delete is deleteOperation's. Every
// pmdoc call of it that would validate the tree takes the batch's one validation of it instead
// (batchTree).
func applyOperation(tree *batchTree, op model.EditOp, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	switch op.Op {
	case "replace":
		return applyReplace(tree, op, budget)
	case "insert":
		if op.Markdown == "" {
			return nil, invalidOp("markdown")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		target, err := insertTarget(tree, op, anchorField(after), anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		valid, err := tree.validated("InsertTableRows")
		if err != nil {
			return nil, invalidSchemaOp("markdown", err)
		}
		// Rows first, judged against the target table's width; the block path below reads the
		// fragment as a document of its own, whose tables are its own.
		if out, inserted, err := valid.InsertTableRows(target, op.Markdown, after, budget); err != nil || inserted {
			return out, invalidSchemaOp("markdown", err)
		}
		position := target.From
		if after {
			position = target.To
		}
		// An anchor on a block lands the insert beside that block among the children of its own
		// parent - the innermost typed block, list item or blockquote holding it - or of the
		// document where none does (pmdoc.ValidDocument.BlockBoundary), so markdown anchored on a
		// requirement's paragraph is that requirement's, where it was written after the requirement
		// (dispatch://INFRA-3142), and markdown anchored on a list item's text is that item's.
		var boundary pmdoc.Boundary
		if anchor != "start" && anchor != "end" {
			if boundary, err = valid.BlockBoundary(target, after); err != nil {
				return nil, err
			}
			position = boundary.Position
		}
		// A document with nothing in it holds one empty paragraph, which the insert takes the place
		// of rather than leaving an empty line beside what it writes, so it lands at the start.
		empty := pmdoc.EmptyDocument(tree.node)
		at := pmdoc.Range{From: position, To: position}
		if empty {
			at = pmdoc.Range{From: 0, To: pmdoc.Size(tree.node)}
		}
		// Front matter opens only the document's start, so only there does the insert read it.
		with, err := parseFragmentInput(op.Markdown, opensDocument(tree.node, at.From), budget)
		if err != nil {
			return nil, invalidMarkdownOp("markdown", err)
		}
		// List content anchored inside a list joins that list instead: beside the item the anchor
		// stands in, or as the first or last item of the list whose own block it is
		// (pmdoc.ValidDocument.ListBoundary), unless a typed block or a quote between the list and
		// the anchor holds it, as it holds a paragraph. A fragment that is list content holds no
		// front matter, so the reading above is the same wherever it lands, and an empty document
		// holds no list for it to join.
		if anchor != "start" && anchor != "end" && pmdoc.ListContent(with) {
			if boundary, err = valid.ListBoundary(target, after); err != nil {
				return nil, err
			}
			if !empty {
				at = pmdoc.Range{From: boundary.Position, To: boundary.Position}
			}
		}
		if err := insertDoesNotFit(boundary, with, anchorField(after), anchor); err != nil {
			return nil, err
		}
		out, err := valid.Splice(at, with)
		if err != nil {
			return nil, invalidSchemaOp("markdown", err)
		}
		// A list item that takes a block beside its text is written with a blank line between the
		// two, which reads back as a spread item in a spread list, so the lists and items of the
		// document-level block the insert landed in take the spread their markdown reads back with
		// (pmdoc.AgreeWithReadBack), as the blocks a block replacement writes do (settleAccepted).
		if boundary.Holder != nil && boundary.Holder.Type == "list_item" {
			index, _, err := valid.BlockIndexes(at)
			if err != nil {
				return nil, err
			}
			out = pmdoc.AgreeWithReadBack(tree.node, out, index, index, false)
		}
		if err := pmdoc.RepeatedBlockID(tree.node, out, with); err != nil {
			return nil, &ErrInvalidOp{Field: "markdown", Reason: err.Error()}
		}
		// What is stored is the document's rendering, so an insert that leaves it reading back
		// otherwise than it did would store another document than the one it wrote.
		if err := pmdoc.RefuseMisreadWrite(tree.node, out); err != nil {
			return nil, invalidSchemaOp("markdown", err)
		}
		return out, nil
	case "delete_row":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		index, err := tableIndex(tree.node, op.Block, "row", op.Index)
		if err != nil {
			return nil, err
		}
		valid, err := tree.validated("DeleteTableRow")
		var out *pmdoc.Node
		if err == nil {
			out, err = valid.DeleteTableRow(op.Block, index)
		}
		return out, invalidTableIndexOp(err)
	case "delete_column":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		index, err := tableIndex(tree.node, op.Block, "column", op.Index)
		if err != nil {
			return nil, err
		}
		valid, err := tree.validated("DeleteTableColumn")
		var out *pmdoc.Node
		if err == nil {
			out, err = valid.DeleteTableColumn(op.Block, index)
		}
		return out, invalidTableIndexOp(err)
	case "move":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		field := anchorField(after)
		target, err := insertTarget(tree, op, field, anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		// The document edges are empty ranges; MoveBlock lands on the edge they name.
		switch anchor {
		case "start":
			after = false
		case "end":
			after = true
		}
		valid, err := tree.validated("MoveBlock")
		var out *pmdoc.Node
		if err == nil {
			out, err = valid.MoveBlock(op.Block, target, after)
		}
		err = blockNotFound(tree, op, "block", op.Block, err)
		if errors.Is(err, pmdoc.ErrMoveInsideItself) {
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "block", Reason: err.Error()}
		}
		return out, err
	case "retype":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		if op.Type == "" {
			return nil, invalidOp("type")
		}
		out, err := pmdoc.RetypeBlock(tree.node, op.Block, op.Type, pmdoc.Attrs(op.Attributes))
		err = blockNotFound(tree, op, "block", op.Block, err)
		if errors.Is(err, pmdoc.ErrSchema) {
			field := "attributes"
			switch {
			case errors.Is(err, pmdoc.ErrUnknownBlockType):
				field = "type"
			case errors.Is(err, pmdoc.ErrBlockNotRetypable):
				field = "block"
			}
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		return out, err
	default:
		return nil, invalidOp("op")
	}
}

// listItemSelected is the id of the list item a find's list marker selected (resolveEditQuote's
// byMarker), whose text r lies in at, the textblock findEditQuote found it in: the marker selects
// an item by its first paragraph (pmdoc.QuoteFinder.FindListItem), so the item is that
// paragraph's parent.
func listItemSelected(at pmdoc.TextblockAt, r pmdoc.Range) (string, error) {
	if len(at.Ancestors) == 0 || at.Ancestors[0].Type != "list_item" || blockID(at.Ancestors[0]) == "" {
		return "", fmt.Errorf("%w: no list item carrying a block id holds the text a list marker selected at %d", pmdoc.ErrSchema, r.From)
	}
	return blockID(at.Ancestors[0]), nil
}

// deleteTarget is what a delete removes, as resolveDelete reads it over the tree the delete
// applies to: a block, with the ids of the blocks that delete takes with it, or a run of text.
type deleteTarget struct {
	// field is the operation field the target was written in, which its refusals name: "block",
	// or "find", whose text or whose list marker selected it.
	field string
	// block is the id of the block the delete removes whole - the block it names, or the list item
	// its find's marker selected - and "" for a delete of text.
	block string
	// held is the ids the delete of block removes: that block's and those of every block inside
	// it but the blocks an earlier insert or move of the batch wrote there, which stay where it
	// stood (pmdoc.ValidDocument.DeleteBlockKeeping).
	held map[string]struct{}
	// text is the range of the text a find matched: the text a delete of text removes, or the
	// text of the item a find's marker selected.
	text pmdoc.Range
}

// deleteOperation writes the delete op over tree, and answers what it removed, the one reading of
// it (resolveDelete) that the batch records (applyBatch). A delete naming both a block and a find
// is refused before either is read, as the write always has refused it. A block guard reads such
// a delete by its block (operationPreconditionBlocks), so a guarded one naming a block the
// document does not carry is refused naming that block.
func deleteOperation(tree *batchTree, op model.EditOp) (deleteTarget, *pmdoc.Node, error) {
	if op.Block != "" && op.Find != "" {
		return deleteTarget{}, nil, &ErrInvalidOp{Field: "find", Reason: "delete takes find or block, not both"}
	}
	target, err := resolveDelete(tree, op)
	if err != nil {
		return deleteTarget{}, nil, err
	}
	next, err := applyDelete(tree, op, target)
	return target, next, err
}

// resolveDelete is what the delete op removes from tree, the one reading of it that the write
// (deleteOperation), the block guard (deletePreconditionBlocks) and the batch's record of removed
// blocks (applyBatch) all take, so none of them can disagree with another on what goes. A find
// whose list marker selected an item removes that item, as the marker selects it: all its text
// and the items nested in it, where it deleted the words the find matched and left the rest of
// the item (dispatch://INFRA-4517); so a link the find spells is weighed over all of the item's
// text (findEditQuote, removedText). A block removes what it holds but the blocks an earlier insert
// or move of the batch wrote inside it: list content an insert joined to the list it was anchored
// on, a paragraph an insert wrote into a typed block, a block a move put there. A block a replace
// wrote in place of text, or under the id of a typed block it rewrote, is no insert's, and goes
// with the block holding it; so does everything inside a block an insert or a move wrote, which
// is that block's own. A delete naming both a block and a find is read by its block; the write
// refuses it first (deleteOperation).
func resolveDelete(tree *batchTree, op model.EditOp) (deleteTarget, error) {
	if op.Block != "" {
		return heldDelete(tree, op, "block", op.Block)
	}
	if op.Find == "" {
		return deleteTarget{}, invalidOp("find or block")
	}
	r, at, _, byMarker, err := findEditQuote(tree, op)
	if err != nil {
		return deleteTarget{}, err
	}
	if byMarker {
		item, err := listItemSelected(at, r)
		if err != nil {
			return deleteTarget{}, err
		}
		target, err := heldDelete(tree, op, "find", item)
		target.text = r
		return target, err
	}
	return deleteTarget{field: "find", text: r}, nil
}

// heldDelete is the delete of the block blockID names, written in field of op, with the ids it
// removes (deleteTarget.held).
func heldDelete(tree *batchTree, op model.EditOp, field, blockID string) (deleteTarget, error) {
	valid, err := tree.validated("BlockDescendantIDs")
	if err != nil {
		return deleteTarget{}, invalidSchemaOp(field, err)
	}
	ids, err := valid.BlockDescendantIDs(blockID)
	if err != nil {
		return deleteTarget{}, invalidSchemaOp(field, blockNotFound(tree, op, field, blockID, err))
	}
	_, written := tree.wrote[blockID]
	held := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, kept := tree.wrote[id]; kept && !written {
			continue
		}
		held[id] = struct{}{}
	}
	return deleteTarget{field: field, block: blockID, held: held}, nil
}

// applyDelete writes the delete op of target (resolveDelete) over tree.
func applyDelete(tree *batchTree, op model.EditOp, target deleteTarget) (*pmdoc.Node, error) {
	if target.block != "" {
		return deleteHeldBlock(tree, op, target)
	}
	valid, err := tree.validated("DeleteTextblock")
	if err != nil {
		return nil, invalidSchemaOp("find", err)
	}
	if out, removed, err := valid.DeleteTextblock(target.text); err != nil || removed {
		return out, invalidSchemaOp("find", err)
	}
	empty, err := parseInput("")
	if err != nil {
		return nil, err
	}
	out, err := valid.Splice(target.text, empty)
	if err != nil {
		return nil, invalidSchemaOp("find", err)
	}
	found, err := matchedBlocks(valid, target.text)
	if err != nil {
		return nil, err
	}
	return out, refuseCodePipeInTableCell(valid, out, found, "find", "deleting find", op.Find, "")
}

// deleteHeldBlock deletes target's block, and keeps where it stood the blocks inside it the
// delete does not remove (deleteTarget.held).
func deleteHeldBlock(tree *batchTree, op model.EditOp, target deleteTarget) (*pmdoc.Node, error) {
	valid, err := tree.validated("DeleteBlock")
	if err != nil {
		return nil, invalidSchemaOp(target.field, err)
	}
	out, err := valid.DeleteBlockKeeping(target.block, target.held)
	if errors.Is(err, pmdoc.ErrKeptBlocksDoNotFit) {
		return nil, &ErrInvalidOp{Field: target.field, Reason: fmt.Sprintf(
			"block %q holds blocks an earlier insert or move of this edit wrote inside it, which its delete keeps where it stood, and %v; delete those blocks by an operation of their own, or write them outside that block",
			target.block, err,
		)}
	}
	return out, invalidSchemaOp(target.field, blockNotFound(tree, op, target.field, target.block, err))
}

// anchorOf returns the one insert or move anchor an operation names.
func anchorOf(op model.EditOp) (anchor string, after bool, err error) {
	if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
		return "", false, invalidOp("after or before")
	}
	if op.After != "" {
		return op.After, true, nil
	}
	return op.Before, false, nil
}

// anchorField names the operation field an anchor came from, for the errors it produces.
func anchorField(after bool) string {
	if after {
		return "after"
	}
	return "before"
}

// replacementMarkdown resolves `with` against the marker the matched block already renders.
// The marked line is the matched block's own first line, so a `with` that opens with the block's
// own marker would write that marker twice (`## ##`, `7. 7\.`, `4. 4\.`, `-    - `). A heading
// rename is one shape that keeps working: `find` carried the marker through the match, so an
// identical one in `with` is the block's, and it is dropped. A list item a `find`'s marker
// selected (byMarker, resolveEditQuote) is the other. Every other repetition is refused, and a
// marker of a different kind stays the literal text it has always been.
func replacementMarkdown(tree *pmdoc.Node, r pmdoc.Range, find, with string, byMarker bool) (text string, level int, err error) {
	own, ok := pmdoc.MarkerAt(tree, r.From)
	if !ok || own.Kind == pmdoc.MarkerNone {
		return with, 0, nil
	}
	written, width := pmdoc.LeadingBlockMarker(with)
	if written.Kind != own.Kind {
		return with, 0, nil
	}
	// A `find` that carried the heading's own marker is renaming that heading, so the marker in
	// `with` is the block's and is dropped. A different level applies only when `find` named the
	// block's actual level: the caller has then shown they know what it is. `# ` is the documented
	// level-blind way to select a heading, so a generic `find` renames the text and keeps the
	// level it selected.
	if own.Kind == pmdoc.MarkerHeading && pmdoc.HeadingMarker(find) != "" {
		selector, _ := pmdoc.LeadingBlockMarker(find)
		if selector.Level == own.Level && written.Level != own.Level {
			return with[width:], written.Level, nil
		}
		return with[width:], 0, nil
	}
	// A list item the `find`'s own marker selected (the find matched no text as written) is
	// renamed as a heading is, so the marker in `with`, of the item's kind as the selection
	// required, is the item's and is dropped. The item keeps its number: a list numbers its items
	// by place. A `find` that matched as text carried its marker as text, and selected nothing.
	if byMarker {
		text := with[width:]
		bare := text
		// A box opening the text is the item's checkbox only on a task item, as the tick below
		// reads it; on a plain item it is text.
		if _, found, boxWidth := pmdoc.LeadingTaskCheckbox(text); found && pmdoc.TaskItemAt(tree, r.From) {
			bare = text[boxWidth:]
		}
		// A with that is the marker alone would empty the item's text with no word of it, which
		// an empty with does on purpose.
		if strings.TrimSpace(bare) == "" {
			return "", 0, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
				"with %q is the selected item's marker with no text after it, which would empty the item; give the item's new text after the marker, or pass an empty with to delete the item's text on purpose",
				with,
			)}
		}
		return text, 0, nil
	}
	return "", 0, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
		"with begins with a marker of the same kind as the matched block's own (%s, which the block renders as %q), so the result would carry it twice; omit the marker to replace the block's text, backslash-escape it (%s) to keep prose that merely looks like a marker, or write the block's new text on the first line and the new blocks on the lines under it (`p\\n- q` gives the block the text `p` and writes the list under it)",
		written.Kind, own.Markdown(), escapedMarkerExample(with, width),
	)}
}

// escapedMarkerExample is the caller's own text with a backslash before the character that makes
// its opening a marker, which is how prose that merely looks like one (`1999. was a year`) is
// written as text. Everything else, the separator the marker needs included, stays as written: an
// example the caller cannot paste back verbatim teaches the wrong escape.
func escapedMarkerExample(with string, width int) string {
	marker := with[:width]
	cut := strings.IndexAny(marker, "#-*+")
	if punctuation := strings.IndexAny(marker, ".)"); punctuation >= 0 {
		cut = punctuation
	}
	if cut < 0 {
		return "`" + with + "`"
	}
	return "`" + marker[:cut] + `\` + marker[cut:] + with[width:] + "`"
}

// invalidSchemaOp reports a tree operation that would leave a container outside
// its content rule as an invalid operation on the field that named the target.
func invalidSchemaOp(field string, err error) error {
	if errors.Is(err, pmdoc.ErrSchema) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

// invalidBlockPointerOp reports a batch introducing a block pointer to a block of a type its
// attribute does not allow (pmdoc.ErrBlockPointer) as an invalid operation on its attributes, as
// the route's other attribute refusals are.
func invalidBlockPointerOp(err error) error {
	if errors.Is(err, pmdoc.ErrBlockPointer) {
		return &ErrInvalidOp{Field: "attributes", Reason: err.Error()}
	}
	return err
}

func invalidTableIndexOp(err error) error {
	var invalidIndex *pmdoc.TableIndexError
	if errors.As(err, &invalidIndex) {
		return &ErrInvalidOp{Field: "index", Reason: err.Error()}
	}
	return invalidSchemaOp("block", err)
}

func tableIndex(tree *pmdoc.Node, blockID, axis string, raw json.RawMessage) (int, error) {
	supplied := strings.TrimSpace(string(raw))
	if supplied == "" {
		return 0, invalidTableIndex(tree, blockID, axis, "missing", "index is required")
	}
	if supplied == "0" {
		return 0, nil
	}
	if supplied[0] < '1' || supplied[0] > '9' {
		return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
	}
	for _, character := range supplied[1:] {
		if character < '0' || character > '9' {
			return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
		}
	}
	index, err := strconv.Atoi(supplied)
	if err != nil {
		return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
	}
	return index, nil
}

func invalidTableIndex(tree *pmdoc.Node, blockID, axis, supplied, problem string) error {
	return invalidTableIndexOp(pmdoc.TableIndexErrorFor(tree, blockID, axis, supplied, problem))
}

func invalidMarkdownOp(field string, err error) error {
	if errors.Is(err, ErrInvalidMarkdown) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

// quoteInOneTextblock resolves quote, written in field, through the batch's one quote finder over
// that tree (resolveEditQuote, batchTree.finder), and refuses a match that reaches past one
// textblock (quoteSpansBlocks), so a miss or a refusal names the field the caller wrote. spelled
// is the markup the quote spells over its match (pmdoc.Spelling), and byMarker reports that its
// leading list marker selected the item it matched. An insert's or a move's anchor resolves here
// alone: it names a place, not text to change, so it finds its block whatever it spells a link
// with and whatever span's edge it crosses, and the link stays as the document holds it. A find
// resolves here and is then weighed against the spans it crosses (findEditQuote).
func quoteInOneTextblock(tree *batchTree, field, quote string, occurrence *int) (r pmdoc.Range, spelled pmdoc.Spelling, byMarker bool, err error) {
	r, spelled, byMarker, err = resolveEditQuote(tree, field, quote, occurrence)
	if err != nil {
		return pmdoc.Range{}, pmdoc.Spelling{}, false, err
	}
	if pmdoc.TargetSpansBlocks(tree.node, r) {
		return pmdoc.Range{}, pmdoc.Spelling{}, false, quoteSpansBlocks(tree, field, quote, r)
	}
	return r, spelled, byMarker, nil
}

// findEditQuote resolves op's find, a replace's or a delete's (quoteInOneTextblock), and refuses
// one that crosses a marked span (pmdoc.TextblockAt.CrossedSpan) over the text op removes there
// (removedText). at is the textblock its match lies in, which a replace writes in and a list
// marker's delete reads its item from; spelled is the markup the find spells over its match, so a
// replace reads its find as markup only where the finder did; and byMarker reports that the
// find's leading list marker selected the item it matched. The write and the block guard each
// resolve a find here, so they read it the same way.
//
// A find names text to change: a link the edit removes whole must be spelled as it is wherever
// the find spells it, since the edit would remove a link another writer may have changed, and a
// replace's find, whose with is written over its match, is refused for crossing any span's edge
// besides. A replace's find is refused before its with is read. The refusal names the find that
// covers the text it named, selects as it does and is taken there (ErrFindCrossesSpan).
func findEditQuote(tree *batchTree, op model.EditOp) (r pmdoc.Range, at pmdoc.TextblockAt, spelled pmdoc.Spelling, byMarker bool, err error) {
	r, spelled, byMarker, err = quoteInOneTextblock(tree, "find", op.Find, op.Occurrence)
	if err != nil {
		return pmdoc.Range{}, pmdoc.TextblockAt{}, pmdoc.Spelling{}, false, err
	}
	at, _ = pmdoc.ContainingTextblock(tree.node, r.From)
	if err := refuseCrossing(tree, at, op, r, removedText(at, r, op.Op == "replace", byMarker), spelled, byMarker); err != nil {
		return pmdoc.Range{}, pmdoc.TextblockAt{}, pmdoc.Spelling{}, false, err
	}
	return r, at, spelled, byMarker, nil
}

// refuseCrossing is the refusal of op's find where it crosses a marked span over removed, the text
// op removes there (pmdoc.TextblockAt.CrossedSpan), text being written over its match where op is
// a replace, and nil where it crosses none. The find matched r in the textblock at, spelling
// spelled over it, and selected the item r opens where byMarker says its list marker did. The
// refusal names the find to send in its place (findKeepingRange), or else the span that stops
// every find and what of it the find leaves on a space.
func refuseCrossing(tree *batchTree, at pmdoc.TextblockAt, op model.EditOp, r, removed pmdoc.Range, spelled pmdoc.Spelling, byMarker bool) error {
	writes := op.Op == "replace"
	span, crossing := at.CrossedSpan(r, removed, spelled, writes)
	if crossing == pmdoc.CrossesNoSpan {
		return nil
	}
	refusal := &ErrFindCrossesSpan{Quote: op.Find, Span: span, Crossing: crossing, Writes: writes}
	own, err := at.SpanOver(span.Mark, r)
	if err != nil {
		return err
	}
	refusal.OwnIsText = own == span.Text
	// A find whose list marker selected its item is named with that marker, the text after it
	// being what the marker made it match (pmdoc.ParseListItemQuote), so the find named selects
	// that item too and the edit removes what the refused one would have.
	if byMarker && own != "" {
		own = strings.TrimSuffix(op.Find, pmdoc.ParseListItemQuote(op.Find).Text) + own
	}
	var blocked pmdoc.Span
	if refusal.Own, blocked = findKeepingRange(tree, at, own, op.Occurrence, r, removed, writes, byMarker); refusal.Own.Quote == "" {
		if blocked.Mark.Type == "" {
			blocked = span
		}
		refusal.Blocked = blocked
		refusal.Kept = at.KeptOnSpace(blocked.Mark, blocked.Range, r)
	}
	return refusal
}

// removedText is the text of the textblock at that op removes where its find matched r there, as
// far as the find decides it, writes saying op is a replace and byMarker that the find's list
// marker selected the item at opens. A delete so selected removes that whole item
// (resolveDelete), so all of at's text: the rest of the item's text lies in other blocks, which
// the find spells nothing of. Every other op removes r: a delete of text removes r, and where r
// is all of a textblock's text, the textblock with it, and the list item, list or quote that
// leaves empty (pmdoc.ValidDocument.DeleteTextblock), none of which holds text r does not; and a
// replace writes over r, and where its with is blocks, which it reads after its find, drops the
// whitespace beside r too (blockReplacementRemoves).
func removedText(at pmdoc.TextblockAt, r pmdoc.Range, writes, byMarker bool) pmdoc.Range {
	if byMarker && !writes {
		return at.Content
	}
	return r
}

// blockReplacementRemoves is the text of the textblock at that a replace whose with is blocks
// removes where its find matched r: r, and the whitespace the split leaves beside r, which the
// write drops in a textblock carrying an id. It is the one place that decides that whitespace: the
// find is weighed over this range (refuseCrossing), and the write cuts exactly it
// (settleBlockReplacement).
func blockReplacementRemoves(at pmdoc.TextblockAt, r pmdoc.Range) (pmdoc.Range, error) {
	if blockID(at.Node) == "" {
		return r, nil
	}
	return at.SpaceBeside(r, markdownSpace)
}

// findKeepingRange is quote as a find to send in place of one that matched r in the textblock at,
// with occurrence, where its op removes removed: quote, with the occurrence at which it matches r,
// where it matches r there, selects as the refused find did, and is taken there; and zero where
// none of its matches is r or it would be refused. It names the occurrence where the refused find
// named one or quote matches more than once: a quote that spells each span the document holds
// over its match matches only where the text holds those spans, so it may count r at another
// place than the refused find did. byMarker says the refused find's list marker selected the item
// r opens, so quote opens with that marker, and its occurrence counts the items it selects
// (pmdoc.QuoteFinder.FindListItem). blocked is the span quote crosses where it matches r and is
// refused there, and zero otherwise.
func findKeepingRange(tree *batchTree, at pmdoc.TextblockAt, quote string, occurrence *int, r, removed pmdoc.Range, writes, byMarker bool) (named NamedFind, blocked pmdoc.Span) {
	if quote == "" {
		return NamedFind{}, pmdoc.Span{}
	}
	finder, err := tree.finder()
	if err != nil {
		return NamedFind{}, pmdoc.Span{}
	}
	occurrenceOf := finder.OccurrenceOf
	if byMarker {
		occurrenceOf = finder.ListItemOccurrenceOf
	}
	index, matches, found := occurrenceOf(quote, r)
	if !found {
		return NamedFind{}, pmdoc.Span{}
	}
	named = NamedFind{Quote: quote}
	if occurrence != nil || matches > 1 {
		named.Occurrence = &index
	}
	// The find named is resolved as the op resolves its find: it must match r and select a list
	// item exactly where the refused find did, so the op removes the same text, removed, the same
	// item where the delete removes one, and cross no span there.
	resolved, spelled, selects, err := resolveEditQuote(tree, "find", named.Quote, named.Occurrence)
	if err != nil || resolved != r || selects != byMarker {
		return NamedFind{}, pmdoc.Span{}
	}
	if crossed, crossing := at.CrossedSpan(r, removed, spelled, writes); crossing != pmdoc.CrossesNoSpan {
		return NamedFind{}, crossed
	}
	return named, pmdoc.Span{}
}

// quoteSpansBlocks is the refusal of quote, written in field, whose match r reaches past one
// textblock. When it starts in one table cell and ends in another, it names both cells.
func quoteSpansBlocks(tree *batchTree, field, quote string, r pmdoc.Range) error {
	refusal := &ErrQuoteSpansBlocks{Quote: quote, Field: field}
	first, startsInCell := tableCellAt(tree.node, r.From)
	last, endsInCell := tableCellAt(tree.node, r.To)
	if startsInCell && endsInCell && first != last {
		refusal.Cells = []string{pmdoc.TextContent(first), pmdoc.TextContent(last)}
	}
	return refusal
}

// resolveEditQuote resolves quote as text the document holds, reporting the markup the quote
// spells over its match in spelled. A quote that opens with a list item's marker and matches no
// text as written (pmdoc.QuoteFinder.FindWritten) selects the items of that kind of list whose
// text starts with the rest (pmdoc.QuoteFinder.FindListItem), reporting that in byMarker, before
// it is read as markdown: that reading takes the marker for list syntax and drops it, so a marker
// before bold text matched the bold text wherever it stood, and a delete of it removed that text
// alone and left the item (dispatch://INFRA-4517). The selection is a document edit's alone: a
// comment's or an ask's quote is text the document held, so its marker is text.
func resolveEditQuote(tree *batchTree, field, quote string, occurrence *int) (r pmdoc.Range, spelled pmdoc.Spelling, byMarker bool, err error) {
	finder, err := tree.finder()
	if err != nil {
		return pmdoc.Range{}, pmdoc.Spelling{}, false, err
	}
	if pmdoc.ParseListItemQuote(quote).Text != "" {
		if r, err = finder.FindWritten(quote, occurrence); err == nil {
			return r, pmdoc.Spelling{}, false, nil
		}
		if !matchedNothing(err) {
			return pmdoc.Range{}, pmdoc.Spelling{}, false, describeQuoteError(tree, field, quote, err)
		}
		selected, selectedSpelling, selectErr := finder.FindListItem(quote, occurrence)
		if selectErr == nil {
			return selected, selectedSpelling, true, nil
		}
		// A selection the occurrence overshoots, or that several items answer, is the quote's
		// answer; a selection of nothing leaves the quote to its reading as markdown.
		if !matchedNothing(selectErr) {
			return pmdoc.Range{}, pmdoc.Spelling{}, false, describeQuoteError(tree, field, quote, selectErr)
		}
	}
	if r, spelled, err = finder.FindSpelling(quote, occurrence, nil); err == nil {
		return r, spelled, false, nil
	}
	return pmdoc.Range{}, pmdoc.Spelling{}, false, describeQuoteError(tree, field, quote, err)
}

// matchedNothing reports whether err is a quote finder's miss of a quote that matches nowhere,
// rather than an occurrence past its matches, an ambiguity, or a tree the finder cannot read.
func matchedNothing(err error) bool {
	var missing *pmdoc.ErrQuoteNotFound
	return errors.As(err, &missing) && missing.Matches == 0
}

// describeQuoteError dresses a quote finder's refusal of quote, written in field, for the caller.
func describeQuoteError(tree *batchTree, field, quote string, err error) error {
	var missing *pmdoc.ErrQuoteNotFound
	if errors.As(err, &missing) {
		if delimiter, unbalanced := pmdoc.UnbalancedInlineMark(quote); unbalanced {
			// The quote may simply be wrong about the text and merely carry an odd delimiter, so
			// the nearest blocks that would fix an ordinary miss stay in the message.
			return &ErrInvalidOp{Field: field, Reason: fmt.Sprintf(
				"inline marks in %s must be balanced: %q opens a span the quote never closes, so it cannot match the text as rendered; quote the whole marked span or none of it; nearest blocks: %s",
				field, delimiter, pmdoc.QuoteBlocks(missing.Nearest),
			)}
		}
		if missing.Matches == 0 {
			if cells := tableRowQuoted(tree.node, quote); len(cells) > 0 {
				return &ErrQuoteNotFound{Quote: quote, Field: field, Cells: cells}
			}
		}
		return &ErrQuoteNotFound{Quote: quote, Nearest: missing.Nearest, Matches: missing.Matches}
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		return &ErrAnchorAmbiguous{Kind: "quote", Target: quote, Candidates: ambiguous.Candidates}
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		return &ErrQuoteSpansBlocks{Quote: quote, Field: field}
	}
	return err
}

// tableRowQuoted reads a quote that matched nothing as a table row written in markdown
// (cellQuoteParts) and finds, in one walk of the document, the first row in document order holding
// a run of consecutive cells that each hold their part (cellHolds). It answers those cells' texts,
// one per part and in the parts' order. A quote of fewer than two parts runs across no cell
// boundary, and one whose parts no run of one row's cells holds answers none: either miss reads as
// any other.
func tableRowQuoted(doc *pmdoc.Node, quote string) []string {
	parts := cellQuoteParts(quote)
	if len(parts) < 2 {
		return nil
	}
	// Each part as the text its cell would show, its inline markdown (emphasis, links, code)
	// rendered, read once rather than once a cell.
	shown := make([]string, len(parts))
	for index, part := range parts {
		shown[index] = renderedInlineText(part)
	}
	var cells []string
	// Walk stops at the first false, so a row that holds no run returns true and the walk goes on;
	// its cells, visited next, are no rows and are passed over.
	pmdoc.Walk(doc, func(node *pmdoc.Node) bool {
		if node.Type != "table_row" && node.Type != "table_header_row" {
			return true
		}
		texts := make([]string, len(node.Children))
		for index, cell := range node.Children {
			texts[index] = pmdoc.TextContent(cell)
		}
		for start := range len(texts) - len(parts) + 1 {
			run := texts[start : start+len(parts)]
			matched := true
			for index, part := range parts {
				if !cellHolds(run[index], part, shown[index]) {
					matched = false
					break
				}
			}
			if matched {
				cells = run
				return false
			}
		}
		return true
	})
	return cells
}

// cellHolds reports whether a cell's text holds part, a piece of a quote, or shown, the text that
// piece renders as, each compared with every run of whitespace as one space. An empty part, the
// space between two adjacent pipes, is an empty cell and holds only one.
func cellHolds(text, part, shown string) bool {
	text = strings.Join(strings.Fields(text), " ")
	if part == "" {
		return text == ""
	}
	for _, piece := range []string{part, shown} {
		piece = strings.Join(strings.Fields(piece), " ")
		if piece != "" && strings.Contains(text, piece) {
			return true
		}
	}
	return false
}

// renderedInlineText is the text inline markdown renders as - `*a*` is `a`, `[x](u)` is `x` - or
// the markdown itself when the parser refuses it.
func renderedInlineText(markdown string) string {
	nodes, err := pmdoc.ParseInline(markdown, pmdoc.NewWriteBudget())
	if err != nil {
		return markdown
	}
	var text strings.Builder
	for _, node := range nodes {
		text.WriteString(pmdoc.TextContent(node))
	}
	return text.String()
}

// cellQuoteParts is quote split as a markdown table row is split into cells: at every pipe after an
// even run of backslashes (none included), inside code spans and links too, since a row ends a cell
// there (pmdoc's splitAtEvenEscapedPipes). Each part is trimmed and kept as the markdown its cell
// would hold: a pipe after an odd run is the pipe a cell holds, its escape taken off, and every
// other backslash stays for the cell's inline parse. An empty part between two pipes is an empty cell and is kept; the empty text
// before a leading pipe and after a trailing one is no cell and is dropped. A quote with no such
// pipe is no row, and has no parts.
func cellQuoteParts(quote string) []string {
	var parts []string
	var part strings.Builder
	split := false
	flush := func() {
		parts = append(parts, strings.TrimSpace(part.String()))
		part.Reset()
	}
	for index := 0; index < len(quote); index++ {
		if quote[index] != '\\' && quote[index] != '|' {
			part.WriteByte(quote[index])
			continue
		}
		run := 0
		for index < len(quote) && quote[index] == '\\' {
			run++
			index++
		}
		if index == len(quote) || quote[index] != '|' {
			part.WriteString(strings.Repeat(`\`, run))
			index--
			continue
		}
		// The row takes only the escape of its own pipe: an odd run's last backslash goes with
		// the pipe it escapes, and every other backslash stays markdown for the cell's inline
		// parse (renderedInlineText) to read, as the table parser leaves it.
		if run%2 == 1 {
			part.WriteString(strings.Repeat(`\`, run-1))
			part.WriteByte('|')
			continue
		}
		part.WriteString(strings.Repeat(`\`, run))
		split = true
		flush()
	}
	flush()
	if !split {
		return nil
	}
	// The text before a leading pipe and after a trailing one is no cell.
	if parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// tableCellAt is the paragraph of the table cell whose content holds position, and reports false
// when position is in no table cell's paragraph.
func tableCellAt(doc *pmdoc.Node, position int) (*pmdoc.Node, bool) {
	at, ok := pmdoc.ContainingTextblock(doc, position)
	if !ok || len(at.Ancestors) == 0 {
		return nil, false
	}
	if parent := at.Ancestors[0].Type; parent != "table_cell" && parent != "table_header" {
		return nil, false
	}
	return at.Node, true
}

// insertTarget is the range anchor, an insert's or a move's written in field of op, names. A quote
// anchor resolves in one textblock (quoteInOneTextblock) - one reaching across two table cells
// otherwise lies inside the table, and would land the insert or the moved block beside it - and
// finds its block whatever it spells a link with, since it names a place and changes no text.
func insertTarget(tree *batchTree, op model.EditOp, field, anchor string, occurrence *int) (pmdoc.Range, error) {
	switch anchor {
	case "start":
		return pmdoc.Range{}, nil
	case "end":
		position := pmdoc.Size(tree.node)
		return pmdoc.Range{From: position, To: position}, nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return pmdoc.Range{}, invalidOp("heading")
		}
		valid, err := tree.validated("FindHeading")
		if err != nil {
			return pmdoc.Range{}, err
		}
		r, err := valid.FindHeading(title, occurrence)
		var ambiguous *pmdoc.ErrTargetAmbiguous
		if errors.As(err, &ambiguous) {
			return pmdoc.Range{}, &ErrAnchorAmbiguous{
				Kind: "heading anchor", Target: title, Candidates: ambiguous.Candidates,
			}
		}
		return r, err
	}
	if blockID, ok := strings.CutPrefix(anchor, "block:"); ok {
		if blockID == "" {
			return pmdoc.Range{}, invalidOp("block")
		}
		valid, err := tree.validated("BlockRange")
		if err != nil {
			return pmdoc.Range{}, err
		}
		r, err := valid.BlockRange(blockID)
		if err != nil {
			return pmdoc.Range{}, blockNotFound(tree, op, field, blockID, err)
		}
		return r, nil
	}
	r, _, _, err := quoteInOneTextblock(tree, field, anchor, occurrence)
	return r, err
}

// refuseUnreadableReplacement refuses a replace that makes the document-level block it lands in
// unreadable: one the parser read back before the replace and refuses after it. What markdown
// reads as a block depends on where the text lands, so the rendered block decides it rather than
// the replacement alone: `<div>x</div>` over a whole paragraph, at a list item's start or after a
// hard break opens an HTML block the Proof schema does not carry, while the same HTML inside a
// line, a table cell or a heading is inline HTML and is kept. A block that was already unreadable,
// or another block that is, is no reason to refuse this replace. backs holds the read-back of each
// block it reads, which the replace's other check of the same blocks shares (blockReadBacks).
func refuseUnreadableReplacement(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, with string, backs blockReadBacks) error {
	_, unreadable, err := replacementBroke(before.Node(), after, found, backs.read)
	if err != nil || unreadable == nil {
		return err
	}
	return &ErrInvalidOp{Field: "with", Reason: unreadableReason(with, unreadable)}
}

// refuseReshapedReplacement refuses a replace whose text the document reads back as blocks of
// another shape where it lands: a heading and a table cell are written on one line, so a line
// break inside a code span or inline HTML there ends the block, as a hard break would
// (hasHardBreak), and the document reads back a heading and a paragraph, or a row as two rows.
func refuseReshapedReplacement(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, with string, backs blockReadBacks) error {
	_, reshaped, err := replacementBroke(before.Node(), after, found, backs.shape)
	if err != nil || reshaped == nil {
		return err
	}
	return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
		"with %q is text the document reads back as another block where it lands (%v); write it inside a line of text",
		with, reshaped,
	)}
}

// refuseCodePipeInTableCell refuses a write that adds, to the document-level blocks it changed
// (matchedBlocks), code in a table cell whose text holds a `|` after an odd run of backslashes
// (pmdoc.TableCodePipes): no markdown of a cell writes that code, so what the document stored would
// read back otherwise. A replace's `with` is read as a paragraph's text, where `\|` in code is the
// code's text, and a delete can cut code down to it. Such code a browser edit already left in those
// blocks is no reason to refuse the write, so a document holding it still takes writes, the one
// that rewrites that code to hold no such `|` among them; code the write adds beside it is. What
// the blocks held before and hold after are compared by the code's text, counted, since a write can
// move a cell to another row, and the refusal names the first code after that the blocks did not
// hold before. field is the field the refusal names, opening the words it opens with and value
// what the caller sent, which the refusal quotes after them, and advice what the caller can do
// beside what pmdoc's refusal says, or "". An error from pmdoc, a panic (pmdoc.ErrPanic), is
// pmdoc's bug, and is the error.
func refuseCodePipeInTableCell(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, field, opening, value, advice string) error {
	beforeTree := before.Node()
	first, last, lastAfter := found.first, found.last, found.lastAfter(after)
	var held map[string]int
	for index := first; index <= last; index++ {
		cells, err := pmdoc.TableCodePipes(beforeTree.Children[index])
		if err != nil {
			return err
		}
		for _, cell := range cells {
			if held == nil {
				held = map[string]int{}
			}
			held[cell.Code]++
		}
	}
	for index := first; index <= lastAfter; index++ {
		cells, err := pmdoc.TableCodePipes(after.Children[index])
		if err != nil {
			return err
		}
		for _, cell := range cells {
			if held[cell.Code] > 0 {
				held[cell.Code]--
				continue
			}
			reason := fmt.Sprintf("%s %q leaves code the document cannot store where it lands: %v", opening, value, cell)
			if advice != "" {
				reason += "; " + advice
			}
			return &ErrInvalidOp{Field: field, Reason: reason}
		}
	}
	return nil
}

// unreadableReason says why a replace left its block unreadable and what to do instead, by cause.
func unreadableReason(with string, unreadable error) string {
	if errors.Is(unreadable, pmdoc.ErrBlockHTML) {
		return fmt.Sprintf(
			"with %q is HTML that opens a block where it lands, and the Proof schema carries no block HTML; keep the HTML inside a line, where it opens no block",
			with,
		)
	}
	return fmt.Sprintf("with %q leaves markdown the document cannot read back where it lands (%v); write it inside a line of text, or insert the block you mean as its own block", with, unreadable)
}

func blockID(block *pmdoc.Node) string {
	id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
	return id
}

// applyReplace writes a replace's `with` over the text its `find` matched, on budget, the batch's.
// The `with` goes one of two ways, and the arm stands apart from the operation switch because the
// two ways meet again in the steps that follow the splice: a `with` naming blocks of its own is
// written as blocks over the match (blockReplacement), and one of a single paragraph continues the
// textblock the match sits in (inlineReplacement). Which way it goes is readReplacementWith's,
// whose reading the block guard takes too (rewrittenTypedBlock).
func applyReplace(tree *batchTree, op model.EditOp, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	if op.Find == "" {
		return nil, invalidOp("find")
	}
	// A replace that names no with, or carries a member replace does not take, is refused by
	// the edit route before the batch reaches here (api.replaceFieldsRefusal), since only the
	// request body says which members a replace named.
	//
	// A find that crosses a marked span's edge is refused here, before its with is read
	// (findEditQuote): the text written over it would land outside the span, so a link it covered
	// would go with its URL, and a span it cut to end on a space would be written with character
	// references (`**Sami said&#32;**loud`). A find that spells the span's own markup over all of
	// the span's text it covers, the whole span or the part of it the find covers, replaces that
	// markup instead. A with of blocks removes the whitespace beside the find too, so the find is
	// weighed again over that text once the with is read.
	r, at, spelled, byMarker, err := findEditQuote(tree, op)
	if err != nil {
		return nil, err
	}
	code := at.Node.Type == "code_block"
	var with *pmdoc.Node
	// blocks says `with` names blocks of its own, which are written over the match by the
	// rules the fit applies (blockReplacement), where one paragraph of text continues the
	// textblock the match sits in (inlineReplacement).
	blocks := false
	level := 0
	var tick *bool
	// removed is the text a with of blocks removes (blockReplacementRemoves): the find is weighed
	// again over it, and the write cuts exactly it (settleBlockReplacement).
	var removed pmdoc.Range
	if code {
		with = codeReplacement(op.With)
	} else {
		read, err := readReplacementWith(tree, at, r, op, byMarker)
		if err != nil {
			return nil, err
		}
		blocks, level, tick = read.blocks, read.level, read.tick
		if blocks {
			// The blocks split the textblock at r, and the write drops the whitespace the split
			// leaves beside r, so a link whose text beyond r is that whitespace alone goes whole:
			// it must be spelled as it is wherever the find spells it.
			if removed, err = blockReplacementRemoves(at, r); err != nil {
				return nil, err
			}
			if err := refuseCrossing(tree, at, op, r, removed, spelled, byMarker); err != nil {
				return nil, err
			}
			// The blocks the document keeps are the second parse's, the one the batch pays for.
			if with, err = parseFragmentInput(read.markdown, opensDocument(tree.node, r.From), budget); err != nil {
				return nil, invalidMarkdownOp("with", err)
			}
		} else if with, err = inlineReplacement(read.markdown, edgesOf(at, r), budget); err != nil {
			return nil, err
		}
	}
	// A textblock written on one line is a heading or a table cell, and the block path refuses
	// a match in either, so only an inline replacement can carry a break into one.
	if !blocks && hasHardBreak(with) && at.OneLine() {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q carries a hard break, which a heading or a table cell cannot hold: it is written on one line, so the break would end the block there; write the text without the break, or insert a new block after this one",
			op.With,
		)}
	}
	// The text written over the match stays inside every ask and comment anchor the match lay
	// wholly inside, so each anchor's quote is still the whole text it covers. The inline path
	// writes one textblock's text and carries the anchors over all of it; the block path writes
	// blocks, and carries them on one block of what it writes (markBlockReplacement).
	//
	// The inline path's text stays inside every bold, italic, struck, link or code span the match
	// lay wholly inside too (pmdoc.MarksCovering), so the span stays whole: `said` replaced in
	// `**Sami said bold**` reads `**Sami answered bold**`, where text without the span's mark split
	// it and the split's edges were written as character references, `**Sami&#32;**&#97;nswered`.
	// Text that writes a mark of the span's type itself keeps its own (pmdoc.AddSpanMarks), and a
	// find that spells a span's markup over the whole match (`**Sami bold**`) leaves that mark to
	// the with (pmdoc.WithoutQuotedMarkup), which keeps it by writing it or drops it by leaving it
	// off; one that spells it over part of the match alone (`a **b** c` over `**a b c**`) keeps
	// the span. Only a find the finder read as markdown spells markup: one it matched as written
	// is text, so `2*3*4` in `*a 2\*3\*4 b*` keeps the italic.
	anchors, spans := pmdoc.MarksCovering(tree.node, r)
	if blocks {
		if err := markBlockReplacement(with, anchors, op.With); err != nil {
			return nil, err
		}
	} else {
		pmdoc.AddMarks(with, anchors)
		pmdoc.AddSpanMarks(with, pmdoc.WithoutQuotedMarkup(spans, spelled))
	}
	valid, err := tree.validated("Splice")
	if err != nil {
		return nil, err
	}
	var next *pmdoc.Node
	if blocks {
		if next, err = blockReplacement(valid, at, r, with, op.With); err != nil {
			return nil, err
		}
	} else {
		next, err = valid.Splice(r, with)
		err = invalidSchemaOp("with", err)
		// A heading rename is one paragraph of text, since the block path refuses every match
		// in a heading, so the level is the inline path's alone.
		if err == nil && level != 0 {
			next, err = pmdoc.SetHeadingLevel(next, r.From, level)
		}
		if err != nil {
			return nil, err
		}
	}
	// A task item's box is set on the tree either path wrote: `done\n\n- a` over `- [ ] todo`
	// writes the item's text and a list under it, and the box is the item's own attribute
	// still.
	if tick != nil {
		if next, err = pmdoc.SetTaskChecked(next, r.From, *tick); err != nil {
			return nil, err
		}
	}
	// The document-level blocks the replace changed, found once over the tree it started from
	// and read by the settlement and by every refusal below, each of which found them for
	// itself before.
	found, err := matchedBlocks(valid, r)
	if err != nil {
		return nil, err
	}
	// What the block path wrote is settled and its block ids carried, on the tree the task
	// box was set on: the settlement removes children and moves every position after them,
	// so nothing that reads a position may run after it.
	if blocks {
		next = settleBlockReplacement(valid, next, found, at, r, removed, with, op.With)
	}
	// The two read-back checks below read the blocks this replace changed, and the block it
	// lands in can hold the whole document's text: each block is rendered and parsed once for
	// the two (blockReadBacks). What the block path wrote is judged after them, over the
	// document as a whole (refuseWrittenBlocks).
	backs := blockReadBacks{}
	if code {
		return next, refuseCodeThatReshapesItsBlock(valid, next, found, at, "with", op.With, backs)
	}
	if err := refuseCodePipeInTableCell(valid, next, found, "with", "with", op.With, ""); err != nil {
		return nil, err
	}
	if err := refuseUnreadableReplacement(valid, next, found, op.With, backs); err != nil {
		return nil, err
	}
	if err := refuseReshapedReplacement(valid, next, found, op.With, backs); err != nil {
		return nil, err
	}
	if !blocks {
		return next, nil
	}
	return next, refuseWrittenBlocks(tree.node, next, with)
}

// replacementWith is a replace's `with` as readReplacementWith reads it over the match.
type replacementWith struct {
	// markdown is what the `with` writes: its text, less a marker the matched block renders
	// already (replacementMarkdown) and a checkbox that sets a task item's box (tick).
	markdown string
	// level is the heading level a heading rename sets, 0 for none.
	level int
	// tick is the task item's box the `with` sets, nil for none.
	tick *bool
	// blocks says the `with` names blocks of its own, and parsed is the parse that says so, read
	// as the parse left it rather than copied as an upload (asUploaded). It is nil where that
	// parse read HTML that opens a block, which goes to the inline path.
	blocks bool
	parsed *pmdoc.Node
}

// readReplacementWith reads op's `with` as the replace writes it over r, the match a find made in
// the textblock at, which is no code block's: the write (applyReplace) and the block guard
// (rewrittenTypedBlock) both take this one reading, so the blocks the guard asks for are the ones
// the `with` rewrites. byMarker reports that the find's list marker selected the item it matched.
func readReplacementWith(tree *batchTree, at pmdoc.TextblockAt, r pmdoc.Range, op model.EditOp, byMarker bool) (replacementWith, error) {
	replacement, level, err := replacementMarkdown(tree.node, r, op.Find, op.With, byMarker)
	if err != nil {
		return replacementWith{}, err
	}
	read := replacementWith{level: level}
	// A task item's text replaced from its start by one opening with a checkbox is the item
	// being ticked (or unticked): the box is the item's `checked` attribute, not text, and
	// written as text it would read `- [ ] [x] …`. Elsewhere `[x] ` stays the literal text it
	// has always been.
	if r.From == at.Content.From && pmdoc.TaskItemAt(tree.node, r.From) {
		if checked, found, width := pmdoc.LeadingTaskCheckbox(replacement); found {
			if strings.TrimSpace(replacement[width:]) == "" {
				// The checkbox alone would tick the item and empty it, and an empty task item
				// renders as a plain `- ` (pmdoc/render.go), so the next version would lose the
				// task: the empty-text refusal LEGION-280 added to inlineReplacement must see the
				// whole `with`, not the text past the box.
				return replacementWith{}, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
					"with %q sets the item's checkbox but leaves it no text, and an empty task item is written as a plain item; write the box and the item's text, or pass an empty with to delete the text",
					op.With,
				)}
			}
			replacement = replacement[width:]
			read.tick = &checked
		}
	}
	read.markdown = replacement
	// `with` is read as markdown written into a document, as an insert's markdown is, so front
	// matter opens the document's start alone. A fragment of one paragraph, and the empty one an
	// empty `with` parses to, go to the inline path, which parses the text again: that path
	// carries the refusal of a `with` that renders to no text, the hard breaks, the whitespace at
	// the text's edges and the anchor marks it strips, and reusing this paragraph would change
	// all four. HTML the parser reads as a block goes there too: the Proof schema holds no block
	// of HTML, so that text names no block, and where it lands it is inline text, as it is today
	// (pmdoc's dropHTMLFlowLines), which the read-back refusals judge. Every other refusal of the
	// parse is the caller's to read, as an insert's markdown is: the parse's own refusal says
	// which, since the name uploadedInput gives it does not carry the refusal it wraps.
	//
	// This parse says which of the two ways the `with` goes and nothing else, so it spends a
	// budget of its own: the batch's budget is charged by the parse whose tree the document
	// keeps, the inline path's parse of the same text or the write's second parse. A write's
	// elements are counted across every parse of its markdown (pmdoc's elementCount), so charging
	// this parse as well would halve the `with` one operation may send. The cost is one uncharged
	// parse an operation, bounded by a budget of its own as every other parse is.
	parsed, err := pmdoc.ParseFragment(replacement, opensDocument(tree.node, r.From), pmdoc.NewWriteBudget())
	switch {
	case err == nil:
		// Which way the `with` goes is its blocks' types and nothing else.
		read.blocks = !pmdoc.EmptyDocument(parsed) && !isInlineDocument(parsed)
		read.parsed = parsed
	case !errors.Is(err, pmdoc.ErrBlockHTML):
		_, refusal := uploadedInput(parsed, err)
		return replacementWith{}, invalidMarkdownOp("with", refusal)
	}
	return read, nil
}

// inlineReplacement parses replace's `with` as one textblock's inline content, which is what a
// `with` of one paragraph writes: it continues the text around the match, so its edge whitespace
// is kept (continueText) and a leading list or heading marker is text, never a new block. A `with`
// that names blocks of its own is written by blockReplacement instead. Its ErrSchema refusal
// names the `with` as HTML that opens a block, which is true of the text it can be given and of
// no other: its caller must reach it only for a `with` the block parser (pmdoc.ParseFragment)
// refused with pmdoc.ErrBlockHTML, or read as one paragraph or as nothing, which is what
// applyReplace does. Text no such parse has sorted could reach that refusal as a footnote
// definition, a link reference definition or two paragraphs across a blank line, none of them
// HTML. The elements it makes are spent from budget, the batch's.
func inlineReplacement(markdown string, edges textEdges, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	inline, err := pmdoc.ParseInline(markdown, budget)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
				"with %q is HTML that opens a block where it lands, and the Proof schema carries no block HTML, so the with writes one textblock's text, which the text beside the HTML is more than (%v); keep the HTML inside a line, where it opens no block, or insert the blocks you mean as blocks of their own",
				markdown, err,
			)}
		}
		return nil, err
	}
	// An empty `with` deletes the matched span on purpose, and is the only `with` that does:
	// whitespace alone, which the caller wrote and which renders to nothing, would splice nothing
	// over the match and silently delete the text they meant to replace (LEGION-280). A line
	// indented four spaces or a tab is a code block, which blockReplacement writes.
	if len(inline) == 0 && markdown != "" {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q is whitespace alone, which renders to no text, so there would be nothing to put in the match's place; write the new text, or pass an empty with to delete the matched text",
			markdown,
		)}
	}
	paragraph := &pmdoc.Node{Type: "paragraph", Children: inline}
	continueText(paragraph, markdown, edges)
	return pmdoc.StripAnchorMarks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}), nil
}

// blockReplacement writes the blocks a `with` names over the match, fitted by the rules
// pmdoc.ValidDocument.Splice applies, which are the rules of the editor a person reads the
// document in: blocks that enter a list become its items, and blocks a typed block can hold go
// inside it. Three places hold no block of their own, and a match in one is refused with the
// document unchanged, since the fit would write the blocks where that place stood or outside it.
func blockReplacement(valid pmdoc.ValidDocument, at pmdoc.TextblockAt, r pmdoc.Range, fragment *pmdoc.Node, with string) (*pmdoc.Node, error) {
	written := pmdoc.BlockNames(fragment.Children)
	if at.Node.Type == "heading" {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q writes %s, and the match is in a heading, which holds one line of text and no block of its own; write the heading's new text alone, or insert the blocks after the heading",
			with, written,
		)}
	}
	// Where the fit does not refuse a cell itself, it lifts the blocks out of the table and
	// leaves it a cell short or splits it in two, so every cell is refused and not the refusing
	// cells alone.
	for _, ancestor := range at.Ancestors {
		if ancestor.Type == "table_cell" || ancestor.Type == "table_header" {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
				"with %q writes %s, and the match is in a table cell, which holds one paragraph of text; write the cell's new text alone, or insert the blocks after the table",
				with, written,
			)}
		}
	}
	next, err := valid.Splice(r, fragment)
	if err != nil {
		return nil, blockSpliceRefusal(with, written, err)
	}
	// A typed block whose content rule cannot hold the blocks keeps its other text and the blocks
	// stand outside it, which is the fit climbing out of that block. A `with` that rewrites the
	// block it lands in under that block's own id is that climb on purpose (pmdoc.RewritesBlock,
	// fitReplacement), and stands.
	holder := typedBlockHolder(at)
	if holder != nil && !pmdoc.RewritesBlock(fragment.Children, holder) && !holdsWrittenBlocks(next, holder, fragment) {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q writes %s, and the %s holding the match cannot hold them, so they would stand outside it; write the new text as paragraphs, which the %s holds, or insert the blocks after it",
			with, written, holderName(holder), holderName(holder),
		)}
	}
	return next, nil
}

// markBlockReplacement carries the anchor marks covering the match onto the blocks a `with`
// names, before the fit writes them, as an accepted suggestion marks its replacement before its
// splice (applySuggestion). One block of what is written carries every mark: the first paragraph
// or heading the fragment holds (anchoredBlock). An anchor's mark covers a run of text, so marking
// every block would make one anchor out of a paragraph and the list beside it, and a code block
// holds text an ask's mark cannot cover.
//
// Where the match lies inside an anchor and the fragment holds no paragraph and no heading - a
// rule alone, a table alone - the operation is refused and the document is left as it was: the
// anchor quotes the words being rewritten, and the write would leave it nothing to quote. The
// same `with` where no anchor covers the match writes its rule or its table.
func markBlockReplacement(fragment *pmdoc.Node, covering []pmdoc.Mark, with string) error {
	if len(covering) == 0 {
		return nil
	}
	anchored := anchoredBlock(fragment)
	if anchored == nil {
		written := pmdoc.BlockNames(fragment.Children)
		return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q writes %s, and the match is inside an open comment or ask, which quotes the words being rewritten; %s holds no line of text to carry that quote; write a line of text with the blocks, or insert the blocks beside this block and leave the quoted text alone",
			with, written, written,
		)}
	}
	pmdoc.AddMarks(anchored, covering)
	return nil
}

// anchoredBlock is the first paragraph or heading of a parsed `with` holding a run of text, in
// document order and at any depth, which for a list is its first item's paragraph. Where the
// fragment holds no such block it is nil.
//
// A block inside a table is passed over: a mark there covers one cell of a row, so the anchor
// would quote that cell's text rather than the words the write puts in the anchor's place. One
// holding no run of text is passed over as well, since the marks go on the runs of text under the
// block (pmdoc.AddMarks) and there are none, so the anchor would be lost without a word.
// Measured on 2026-11-11, a `with` of "#\n\n- a" writes a heading with no text and a list, and
// one of ":::callout{kind=\"note\" title=\"T\"}\n\n:::" a callout holding an empty paragraph.
func anchoredBlock(node *pmdoc.Node) *pmdoc.Node {
	for _, child := range node.Children {
		if child.Type == "table" {
			continue
		}
		if (child.Type == "paragraph" || child.Type == "heading") && holdsTextRun(child) {
			return child
		}
		if found := anchoredBlock(child); found != nil {
			return found
		}
	}
	return nil
}

// holdsTextRun reports whether node holds a run of text, which is what a mark covers.
func holdsTextRun(node *pmdoc.Node) bool {
	held := false
	pmdoc.Walk(node, func(child *pmdoc.Node) bool {
		if child.Type != "text" {
			return true
		}
		held = true
		return false
	})
	return held
}

// refuseWrittenBlocks runs over what the block path wrote the two guards every other path that
// writes blocks runs (the insert arm, an accepted suggestion). `with` reaches the document through
// the parse an insert's markdown reaches it through, so it can name a typed block's id, and it
// lands blocks beside blocks they are rendered and read back with.
//
// A `with` that rewrites the typed block it lands in under that block's own id names one block of
// the document still, so it passes both, as the fit and the third refusal of blockReplacement
// leave it standing.
func refuseWrittenBlocks(before, next, fragment *pmdoc.Node) error {
	if err := pmdoc.RepeatedBlockID(before, next, fragment); err != nil {
		return &ErrInvalidOp{Field: "with", Reason: err.Error()}
	}
	// What is stored is the document's rendering, so a write that leaves it reading back
	// otherwise would store another document than the one it wrote.
	return invalidSchemaOp("with", pmdoc.RefuseMisreadWrite(before, next))
}

// settleBlockReplacement settles what the block path wrote over the match and carries the block
// ids of the textblock it split, over the tree the splice made.
//
// The settlement is the accepted suggestion's (settleAccepted), with what removed, the text the
// replace removes (blockReplacementRemoves), holds beyond r cut off the inner edge of each half
// first (trimSplitEdges): the whitespace the split left there. Measured on 2026-11-11 over
// `Alpha beta gamma.`, a `with` of "beta\n\n- a\n- b" splices to
// "Alpha&#32;\n\nbeta\n\n- a\n- b\n\n&#32;gamma.\n" and pmdoc.AgreeWithReadBack leaves that
// string exactly as it is: it drops a changed child the renderer does not write that carries no
// block id, copies the spread of lists and items from their read-back, and never changes text.
// The inline path drops that same whitespace for itself (continueText), so this is the block path
// taking the rule it already follows.
//
// The ids follow the settlement, which drops an unwritten half only while that half carries no id
// of its own (carryReplacedBlockIDs).
func settleBlockReplacement(valid pmdoc.ValidDocument, next *pmdoc.Node, found matchBlocks, at pmdoc.TextblockAt, r, removed pmdoc.Range, fragment *pmdoc.Node, with string) *pmdoc.Node {
	held := blockID(at.Node)
	trimSplitEdges(splitHalves(next, found, held), r, removed)
	next = settleAccepted(valid, next, found, with)
	carryReplacedBlockIDs(next, found, held, fragment)
	return next
}

// splitHalves are the parts of the textblock the match lay in that the write left, in document
// order: the blocks carrying held, that textblock's own id, among the document-level blocks the
// write changed (matchBlocks). The splice gives each half that one id - measured on 2026-11-11,
// where `Alpha beta gamma.` split over `beta` left it on the first block and on the last - so a
// match inside the text leaves two halves and one at an edge of it leaves one. Only the blocks
// the write changed are read, so a repeat of that id the document already carried elsewhere is
// left alone.
func splitHalves(next *pmdoc.Node, found matchBlocks, held string) []*pmdoc.Node {
	if held == "" {
		return nil
	}
	var halves []*pmdoc.Node
	last := min(found.lastAfter(next), len(next.Children)-1)
	for index := found.first; index <= last; index++ {
		pmdoc.Walk(next.Children[index], func(node *pmdoc.Node) bool {
			if blockID(node) == held {
				halves = append(halves, node)
			}
			return true
		})
	}
	return halves
}

// trimSplitEdges cuts what removed holds beyond r off the inner edge of each half the split left:
// the end of the part before the match, and the start of the part after it. That is the whitespace
// blockReplacementRemoves reads at the end of the last text run before r and at the start of the
// first one after it, which the splice wrote as that half's last and first run. removed reaches
// past r only where text stands beside it, so it reaches before r only where a part before the
// match is left, which is then the first half, and after r only where a part after it is left,
// the last half.
func trimSplitEdges(halves []*pmdoc.Node, r, removed pmdoc.Range) {
	if len(halves) == 0 {
		return
	}
	if units := r.From - removed.From; units > 0 {
		trimTextEnd(halves[0], units)
	}
	if units := removed.To - r.To; units > 0 {
		trimTextStart(halves[len(halves)-1], units)
	}
}

// trimTextEnd cuts units UTF-16 units off the end of a textblock's last run, and the run itself
// where nothing is left of it. The run is the splice's own, written from the text it kept
// (pmdoc's inlineRange), so changing it changes no node of the tree the write started from.
func trimTextEnd(textblock *pmdoc.Node, units int) {
	children := textblock.Children
	last := children[len(children)-1]
	for ; units > 0 && last.Text != ""; units-- {
		char, size := utf8.DecodeLastRuneInString(last.Text)
		last.Text = last.Text[:len(last.Text)-size]
		if char > 0xFFFF {
			units--
		}
	}
	if last.Text == "" {
		textblock.Children = children[:len(children)-1]
	}
}

// trimTextStart is trimTextEnd at a textblock's start.
func trimTextStart(textblock *pmdoc.Node, units int) {
	children := textblock.Children
	first := children[0]
	for ; units > 0 && first.Text != ""; units-- {
		char, size := utf8.DecodeRuneInString(first.Text)
		first.Text = first.Text[size:]
		if char > 0xFFFF {
			units--
		}
	}
	if first.Text == "" {
		textblock.Children = children[1:]
	}
}

// carryReplacedBlockIDs gives held, the id of the textblock the match lay in, to the first block
// in document order that still holds that textblock's text: the part before the match where one
// is left, the part after it where the match opened the text, and the block the write wrote first
// where it replaced the whole text and that block is a paragraph. Every other block the write
// left under held takes a fresh id, so no id names two blocks; the blocks the write wrote carry
// the ids their own parse minted. Where no block holds that text any more the textblock is gone,
// and its id with it.
//
// Both halves of a split carry held until this runs, and a paragraph written over a whole
// textblock's text comes with an id of its own: over `Here is the plan.`, a `with` of
// "Here is the plan:\n\n- first\n- second" left a minted id where the document held the
// paragraph's, measured on 2026-11-11.
func carryReplacedBlockIDs(next *pmdoc.Node, found matchBlocks, held string, fragment *pmdoc.Node) {
	if held == "" {
		return
	}
	halves := splitHalves(next, found, held)
	if len(halves) > 0 {
		for _, half := range halves[1:] {
			mintFreshBlockID(half)
		}
		return
	}
	if len(fragment.Children) == 0 || fragment.Children[0].Type != "paragraph" {
		return
	}
	if written := blockWithID(next, blockID(fragment.Children[0]), "paragraph"); written != nil {
		written.Attrs[pmdoc.BlockIDAttr] = held
	}
}

// mintFreshBlockID gives block the id pmdoc mints for a block carrying none (EnsureBlockIDs). It
// stamps that block alone, so the rest of the document is left as it is: a repeat the document
// already carried is settlement's to repair, not a write's.
func mintFreshBlockID(block *pmdoc.Node) {
	delete(block.Attrs, pmdoc.BlockIDAttr)
	pmdoc.EnsureBlockIDs(block)
}

// blockSpliceRefusal words the refusal of a `with` whose blocks the fit cannot write where the
// match sits, as acceptSpliceRefusal words it for an accepted suggestion, on the field the caller
// wrote and with the operation they can send instead.
func blockSpliceRefusal(with, written string, err error) error {
	switch {
	case errors.Is(err, pmdoc.ErrJoinEmptiesTypedBlock):
		return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q replaces a match running into an ask or a callout from the text before it, and writing it would join the two and leave the ask or the callout empty; match text inside one of them",
			with,
		)}
	case errors.Is(err, pmdoc.ErrReplacementDoesNotFit):
		return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q writes %s, which no part of the document can hold where the match sits; write the match's new text alone, or insert the blocks beside the block the match is in",
			with, written,
		)}
	case errors.Is(err, pmdoc.ErrSchema):
		return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q replaces a match running across blocks that writing it would join, which the document cannot hold together (%v); match text inside one block",
			with, err,
		)}
	}
	return err
}

// typedBlockHolder is the nearest typed block around the match, nil where the match is in none.
func typedBlockHolder(at pmdoc.TextblockAt) *pmdoc.Node {
	for _, ancestor := range at.Ancestors {
		if pmdoc.IsTypedBlock(ancestor.Type) {
			return ancestor
		}
	}
	return nil
}

// holdsWrittenBlocks reports whether the block carrying holder's id still holds every block the
// parse made, which is how a fit that wrote them inside the typed block is told from one that
// left them outside it. The parse stamps an id on each block it makes (pmdoc.EnsureBlockIDs) and
// the fit carries those ids through, measured on 2026-11-10 over a list written into a
// `requirement` and into a `callout`, so the blocks are found by id. A holder the fit took away
// holds nothing, which is the same lifting out.
func holdsWrittenBlocks(next, holder, fragment *pmdoc.Node) bool {
	held := blockWithID(next, blockID(holder), holder.Type)
	if held == nil {
		return false
	}
	inside := map[string]bool{}
	pmdoc.Walk(held, func(node *pmdoc.Node) bool {
		inside[blockID(node)] = true
		return true
	})
	for _, block := range fragment.Children {
		if id := blockID(block); id != "" && !inside[id] {
			return false
		}
	}
	return true
}

// blockWithID is the block of tree of that type carrying id, nil where the tree holds none and
// where id is empty, which no parsed block is (pmdoc.EnsureBlockIDs).
func blockWithID(tree *pmdoc.Node, id, nodeType string) *pmdoc.Node {
	if id == "" {
		return nil
	}
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == nodeType && blockID(node) == id {
			found = node
		}
		return found == nil
	})
	return found
}

// hasHardBreak reports whether a parsed replacement carries a hard break.
func hasHardBreak(with *pmdoc.Node) bool {
	for _, paragraph := range with.Children {
		for _, node := range paragraph.Children {
			if node.Type == "hardbreak" {
				return true
			}
		}
	}
	return false
}

// inlineAware parses a suggestion's replacement as blocks written into the document, keeping the
// edge whitespace of a replacement that stays inline. opensDocument says whether the replacement
// lands where the document begins; its tables are padded on budget, the accept's.
func inlineAware(markdown string, edges textEdges, opensDocument bool, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	tree, err := parseFragmentInput(markdown, opensDocument, budget)
	if err != nil {
		return nil, err
	}
	if isInlineDocument(tree) {
		continueText(tree.Children[0], markdown, edges)
	}
	return tree, nil
}

// refuseCodeThatReshapesItsBlock refuses a replacement into a code block that leaves the
// document-level block holding it reading back as blocks of another shape. A code block's text is
// literal, so only the lines around it could read it differently, and the renderer writes a typed
// block's fence longer than any line of colons in its code that the browser editor's parser, or
// this one, could read as that fence (pmdoc's typedFence). Code that only reads back with
// different whitespace keeps its shape and is not refused.
func refuseCodeThatReshapesItsBlock(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, at pmdoc.TextblockAt, field, with string, backs blockReadBacks) error {
	_, reshaped, err := replacementBroke(before.Node(), after, found, backs.shape)
	if err != nil || reshaped == nil {
		return err
	}
	holder := "block"
	if typed := typedBlockHolder(at); typed != nil {
		holder = typed.Type
	}
	return &ErrInvalidOp{Field: field, Reason: fmt.Sprintf(
		"%s %q changes how the %s holding this code block reads back (%v); move the code block out of the %s",
		field, with, holder, reshaped, holder,
	)}
}

// codeReplacement is what a replacement landing in a code block splices in: a code block's text
// is literal, whitespace, markdown syntax and references alike, so it is the replacement as sent.
func codeReplacement(text string) *pmdoc.Node {
	paragraph := &pmdoc.Node{Type: "paragraph"}
	if text != "" {
		paragraph.Children = []*pmdoc.Node{{Type: "text", Text: text}}
	}
	return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}
}

// textEdges reports which of a replacement's edges meet the edges of the text it lands in, where
// no text is left to continue and whitespace kept there is stripped on the next read, or, after a
// footnote's marker, opens indented code.
type textEdges struct{ start, end bool }

func edgesOf(at pmdoc.TextblockAt, r pmdoc.Range) textEdges {
	return textEdges{start: r.From == at.Content.From, end: r.To == at.Content.To}
}

// continueText restores the spaces and tabs parsing strips from a replacement's edges, so the
// replacement continues the text around it. They go in unmarked text at the paragraph's own
// edges - ` **x**` is a space and then bold, not a bold ` x`, and ` `c` ` leaves the code span's
// text alone. A line break at an edge is no part of an inline replacement and is not restored,
// and whitespace the parser keeps, such as a no-break space, is in the text already. An edge that
// meets the edge of the text it lands in (textEdges) is left off.
func continueText(paragraph *pmdoc.Node, markdown string, edges textEdges) {
	if len(paragraph.Children) == 0 {
		return
	}
	if leading := edgeSpace(markdown[:len(markdown)-len(strings.TrimLeft(markdown, markdownSpace))]); leading != "" && !edges.start {
		if first := paragraph.Children[0]; first.Type == "text" && len(first.Marks) == 0 {
			first.Text = leading + first.Text
		} else {
			paragraph.Children = append([]*pmdoc.Node{{Type: "text", Text: leading}}, paragraph.Children...)
		}
	}
	if trailing := edgeSpace(markdown[len(strings.TrimRight(markdown, markdownSpace)):]); trailing != "" && !edges.end {
		if last := paragraph.Children[len(paragraph.Children)-1]; last.Type == "text" && len(last.Marks) == 0 {
			last.Text += trailing
		} else {
			paragraph.Children = append(paragraph.Children, &pmdoc.Node{Type: "text", Text: trailing})
		}
	}
}

// markdownSpace is the whitespace a paragraph's parse strips from its edges.
const markdownSpace = " \t\n"

// edgeSpace is an edge's whitespace without its line feeds.
func edgeSpace(run string) string {
	return strings.ReplaceAll(run, "\n", "")
}

func isInlineLeaf(node *pmdoc.Node) bool {
	switch node.Type {
	case "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

func isInlineDocument(tree *pmdoc.Node) bool {
	if len(tree.Children) != 1 || tree.Children[0].Type != "paragraph" {
		return false
	}
	for _, child := range tree.Children[0].Children {
		if child.Type != "text" && !isInlineLeaf(child) {
			return false
		}
	}
	return true
}
