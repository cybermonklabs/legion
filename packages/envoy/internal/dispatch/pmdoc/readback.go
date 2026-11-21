package pmdoc

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
)

// skip names what a comparison leaves aside: the attributes it names, or all of them, and with
// text a textblock's inline content.
type skip struct {
	names map[string]bool
	all   bool
	text  bool
}

// shapeOnly compares blocks alone, their kinds and how they nest.
var shapeOnly = skip{all: true, text: true}

func (s skip) has(name string) bool { return s.all || s.names[name] }

// A document's markdown reads back as written when Parse gives back its blocks, their attributes and
// their text, as the markdown writes them (asWritten). Block ids, a typed block's server-owned
// attributes and anchor marks are not written, so they are not compared, and neither is an empty
// paragraph the renderer does not write (writtenChildren).

// WrittenReadBack is a document a write makes rendered - the markdown the write stores it as -
// and that markdown read back (ParseRendering). Every whole document a write makes is read back
// already, to refuse one that reads back otherwise (RefuseMisreadDocument), so ParseWritten hands
// that reading to the write: a step of the same write that would read the same rendering again -
// the task items its issue records, the blocks its advice counts - reads this tree instead of
// making a second one of the same markdown. Markdown is the rendering this tree was read from,
// which a caller compares with the markdown it stored before it shares the tree: a write that
// renders the tree it stores otherwise - an upload whose server-owned attributes are taken off
// before it is rendered (docs' asUploaded) - stored another rendering, which this is not.
type WrittenReadBack struct {
	Markdown string
	Tree     *Node
}

// ReadBack is what doc's markdown reads back as.
func ReadBack(doc *Node) (*Node, error) {
	written, err := readBackWritten(doc, maxSpanCells)
	return written.Tree, err
}

// readBack is what doc's markdown, its tables' spans adding at most spanCells cells (render),
// reads back as (ParseRendering).
func readBack(doc *Node, spanCells int) (*Node, error) {
	written, err := readBackWritten(doc, spanCells)
	return written.Tree, err
}

// readBackWritten is readBack with the markdown it read back, so a caller that stores that
// markdown shares this reading of it.
func readBackWritten(doc *Node, spanCells int) (WrittenReadBack, error) {
	r, err := render(doc, spanCells)
	if err != nil {
		return WrittenReadBack{}, err
	}
	return readStored(doc, r.b.String())
}

// readStored is markdown, the text doc is stored as, read back as every stored text is read
// (ParseRendering). A refusal of blocks nested past the bound is told by how many blocks doc nests
// (renderedNesting), since the line the parser names counts lines of a text nobody wrote.
func readStored(doc *Node, markdown string) (WrittenReadBack, error) {
	back, err := ParseRendering(markdown)
	return WrittenReadBack{Markdown: markdown, Tree: back}, renderedNesting(doc, err)
}

// NewMisread names how after, which a write made from before, reads back otherwise where before
// did not, or is "" when it does not. Each block that reads back otherwise is found as far down as
// its markdown still pairs with what reads back (misreads), and one that before already read back
// otherwise the same way, under the same block id, is not the write's: a textblock whose text
// differs in the same characters, an attribute with the same values, or a block that pairs with
// nothing. So a write beside a stale misread, or inside the block that holds one, is judged by what
// it changed, while a new misread in that block is still named. An attribute before read back with
// another value anywhere (a table cell's alignment) is not compared. A before whose markdown the
// parser refuses (a browser edit can leave one) gives nothing to judge against, and is "" whether
// or not after parses: the checks that read each changed block alone still refuse one the write
// leaves unreadable. A before whose read-back would pad more cells than a read-back's budget is no
// such document, and is the verdict: its markdown reads, so a write into it is refused rather than
// stored unjudged. Only a refusal (ErrSchema) reading either back is a verdict; any other error, a
// panic (ErrPanic) among them, is this package's bug, not a misread, and is the error.
func NewMisread(before, after *Node) (string, error) {
	found, err := newMisread(before, after, maxSpanCells)
	return found.reason, err
}

// newMisread is NewMisread reading before and after back with their tables' spans adding at most
// spanCells cells each, answering the misread itself, the zero misread where there is none. A
// refusal reading either back that is the verdict names no block (refusedMisread).
func newMisread(before, after *Node, spanCells int) (misread, error) {
	backAfter, err := readBack(after, spanCells)
	if err != nil && !errors.Is(err, ErrSchema) {
		return misread{}, err
	}
	written := asWritten(after)
	if err == nil && readDifference(written, backAfter, skip{}) == "" {
		return misread{}, nil
	}
	backBefore, beforeErr := readBack(before, spanCells)
	if beforeErr != nil && !errors.Is(beforeErr, ErrSchema) {
		return misread{}, beforeErr
	}
	if errors.Is(beforeErr, ErrTablePadding) {
		return refusedMisread(beforeErr), nil
	}
	if beforeErr != nil {
		return misread{}, nil
	}
	if err != nil {
		return refusedMisread(err), nil
	}
	previous := asWritten(before)
	drift := skip{names: attributeDrift(previous, backBefore)}
	known := map[sameMisread]bool{}
	for _, stale := range misreads(previous, backBefore, drift) {
		known[sameMisread{stale.id, stale.differs}] = true
	}
	for _, found := range misreads(written, backAfter, drift) {
		if found.id == "" || !known[sameMisread{found.id, found.differs}] {
			return found, nil
		}
	}
	return misread{}, nil
}

// refusedMisread is refused, a schema refusal (ErrSchema) of a read-back, as the misread of no
// block it is: its reason is the refusal's message, which the writer is told, and its kind the
// refusal's class (RefusalClass), which a log line names instead, since the message can quote the
// document.
func refusedMisread(refused error) misread {
	return misread{kind: RefusalClass(refused), reason: refused.Error()}
}

