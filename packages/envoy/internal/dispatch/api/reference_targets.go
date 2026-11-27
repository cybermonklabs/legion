package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// referenceTargetsMax is how many references one batched read may name. Its value is
// contracts.ReferenceTargetsMax, the literal REFERENCE_TARGETS_MAX in packages/contracts, which
// the client batches by too, and whose comment gives the measurement it is set above: the 247
// distinct references that the issue the new issue page was designed on and its four largest
// documents name. So the count of references splits no such load into a second request, and a
// set at the cap costs one statement per node kind (refs.TargetNames), not one per reference.
// The ids that the set writes short cost one statement per kind of item (refs.ResolvePrefixes),
// except the blocks of a document, which cost two: one for the blocks of the latest version of
// each document, and one for the blocks of the versions that the references pin.
//
// The other bound on a batch is the request line itself, which this server sets no limit of its
// own on. A proxy in front of the server can set one; 16 KB is one known such limit, an AWS load
// balancer's. The client keeps each request's query string within a budget of its own,
// REFERENCE_TARGETS_MAX_QUERY_CHARS in packages/dispatch/web/src/api/client.ts, whose own comment
// gives its value and the reason for it. Those 247 references encode to about 17 KB of query
// string, so that budget splits such a load into two requests where the cap splits none.
const referenceTargetsMax = contracts.ReferenceTargetsMax

// getReferenceTargets resolves many dispatch:// references in one read. For each ?ref= it answers
// what that reference points at: its kind, the words a reader is shown for its target in place of
// the target's text (refs.TargetNames, the same words GET /api/v1/references shows beside an edge
// at that node), and whether the target is gone. The answer holds one member per ?ref= in the
// request's order, each echoing the reference as the request wrote it, so a caller reads the
// answer by index or by reference; a reference named twice is answered twice.
//
// It is a GET with one repeated parameter, not a POST of a list: a POST of the page goes through
// the write store with an Idempotency-Key (packages/dispatch/web/src/features/writes/), so a POST
// read would be a second write path, and a GET is what a browser cache, a conditional read and the
// page's request budget are counted by. GET /api/v1/references reads one node's edges and stays
// as it is; this read names no edges at all.
//
// A reference the grammar cannot read is refused by name (parseRequestRef, as the per-reference
// route refuses its own), and so is a set past referenceTargetsMax, which is refused before
// anything is parsed. A reference that reads but names nothing is answered, gone: a chip whose
// target was deleted still has to draw.
//
// A member naming a comment or a message, or a block of one, carries its post's marks as every
// read of the post does (refs.TargetName): edited_at where its author edited it, and deleted_at
// and deleted_by where its author deleted it. A deleted post keeps its place, so its member is
// not gone and is named "", the body the delete cleared; a block of it is gone, with no name and
// no tree, as a block read of it is refused naming the deletion (itemBlockNotFound): the delete
// indexes the empty body, which takes every block out of the post (planEditedItem, in
// clearDeletedPostBody).
//
// A reference whose item id or block id is written short is read as GET /api/v1/references reads
// its ?to= (wholeReferences): as the one item that carries the prefix among the items of the
// owner that the reference names. A prefix that nothing carries leaves the reference as written,
// which then names nothing and is answered gone. A prefix that two items or more carry is
// answered gone too, with ambiguous true, and every other member of the read is answered as it
// stands. GET /api/v1/references refuses such a prefix with a 409, but one reference must not
// refuse a whole batch: the page reads its batches together, so one refusal would blank the
// name of every reference in the batch.
//
// It answers for the caller's reader (readerOf), as every read of the graph does: a target the
// reader may not see is answered gone, as a target nothing addresses is (refs.TargetNames), and a
// short id is resolved among the items the reader may see alone (refs.ResolvePrefixes), so an
// item the reader may not see makes no short id ambiguous and names none.
//
// A caller that sent `Dispatch-Trees: 1` is given each block's whole text as its text_tree
// (blockTrees). The trees are resolved before the ETag is made, because the ETag hashes them: a
// block's name stops at its stored excerpt, so a change past it moves the tree and not the name.
func (s *server) getReferenceTargets(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	targets, err := s.referenceTargets(r, who, trees)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees.resolve()
	etag := referenceTargetsETag(targets)
	if answerNotModified(w, r, etag) {
		return
	}
	WriteJSON(w, http.StatusOK, model.ReferenceTargets{Targets: targets})
}

