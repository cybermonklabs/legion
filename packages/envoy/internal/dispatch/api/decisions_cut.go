package api

import (
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// decisionCut is one decision as a document's markdown holds it: the text the decisions view shows
// for it (decisionSections), that text's sum (decisionTextSum), where its heading's line starts in
// the markdown, and whether it stands alone (decisionCuts). A keep holds the sum of the text the
// view showed the person who kept the decision, and a keep carries across a renderer change only
// where the cut of the version it names has that sum (markKeptAcrossRenderers), so the cut must be
// exactly the view's text.
type decisionCut struct {
	text  string
	sum   string
	start int
	alone bool
}

// decisionCuts is a document's markdown cut into its decisions, each one's text and the sum of
// that text. Each cut is the text the decisions view shows for its decision (decisionSections),
// and a decision the view reads whose heading a list item, a quote or a footnote definition holds
// has none. It cuts by the rule the view's block walk cuts the document that stands by: a `##`
// heading outside `## Replaced` and a `###` heading under it opens a decision, and a heading of
// its own level or higher closes it wherever it stands - `#` included, which opens none, and a
// heading inside a typed block, a list item, a quote or a footnote definition included - and a
// deeper heading is text of the decision it stands in.
//
// A stored version holds no block id, so a decision is keyed by its heading and by how many
// decisions before it this cut gives that heading (sectionKey): a document that carries one
// heading twice, which a replaced decision made again does, is two decisions here as it is two in
// the view, rather than one the sum cannot name. Its text ends where its last block ends, as the
// view's does, so the same section of two versions sums alike whatever follows it; the text is a
// slice of markdown, so the cut copies nothing.
//
// The cut takes the document's headings and typed blocks from pmdoc's parse of the markdown as a
// stored rendering (pmdoc.RenderingOutline), not from its lines, so it places each where the
// parser does: a heading inside fenced code is no heading, and a list item's fenced code or typed
// block, whose closing line carries the item's indentation, opens no code and closes no typed
// block of the document's. A heading inside a typed block can open a decision. A heading inside a
// list item, a quote or a footnote definition opens none, so the decision the view opens there
// has no cut, but it closes the open one and sets whether the headings after it stand under
// `## Replaced`, as the view's walk reads it. Each heading's level and title are read from its
// line, from its `#` on (pmdoc.OutlineHeading.Start, markdownHeading), as headingOf reads a heading
// block's, and a decision's text starts where its heading's line does
// (pmdoc.OutlineHeading.LineStart).
//
// The view's text of a decision runs from its heading to the end of the furthest block between
// that heading and the heading that closes it, the blocks inside typed blocks and every other
// container included. The renderer writes one blank line between two blocks at the document's
// level or in a typed block the outline holds, one line feed between a typed block's last block
// and its closing line, and one after the document's last block, and an empty paragraph, which
// writes no text, ends where it starts, right after the blank line before it
// (pmdoc.RenderWithBlockOffsets). So the section's last block ends where the separator the
// renderer writes ahead of the line that closes the section starts: a cut's text runs from its
// heading's line to the start of the two line feeds, a line's end and a blank line, ahead of the
// line of the heading that closes it, or of the one line feed that ends the markdown, so it holds
// the separator before each empty paragraph that ends the section, as the view's text does. Where
// fewer line feeds stand there (the heading that closes it is a typed block's first block, one line
// feed after the block's opening line, which the rule below holds whole, or the markdown is not the
// renderer's), the text ends where they start. Where that text then ends with the closing line of a
// typed block that holds its heading, the section's last block stands inside that typed block, so
// the text ends at the line feed ahead of that closing line, again holding the separator before
// each empty paragraph that ends the section there; the typed blocks that hold the heading are read
// so from the outermost in, since the outermost one's closing line comes last. Where an empty
// paragraph right after that closing line ends the section, the text holds that closing line. Each
// typed block that opens inside the cut, before the line of the heading that closes it, is a block
// of the section, so the text runs at least to the end of that block's closing line: a section that
// a heading inside a typed block closes holds that typed block whole. The outermost list, quote or
// footnote definition that holds the heading that closes the decision opens inside the cut too,
// before that heading, so it is a block of the section and the text runs at least to its end: a
// section that a heading inside a list item, a quote or a footnote definition closes holds that
// whole list, quote or footnote definition. The outline gives where each typed block's closing line
// ends (pmdoc.OutlineTypedBlock.End), where the outermost list, quote or footnote definition that
// holds each heading ends (pmdoc.OutlineHeading.ContainerEnd), the innermost typed block that holds
// each heading (pmdoc.OutlineHeading.Within), and the innermost one that holds each typed block
// (pmdoc.OutlineTypedBlock.Within).
//
// A cut stands alone (decisionCut.alone) unless it runs past the closing line of a typed block
// that holds its heading, and only a cut that stands alone then and now is read alone
// (pmdoc.FragmentReadsAs); any other is compared by its sum alone. Such a cut holds that closing
// line without the block's opening line, a fence its text read alone does not read as that
// block's, so two trees can read as one. Typed blocks nest, so the innermost one decides: a cut
// that ends at or before that block's closing line ends before the closing line of every typed
// block that holds it. Such a cut, as a cut whose heading no typed block holds, lies wholly inside
// each typed block that holds its heading and holds whole each typed block that opens inside it,
// so it holds no typed block's opening or closing line without the other, and it holds whole each
// list, quote or footnote definition it touches, so it loses none of their prefixes
// (pmdoc.FragmentReadsAs). A typed block writes its lines with no prefix, so such a cut reads
// alone as it stands.
//
// A document the parser refuses gives no cuts, and read false: as the document that stands, every
// decision then has no cut, and as a stored version, it is one the view could not read
// (storedDecisions.refused).
//
// Each call parses the whole markdown, 8.5 to 13.4 ms for a document of 25 to 43 KB, so a view
// cuts a markdown only where the process keeps no cut of it (documentCuts): the document as it
// stands (readDecisions) and each stored version it reads (scanStoredDecisions). After the parse
// the cut reads each heading and each typed block once, and for each decision the typed blocks
// that hold its heading.
func decisionCuts(markdown string) (cuts map[sectionKey]decisionCut, read bool) {
	if decisionCutsObserver != nil {
		decisionCutsObserver(markdown)
	}
	outline, err := pmdoc.RenderingOutline(markdown)
	if err != nil {
		return map[sectionKey]decisionCut{}, false
	}
	typed := outline.TypedBlocks
	cuts = map[sectionKey]decisionCut{}
	seen := map[string]int{}
	type section struct {
		key   sectionKey
		level int
		start int
		// within is the innermost typed block that holds the heading, as its index in typed, or
		// -1 where none does (pmdoc.OutlineHeading.Within).
		within int
	}
	var open *section
	inReplaced := false
	// holders lists the typed blocks that hold the open decision's heading, the innermost first.
	var holders []int
	// next is the first typed block no cut has read. The cuts come in document order, and each
	// reads the typed blocks that open before the line of the heading that closes it, where the
	// next cut starts at the earliest, so no two cuts read one typed block.
	next := 0
	// ended is where the open decision's text ends where the line at closing closes the section
	// and the renderer writes separator line feeds ahead of that line: closing, backed up over at
	// most separator line feeds, and so over fewer where fewer stand there.
	ended := func(closing, separator int) int {
		end := closing
		for end > open.start && closing-end < separator && markdown[end-1] == '\n' {
			end--
		}
		return end
	}
	// closeAt closes the open decision at closing, which separator line feeds stand ahead of: the
	// start of the line of the heading that closes it, after a blank line, or the markdown's end,
	// after the line feed that ends its last line. containerEnd is where the outermost list, quote
	// or footnote definition that holds that heading ends (pmdoc.OutlineHeading.ContainerEnd), or
	// -1 where none does.
	closeAt := func(closing, separator, containerEnd int) {
		if open == nil {
			return
		}
		end := ended(closing, separator)
		holders = holders[:0]
		for index := open.within; index >= 0; index = typed[index].Within {
			holders = append(holders, index)
		}
		// The section's last block stands before the closing line of each typed block that holds
		// the heading and that the text ends with, the outermost first, and ends at the line feed
		// ahead of that line.
		for index := len(holders) - 1; index >= 0; index-- {
			if end == typed[holders[index]].End {
				end = strings.LastIndexByte(markdown[:end], '\n')
			}
		}
		// A list, quote or footnote definition that holds the heading that closes the decision is
		// a block of the section, held whole, and so is each typed block that opens inside the cut.
		end = max(end, containerEnd)
		for next < len(typed) && typed[next].Start <= open.start {
			next++
		}
		for ; next < len(typed) && typed[next].Start < closing; next++ {
			end = max(end, typed[next].End)
		}
		text := markdown[open.start:end]
		cuts[open.key] = decisionCut{
			text:  text,
			sum:   decisionTextSum(text),
			start: open.start,
			alone: open.within == -1 || end <= typed[open.within].End,
		}
		open = nil
	}
	for _, at := range outline.Headings {
		line, _, _ := strings.Cut(markdown[at.Start:], "\n")
		level, heading := markdownHeading(line)
		if level == 0 {
			continue
		}
		if open != nil && level <= open.level {
			closeAt(at.LineStart, len("\n\n"), at.ContainerEnd)
		}
		if level <= 2 {
			inReplaced = level == 2 && heading == replacedHeading
		}
		// A heading inside a list item, a quote or a footnote definition opens no decision.
		if open == nil && at.ContainerEnd < 0 && isDecisionHeading(level, inReplaced) {
			occurrence := seen[heading]
			seen[heading] = occurrence + 1
			open = &section{
				key:    sectionKey{heading: heading, occurrence: occurrence},
				level:  level,
				start:  at.LineStart,
				within: at.Within,
			}
		}
	}
	closeAt(len(markdown), len("\n"), -1)
	return cuts, true
}

// decisionCutsObserver, where a test sets it, is given each markdown decisionCuts parses.
var decisionCutsObserver func(markdown string)

// documentCut is a markdown cut into its decisions (decisionCuts): its cuts, and whether the
// parser read it. The process keeps each one, the document's as it stands and each stored
// version's, by its key (documentCuts), shared by every view that reads it, so nothing writes to
// one once it is cut.
type documentCut struct {
	markdown string
	cuts     map[sectionKey]decisionCut
	read     bool
}

// sectionKey names one decision of a document across its versions: its heading, and how many
// decisions before it the document's cut gives that heading (decisionCuts). A decision the view
// reads that has no cut - its heading stands in a list item, a quote or a footnote definition, or
// the parser refuses the document - is keyed by the occurrence noCut, which no cut of any version
// holds.
type sectionKey struct {
	heading    string
	occurrence int
}

// noCut is the occurrence of a decision that has no cut (sectionKey).
const noCut = -1

// markdownHeading is a line's ATX heading level and title, or level 0 for a line that is no
// heading. It reads a title as `headingOf` reads one off a heading block's own markdown, closing
// hashes and all, so the heading a version is keyed by is the heading the view names.
func markdownHeading(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}