// sameMisread is what makes a misread one a document already held (NewMisread,
// RefuseMisreadStored): the block's id and what differs, which is the same for the same misread
// whatever else changed around it.
type sameMisread struct{ id, differs string }

// documentMisread is how doc, a whole document a write makes, reads back otherwise, the zero
// misread when it does not: its rendering is judged as the text the write stores (storedMisread),
// so every block that reads back otherwise is the write's, the first named as far down as its
// markdown still pairs (misreads). A doc the renderer refuses, or whose markdown no reader of this
// package reads at all, is that refusal instead, answered as refused, which refuseMisread turns
// into ErrRenderingRefused. Only a refusal (ErrSchema) is a verdict; any other error is the error.
// It answers the read-back it judged as well, which is the zero value where it made none, so a
// write that stores that rendering shares the one parse of it (WrittenReadBack).
func documentMisread(doc *Node) (read WrittenReadBack, found misread, refused error, err error) {
	r, err := render(doc, maxSpanCells)
	if err != nil {
		if errors.Is(err, ErrSchema) {
			return WrittenReadBack{}, misread{}, err, nil
		}
		return WrittenReadBack{}, misread{}, nil, err
	}
	reading, refused, err := storedMisread(doc, r.b.String())
	return reading.read, reading.misread(), refused, err
}

// storedReading is how a stored text reads back (storedMisread): the read-back itself, the first
// way the whole reads back otherwise (readDifference), "" where it reads back as written, and each
// block that reads back otherwise (misreads), which is listed only where difference is not "".
type storedReading struct {
	read       WrittenReadBack
	difference string
	found      []misread
}

// misread is how the stored text reads back otherwise, the zero misread where it does not: the
// first block that does, as far down as its markdown still pairs, or where no block is named the
// whole's difference, of kind "doc", the document's own type.
func (s storedReading) misread() misread {
	switch {
	case s.difference == "":
		return misread{}
	case len(s.found) > 0:
		return s.found[0]
	}
	return misread{kind: "doc", reason: s.difference}
}

// storedMisread is how markdown, the text a write stores doc as, reads back: markdown parsed as
// every stored text is read (readStored) and compared with doc as written (asWritten), every
// attribute and every textblock's text and marks. It is the one comparison an upload's rendering is
// judged by (documentMisread) and a write into a document judges the very text it stores by
// (RefuseMisreadStored), so the two cannot disagree. A markdown no reader of this package reads at
// all is that reader's refusal, answered as refused. Only a refusal (ErrSchema) is a verdict; any
// other error is the error.
func storedMisread(doc *Node, markdown string) (reading storedReading, refused error, err error) {
	read, err := readStored(doc, markdown)
	if err != nil {
		if errors.Is(err, ErrSchema) {
			return storedReading{}, err, nil
		}
		return storedReading{}, nil, err
	}
	written := asWritten(doc)
	reading = storedReading{read: read, difference: readDifference(written, read.Tree, skip{})}
	if reading.difference != "" {
		reading.found = misreads(written, read.Tree, skip{})
	}
	return reading, nil, nil
}

// RefuseMisreadDocument refuses (ErrSchema) a whole document a write makes, a spec, an upload or a
// version, that reads back otherwise (documentMisread), and RefuseMisreadWrite a write into a
// document, an insert, whose result reads back otherwise where before did not (NewMisread). What
// is stored is the rendering, which the next read would give back as another document or refuse.
// The rules that read a shape refuse what they know first, so a refusal here names a shape none of
// them reads, and every one of them is logged (refuseMisread). Any other error reading it back, a
// panic (ErrPanic) among them, is the error.
func RefuseMisreadDocument(doc *Node) error {
	_, found, refused, err := documentMisread(doc)
	return refuseMisread(found, refused, err)
}

// StoredMisread is a block a stored text reads back otherwise in, named without the document's
// text: its block id and the kind of difference (misread.kind) - a paragraph's text, a list item
// that reads back as a paragraph, a heading's level - never what the block holds, so a log line
// may carry it.
type StoredMisread struct {
	Block, Kind string
}

// StoredVerdict is RefuseMisreadStored's verdict on the text a write into a document stores.
// Refusal is nil where the text may be stored, and otherwise the refusal an upload of that text
// gets (ErrMisreadRendering, ErrRenderingRefused); Block is the id of the block it names, "" where
// it names none; Excused is each block that reads back otherwise, or that the parser refuses read
// alone, as it already did in the text the document was stored as before the write.
type StoredVerdict struct {
	Refusal error
	Block   string
	Excused []StoredMisread
}

