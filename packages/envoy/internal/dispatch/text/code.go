package text

import (
	"slices"
	"strings"
)

// codeRange is a half-open byte range of a body, [start, end).
type codeRange struct {
	start int
	end   int
}

// container is one block quote or list item open at the line being read: what the line carries
// before its own content. A block quote is held by its `>` marker; a list item by the column its
// own content starts at, which the lines under it carry as indentation and which is where code
// inside that item opens from.
type container struct {
	quote   bool
	content int
}

// tabWidth is the stop a tab advances the column to, as CommonMark measures indentation.
const tabWidth = 4

// leafKind is the code block the line being read stands in, if any.
type leafKind uint8

const (
	noLeaf leafKind = iota
	fencedLeaf
	indentedLeaf
)

// codeRanges is every code context of body, in order and never overlapping: a reference written
// inside one is read and indexed as every other reference is and names nobody, so no party is
// delivered to for it (Located.InCode). It is computed once per body, by the one walk every
// extraction goes through (extractSpans), and read with inCode.
//
// Every line is read through the containers open at it - a block quote's `>` with the one space
// after it, a list item's own content indentation - so a block begins and ends where CommonMark
// begins and ends it inside the container holding it: a fence written inside a quote or an item
// is code, and the four spaces an ordinary nested bullet carries are the item's own text.
//
// The three contexts, and nothing else:
//
//   - A fenced code block. Its opening line carries, past the containers open at it, at most
//     three columns of indentation and then three or more backticks or tildes, a backtick
//     fence's info string holding no backtick of its own. It closes on the first line that holds
//     those same containers and carries at most three columns of indentation, a run of the same
//     character at least as long as the opener, and nothing after it but spaces and tabs; a
//     fence nothing closes runs to the end of the body, and one whose containers a later line
//     leaves ends with the line before that one. Both fence lines are part of the range, so an
//     address written in an info string is in code too.
//   - An indented code block: a line carrying four columns past where its containers' content
//     starts, where such a line may open one at all - the line before it is blank or there is
//     none, four columns under a paragraph being that paragraph's own text. It runs through the
//     indented lines that follow, blank lines between them included, and ends at the last of
//     them.
//   - A code span, as CommonMark reads one: a run of N backticks opens one and the next run of
//     exactly N closes it, the search ending where the paragraph does - at a blank line, at a
//     line blank past the containers it carries such as a bare `>` inside a quote, at a list
//     item's marker, at an ATX heading, at a thematic break - so a run nothing closes inside
//     that paragraph is literal text. A backslash takes the backtick after it as a literal
//     character, which opens no span, while a run that closes one is read as written, escaped
//     or not, which is where CommonMark puts the escape too. Spans are read in the text the
//     blocks leave, so a backtick inside a fenced block opens nothing and no span reaches across
//     a block.
//
// The containers it reads are a block quote, nested in another or not, and a list item of a `-`,
// `+`, `*` or numbered marker, whose content column is where the item's own text starts - a
// marker followed by five columns or more of spaces leaving the rest of them to indented code
// inside the item, and one that cannot interrupt a paragraph (an empty item, or a number other
// than 1) opening nothing. A line carrying none of the containers it stands in while a paragraph
// is open is that paragraph's own text, a lazy continuation line, which opens nothing and leaves
// those containers open; any other line closes the containers it does not carry. A thematic
// break is read too, since a line of `-`, `_` or `*` would otherwise open a list item.
//
// What it deliberately does not read, because the dashboard's reader must agree with it on every
// row of DISPATCH_TEXT_REFERENCES and each of these would cost both readers a markdown parse:
//
//   - A line that is no paragraph's and opens none: a setext heading's underline, a table's
//     rows, a block of HTML, a link reference definition. An indented line right under one is
//     read as that paragraph's text, so a code block opening there is read as prose and the
//     party it holds is delivered to, where no reader of the record sees one named. An ATX
//     heading is not one of them: it is read, and ends the paragraph above it.
//   - A footnote definition's container. Its content, four columns in, is read as an indented
//     code block where a blank line stands between it and the `[^id]:` line - a party written
//     there is delivered to nobody, which is the silent half of being wrong.
//   - Tab stops a container's marker splits. A tab is measured to the next four-column stop, but
//     one straddling a container's own content column is taken whole by that container, so a tab
//     after a list marker is the item's indentation rather than indented code inside it: code
//     read as prose again.
//   - A code span crossing a construct only a real parse resolves. The paragraph a line of the
//     first kind ends carries on for this reader, so a run opened before a setext heading's
//     underline, a table's rows or a block of HTML can be closed by a run after it.
func codeRanges(body string) []codeRange {
	blocks, breaks := codeBlocks(body)
	if len(blocks) == 0 {
		return appendCodeSpans(nil, body, 0, len(body), breaks)
	}
	ranges := make([]codeRange, 0, 2*len(blocks)+1)
	at := 0
	for _, block := range blocks {
		ranges = appendCodeSpans(ranges, body, at, block.start, breaks)
		ranges = append(ranges, block)
		at = block.end
	}
	return appendCodeSpans(ranges, body, at, len(body), breaks)
}