// referenceTargets reads every ?ref= of the request, writes each id that a reference writes
// short as its whole id in one batch (wholeReferences), and then resolves the references in one
// query per node kind. The kind of each answer is the kind the reference itself names, which is
// the kind its node resolves to (refs.TargetOf stores a block of an ask, a comment or a message
// under its item's block kind, and those resolve to nodes of kind block), so a gone target is
// answered under its kind too. A reference whose short id two items or more carry names no one
// target, so it is looked up as nothing (the zero refs.Target, which has no name and no tree),
// and is answered gone and ambiguous.
//
// A reference is read past the whitespace around it, and echoed as the request wrote it, that
// whitespace and an id written short included: the answer is read by index or by reference, and
// a caller matching the members against the strings it sent finds none of them under a
// reference this read had rewritten.
//
// who is the reader the read answers for, whose visibility predicates both the short ids and the
// names are read under. trees is the request's own reader, nil for a caller that asked for no
// tree, which then reads no block's text at all.
func (s *server) referenceTargets(r *http.Request, who model.Actor, trees *displayTreeReader) ([]model.ReferenceTarget, error) {
	raw := r.URL.Query()["ref"]
	if len(raw) == 0 {
		return nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "ref is required, once per reference to resolve")
	}
	if len(raw) > referenceTargetsMax {
		return nil, countExceededError("CAP_EXCEEDED", "ref", len(raw), referenceTargetsMax)
	}
	parsed := make([]text.Ref, len(raw))
	for index, value := range raw {
		ref, err := parseRequestRef(strings.TrimSpace(value), s.deps.Origins)
		if err != nil {
			return nil, err
		}
		parsed[index] = ref
	}
	ambiguous, err := s.wholeReferences(r.Context(), who, parsed)
	if err != nil {
		return nil, err
	}
	answers := make([]model.ReferenceTarget, len(raw))
	named := make([]refs.Target, len(raw))
	for index, ref := range parsed {
		answers[index] = model.ReferenceTarget{Ref: raw[index], Kind: ref.Kind, Gone: true}
		if ambiguous != nil && ambiguous[index] {
			answers[index].Ambiguous = true
			continue
		}
		// Every reference parseRequestRef answers has a target: it refuses a party, and the
		// grammar reads no external URL as one.
		target, _ := refs.TargetOf(ref)
		named[index] = target
	}
	names, err := refs.TargetNames(r.Context(), s.deps.Store.Pool, who, named, s.deps.Origins.Canonical())
	if err != nil {
		return nil, err
	}
	for index, target := range named {
		if name, found := names[target]; found {
			answer := &answers[index]
			answer.Name, answer.Gone = name.Name, name.Gone
			answer.EditedAt, answer.DeletedAt, answer.DeletedBy = name.EditedAt, name.DeletedAt, name.DeletedBy
		}
	}
	if err := s.blockTrees(r.Context(), answers, named, trees); err != nil {
		return nil, err
	}
	return answers, nil
}