// RefuseMisreadStored judges markdown, the text a write into a document stores after as, by the
// comparison an upload is refused by (storedMisread), so a write stores no text an upload of it
// would be refused for: what is stored is the text, and the next read gives it back as another
// document. The text is judged exactly as it is stored, whatever wrote it, so the verdict does not
// rest on the renderer writing every tree in a spelling that reads back.
//
// Two differences are excused. One is a block that reads back otherwise under the same block id
// and with the same difference (sameMisread) in previous, the text the document was stored as
// before the write, read against before, the tree previous was stored from. So a write beside a
// block that a browser edit or an earlier writer left reading back otherwise is stored, while a new
// misread in that block is still refused. The other is the spread an edit keeps (SettleDerived): a
// list or list item that reads back otherwise in its spread alone, which before holds unchanged
// under its id, and whose spread previous already read back otherwise, paired as SettleDerived
// pairs a spread (spreadDifferences), which leaves the spread aside. previous can name that misread
// a level higher, as a block that pairs with nothing: a typed block's parse fills an empty
// paragraph in ahead of a list it opens with, so its children do not pair one to one, and a write
// that deletes that list makes them pair and names the spread of an item it left as it was.
// previous is parsed only where markdown reads back otherwise, so a write whose text reads back
// pays for one parse, as an upload does. A nil before - the document stood outside the schema,
// and no text of it was read - excuses nothing, and neither does a previous the parser refuses.
// A refusal of markdown by the parser itself names no block, and is excused only as one previous
// already had (staleRefusal): a write into a document whose text the parser refuses is judged as
// a replace's own check judges the block it lands in, which refuses only a block the replace
// makes unreadable. Only a refusal (ErrSchema) is a verdict; any other error, a panic (ErrPanic)
// among them, is the error.
func RefuseMisreadStored(before *Node, previous string, after *Node, markdown string) (StoredVerdict, error) {
	reading, refused, err := storedMisread(after, markdown)
	switch {
	case err != nil:
		return StoredVerdict{}, err
	case refused != nil:
		excused, err := staleRefusal(before, previous, after, refused)
		if err != nil || excused != nil {
			return StoredVerdict{Excused: excused}, err
		}
		return StoredVerdict{Refusal: refuseMisread(misread{}, refused, nil)}, nil
	case reading.difference == "":
		return StoredVerdict{}, nil
	}
	known, err := storedMisreadsOf(before, previous)
	if err != nil {
		return StoredVerdict{}, err
	}
	var verdict StoredVerdict
	for _, found := range reading.found {
		if found.id != "" && (known.same[sameMisread{found.id, found.differs}] || known.keptSpread(found, before, after)) {
			verdict.Excused = append(verdict.Excused, StoredMisread{Block: found.id, Kind: found.kind})
			continue
		}
		if verdict.Refusal == nil {
			verdict.Refusal, verdict.Block = refuseMisread(found, nil, nil), found.id
		}
	}
	if len(reading.found) == 0 {
		verdict.Refusal = refuseMisread(reading.misread(), nil, nil)
	}
	return verdict, nil
}

// staleRefusal is each document-level block of after that explains refused, the parser's refusal
// of the text after is stored as, as a refusal the text before already had, or nil where they do
// not explain it: previous, the text before was stored as, is refused by the parser too, every
// document-level block of after the parser refuses read alone (ReadBackBlock) carries a block id
// under which before holds a document-level block the parser refused alone with a refusal of the
// same class (RefusalClass), and refused is of the class of one of them. A block refused anew, one
// with no id or none before, or a refusal no block read alone explains, is the write's. Such a
// text reads back as nothing, so nothing else in it is compared, as NewMisread compares nothing
// against a before the parser refuses; the checks that read each block a write changed alone still
// refuse one it leaves unreadable. Only a refusal (ErrSchema) reading a block alone is a verdict;
// any other error is the error.
func staleRefusal(before *Node, previous string, after *Node, refused error) ([]StoredMisread, error) {
	if before == nil {
		return nil, nil
	}
	if _, previousRefused, err := storedMisread(before, previous); err != nil || previousRefused == nil {
		return nil, err
	}
	whole := RefusalClass(refused)
	var held map[string]*Node
	var excused []StoredMisread
	explained := false
	for _, block := range after.Children {
		alone := ReadBackBlock(block).Read
		if alone == nil {
			continue
		}
		if !errors.Is(alone, ErrSchema) {
			return nil, alone
		}
		id := blockIDOf(block)
		if id == "" {
			return nil, nil
		}
		if held == nil {
			held = make(map[string]*Node, len(before.Children))
			for _, stale := range before.Children {
				if staleID := blockIDOf(stale); staleID != "" {
					held[staleID] = stale
				}
			}
		}
		stale, ok := held[id]
		if !ok {
			return nil, nil
		}
		staleAlone := ReadBackBlock(stale).Read
		if staleAlone != nil && !errors.Is(staleAlone, ErrSchema) {
			return nil, staleAlone
		}
		class := RefusalClass(alone)
		if staleAlone == nil || RefusalClass(staleAlone) != class {
			return nil, nil
		}
		explained = explained || class == whole
		excused = append(excused, StoredMisread{Block: id, Kind: class})
	}
	if !explained {
		return nil, nil
	}
	return excused, nil
}

// storedMisreads is what previous, the text before was stored as, already read back otherwise
// (storedMisreadsOf): each block's misread (sameMisread), and the block ids of the lists and list
// items whose spread it read back otherwise (spreadDifferences). before and after hold the lists
// and list items of the two trees by block id, read only where a spread is to be excused.
type storedMisreads struct {
	same          map[sameMisread]bool
	spreads       map[string]bool
	before, after map[string]*Node
}

// storedMisreadsOf is what previous, the text before was stored as, reads back otherwise in,
// judged as RefuseMisreadStored judges a write's text: nothing where before is nil or the parser
// refuses previous, since then nothing was read back to excuse.
func storedMisreadsOf(before *Node, previous string) (*storedMisreads, error) {
	known := &storedMisreads{}
	if before == nil {
		return known, nil
	}
	reading, refused, err := storedMisread(before, previous)
	if err != nil || refused != nil {
		return known, err
	}
	known.same = make(map[sameMisread]bool, len(reading.found))
	for _, stale := range reading.found {
		known.same[sameMisread{stale.id, stale.differs}] = true
	}
	known.spreads = map[string]bool{}
	spreadDifferences(before, reading.read.Tree, 0, len(before.Children)-1, func(node, _ *Node) {
		known.spreads[blockIDOf(node)] = true
	})
	return known, nil
}

// keptSpread reports whether found, a block after's text reads back otherwise in, is a spread an
// edit kept from before (SettleDerived): a list or list item that reads back otherwise in its
// spread alone, whose spread previous already read back otherwise, and that before holds unchanged
// under its id, so the spread is the one previous was stored with and not the write's.
func (m *storedMisreads) keptSpread(found misread, before, after *Node) bool {
	if !found.spreadAlone || !m.spreads[found.id] {
		return false
	}
	if m.before == nil {
		m.before, m.after = derivedByID(before), derivedByID(after)
	}
	previous, held := m.before[found.id]
	return held && previous.Equal(m.after[found.id])
}