// inCode reports whether the byte at offset is inside one of ranges, which codeRanges wrote in
// order: the reference whose first character it is was written in a code context.
func inCode(ranges []codeRange, offset int) bool {
	_, found := slices.BinarySearchFunc(ranges, offset, func(candidate codeRange, offset int) int {
		switch {
		case candidate.end <= offset:
			return -1
		case candidate.start > offset:
			return 1
		}
		return 0
	})
	return found
}

// codeBlocks is the fenced and indented code blocks of body, in order, each from the first byte
// of its opening line to the end of its last line, and the offsets where a paragraph ends, in
// order too: both are read in the one walk of the body's lines. The walk holds the containers
// open at the line (stack), whether the line before it was a paragraph's - which is what an
// indented line needs not to be to open a block of its own - and the code block the line stands
// in, if any. A block's range runs from the start of its opening line, containers' markers
// included, since nothing of a line before the code is a reference. A break is the start of a
// line that ends the paragraph above it, which is where a code span's search for its closing
// run ends too (appendCodeSpans).
func codeBlocks(body string) (blocks []codeRange, breaks []int) {
	var stack []container
	paragraph := false
	leaf := noLeaf
	start, last := 0, 0
	var fenceChar byte
	fenceWidth := 0

	for at := 0; at < len(body); {
		end, next := lineBounds(body, at)
		matched, content, col := matchContainers(body, at, end, stack)
		indent, column := indentEnd(body, content, end, col)

		switch leaf {
		case fencedLeaf:
			if matched == len(stack) {
				if column-col <= 3 && closesFence(body, indent, end, fenceChar, fenceWidth) {
					blocks = append(blocks, codeRange{start: start, end: next})
					leaf = noLeaf
				}
				at = next
				continue
			}
			// The line left the containers the fence opened in, and no code block is carried on
			// by a line that leaves them: the block ends with the line before this one, which is
			// then read as any other.
			blocks = append(blocks, codeRange{start: start, end: at})
			leaf = noLeaf
		case indentedLeaf:
			if matched == len(stack) {
				if indent >= end {
					// A blank line holds the block open; the blanks trailing it are not its own,
					// so last stays where the last indented line ended.
					at = next
					continue
				}
				if column-col >= 4 {
					last, at = next, next
					continue
				}
			}
			blocks = append(blocks, codeRange{start: start, end: last})
			leaf = noLeaf
		}

		// The containers this line opens of its own, each inside the last: its content is what
		// is left after them.
		depth := matched
		thematic := false
		for indent < end && column-col <= 3 {
			if body[indent] == '>' {
				stack = append(stack[:depth], container{quote: true})
				depth++
				content, col = quoteContent(body, indent, end, column)
				paragraph = false
				indent, column = indentEnd(body, content, end, col)
				continue
			}
			if isThematicBreak(body, indent, end) {
				thematic = true
				break
			}
			itemContent, itemColumn, itemIndent, ok := listMarker(body, indent, end, column, paragraph)
			if !ok {
				break
			}
			stack = append(stack[:depth], container{content: itemIndent})
			depth++
			content, col = itemContent, itemColumn
			paragraph = false
			indent, column = indentEnd(body, content, end, col)
		}

		// A line opening a container of its own - a list item's marker, a `>` - interrupts the
		// paragraph above it.
		ends := depth > matched
		heading := false
		prose := false
		switch {
		case indent >= end || thematic:
			// A line blank past the containers it carries - a bare `>` among them - and a
			// thematic break open nothing and end the paragraph above them.
			ends = true
		case column-col >= 4 && !paragraph:
			leaf, start, last = indentedLeaf, at, next
		case column-col <= 3:
			char, width, ok := fenceOpener(body, indent, end)
			switch {
			case ok:
				leaf, start, fenceChar, fenceWidth = fencedLeaf, at, char, width
			case isATXHeading(body, indent, end):
				// An ATX heading is no paragraph's line and opens none: it ends the paragraph
				// above it, and a break on the line under it leaves the heading's own text to
				// itself, where a span of its own may stand.
				ends, heading = true, true
			default:
				prose = true
			}
		default:
			// Four columns or more under a paragraph are that paragraph's own text.
			prose = true
		}
		if ends {
			breaks = append(breaks, at)
		}
		if heading {
			breaks = append(breaks, next)
		}
		// A line that carries none of the containers it stands in, opens nothing and continues
		// the paragraph above it is that paragraph's own text: the containers stay open for the
		// lines after it. Every other line closes the ones it does not carry.
		if !prose || !paragraph || depth > matched {
			stack = stack[:depth]
		}
		paragraph = prose
		at = next
	}

	switch leaf {
	case fencedLeaf:
		blocks = append(blocks, codeRange{start: start, end: len(body)})
	case indentedLeaf:
		blocks = append(blocks, codeRange{start: start, end: last})
	}
	return blocks, breaks
}