// wholeReferences writes, in place, the whole id into each of references whose item id or block
// id is written short, by the rules that GET /api/v1/references reads one ?to= by
// (wholeReference). The owner that the prefix is resolved within is read from the reference
// (refs.PrefixScope), and only an id that text.IsIDPrefix accepts is resolved. The one item that
// carries the prefix gives the reference its whole id, and a prefix that nothing carries leaves
// the reference as written. A prefix that two items or more carry leaves the reference as
// written too, and ambiguous is true at that reference's index, so that the caller answers that
// reference alone as ambiguous and every other reference as it stands. ambiguous is nil when no
// reference is ambiguous.
//
// Every short id of the set is resolved in one batch (refs.ResolvePrefixes), which reads each
// distinct id once, in one statement per kind of item and two for the blocks of a document, so
// a set with no short id costs no statement. The batch reads for who, so an item who may not see
// is no candidate for any prefix, as GET /api/v1/references reads one prefix for its reader
// (wholeReference).
func (s *server) wholeReferences(ctx context.Context, who model.Actor, references []text.Ref) (ambiguous []bool, err error) {
	short := make([]refs.ShortID, len(references))
	var asked []refs.ShortID
	for index, ref := range references {
		block := ref.Kind == "block" && ref.Item == ""
		kind, scope, written, ok := refs.PrefixScope(text.Prefix{Ref: ref, Block: block})
		if ok && text.IsIDPrefix(written) {
			short[index] = refs.ShortID{Kind: kind, Scope: scope, Prefix: written}
			asked = append(asked, short[index])
		}
	}
	if len(asked) == 0 {
		return nil, nil
	}
	whole, many, err := refs.ResolvePrefixes(ctx, s.deps.Store.Pool, &who, asked, s.deps.Origins.Canonical())
	if err != nil {
		return nil, err
	}
	for index, id := range short {
		// A reference that writes no short id holds the zero ShortID, which neither answer ever
		// holds, since no prefix is empty.
		if many[id] {
			if ambiguous == nil {
				ambiguous = make([]bool, len(references))
			}
			ambiguous[index] = true
			continue
		}
		candidate, found := whole[id]
		if !found {
			continue
		}
		if ref := &references[index]; ref.Kind == "block" && ref.Item == "" {
			ref.Block = candidate.ID
		} else {
			ref.ID = candidate.ID
		}
	}
	return ambiguous, nil
}