// derivedByID is tree's blocks that carry an attribute the renderer derives from more than them
// (derivedAttribute) by block id: its lists, list items and table cells.
func derivedByID(tree *Node) map[string]*Node {
	derived := map[string]*Node{}
	Walk(tree, func(node *Node) bool {
		if id := blockIDOf(node); id != "" && derivedAttribute(node.Type) != "" {
			derived[id] = node
		}
		return true
	})
	return derived
}

// RefuseMisreadWrite refuses a write into a document; see RefuseMisreadDocument. It reads both
// documents back as renderSpanless writes them. An insert checks its write with it once for each
// operation of a batch, and a batch holds as many as a request carries, where a budget of span
// cells for each read-back would let one batch spend two for every operation. An insert writes
// markdown, which carries no span, between document-level blocks, so it changes no table, and every
// table reads back the same way before and after it under the same block ids: a table that reads
// back otherwise does so in both, and NewMisread's filter of misreads before already held, which
// keys on the block id, sets it aside either way. Spanless, a render writes no more than the cells
// the tables hold, and its parse pads the rows a span leaves short on the read-back's own budget
// (ParseRendering).
func RefuseMisreadWrite(before, after *Node) error {
	found, err := newMisread(before, after, 0)
	return refuseMisread(found, nil, err)
}

// ErrRenderingRefused is the refusal of a document whose own rendering no reader of this package
// reads at all. It wraps that reader's own refusal, which names the shape and carries ErrSchema,
// so a caller tests for either with errors.Is.
var ErrRenderingRefused = errors.New("the markdown the document would be stored as cannot be read back")

// ErrMisreadRendering is the refusal of a document whose own rendering reads back as another
// document. A caller that answers such a write with a reason of its own (docs' ask rules) tests
// for this and for ErrRenderingRefused rather than for either sentence.
var ErrMisreadRendering = fmt.Errorf("%w: the markdown the document would be stored as reads back otherwise", ErrSchema)

// refuseMisread is the one place either refusal is built: refused is the rendering's own refusal
// where no reader read it, found how it reads back otherwise where one did, and err anything else,
// which is the error. The refusal the writer gets carries the reason, the write's own text among
// it. Each refusal is logged too, but by what names none of the document's text: a misread by its
// block id and kind, and a refusal by its class (RefusalClass), never by the refusal itself. A
// reason quotes a block's text, and a refusal can quote an attribute's value, either of which can
// be a person's address or other private text, and a log line is read by whoever reads the
// server's logs.
func refuseMisread(found misread, refused, err error) error {
	switch {
	case err != nil:
		return err
	case refused != nil:
		slog.Warn("pmdoc: refused a write whose markdown cannot be read back", "refusal", RefusalClass(refused))
		return fmt.Errorf("%w: %w", ErrRenderingRefused, refused)
	case found.reason == "":
		return nil
	}
	slog.Warn("pmdoc: refused a write whose markdown reads back otherwise", "block", found.id, "difference", found.kind)
	return fmt.Errorf("%w (%s)", ErrMisreadRendering, found.reason)
}

// misread is a block that reads back otherwise: its id, what differs (the same for the same
// misread whatever else changed around it), the kind of difference, which names block types and
// an attribute's name, or a refusal's class (refusedMisread), and never the document's text or an
// attribute's value (StoredMisread), and the reason, how a reader is told, which quotes them. A
// misread of the whole document names no block, and its kind is "doc".
type misread struct {
	id, differs, kind, reason string
	// spreadAlone says the block differs from its read-back in its spread and in no other attribute
	// of its own: the one misread an edit can keep from the text before it (keptSpread).
	spreadAlone bool
}

// misreads lists the blocks under want, a block as written, that read back otherwise in got, in
// document order, each as far down as the two still pair: children pair one to one when both hold
// as many, and otherwise as alignChildren pairs them, a child it cannot pair naming itself.
func misreads(want, got *Node, ignore skip) []misread {
	var found []misread
	collectMisreads(want, got, ignore, &found)
	return found
}

func collectMisreads(want, got *Node, ignore skip, found *[]misread) {
	add := func(id, differs, kind, reason string) {
		*found = append(*found, misread{id: id, differs: differs, kind: kind, reason: reason})
	}
	own := selfDifference(want, got, ignore)
	switch {
	case own.kind != "":
		add(blockIDOf(want), want.Type+" as "+got.Type, want.Type+" as "+got.Type, own.kind)
		return
	case own.attribute != "":
		add(blockIDOf(want), want.Type+" "+own.attribute, want.Type+" "+own.attributeKey, own.attribute)
		(*found)[len(*found)-1].spreadAlone = own.attributeKey == "spread" && !differsBeside(want, got, ignore, "spread")
	}
	if isTextblock(want.Type) {
		if own.text != "" {
			add(blockIDOf(want), want.Type+" text "+own.textDiffers, want.Type+" text", own.text)
		}
		return
	}
	written := writtenChildren(want)
	pairs, unpaired := pairChildren(want.Type, written, got.Children, ignore)
	for _, pair := range pairs {
		collectMisreads(written[pair[0]], got.Children[pair[1]], ignore, found)
	}
	indexes := make([]int, 0, len(unpaired))
	for index := range unpaired {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		child := written[index]
		add(blockIDOf(child), child.Type+" unpaired", child.Type+" unpaired", unpaired[index])
	}
}

// pairChildren pairs written, a parent's children as written, with back's: one to one when both
// hold as many, so a pair that reads back otherwise is looked into, and otherwise as alignChildren
// pairs them, with the children it cannot pair.
func pairChildren(parent string, written, back []*Node, ignore skip) ([][2]int, map[int]string) {
	if len(written) != len(back) {
		return alignChildren(parent, written, back, ignore)
	}
	pairs := make([][2]int, len(written))
	for index := range written {
		pairs[index] = [2]int{index, index}
	}
	return pairs, nil
}