// matchContainers reads the line [at, end) through the containers of stack: how many of them, in
// order, hold the line, where the content of the innermost of those begins and the column there.
// A block quote holds a line carrying its `>` under four columns of indentation, the marker and
// one space or tab after it belonging to the quote; a list item holds a line indented to the
// item's content column, and a blank line, which no block quote holds.
func matchContainers(body string, at, end int, stack []container) (matched, content, col int) {
	content = at
	for matched < len(stack) {
		if stack[matched].quote {
			marker, column := indentEnd(body, content, end, col)
			if marker >= end || column-col > 3 || body[marker] != '>' {
				return
			}
			content, col = quoteContent(body, marker, end, column)
			matched++
			continue
		}
		if isBlank(body, content, end) {
			matched++
			continue
		}
		past, column := indentTo(body, content, end, col, stack[matched].content)
		if column < stack[matched].content {
			return
		}
		content, col = past, column
		matched++
	}
	return
}

// quoteContent is where the content of the block quote whose `>` stands at marker, in column
// col, begins and the column there: past the marker and past the one space or tab that may
// follow it, which the quote takes as its own.
func quoteContent(body string, marker, end, col int) (int, int) {
	past, column := marker+1, col+1
	if past < end {
		switch body[past] {
		case ' ':
			return past + 1, column + 1
		case '\t':
			return past + 1, column + tabWidth - column%tabWidth
		}
	}
	return past, column
}

