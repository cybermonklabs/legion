package refs

import (
	"strings"

	"github.com/google/uuid"
	"github.com/yuin/goldmark/util"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// questionBlocks is every question block markdown declares, in document order, the first of each
// id: each typed block named ask that holds an id, where pmdoc's parser opens one. A fenced
// example of a directive is code to pmdoc, and a line of front matter is no markdown to it, so a
// `copied_from` either carries names no question of this document.
//
// It reads the document's blocks line by line as goldmark (github.com/yuin/goldmark v1.8.6) and
// pmdoc's own block parsers read them, and builds no tree: pmdoc.ParseRendering of an 848 kB
// document took 224 ms, and this read of it 5 ms, measured on one machine. The CLI and the web
// read ask openers with a TypeScript twin of this reader (readAskBlocks, packages/contracts'
// dispatch-block-reader.ts). The rules:
//
//   - A line first goes past the containers it continues: a quote's `>` and one column after it, a
//     list item's content columns, and a footnote definition's four columns. A blank line continues
//     a list item and a footnote definition, but not a quote.
//   - A block opens only within three columns of those containers. A line four columns or more past
//     them continues the paragraph before it, or else opens indented code. So the later line of an
//     inline code span, which keeps its spaces, opens no block there.
//   - A container opens within three columns too: a `>`, a list marker (`-`, `*`, `+`, or one to
//     nine digits and `.` or `)`) followed by a space, a tab or the line's end, or a footnote label
//     (footnoteLabelEnd). Every space and tab after a label belongs to it.
//   - An empty list item, or one numbered other than 1, opens no list where it would interrupt a
//     paragraph, or a block that the browser editor's parser holds open (interruptsOpenBlock).
//   - A list stays open at a line that reaches its empty last item's content column, with no blank
//     line before it (pmdoc's emptyItemGuard).
//   - A fence is three or more backticks or tildes, and a backtick fence's info string holds no
//     backtick. A run of the same character at least as long closes it, within three columns of its
//     containers and with only spaces and tabs after it. A container that ends ends the fence in it.
//   - A typed block's opening line is read by pmdoc.ParseTypedDirectiveOpen. The block closes at a
//     line of as many colons or more, whatever block inside it the line would otherwise continue.
//   - A line that a container does not continue, and that opens no block, continues the paragraph
//     in that container (a lazy line), and the container stays open.
//   - No line of the front matter the document opens with opens a block (frontMatterEnd). After a
//     front-matter opener that no later line closes, no quote, list, footnote definition or typed
//     block opens at the document's level (pmdoc's frontmatterAttempt).
//
// HTML blocks, which the server refuses in a write, are not read.
func questionBlocks(markdown string) []questionBlock {
	lines := strings.Split(markdown, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSuffix(line, "\r")
	}
	start, unclosed := frontMatterEnd(lines)
	scan := blockScan{
		lines:               lines[start:],
		unclosedFrontMatter: unclosed,
		blocks:              []questionBlock{},
		seen:                map[string]struct{}{},
	}
	document := &scanBlock{kind: scanDocument}
	var line scanLine
	for index, text := range scan.lines {
		scan.index = index
		line = scanLine{text: text}
		scan.readLine(document, &line)
	}
	return scan.blocks
}

// frontMatterEnd is the index of the first of lines past the front matter they open with, as
// pmdoc reads it (parseFrontmatterBlock): a first line that is `---` and any spaces or tabs opens
// it, and the first later line that is the same closes it. pmdoc keeps it as one node, word for
// word, so no line of it opens a block. It is 0 where the lines open with none, or with an opener
// no line closes, which the renderer never writes: a rule opening a document is written `***`.
// unclosed reports such an opener (questionBlocks says what it changes).
func frontMatterEnd(lines []string) (end int, unclosed bool) {
	if !frontMatterFence(lines[0]) {
		return 0, false
	}
	for index := 1; index < len(lines); index++ {
		if frontMatterFence(lines[index]) {
			return index + 1, false
		}
	}
	return 0, true
}

// frontMatterFence reports whether line opens or closes front matter: `---` and any spaces or tabs.
func frontMatterFence(line string) bool {
	return strings.TrimRight(line, " \t") == "---"
}

// blockScan is what questionBlocks carries from line to line, as goldmark's parser context
// carries it.
type blockScan struct {
	// lines is the markdown's lines past its front matter, and index the one being read.
	lines []string
	index int
	// opened is the blocks open, outermost first, as goldmark's pc.OpenedBlocks(). The document
	// is not among them.
	opened []*scanBlock
	// emptyItemWithBlankLines is goldmark's emptyListItemWithBlankLines, and skipList its
	// skipListParserKey: a list item closed at a sibling's marker.
	emptyItemWithBlankLines, skipList bool
	// unclosedFrontMatter is that the markdown opens with a front-matter opener that no later line
	// closes (frontMatterEnd).
	unclosedFrontMatter bool
	// blocks is each question block found so far, and seen holds their ids.
	blocks []questionBlock
	seen   map[string]struct{}
}

// scanKind is the kind of a block that blockScan holds open.
type scanKind uint8

const (
	scanDocument scanKind = iota
	scanQuote
	scanFootnote
	// scanLeaf is a block of one line: a heading, a rule or a directive the server refuses.
	scanLeaf
	scanParagraph
	// scanCode is indented code.
	scanCode
	scanList
	scanItem
	scanTyped
	// scanFence is fenced code.
	scanFence
)

// scanBlock is one block as blockScan holds it open.
type scanBlock struct {
	kind   scanKind
	parent *scanBlock
	// previous is the block opened before it in its parent (goldmark's PreviousSibling), and last
	// the last block opened in it, of children.
	previous, last *scanBlock
	children       int
	// lastLine is the index of a paragraph's last line, and of an indented code block's last line
	// that is not blank.
	lastLine int
	// table is whether a line of a paragraph after its first is a table's delimiter row.
	table bool
	// blankBefore is whether a blank line, or the document's start, stands before an indented code
	// block.
	blankBefore bool
	// char is the character a list continues only with (its bullet, or its ordered delimiter), or
	// a fence's character.
	char byte
	// run is how many colons a typed block opened with, or how many characters a fence's run holds:
	// only a line of as many or more closes it.
	run int
	// offset is the columns a list item's content stands past its container's, or a typed block's
	// opening line, which it takes off its lines.
	offset int
}

// scanContinuation is what a block's continuation does with a line, as goldmark's State.
type scanContinuation uint8

const (
	scanClose scanContinuation = iota
	scanContinue
	scanChildren
)

// scanOpened is what blockScan.openBlocks did with a line.
type scanOpened uint8

const (
	openedNone scanOpened = iota
	openedBlock
	continuedParagraph
)

// scanLine is one line of markdown, read from the column that the containers around a block have
// taken. A tab reaches the next multiple of four columns.
type scanLine struct {
	text string
	// pos is the index of the first character not taken, and start the column it starts at.
	// column is the column taken so far: start, or more where a tab at pos is partly taken.
	pos, start, column int
	// ahead is the index of the first character at or after pos that is no space or tab, and
	// aheadColumn the column it starts at, where measured: indent reads them again only once pos
	// passes ahead, so each open block's continuation does not count the same spaces again.
	measured           bool
	ahead, aheadColumn int
}

// nextColumn is the column after char, which starts at column.
func nextColumn(column int, char byte) int {
	if char == '\t' {
		return column + 4 - column%4
	}
	return column + 1
}

// columnAt is the column that the character at index, pos or after it, starts at.
func (line *scanLine) columnAt(index int) int {
	column := line.start
	for at := line.pos; at < index; at++ {
		column = nextColumn(column, line.text[at])
	}
	return column
}

// afterSpaces is the index of the first character from index on that is no space or tab.
func (line *scanLine) afterSpaces(index int) int {
	for index < len(line.text) && (line.text[index] == ' ' || line.text[index] == '\t') {
		index++
	}
	return index
}

// spacesAt is the columns of the spaces and tabs that start at index, pos or after it.
func (line *scanLine) spacesAt(index int) int {
	return line.columnAt(line.afterSpaces(index)) - line.columnAt(index)
}

// indent is the columns of space and tab from the column taken, and the index of the character
// after them.
func (line *scanLine) indent() (width, at int) {
	if !line.measured || line.pos > line.ahead {
		line.ahead = line.afterSpaces(line.pos)
		line.aheadColumn = line.columnAt(line.ahead)
		line.measured = true
	}
	return line.aheadColumn - line.column, line.ahead
}

// blank reports whether nothing but spaces and tabs is left.
func (line *scanLine) blank() bool {
	_, at := line.indent()
	return at == len(line.text)
}

// skip takes columns columns of the spaces and tabs ahead, a tab partly where it spans more.
func (line *scanLine) skip(columns int) {
	line.column += columns
	for line.pos < len(line.text) {
		next := nextColumn(line.start, line.text[line.pos])
		if next > line.column {
			return
		}
		line.pos, line.start = line.pos+1, next
	}
}

// advanceTo takes the line up to the character at index, pos or after it.
func (line *scanLine) advanceTo(index int) {
	line.start = line.columnAt(index)
	line.pos, line.column = index, line.start
}

// takeAll takes the rest of the line, as a block of the rest of the line does. It counts no
// columns: nothing is left to measure.
func (line *scanLine) takeAll() {
	line.pos, line.start = len(line.text), line.column
}

// lastOpened is the block opened last, nil where none is open.
func (scan *blockScan) lastOpened() *scanBlock {
	if len(scan.opened) == 0 {
		return nil
	}
	return scan.opened[len(scan.opened)-1]
}

// readLine takes one line through the open blocks, then opens the blocks it opens: the body of
// goldmark's parseBlocks loop.
func (scan *blockScan) readLine(document *scanBlock, line *scanLine) {
	if len(scan.opened) == 0 {
		if !line.blank() {
			scan.openBlocks(document, line)
		}
		return
	}
	last := len(scan.opened) - 1
	for index := range last + 1 {
		block := scan.opened[index]
		// openBlocks continues a paragraph, where the line opens no block that interrupts it.
		if block.kind != scanParagraph {
			continuation := scan.continueBlock(block, line)
			if continuation == scanChildren && index == last {
				scan.openBlocks(block, line)
				return
			}
			if continuation != scanClose {
				continue
			}
		}
		// The block closes here, or the line continues the paragraph opened last as a lazy line.
		parent := document
		if index > 0 {
			parent = scan.opened[index-1]
		}
		if scan.openBlocks(parent, line) != continuedParagraph {
			scan.opened = append(scan.opened[:index], scan.opened[last+1:]...)
		}
		return
	}
}

// openBlocks opens the blocks line opens inside parent, else continues the paragraph opened last
// with it: goldmark's openBlocks.
func (scan *blockScan) openBlocks(parent *scanBlock, line *scanLine) scanOpened {
	last := scan.lastOpened()
	continuable := last != nil && last.kind == scanParagraph
	result := openedNone
	for into := parent; ; {
		width, at := line.indent()
		if at == len(line.text) {
			break
		}
		block, children := scan.openBlock(into, line, width, at, continuable && result == openedNone)
		if block == nil {
			break
		}
		into.children++
		into.last = block
		scan.opened = append(scan.opened, block)
		result = openedBlock
		if !children {
			break
		}
		into = block
	}
	if result == openedNone && continuable && !line.blank() {
		if tableDelimiterRow(line.text[line.pos:]) {
			last.table = true
		}
		last.lastLine = scan.index
		return continuedParagraph
	}
	return result
}

// block is a new block of kind, empty, inside parent.
func (scan *blockScan) block(kind scanKind, parent *scanBlock) *scanBlock {
	return &scanBlock{kind: kind, parent: parent, previous: parent.last}
}

// leaf is a block of one line inside parent, which takes the rest of line.
func (scan *blockScan) leaf(parent *scanBlock, line *scanLine) *scanBlock {
	line.takeAll()
	return scan.block(scanLeaf, parent)
}

// frontMatterHolds reports whether a front-matter opener that no later line closes keeps a
// container from opening in parent (frontMatterEnd).
func (scan *blockScan) frontMatterHolds(parent *scanBlock) bool {
	return scan.unclosedFrontMatter && parent.kind == scanDocument
}

// openBlock is the block line opens inside parent, its text at index at and width columns past
// parent's, and children is whether that block holds the blocks the rest of the line opens. It
// tries goldmark's block parsers and Dispatch's own in goldmark's order of priority: setext heading
// (100), thematic break (200), list (300), list item (400), indented code (500), ATX heading (600),
// fenced code (700), blockquote (800), Dispatch's refused directive (900) and typed block (950),
// footnote definition (999), paragraph (1000). Where interrupting, the line would interrupt the
// paragraph opened last, which indented code and a paragraph do not (CanInterruptParagraph). No
// block but indented code opens four columns or more past parent (CanAcceptIndentedLine).
func (scan *blockScan) openBlock(parent *scanBlock, line *scanLine, width, at int, interrupting bool) (*scanBlock, bool) {
	if width > 3 {
		if interrupting {
			return nil, false
		}
		line.skip(4)
		code := scan.block(scanCode, parent)
		code.lastLine = scan.index
		code.blankBefore = scan.index == 0 || blankText(scan.lines[scan.index-1])
		return code, false
	}
	text := line.text[at:]
	last := scan.lastOpened()
	switch char := text[0]; char {
	case '-', '=', '*', '_', '+', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if (char == '-' || char == '=') && last != nil && last.kind == scanParagraph && last.parent == parent && setextUnderline(text) {
			return scan.leaf(parent, line), false
		}
		if (char == '-' || char == '*' || char == '_') && thematicBreak(text) {
			return scan.leaf(parent, line), false
		}
		if char == '=' || char == '_' {
			break
		}
		if list := scan.openList(parent, line, last); list != nil {
			return list, true
		}
		if item, children := scan.openItem(parent, line); item != nil {
			return item, children
		}
	case '#':
		if run := runOf(text, '#'); run <= 6 && (run == len(text) || text[run] == ' ' || text[run] == '\t') {
			return scan.leaf(parent, line), false
		}
	case '`', '~':
		run := runOf(text, char)
		// A backtick fence's info string holds no backtick: such a line is inline code.
		if run < 3 || char == '`' && strings.IndexByte(text[run:], '`') >= 0 {
			break
		}
		line.takeAll()
		fence := scan.block(scanFence, parent)
		fence.char, fence.run = char, run
		return fence, false
	case '>':
		if !scan.frontMatterHolds(parent) && quoteMarker(line) {
			return scan.block(scanQuote, parent), true
		}
	case ':':
		if block, ok := scan.openDirective(parent, line, width, text); ok {
			return block, block.kind == scanTyped
		}
	case '[':
		label, ok := footnoteLabelEnd(line.text, at)
		if !ok || scan.frontMatterHolds(parent) {
			break
		}
		// Every space and tab after the label's `]:` belongs to the definition's prefix, so a block
		// written on the same line starts at column zero inside it: pmdoc's
		// footnoteDefinitionParser.Open skips them, where goldmark leaves them to that block. A line
		// holding nothing but whitespace after the label keeps it.
		if content := line.afterSpaces(label); content < len(line.text) {
			label = content
		}
		line.advanceTo(label)
		return scan.block(scanFootnote, parent), true
	}
	if interrupting {
		return nil, false
	}
	line.takeAll()
	paragraph := scan.block(scanParagraph, parent)
	paragraph.lastLine = scan.index
	return paragraph, false
}

// openList is the list line opens inside parent, nil where it opens none: goldmark's list parser,
// held off an empty item, or one numbered other than a lone `1`, that would interrupt a paragraph
// or an indented code block (pmdoc's emptyItemGuard, stricter than goldmark, which lets `01.`
// interrupt a paragraph and anything interrupt code). last is the block opened last.
func (scan *blockScan) openList(parent *scanBlock, line *scanLine, last *scanBlock) *scanBlock {
	if last != nil && last.kind == scanList || scan.skipList {
		scan.skipList = false
		return nil
	}
	marker, ok := listMarker(line)
	if !ok || scan.frontMatterHolds(parent) {
		return nil
	}
	if marker.empty || marker.number != "" && marker.number != "1" {
		if last != nil && last.kind == scanParagraph && last.parent == parent || scan.interruptsOpenBlock(parent) {
			return nil
		}
	}
	scan.emptyItemWithBlankLines = false
	list := scan.block(scanList, parent)
	list.char = marker.char
	return list
}

// openItem is the list item line opens inside parent, a list, and whether it holds the blocks the
// rest of the line opens: goldmark's list item parser, the spaces and tabs after the marker
// measured from the column they stand at (pmdoc's listItemColumns).
func (scan *blockScan) openItem(parent *scanBlock, line *scanLine) (*scanBlock, bool) {
	if parent.kind != scanList {
		return nil, false
	}
	previous := 0
	if parent.last != nil && parent.last.kind == scanItem {
		previous = parent.last.offset
	}
	marker, ok := listMarker(line)
	if !ok || marker.indent-previous > 3 {
		return nil, false
	}
	scan.emptyItemWithBlankLines = false
	// The content's columns past the marker: one, where the item opens with indented code.
	spacing := line.spacesAt(marker.end)
	if marker.empty || spacing > 4 {
		spacing = 1
	}
	item := scan.block(scanItem, parent)
	item.offset = line.columnAt(marker.end) - line.column + spacing
	if marker.empty {
		return item, false
	}
	line.advanceTo(marker.end)
	line.skip(spacing)
	return item, true
}

// openDirective is the block a line of directive syntax, text, opens inside parent, width columns
// past it, and ok is false where it opens none. A line that pmdoc refuses
// (unsupportedDirectiveReason) opens a leaf; a stored document holds none. A typed block's opening
// line is read by the one parser of that line (pmdoc.ParseTypedDirectiveOpen), so an id written
// either way (`#id` and `id="…"`, pmdoc.TypedBlockIDAttribute) is read as the document's own parse
// reads it. An ask block's id and copied_from are recorded (record).
func (scan *blockScan) openDirective(parent *scanBlock, line *scanLine, width int, text string) (*scanBlock, bool) {
	if !strings.HasPrefix(text, ":::") {
		// A leaf directive (`::name`), and a text directive (`:name{…}`) opening the line.
		if strings.HasPrefix(text, "::") && len(text) > 2 && pmdoc.DirectiveNameByte(text[2]) ||
			len(text) > 1 && pmdoc.DirectiveNameByte(text[1]) && strings.Contains(text, "{") {
			return scan.leaf(parent, line), true
		}
		return nil, false
	}
	if text == ":::" {
		return nil, false
	}
	name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(text)
	if !ok {
		return scan.leaf(parent, line), true
	}
	if scan.frontMatterHolds(parent) {
		return nil, false
	}
	if name == "ask" {
		scan.record(attrs)
	}
	line.takeAll()
	typed := scan.block(scanTyped, parent)
	typed.run, typed.offset = runOf(text, ':'), width
	return typed, true
}

// record adds the question block an ask block's opening line names by attrs, the first of each
// id: the block's id and, where the line carries one, the uuid of the ask its `copied_from`
// names. An ask block with no id names no question.
func (scan *blockScan) record(attrs pmdoc.Attrs) {
	id, _ := attrs[pmdoc.BlockIDAttr].(string)
	if id == "" {
		return
	}
	if _, repeated := scan.seen[id]; repeated {
		return
	}
	scan.seen[id] = struct{}{}
	block := questionBlock{id: id}
	// Only the one attribute carries an ask: every other value on the line - the state, the
	// asker, the question's own wording - is left unread.
	if copied, ok := attrs["copied_from"].(string); ok {
		if parsed, err := uuid.Parse(copied); err == nil {
			block.copiedFrom = parsed.String()
		}
	}
	scan.blocks = append(scan.blocks, block)
}

// interruptsOpenBlock is pmdoc's interruptsOpenBlock: whether a list opening in parent interrupts
// a block the browser editor's parser holds open there, other than a paragraph parent holds: a
// paragraph that ends on the line before, reached across the containers the line opens first and
// forming no table, or an indented code block with only blank lines after it, unless that code
// stands right after a list, or right after a quote with no blank line between them. Where the
// code is parent's own last block, a line of quote markers after it leaves it open too.
func (scan *blockScan) interruptsOpenBlock(parent *scanBlock) bool {
	previous := parent.last
	opensContainers := previous == nil
	for node := parent; previous == nil && node != nil && node.kind != scanDocument; node = node.parent {
		previous = node.previous
	}
	if previous == nil {
		return false
	}
	switch previous.kind {
	case scanParagraph:
		return opensContainers && !previous.table && scan.index-previous.lastLine <= 1
	case scanCode:
		if before := previous.previous; before != nil && (before.kind == scanList || before.kind == scanQuote && !previous.blankBefore) {
			return false
		}
		for _, row := range scan.lines[previous.lastLine+1 : scan.index] {
			if !blankText(row) && (opensContainers || strings.Trim(row, " \t>") != "") {
				return false
			}
		}
		return true
	}
	return false
}

// continueBlock is what block, open, does with line: goldmark's Continue of each block parser.
func (scan *blockScan) continueBlock(block *scanBlock, line *scanLine) scanContinuation {
	switch block.kind {
	case scanQuote:
		if quoteMarker(line) {
			return scanChildren
		}
		return scanClose
	case scanList:
		return scan.continueList(block, line)
	case scanItem:
		return scan.continueItem(block, line)
	case scanFootnote:
		if line.blank() {
			return scanChildren
		}
		if width, _ := line.indent(); width < 4 {
			return scanClose
		}
		line.skip(4)
		return scanChildren
	case scanTyped:
		width, at := line.indent()
		// A line of at least the fence's colons closes the block, whatever block inside it the line
		// would otherwise continue (pmdoc's typedDirectiveParser.Continue).
		if width < 4 && at < len(line.text) && pmdoc.FenceColons(line.text[at:]) >= block.run {
			line.takeAll()
			return scanClose
		}
		if at < len(line.text) {
			line.skip(min(width, block.offset))
		}
		return scanChildren
	case scanFence:
		width, at := line.indent()
		if run := runOf(line.text[at:], block.char); width < 4 && run >= block.run && blankText(line.text[at+run:]) {
			line.takeAll()
			return scanClose
		}
		line.takeAll()
		return scanContinue
	case scanCode:
		if line.blank() {
			return scanContinue
		}
		if width, _ := line.indent(); width < 4 {
			return scanClose
		}
		block.lastLine = scan.index
		line.takeAll()
		return scanContinue
	}
	// A heading, a rule or a refused directive holds one line.
	return scanClose
}

// continueList is goldmark's listParser.Continue, which pmdoc's emptyItemGuard keeps from closing
// the list at a line that holds its empty last item's content: a line that reaches the item's
// content column, a list marker there included, with no blank line before it.
func (scan *blockScan) continueList(list *scanBlock, line *scanLine) scanContinuation {
	last := list.last
	if line.blank() {
		if last == nil || last.children == 0 {
			scan.emptyItemWithBlankLines = true
		}
		return scanChildren
	}
	width, at := line.indent()
	continuation := scan.continueListAt(list, line, width, at)
	if continuation == scanClose && last != nil && last.kind == scanItem && last.children == 0 && width >= last.offset &&
		scan.index > 0 && strings.Trim(scan.lines[scan.index-1], " \t>") != "" {
		return scanChildren
	}
	return continuation
}

// continueListAt is goldmark's listParser.Continue of a line that is not blank, its text at index
// at and width columns past the list's container.
func (scan *blockScan) continueListAt(list *scanBlock, line *scanLine, width, at int) scanContinuation {
	last := list.last
	lastIsEmpty := last == nil || last.children == 0
	offset := 0
	if last != nil && last.kind == scanItem {
		offset = last.offset
	}
	if width < offset || lastIsEmpty {
		if marker, ok := listMarker(line); ok && width < 4 && marker.indent-offset < 4 {
			if marker.char != list.char {
				return scanClose
			}
			// A thematic break outranks a list item, unless it underlines a paragraph as a heading.
			text := line.text[at:]
			opened := scan.lastOpened()
			heading := opened != nil && opened.kind == scanParagraph && setextUnderline(text)
			if thematicBreak(text) && !heading {
				return scanClose
			}
			return scanChildren
		}
		if !lastIsEmpty {
			return scanClose
		}
	}
	if lastIsEmpty && width < offset {
		return scanClose
	}
	// Content cannot follow an empty item and blank lines in the same list.
	if scan.emptyItemWithBlankLines {
		return scanClose
	}
	return scanChildren
}

// continueItem is goldmark's listItemParser.Continue, a blank line taking no more than the item's
// columns (pmdoc's listItemColumns).
func (scan *blockScan) continueItem(item *scanBlock, line *scanLine) scanContinuation {
	width, _ := line.indent()
	if line.blank() {
		line.skip(min(width, item.offset))
		return scanChildren
	}
	isEmpty := item.children == 0 && scan.emptyItemWithBlankLines
	if (isEmpty || width < item.offset) && width < 4 {
		if _, ok := listMarker(line); ok {
			scan.skipList = true
			return scanClose
		}
		if !isEmpty {
			return scanClose
		}
	}
	line.skip(min(width, item.offset))
	return scanChildren
}

// scanListMarker is a list item's marker opening what is left of a line.
type scanListMarker struct {
	// indent is the columns of indentation before it, and end the index after it.
	indent, end int
	// char is the character a list continues only with: the bullet, or the ordered delimiter.
	char byte
	// number is an ordered marker's number as written, "" for a bullet.
	number string
	// empty is that nothing but spaces and tabs follows it.
	empty bool
}

// listMarker is the list item's marker that opens what is left of line, ok false where none does:
// goldmark's parseListItem, its indentation measured in the columns its tabs span (pmdoc's
// tabIndented).
func listMarker(line *scanLine) (marker scanListMarker, ok bool) {
	width, at := line.indent()
	if width > 3 || at == len(line.text) {
		return scanListMarker{}, false
	}
	text := line.text
	end := at
	if first := text[at]; first == '-' || first == '*' || first == '+' {
		end++
	} else {
		for end < len(text) && end-at <= 9 && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if end == at || end-at > 9 || end == len(text) || text[end] != '.' && text[end] != ')' {
			return scanListMarker{}, false
		}
		marker.number = text[at:end]
		end++
	}
	if end < len(text) && text[end] != ' ' && text[end] != '\t' {
		return scanListMarker{}, false
	}
	marker.indent, marker.end, marker.char, marker.empty = width, end, text[end-1], blankText(text[end:])
	return marker, true
}

// quoteMarker takes a quote's marker off what is left of line, as goldmark's blockquote parser
// does: a `>` within three columns, then one column of the space or tab after it. It reports
// whether the line holds one, and takes nothing where it does not.
func quoteMarker(line *scanLine) bool {
	width, at := line.indent()
	if width > 3 || at == len(line.text) || line.text[at] != '>' {
		return false
	}
	line.advanceTo(at + 1)
	if at+1 < len(line.text) && (line.text[at+1] == ' ' || line.text[at+1] == '\t') {
		line.skip(1)
	}
	return true
}

// footnoteLabelEnd is the index after the `]:` of the footnote label at index at of text, as
// goldmark's footnote parser reads one (extension/footnote.go, through util.FindClosure) and pmdoc
// keeps it: `[^`, the label, `]`, then `:` at once. A backslash before an ASCII punctuation
// character (util.IsPunct) escapes that character, so `\]` and `\[` are part of the label, as the
// renderer writes a label that holds a bracket (pmdoc's escapeFootnoteLabel). An unescaped `[`
// makes the line no label, and so does a blank label (util.IsBlank). ok is false where no label
// stands at at.
func footnoteLabelEnd(text string, at int) (end int, ok bool) {
	if !strings.HasPrefix(text[at:], "[^") {
		return 0, false
	}
	bracket := at + 2
	for ; bracket < len(text) && text[bracket] != ']'; bracket++ {
		if text[bracket] == '[' {
			return 0, false
		}
		if text[bracket] == '\\' && bracket+1 < len(text) && util.IsPunct(text[bracket+1]) {
			bracket++
		}
	}
	// goldmark's white space is the space, the tab, the line feed and the carriage return alone, so
	// a label of a vertical tab or a form feed is no blank label.
	if bracket+1 >= len(text) || text[bracket+1] != ':' || util.IsBlank([]byte(text[at+2:bracket])) {
		return 0, false
	}
	return bracket + 2, true
}

// thematicBreak is goldmark's isThematicBreak: three or more of one of `*`, `-` and `_`, with
// spaces and tabs between them and nothing else.
func thematicBreak(text string) bool {
	var mark byte
	count := 0
	for index := range len(text) {
		switch char := text[index]; {
		case char == ' ' || char == '\t':
		case char != '*' && char != '-' && char != '_', mark != 0 && char != mark:
			return false
		default:
			mark = char
			count++
		}
	}
	return count >= 3
}

// setextUnderline is goldmark's matchesSetextHeadingBar: a run of `=` or of `-`, then spaces and
// tabs alone.
func setextUnderline(text string) bool {
	run := runOf(text, text[0])
	return (text[0] == '=' || text[0] == '-') && blankText(text[run:])
}

// tableDelimiterRow reports whether text is a table's delimiter row (GFM's tables), which makes
// the paragraph above it a table: cells of one or more `-`, each with an optional `:` on either
// side, between `|`, with at least one `|` and an optional one at either end.
func tableDelimiterRow(text string) bool {
	text = strings.Trim(text, " \t")
	if !strings.Contains(text, "|") {
		return false
	}
	text = strings.TrimPrefix(text, "|")
	text = strings.TrimSuffix(text, "|")
	for cell := range strings.SplitSeq(text, "|") {
		cell = strings.TrimPrefix(strings.TrimSuffix(strings.Trim(cell, " \t"), ":"), ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

// runOf is how many times char stands in a row at the start of text.
func runOf(text string, char byte) int {
	run := 0
	for run < len(text) && text[run] == char {
		run++
	}
	return run
}

// blankText reports whether text holds nothing but spaces and tabs.
func blankText(text string) bool {
	return strings.Trim(text, " \t") == ""
}