// ownDifference is how a block differs from its read-back apart from its child blocks: its kind,
// its first differing attribute and that attribute's name, and a textblock's text, each "" where
// they agree.
type ownDifference struct {
	kind, attribute, attributeKey, text, textDiffers string
}

func selfDifference(want, got *Node, ignore skip) ownDifference {
	if want.Type != got.Type {
		return ownDifference{kind: blockName(want.Type) + " reads back as " + blockName(got.Type)}
	}
	own := ownDifference{}
	own.attribute, own.attributeKey = attributeReason(want, got, ignore)
	if isTextblock(want.Type) && !ignore.text && !inlineEqual(want.Children, got.Children) {
		wanted, read := TextContent(want), TextContent(got)
		own.text = fmt.Sprintf("%s reads back holding %q, not %q", blockName(want.Type), read, wanted)
		own.textDiffers = differingSpan(wanted, read)
	}
	return own
}

// differingSpan is what wanted and read hold between the text they start and end with alike.
func differingSpan(wanted, read string) string {
	w, r := []rune(wanted), []rune(read)
	prefix := 0
	for prefix < len(w) && prefix < len(r) && w[prefix] == r[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(w)-prefix && suffix < len(r)-prefix && w[len(w)-1-suffix] == r[len(r)-1-suffix] {
		suffix++
	}
	return fmt.Sprintf("%q as %q", string(w[prefix:len(w)-suffix]), string(r[prefix:len(r)-suffix]))
}

// AgreeWithReadBack is doc with its document-level blocks first to last, which a write changed,
// holding what their markdown reads back as where the write left a shape the markdown cannot carry
// but reads back unambiguously, touching only what the write wrote. With halves, an empty
// paragraph without a block id that the renderer does not write goes, as Splice leaves the empty
// halves of the textblock a block replacement lands in. Each list and list item takes the spread
// its markdown reads back with, paired as far down as misreads pairs blocks, so one in a list that
// already reads back otherwise elsewhere takes it too, and so does an item the write left alone
// in a list it made loose, since a list's looseness is its whole markdown's. The exception is one
// before holds unchanged under its id whose spread already read back otherwise there (or when
// before does not read back): that spread is not the write's, and it is kept. Blocks outside
// first to last are doc's own.
func AgreeWithReadBack(before, doc *Node, first, last int, halves bool) *Node {
	out := &Node{Type: doc.Type, Attrs: doc.Attrs, Children: slices.Clone(doc.Children)}
	for index := first; index <= last; index++ {
		out.Children[index] = cloneNode(doc.Children[index])
	}
	if halves {
		kept := make([]*Node, 0, len(out.Children))
		written := writtenChildren(out)
		dropped := 0
		for index, child := range out.Children {
			changed := index >= first && index <= last
			if changed {
				dropUnwrittenHalves(child)
			}
			if changed && !slices.Contains(written, child) && blockIDOf(child) == "" {
				dropped++
				continue
			}
			kept = append(kept, child)
		}
		out.Children = kept
		last -= dropped
	}
	back, err := ReadBack(out)
	if err != nil {
		return out
	}
	held := map[string]*Node{}
	Walk(before, func(node *Node) bool {
		if id := blockIDOf(node); id != "" {
			held[id] = node
		}
		return true
	})
	stale, known := staleSpreads(before)
	spreadDifferences(out, back, first, last, func(node, read *Node) {
		id := blockIDOf(node)
		if previous, ok := held[id]; ok && previous.Equal(node) && (!known || stale[id]) {
			return
		}
		if node.Attrs == nil {
			node.Attrs = Attrs{}
		}
		node.Attrs["spread"] = read.Attrs["spread"]
	})
	return out
}

// staleSpreads is the block ids of before's lists and list items whose spread its markdown already
// reads back otherwise, and false when before does not read back.
func staleSpreads(before *Node) (map[string]bool, bool) {
	back, err := ReadBack(before)
	if err != nil {
		return nil, false
	}
	stale := map[string]bool{}
	spreadDifferences(before, back, 0, len(before.Children)-1, func(node, _ *Node) {
		stale[blockIDOf(node)] = true
	})
	return stale, true
}

// SettleDerived is doc, which an edit batch made from before, with each block in the
// document-level blocks the batch changed taking the attribute the renderer derives from more than
// that block (derivedAttribute) as its markdown reads it back: each list and list item its spread,
// by AgreeWithReadBack's rule, and each table cell its alignment. The renderer writes a list loose
// or tight from what its items hold, not from the spread they carry: an item whose first paragraph
// an edit emptied writes its next block on the marker's line, and reads back tight whatever it
// carried. It writes a table's delimiter row from the alignments its header cells carry, and every
// cell of a column reads back with that column's: a row an insert adds holds cells with none, which
// read back aligned as their column is. Left as the batch made it, such an attribute would be
// stored in a text that reads back with another, which RefuseMisreadStored refuses, and the room's
// tree and its stored text would disagree. A changed block is one before does not hold unchanged
// under its block id. Each one holding a list or a table is read back on its own, as the checks of
// the blocks a write changed read them (ReadBackBlock): a batch pays for no render or parse of the
// whole document, and one that changes no list or table pays for none. The exception is a block
// before holds unchanged under its id whose attribute already read back otherwise in before's
// document-level block holding it, or where that block does not read back: that attribute is not
// the batch's, so it is kept, and RefuseMisreadStored excuses it. A changed block whose markdown is
// refused, or reads back as other blocks, is left as it is for RefuseMisreadStored to judge.
// Neither tree is changed: a block that takes an attribute is a copy, and so is doc's root where
// one does.
func SettleDerived(before, doc *Node) *Node {
	tops := make(map[string]*Node, len(before.Children))
	for _, block := range before.Children {
		if id := blockIDOf(block); id != "" {
			tops[id] = block
		}
	}
	var held *heldDerived
	out := doc
	for index, block := range doc.Children {
		if !holdsDerived(block) {
			continue
		}
		if previous := tops[blockIDOf(block)]; previous == block || previous.Equal(block) {
			continue
		}
		back := readBackAlone(block)
		if back == nil {
			continue
		}
		var settled map[*Node]any
		blockDerivedDifferences(block, back, func(node, read *Node, name string) {
			if held == nil {
				held = newHeldDerived(before)
			}
			if held.kept(node) {
				return
			}
			if settled == nil {
				settled = map[*Node]any{}
			}
			settled[node] = read.Attrs[name]
		})
		if len(settled) == 0 {
			continue
		}
		if out == doc {
			out = &Node{Type: doc.Type, Attrs: doc.Attrs, Children: slices.Clone(doc.Children)}
		}
		out.Children[index] = withDerived(block, settled)
	}
	return out
}

// derivedAttribute is the attribute of a block of kind nodeType that the renderer writes from more
// than that block, "" for a kind it writes no such attribute of: a list's or a list item's spread,
// which the blocks the items hold decide, and a table cell's alignment, which its column's header
// cell decides.
func derivedAttribute(nodeType string) string {
	switch nodeType {
	case "bullet_list", "ordered_list", "list_item":
		return "spread"
	case "table_header", "table_cell":
		return "alignment"
	}
	return ""
}

// derivedValue is node's value of its derivedAttribute name as its markdown writes it: a spread as
// it is, and an alignment as writtenAttrs gives it, so that none - null, or the "none" this parser
// once stored - is one value, as the delimiter row writes it.
func derivedValue(node *Node, name string) any {
	if name == "alignment" {
		return writtenAttrs(node)[name]
	}
	return node.Attrs[name]
}

// holdsDerived reports whether block holds a list or a table, itself included, whose blocks carry
// an attribute the renderer derives from more than them (derivedAttribute).
func holdsDerived(block *Node) bool {
	found := false
	Walk(block, func(node *Node) bool {
		found = isList(node) || node.Type == "table"
		return !found
	})
	return found
}

// readBackAlone is what block, a document-level block, reads back as on its own (readAlone), as
// ReadBackBlock reads it, or nil where the renderer or the parser refuses its markdown or it reads
// back as other blocks.
func readBackAlone(block *Node) *Node {
	alone := readAlone(block)
	markdown, err := renderSpanless(alone)
	if err != nil {
		return nil
	}
	back, err := ParseRendering(markdown)
	if err != nil || len(back.Children) != len(alone.Children) {
		return nil
	}
	if read := back.Children[len(back.Children)-1]; read.Type == block.Type {
		return read
	}
	return nil
}

// withDerived is a copy of node in which each node settled names takes the value it maps to of its
// derivedAttribute.
func withDerived(node *Node, settled map[*Node]any) *Node {
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: node.Text, Marks: cloneMarks(node.Marks)}
	if value, ok := settled[node]; ok {
		if out.Attrs == nil {
			out.Attrs = Attrs{}
		}
		out.Attrs[derivedAttribute(node.Type)] = value
	}
	if len(node.Children) > 0 {
		out.Children = make([]*Node, len(node.Children))
		for index, child := range node.Children {
			out.Children[index] = withDerived(child, settled)
		}
	}
	return out
}