// listMarker reads the list item marker standing at marker, the first character of a line's own
// content, in column col: a `-`, `+` or `*`, or one to nine digits and a `.` or `)`, followed by
// a space, a tab or the end of the line. It is where the item's content begins on this line, the
// column there, and the column the item's content is indented to, which the lines under it carry
// to stay inside it. Five columns or more of spaces after the marker are indented code inside
// the item, so the item takes one of them and leaves the rest. A marker that cannot interrupt a
// paragraph opens nothing while one is open: an empty item's, and an ordered one that does not
// count from 1.
func listMarker(body string, marker, end, col int, paragraph bool) (content, column, indent int, ok bool) {
	past := marker
	switch body[marker] {
	case '-', '+', '*':
		past++
	default:
		digits := past
		for past < end && body[past] >= '0' && body[past] <= '9' {
			past++
		}
		if past == digits || past-digits > 9 || past >= end || (body[past] != '.' && body[past] != ')') {
			return 0, 0, 0, false
		}
		if paragraph && (past-digits != 1 || body[digits] != '1') {
			return 0, 0, 0, false
		}
		past++
	}
	width := col + past - marker
	spaces, spacesColumn := indentEnd(body, past, end, width)
	if spaces >= end {
		if paragraph {
			return 0, 0, 0, false
		}
		return past, width, width + 1, true
	}
	if spaces == past {
		return 0, 0, 0, false
	}
	if spacesColumn-width > 4 && body[past] == ' ' {
		return past + 1, width + 1, width + 1, true
	}
	return spaces, spacesColumn, spacesColumn, true
}