// blockTrees registers, for each member that names a block something answers to, that block's
// whole text with trees as the member's text_tree: the page draws a plan task, a requirement or a
// check it reaches by reference from it, where the member's name is the block's stored excerpt,
// cut at refs.BlockExcerptRunes and holding a typed block's opening line. The texts are read in
// one statement per kind of block the request names (refs.BlockTexts), beside the one per kind
// the names cost, and no document is loaded for them: a block of a document, a comment or an ask
// has its text read from the block index, and a block of a message has its text split again from
// its message's body, since no table records it.
//
// A block is read only for a member that is not gone, and a member is not gone only where
// refs.TargetNames named its target for the request's reader. refs.BlockTexts binds no
// visibility predicate of its own, so that test is what keeps the text of a block the reader may
// not see out of the answer: such a block is gone, and no text of it is read.
//
// answers must have stopped growing: what is registered is a pointer into each member.
func (s *server) blockTrees(ctx context.Context, answers []model.ReferenceTarget, named []refs.Target, trees *displayTreeReader) error {
	if trees == nil {
		return nil
	}
	var blocks []refs.Target
	seen := map[refs.Target]struct{}{}
	for index, target := range named {
		if answers[index].Kind != "block" || answers[index].Gone {
			continue
		}
		if _, duplicate := seen[target]; !duplicate {
			seen[target] = struct{}{}
			blocks = append(blocks, target)
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	texts, err := refs.BlockTexts(ctx, s.deps.Store.Pool, blocks)
	if err != nil {
		return err
	}
	contents := make(map[refs.Target]string, len(texts))
	for target, whole := range texts {
		contents[target] = blockTextContent(whole, target.Kind == "block")
	}
	for index, target := range named {
		if answers[index].Kind == "block" && !answers[index].Gone {
			trees.text(&answers[index].TextTree, contents[target])
		}
	}
	return nil
}

// blockTextContent is the markdown a block's text_tree is read from, given the block's whole
// text as the index holds it and whether the block is a document's (the target kind "block")
// rather than a comment's, a message's or an ask's. A typed block's text opens with its directive
// line and closes with its fence, which are no part of what the block says: its tree is of its
// own content, the tree GET /api/v1/issues/{key}/coverage and /results answer for the same block
// (typedBlocksIn). Every other block's text is its content already.
//
// The content is cut from the text itself and is never rendered again. A parse gives each typed
// block that has no id a new random one (pmdoc.EnsureBlockIDs), and a rendering writes that id
// into the block's opening line. So for a block that holds a typed block written without an id, a
// content rendered from a parse would change on every read, and its tree and the ETag with it.
//
// The cut drops the first line and the last one, the closing fence, and reads the lines between
// them by the block's kind. A document's block can stand in a list item or in a footnote
// definition. Its text starts at the directive, so there it carries the container's indentation
// on every line but its first, and left in, that indentation would make the content after a
// marker such as `10. ` an indented code block. So the cut takes the closing fence's indentation
// out of each line of a document's block. A blank line, which the renderer can write without
// that indentation, is kept as it stands, and a text with a line that is not blank and does not
// carry that indentation is answered as it stands. A block of a comment, a message or an ask is a
// top-level block of its item's own text, stored from the start of its line, so its opening line
// can stand up to three spaces in. The cut takes that indentation off the opening line before it
// reads the directive, and off each later line as the item's own parse takes it (columnsOff). An
// opening line that stands a tab or four spaces in opens no typed block, so its text is answered
// as it stands. With an opener at column 0 every line is as written: under a fence two spaces in,
// a line four spaces in is an indented code block, as the item reads it, and a line at column 0
// stays at column 0. Wherever the opener stands, a fence fewer than four columns in closes the
// block.
//
// Of either kind, the blank lines at the start and at the end of the content are no part of it,
// so a block that holds nothing has no content; nor are the lines of only spaces and tabs, one or
// several, that end a code block which ends the content, an indented block or a fenced one its
// own fence does not close, wherever the opening line stands, though the item's own parse holds
// them in the code. A first line `---` is written `***`, the same rule: at the start of a
// document of its own, `---` opens front matter, which it does not in the block.
//
// The text is cut only where its last line is a fence and its first line, the lines as the kind
// reads them and that fence read back as one typed block (pmdoc.ParseRendering): a document's
// block with the fence's indentation taken out of the fence too, an item's block as it stands.
// The parse checks the shape alone; nothing of the content comes from it. A text that fails that
// check is answered as it stands. So is an item's block whose fence stands four spaces or a tab
// in: its lines start at column 0, where such a fence closes nothing, so the lines above it are no
// typed block. So is a typed block in a quote, whose lines after the first carry the quote's `>`:
// its last line is no fence, and its tree is the tree of its whole stored text, the line that
// opens the typed block included.
//
// In the markdown a document stores, which gives every typed block its id, the cut reads back as
// a rendering of the block's content does (blockContent), and it is the same bytes except in
// lines that read as nothing or as the same text: an escape that keeps a line from closing the
// block, the blank line of an empty code block, the blank lines a rendering writes for the empty
// paragraph an ask holds ahead of a list. One shape reads otherwise. In a footnote definition, a
// blank line of code is written without the definition's indentation, and the cut cannot tell
// those columns from the code's, so such a line can keep other spaces than the document reads in
// it.
func blockTextContent(whole string, document bool) string {
	opening, rest, _ := strings.Cut(whole, "\n")
	columns := 0
	if !document {
		columns = len(opening) - len(strings.TrimLeft(opening, " "))
	}
	if columns > 3 {
		return whole
	}
	if _, _, typed := pmdoc.ParseTypedDirectiveOpen(strings.TrimRight(opening[columns:], "\r")); !typed {
		return whole
	}
	inner, closing := "", rest
	if end := strings.LastIndexByte(rest, '\n'); end >= 0 {
		inner, closing = rest[:end+1], rest[end+1:]
	}
	fence := strings.TrimLeft(closing, " \t")
	if colons := strings.TrimRight(fence, " \t\r"); colons == "" || strings.Trim(colons, ":") != "" {
		return whole
	}
	lines, standing := columnsOff(inner, columns), whole
	if indent := closing[:len(closing)-len(fence)]; document && indent != "" {
		outdent, cut := outdented(inner, indent)
		if !cut {
			return whole
		}
		lines, standing = outdent, opening+"\n"+outdent+fence
	}
	tree, err := pmdoc.ParseRendering(standing)
	if err != nil || len(tree.Children) != 1 || !pmdoc.IsTypedBlock(tree.Children[0].Type) {
		return whole
	}
	content := withoutBlankEnds(lines)
	if first, _, _ := strings.Cut(content, "\n"); strings.TrimRight(first, " \t") == "---" {
		return "***" + content[len("---"):]
	}
	return content
}

// outdented is lines, each ending in its line feed, with indent taken from the start of each
// line that carries it. A blank line, one of no more than spaces and tabs, that does not carry
// indent is kept as it stands. cut is false when a line that is not blank does not carry indent.
func outdented(lines, indent string) (text string, cut bool) {
	var written strings.Builder
	written.Grow(len(lines))
	for line := range strings.Lines(lines) {
		rest, carried := strings.CutPrefix(line, indent)
		if !carried && strings.Trim(line, " \t\r\n") != "" {
			return "", false
		}
		written.WriteString(rest)
	}
	return written.String(), true
}

// columnsOff is lines with columns columns of indentation taken off each line, as the item's own
// parse reads a typed block whose opening line stands columns spaces in
// (pmdoc.typedDirectiveParser.Continue); with columns 0 it is lines. Otherwise each line's columns
// are counted as stored, a space one column and a tab reaching the next multiple of four, and the
// line is written again: the spaces and tabs it starts with as spaces, as many as the columns they
// reach less columns and never fewer than none; then, while what follows starts with a container
// marker (containerMarker), the marker as it stands and the spaces and tabs after it as spaces, as
// many as the columns they span; then the rest of the line as it stands. But a run after a marker
// that reaches the line's end is written as spaces only after a `>` taken for a quote's: one the
// line starts with, three columns in at most once columns are taken off, or one that follows such
// a `>` with at most four columns between them. After any other marker that run is kept as it
// stands, and the walk stops there.
//
// The item's own parse measures every tab from the column it stands at, after a list marker too
// (pmdoc's listItemColumns) and after `>` (goldmark's blockquote parser, which takes util.TabWidth
// from the line's offset). Kept as a tab, a tab past columns would reach another stop once the
// line starts columns earlier, and the content would read with another block structure: under an
// opening line two spaces in, `-\tItem.` holds its text one column past the marker, and at column
// 0 three, so a second paragraph two columns in would fall out of the list item. Written as
// spaces, a run reaches the column it reaches in the stored text, less the columns taken off the
// line's start, so it moves nothing after it. A wide run does not end the walk: under an opening
// line one space in, the lines ` > 1. a` and ` >\t\t-\t\tb` leave five columns after the quote's
// own, the list item in the quote takes three of them, and the `-` two columns into that item
// opens a list. A run to the line's end after a list marker decides no column, since such an item
// takes its content a column past the marker, and a marker that opens no list is a paragraph's
// text, whose line that run written as spaces would end in a hard break. Any other `>` can be such
// text too, on a lazy line or after a marker that opens no list. A `>` taken for a quote's opens or
// continues one outside code and HTML, since a quote can interrupt a paragraph: its line is blank
// in the quote, and in code the quote holds, the run's columns past the code's are the code's text.
//
// So the content reads as the item's own parse reads the block but in three ways, the block
// structure the same. A tab past the opener's columns that the item keeps as text, in code or
// after a marker that opens no container, is written as spaces, so that text holds spaces where
// the item's holds a tab. A `>` four columns or more into a list item (`10. >`, `-   >`,
// `> 1.  >`) is a quote's that the walk does not take for one, so on a line of only spaces and
// tabs in code that quote holds, the run kept as it stands starts columns earlier and a tab in it
// can reach another stop: that line holds up to three columns more or fewer of whitespace than
// the item's. And a code span that crosses lines, one of whose ends a run written as spaces
// decides, can differ from the item's by one space or line ending at each end, either way:
// CommonMark strips those two ends from a span whose text begins and ends with a space and is not
// all spaces, and a tab is no space, so where the item's span keeps its ends the content's can
// lose them, and where the item's loses them the content's can keep them.
func columnsOff(lines string, columns int) string {
	if columns == 0 {
		return lines
	}
	var written strings.Builder
	written.Grow(len(lines))
	for line := range strings.Lines(lines) {
		at, column := blankRun(line, 0, 0)
		writeSpaces(&written, column-columns)
		quote := column-columns <= 3
		for {
			marker := containerMarker(line[at:])
			if marker == 0 {
				break
			}
			quote = quote && line[at] == '>'
			written.WriteString(line[at : at+marker])
			at, column = at+marker, column+marker
			end, reached := blankRun(line, at, column)
			if !quote && (end == len(line) || line[end] == '\n' || line[end] == '\r') {
				break
			}
			quote = quote && reached-column <= 4
			writeSpaces(&written, reached-column)
			at, column = end, reached
		}
		written.WriteString(line[at:])
	}
	return written.String()
}

// blankRun is where the run of spaces and tabs in line from at ends, and the column it reaches
// from column, counted as columnsOff counts a line's columns.
func blankRun(line string, at, column int) (end, reached int) {
	for ; at < len(line) && (line[at] == ' ' || line[at] == '\t'); at++ {
		if line[at] == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
	}
	return at, column
}

// writeSpaces writes count spaces, and none when count is below one.
func writeSpaces(written *strings.Builder, count int) {
	for ; count > 0; count-- {
		written.WriteByte(' ')
	}
}

// containerMarker is the length of the container marker rest starts with, or 0 when it starts
// with none: a quote's `>`, or a list item's `-`, `*`, `+` or one to nine digits and `.` or `)`
// followed by a space, a tab or the line's end.
func containerMarker(rest string) int {
	if rest == "" {
		return 0
	}
	length := 1
	switch rest[0] {
	case '>':
		return 1
	case '-', '*', '+':
	default:
		digits := 0
		for digits < len(rest) && digits < 9 && '0' <= rest[digits] && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits == len(rest) || (rest[digits] != '.' && rest[digits] != ')') {
			return 0
		}
		length = digits + 1
	}
	if length < len(rest) && rest[length] != ' ' && rest[length] != '\t' && rest[length] != '\r' && rest[length] != '\n' {
		return 0
	}
	return length
}

// withoutBlankEnds is lines from the first that holds more than whitespace to the line ending
// of the last that does, and "" when none does. The lines it drops at the end include the lines
// of only spaces and tabs, one or several, that end a code block which ends lines, an indented
// block or a fenced one its own fence does not close, which the item's own parse keeps in the code.
func withoutBlankEnds(lines string) string {
	from, to, at := -1, 0, 0
	for line := range strings.Lines(lines) {
		if strings.Trim(line, " \t\r\n") != "" {
			if from < 0 {
				from = at
			}
			to = at + len(line)
		}
		at += len(line)
	}
	if from < 0 {
		return ""
	}
	return lines[from:to]
}

// referenceTargetsETag hashes the answer itself, every member in order: its reference, its kind,
// whether it is gone, whether it is ambiguous, its post's marks, its name and its tree, each of
// the last two after its length so no name's own line endings can read as another member's
// fields. The answer depends on nothing else, so a set whose members all stand as they did is
// answered 304, and a member renamed, deleted or restored changes the tag; for a caller given
// trees, a block whose text changed anywhere changes the tag. An edit of a post changes the tag
// of every member naming it or a block of it, the block the edit left as it was included, and so
// does its delete. A member gone because its short id is ambiguous and a member gone because its
// target is deleted never share a tag, so a short id that stops being ambiguous, or starts to be,
// changes the tag too. A caller given trees and one given none are answered different bodies
// wherever a block is named, and different tags with them.
func referenceTargetsETag(targets []model.ReferenceTarget) string {
	hash := sha256.New()
	for _, target := range targets {
		_, _ = fmt.Fprintf(hash, "%s %s %t %t %s %s %s %d %s %d %s\n", target.Ref, target.Kind, target.Gone,
			target.Ambiguous, etagTime(target.EditedAt), etagTime(target.DeletedAt), etagActor(target.DeletedBy),
			len(target.Name), target.Name, len(target.TextTree), target.TextTree)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

// etagTime is at as referenceTargetsETag hashes it: "-" for no time, which no time's own form
// reads as.
func etagTime(at *time.Time) string {
	if at == nil {
		return "-"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// etagActor is actor as referenceTargetsETag hashes it: "-" for no actor, else its kind and id.
func etagActor(actor *model.Actor) string {
	if actor == nil {
		return "-"
	}
	return actor.Kind + "/" + actor.ID
}