// heldDerived is what SettleDerived reads of before, the tree a batch started from: each block
// carrying a derivedAttribute by block id with the document-level block holding it, and, for each
// such block it has had to read, the ids whose attribute that block's markdown already read back
// otherwise, nil where the block does not read back.
type heldDerived struct {
	derived, blocks map[string]*Node
	stale           map[*Node]map[string]bool
}

func newHeldDerived(before *Node) *heldDerived {
	held := &heldDerived{derived: map[string]*Node{}, blocks: map[string]*Node{}, stale: map[*Node]map[string]bool{}}
	for _, block := range before.Children {
		for id, node := range derivedByID(block) {
			held.derived[id], held.blocks[id] = node, block
		}
	}
	return held
}

// kept reports whether node, a block of a block the batch changed, keeps its derivedAttribute:
// before holds it unchanged under its id, and that attribute already read back otherwise in
// before's block holding it, or that block does not read back.
func (held *heldDerived) kept(node *Node) bool {
	id := blockIDOf(node)
	previous, ok := held.derived[id]
	if !ok || !previous.Equal(node) {
		return false
	}
	block := held.blocks[id]
	stale, read := held.stale[block]
	if !read {
		stale = staleBlockDerived(block)
		held.stale[block] = stale
	}
	return stale == nil || stale[id]
}

// staleBlockDerived is the block ids of the blocks in block, a document-level block, whose
// derivedAttribute its markdown read back on its own (readBackAlone) already reads back otherwise,
// and nil where it does not read back.
func staleBlockDerived(block *Node) map[string]bool {
	back := readBackAlone(block)
	if back == nil {
		return nil
	}
	stale := map[string]bool{}
	blockDerivedDifferences(block, back, func(node, _ *Node, _ string) {
		stale[blockIDOf(node)] = true
	})
	return stale
}

// spreadDifferences pairs doc's document-level blocks first to last with back, doc's read-back, as
// misreads does, and calls visit with each list and list item under them and the block it pairs
// with when the two hold different spreads.
func spreadDifferences(doc, back *Node, first, last int, visit func(node, read *Node)) {
	pairs, _ := pairChildren("doc", writtenChildren(StripAnchorMarks(doc)), back.Children, spreadAside)
	writtenIndex := writtenIndexes(doc)
	for _, pair := range pairs {
		if index := writtenIndex[pair[0]]; index >= first && index <= last {
			blockSpreadDifferences(doc.Children[index], back.Children[pair[1]], visit)
		}
	}
}