// isThematicBreak reports whether the content of a line at from is a thematic break: three or
// more of `-`, `_` or `*`, all the same one, and nothing else but spaces and tabs. It opens no
// list item and no paragraph, so an indented line right under one opens a code block, as
// CommonMark opens one there.
func isThematicBreak(body string, from, end int) bool {
	char := body[from]
	if char != '-' && char != '_' && char != '*' {
		return false
	}
	count := 0
	for at := from; at < end; at++ {
		switch body[at] {
		case char:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

// isATXHeading reports whether the content of a line at from, past its containers and its own
// indentation, is an ATX heading: one to six `#` and then a space, a tab or the end of the line.
// It is no paragraph's line and opens none, so it ends the paragraph above it and an indented
// line right under it opens a code block, as CommonMark opens one there.
func isATXHeading(body string, from, end int) bool {
	at := from
	for at < end && body[at] == '#' {
		at++
	}
	if at == from || at-from > 6 {
		return false
	}
	return at >= end || body[at] == ' ' || body[at] == '\t'
}

// indentEnd is past the spaces and tabs starting at at, within [at, end), and the column there,
// read from column col: a tab advances to the next four-column stop. It lands on end where the
// rest of the line is blank.
func indentEnd(body string, at, end, col int) (int, int) {
	for at < end {
		switch body[at] {
		case ' ':
			col++
		case '\t':
			col += tabWidth - col%tabWidth
		default:
			return at, col
		}
		at++
	}
	return at, col
}

// indentTo is where the spaces and tabs starting at at reach column want, and the column there:
// the indentation a container takes off a line, the rest of it being the line's own. A tab is
// never split, so one straddling want lands past it.
func indentTo(body string, at, end, col, want int) (int, int) {
	for at < end && col < want {
		switch body[at] {
		case ' ':
			col++
		case '\t':
			col += tabWidth - col%tabWidth
		default:
			return at, col
		}
		at++
	}
	return at, col
}

// appendCodeSpans appends the code spans of body[from:to] to spans, in order, each read inside
// the paragraph that holds it: the search for a span's closing run ends where that paragraph
// ends, at the first of breaks past where the search began, so an unmatched backtick mutes
// nothing written past its own paragraph. The breaks are the ones codeBlocks read, in order
// (a blank line, a line blank past its containers, a list item's marker, an ATX heading, a
// thematic break), and the dashboard's codeSpans reads the same rule
// (`packages/dispatch/web/src/features/refs/code-ranges.ts`).
func appendCodeSpans(spans []codeRange, body string, from, to int, breaks []int) []codeRange {
	// A break on from itself cuts nothing, the paragraph starting there.
	index, _ := slices.BinarySearch(breaks, from+1)
	start := from
	for ; index < len(breaks) && breaks[index] < to; index++ {
		spans = appendParagraphSpans(spans, body, start, breaks[index])
		start = breaks[index]
	}
	return appendParagraphSpans(spans, body, start, to)
}

// appendParagraphSpans appends the code spans of [from, to), a stretch of body no paragraph
// break cuts: the run of backticks at a span's start is closed by the next run of exactly that
// length, a longer run being no closer, and a run with no closer is literal text that the next
// character is read on from. A backslash takes the character after it, so an escaped backtick
// opens no span; a run that closes one is read as written, as CommonMark reads a closer.
func appendParagraphSpans(spans []codeRange, body string, from, to int) []codeRange {
	for at := from; at < to; {
		if body[at] == '\\' {
			at += 2
			continue
		}
		if body[at] != '`' {
			at++
			continue
		}
		opener := backtickRunEnd(body, at, to)
		width := opener - at
		closer := nextBacktickRun(body, opener, to, width)
		if closer < 0 {
			at = opener
			continue
		}
		spans = append(spans, codeRange{start: at, end: closer + width})
		at = closer + width
	}
	return spans
}

// backtickRunEnd is the end of the run of backticks starting at offset, within [offset, to).
func backtickRunEnd(text string, offset, to int) int {
	for offset < to && text[offset] == '`' {
		offset++
	}
	return offset
}

// nextBacktickRun is where the first run of exactly width backticks at or after from starts,
// within [from, to), or -1 when there is none: a run of any other length closes no span of that
// width, so the search carries on past it.
func nextBacktickRun(text string, from, to, width int) int {
	for at := from; at < to; {
		found := strings.IndexByte(text[at:to], '`')
		if found < 0 {
			return -1
		}
		start := at + found
		at = backtickRunEnd(text, start, to)
		if at-start == width {
			return start
		}
	}
	return -1
}

// lineBounds is where the line starting at offset ends, before its line ending, and where the
// next line starts (the end of the body when this is its last line). A lone `\r` is the line's
// own character.
func lineBounds(body string, offset int) (end, next int) {
	found := strings.IndexByte(body[offset:], '\n')
	if found < 0 {
		return len(body), len(body)
	}
	end = offset + found
	if end > offset && body[end-1] == '\r' {
		return end - 1, end + 1
	}
	return end, end + 1
}

// isBlank reports whether [at, end) holds nothing but spaces and tabs.
func isBlank(body string, at, end int) bool {
	for ; at < end; at++ {
		if body[at] != ' ' && body[at] != '\t' {
			return false
		}
	}
	return true
}

// fenceOpener reads the opening fence of a line's content at at, past its containers and its own
// indentation, which its caller has read as under four columns: the fence's character and the
// length of its run. A backtick fence whose info string holds a backtick is no fence at all, the
// backtick being a code span's.
func fenceOpener(body string, at, end int) (char byte, width int, ok bool) {
	char = body[at]
	if char != '`' && char != '~' {
		return 0, 0, false
	}
	past := at
	for past < end && body[past] == char {
		past++
	}
	width = past - at
	if width < 3 || (char == '`' && strings.IndexByte(body[past:end], '`') >= 0) {
		return 0, 0, false
	}
	return char, width, true
}

// closesFence reports whether a line's content at at, its own indentation read as under four
// columns, closes a fence of char opened with width characters: a run of char at least as long,
// and nothing after it but spaces and tabs.
func closesFence(body string, at, end int, char byte, width int) bool {
	past := at
	for past < end && body[past] == char {
		past++
	}
	return past-at >= width && isBlank(body, past, end)
}