// spreadAside compares blocks leaving their spread aside.
var spreadAside = skip{names: map[string]bool{"spread": true}}

func dropUnwrittenHalves(node *Node) {
	if isTextblock(node.Type) {
		return
	}
	written := writtenChildren(node)
	kept := node.Children[:0]
	for _, child := range node.Children {
		if !slices.Contains(written, child) && blockIDOf(child) == "" {
			continue
		}
		dropUnwrittenHalves(child)
		kept = append(kept, child)
	}
	node.Children = kept
}

// blockSpreadDifferences is spreadDifferences for node, which pairs with back, and the blocks it
// holds, visiting node before them.
func blockSpreadDifferences(node, back *Node, visit func(node, read *Node)) {
	blockDerivedDifferences(node, back, func(node, read *Node, name string) {
		if name == "spread" {
			visit(node, read)
		}
	})
}

// blockDerivedDifferences calls visit with node, which pairs with back, and with each block it
// holds, paired as misreads pairs them with the derived attributes left aside (derivedAside), where
// the two hold different values of the block's derivedAttribute, name, as its markdown writes them
// (derivedValue), visiting node before the blocks it holds.
func blockDerivedDifferences(node, back *Node, visit func(node, read *Node, name string)) {
	if node.Type != back.Type || isTextblock(node.Type) {
		return
	}
	if name := derivedAttribute(node.Type); name != "" && derivedValue(node, name) != derivedValue(back, name) {
		visit(node, back, name)
	}
	written := writtenChildren(node)
	pairs, _ := pairChildren(node.Type, written, back.Children, derivedAside)
	for _, pair := range pairs {
		blockDerivedDifferences(written[pair[0]], back.Children[pair[1]], visit)
	}
}

// derivedAside compares blocks leaving aside each attribute the renderer derives from more than
// the block carrying it (derivedAttribute).
var derivedAside = skip{names: map[string]bool{"spread": true, "alignment": true}}

func blockIDOf(node *Node) string {
	id, _ := node.Attrs[BlockIDAttr].(string)
	return id
}

// writtenChildren is node's children as its markdown writes them: an empty paragraph is not
// written, except where the parser reads one as the browser editor does (emptyParagraphFirst), as
// its container's only child, ahead of a list item's first block when that is not a paragraph, and
// ahead of the first block of a typed block whose schema opens with a paragraph
// (opensWithAParagraph), which both parsers fill in again.
func writtenChildren(node *Node) []*Node {
	only := len(node.Children) == 1
	fills := node.Type == "list_item" || opensWithAParagraph(node.Type)
	written := make([]*Node, 0, len(node.Children))
	for index, child := range node.Children {
		readFirst := fills && index == 0 && len(node.Children) > 1 && node.Children[1].Type != "paragraph"
		if only || readFirst || child.Type != "paragraph" || len(child.Children) != 0 {
			written = append(written, child)
		}
	}
	return written
}

// writtenIndexes maps each of doc's written document-level blocks to its index in doc.
func writtenIndexes(doc *Node) []int {
	written := writtenChildren(doc)
	indexes := make([]int, 0, len(written))
	for index, child := range doc.Children {
		if slices.Contains(written, child) {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// resyncWindow bounds how far alignBlocks looks past a block that reads back otherwise for the next
// pair that reads back as written.
const resyncWindow = 8

// alignBlocks pairs doc's written document-level blocks, anchor marks stripped, with back's, what
// its markdown reads back as (alignChildren), naming each block that reads back otherwise by its
// index in doc.
func alignBlocks(doc, back *Node, ignore skip) ([][2]int, map[int]string) {
	indexes := writtenIndexes(doc)
	pairs, unpaired := alignChildren(doc.Type, writtenChildren(doc), back.Children, ignore)
	misread := make(map[int]string, len(unpaired))
	for index, reason := range unpaired {
		misread[indexes[index]] = reason
	}
	return pairs, misread
}

// alignChildren pairs written blocks, a parent's children, with back's in order: each pair reads
// back as written
// (what ignore names aside), and each written block that does not is named by its index with how
// it reads back. Past a block that reads back otherwise the pairing resumes at the nearest pair
// that reads back as written, so two lists that meet are both named and the blocks after them pair
// again.
func alignChildren(parent string, written, back []*Node, ignore skip) ([][2]int, map[int]string) {
	var pairs [][2]int
	misread := map[int]string{}
	i, j := 0, 0
	for i < len(written) && j < len(back) {
		reason := readDifference(written[i], back[j], ignore)
		if reason == "" {
			pairs = append(pairs, [2]int{i, j})
			i, j = i+1, j+1
			continue
		}
		di, dj, found := resync(written, back, i, j, ignore)
		if !found {
			break
		}
		if di == 0 {
			// back holds blocks the document does not: the block before wrote them.
			misread[max(i-1, 0)] = reason
		}
		for k := i; k < i+di; k++ {
			misread[k] = reason
		}
		i, j = i+di, j+dj
	}
	for k := i; k < len(written); k++ {
		reason := blockName(written[k].Type) + " reads back as nothing"
		if j < len(back) {
			reason = readDifference(written[k], back[j], ignore)
		}
		misread[k] = reason
	}
	if i == len(written) && j < len(back) && len(written) > 0 {
		misread[len(written)-1] = endOf(parent) + " reads back as " + blockName(back[j].Type)
	}
	return pairs, misread
}

// resync finds the nearest blocks past written[i] and back[j] that read back as written, nearest by
// how many blocks it skips on both sides.
func resync(written, back []*Node, i, j int, ignore skip) (int, int, bool) {
	for distance := 1; distance <= 2*resyncWindow; distance++ {
		for di := range distance + 1 {
			dj := distance - di
			if di > resyncWindow || dj > resyncWindow || i+di >= len(written) || j+dj >= len(back) {
				continue
			}
			if readDifference(written[i+di], back[j+dj], ignore) == "" {
				return di, dj, true
			}
		}
	}
	return 0, 0, false
}

// attributeDrift is the attributes doc's blocks read back with other values where they read back
// holding the same blocks and text - but a task item's checked, which is its own item's: a task
// item emptied of its text is written as a plain empty item and reads back with checked none, and
// NewMisread tells a stale one from a new one by its block id, so the attribute drifting would hide
// every other task item emptied.
func attributeDrift(doc, back *Node) map[string]bool {
	drift := map[string]bool{}
	pairs, _ := alignBlocks(doc, back, skip{all: true})
	written := writtenChildren(doc)
	for _, pair := range pairs {
		collectDrift(written[pair[0]], back.Children[pair[1]], drift)
	}
	delete(drift, "checked")
	return drift
}

func collectDrift(node, back *Node, drift map[string]bool) {
	for {
		key, _, _, differs := attributeDifference(node, back, skip{names: drift})
		if !differs {
			break
		}
		drift[key] = true
	}
	children := back.Children
	if isTextblock(node.Type) {
		return
	}
	for index, child := range writtenChildren(node) {
		if index < len(children) {
			collectDrift(child, children[index], drift)
		}
	}
}

// readDifference names how got, what a block's markdown reads back as, differs from want, the
// block as written: its kind, its blocks, its text or an attribute; "" when it reads back as
// written. Attributes ignore names are not compared.
func readDifference(want, got *Node, ignore skip) string {
	own := selfDifference(want, got, ignore)
	if own.kind != "" {
		return own.kind
	}
	// A list's own attributes follow its items, which name a list the next one joins; any other
	// block's come first, which names a task item that reads back plain rather than its text.
	list := want.Type == "bullet_list" || want.Type == "ordered_list"
	if !list && own.attribute != "" {
		return own.attribute
	}
	if isTextblock(want.Type) {
		return own.text
	}
	written := writtenChildren(want)
	for index := range max(len(written), len(got.Children)) {
		switch {
		case index >= len(written):
			if list && got.Children[index].Type == "list_item" {
				// A list of the same kind right after it continues it.
				return blockName(want.Type) + " beside another of its kind reads back as one list"
			}
			return endOf(want.Type) + " reads back as " + blockName(got.Children[index].Type)
		case index >= len(got.Children):
			return blockName(written[index].Type) + " reads back as nothing"
		}
		if reason := readDifference(written[index], got.Children[index], ignore); reason != "" {
			return reason
		}
	}
	return own.attribute
}

// attributeReason names the first attribute want and got hold with different values, and the
// attribute's name, or is "" twice.
func attributeReason(want, got *Node, ignore skip) (reason, key string) {
	key, wanted, read, differs := attributeDifference(want, got, ignore)
	switch {
	case !differs:
		return "", ""
	case want.Type == "list_item" && key == "checked" && read == nil:
		return "a task item reads back as a plain list item", key
	default:
		return fmt.Sprintf("%s reads back with %s %v, not %v", blockName(want.Type), key, orNone(read), orNone(wanted)), key
	}
}

func orNone(value any) any {
	if value == nil {
		return "none"
	}
	return value
}

// attributeDifference is the first attribute, by name, that want and got hold with different
// values, block ids, server-owned attributes (tokenAttrs) and those ignore names aside.
func attributeDifference(want, got *Node, ignore skip) (string, any, any, bool) {
	if ignore.all {
		return "", nil, nil, false
	}
	wanted, read := writtenAttrs(want), tokenAttrs(got.Type, got.Attrs)
	// The capacity is a hint that only moves where a growth happens, so it names the written
	// attributes alone rather than their sum with the read ones: a sum of two lengths is what
	// CodeQL's go/allocation-size-overflow refuses, and the same keys are appended in the same
	// order either way.
	keys := make([]string, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	for key := range read {
		if _, ok := wanted[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !ignore.has(key) && !reflect.DeepEqual(wanted[key], read[key]) {
			return key, wanted[key], read[key], true
		}
	}
	return "", nil, nil, false
}

// differsBeside reports whether want and got hold different values of an attribute other than
// name, those ignore names aside as well.
func differsBeside(want, got *Node, ignore skip, name string) bool {
	names := map[string]bool{name: true}
	maps.Copy(names, ignore.names)
	_, _, _, differs := attributeDifference(want, got, skip{all: ignore.all, names: names})
	return differs
}

// writtenAttrs is the attributes node's markdown writes: tokenAttrs, with a table column that has
// no alignment - null, or the "none" this parser once stored - written unaligned (tableAlignment),
// which reads back with none.
func writtenAttrs(node *Node) Attrs {
	attrs := tokenAttrs(node.Type, node.Attrs)
	if node.Type == "table_header" || node.Type == "table_cell" {
		switch attrs["alignment"] {
		case "left", "center", "right":
		default:
			delete(attrs, "alignment")
		}
	}
	return attrs
}

// inlineEqual compares a textblock's inline content: each node's kind, text, attributes and marks,
// the marks in any order and without the attributes the rendering does not write
// (sameWrittenMarks), which a read-back holds whatever the written tree did.
func inlineEqual(want, got []*Node) bool {
	if len(want) != len(got) {
		return false
	}
	for index := range want {
		w, g := want[index], got[index]
		if w.Type != g.Type || w.Text != g.Text || !reflect.DeepEqual(tokenAttrs(w.Type, w.Attrs), tokenAttrs(g.Type, g.Attrs)) || !sameWrittenMarks(sortedMarks(w.Marks), sortedMarks(g.Marks)) || !inlineEqual(w.Children, g.Children) {
			return false
		}
	}
	return true
}

func sortedMarks(marks []Mark) []Mark {
	sorted := slices.Clone(marks)
	sortMarks(sorted)
	return sorted
}
