package pmdoc

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
)

// DecisionCard is where the browser reader draws a decision card in a body: Position is the place,
// counted from 1, of the top-level block that holds the first `ask` the reader draws a card for,
// and Type is that block's type, named as the conversion names the Proof node it makes of such a
// block (blockType). The decision block's own type is that block's type only where the decision
// block stands at the top of the body. A card nowhere is the zero DecisionCard.
type DecisionCard struct {
	Position int
	Type     string
}

// FindDecisionCard answers where the browser reader draws the first decision card in markdown, and
// whether it draws one at all.
//
// It exists for a body the parse refuses. The reader draws a card for each `ask` typed directive
// its parser makes, whatever else in the body the server's parse refuses: a body refused for a link
// reference definition, an HTML comment, an unknown directive, a malformed table or a deep nesting
// draws its card in full - header, urgency pill and both option rows - and nobody can answer it
// where it stands (INFRA-1201). The parse answers its refusal and throws that tree away; this reads
// the same tree and keeps what it says, so the rule over a refused body reads what the reader
// reads (itemblocks.Check).
//
// Where that tree holds no `ask`, the body is read once more with every line the reader opens an
// `ask` container on written in the spelling this parser's narrower grammar reads (askOpener):
// the card a reader must answer stands wherever the reader's own container rule opens the block,
// so what this parser makes of that line decides nothing here.
//
// It is read-only. It reads markdown and names a place in it: it parses nothing differently,
// refuses nothing, mints no block id, stores nothing and touches no tree, budget or document of its
// caller's. Every existing parse, refusal and schema rule stands as it did; a body this names is
// refused by its caller, never here.
func FindDecisionCard(markdown string) (DecisionCard, bool) {
	card, err := findDecisionCard(markdown)
	if err != nil {
		// A panic in this read is this package's bug, never a shape of the body: the body keeps
		// the verdict its own parse gave it, which is what it had before this reader stood here.
		return DecisionCard{}, false
	}
	return card, card.Position > 0
}

// findDecisionCard is FindDecisionCard with a panic in the read answered as an error, as every
// other entry point of this package answers one (recoverPanic).
func findDecisionCard(markdown string) (card DecisionCard, err error) {
	defer recoverPanic(&card, &err, "finding a decision card")
	source := []byte(nulAsReplacement(markdown))
	if card := cardInSource(source); card.Position > 0 {
		return card, nil
	}
	// The browser's parser opens an `ask` container on far more spellings than this one reads as
	// a typed directive - an opener carrying no attributes, a label before them, a space after
	// them - and the reader draws its decision card for every one of them, so a body holding one
	// is read again with those openers spelled as this parser reads them.
	if opened, rewritten := askOpenersRespelled(source); rewritten {
		return cardInSource(opened), nil
	}
	return DecisionCard{}, nil
}

// cardInSource is one reading of source: the place of the first top-level block whose tree holds
// an `ask`, and the zero DecisionCard where it holds none.
func cardInSource(source []byte) DecisionCard {
	front, rest, unclosedFrontmatter := parseFrontmatterBlock(source)
	source = source[rest:]
	// Front matter is the body's first top-level block, as it is in the tree Parse builds
	// (parseMarkdown), so the blocks goldmark reads after it are counted from 2.
	position := 0
	if front != nil {
		position = 1
	}
	for {
		// The read is blockReader's own, in the context every parse of a document's markdown runs
		// in (readContext), and goldmark's tree is taken as it is read (readSource): the bounds a
		// parse holds against that tree - blocks and marks nested past maxNesting, maxTypedNesting
		// and maxInlineNesting, the write's elements (ErrTooManyElements), the cells its tables
		// would be padded with (ErrTablePadding) - are refusals of what the tree holds, raised
		// once it is built, and none of them changes a block of it. So the tree a refused body
		// leaves is the tree the reader reads, and the refusals are simply not asked for here.
		pc, count := readContext(unclosedFrontmatter, NewWriteBudget(), 0)
		root := readSource(blockReader.md.Parser(), source, pc)
		blocks := 0
		for block := root.FirstChild(); block != nil; block = block.NextSibling() {
			blocks++
			if holdsAnAsk(block) {
				return DecisionCard{Position: position + blocks, Type: blockType(block)}
			}
		}
		if count == nil || !count.stopped {
			return DecisionCard{}
		}
		// The one bound that does change the tree is the count's guard: a body past
		// nodeGuard*MaxDocumentElements nodes leaves goldmark at the line it stopped on, and the
		// rest of the body stands in the tree as one raw block nothing read (elementBlockCounter).
		// Reading the body on from that line, on a budget of its own, reads the lines the guard
		// left - a body of a mebibyte can carry an `ask` well past them - while each round still
		// builds no more of a tree than the guard allows. Every round reads the lines a guard's
		// worth of nodes takes, so a body is read in bounded rounds, and the block the guard broke
		// off is read again as the next round's first block, so it is counted once.
		next := bytes.LastIndexByte(source[:count.at], '\n') + 1
		if next == 0 {
			// The guard passed on the round's own first line, which reading on would read again.
			return DecisionCard{}
		}
		position += blocks - 1
		source = source[next:]
		unclosedFrontmatter = false
	}
}

// holdsAnAsk reports whether block is a decision block or holds one wherever it stands: in a
// quote, in a list item, in another typed block. The decision block is an `ask` typed directive,
// which the parser makes for `:::ask{…}` alone; an opener the reader opens an `ask` on and this
// parser does not is written in that spelling before the body is read again (askOpener).
//
// It is the goldmark tree's twin of WalkAskBlocks, which walks the Proof tree of a parse that
// stood. It descends into blocks alone, as that walk descends into everything but text, since a
// text's inline nodes hold no block, so it recurses no deeper than the nesting guard lets blocks
// nest (maxNesting).
func holdsAnAsk(block ast.Node) bool {
	if directive, ok := block.(*typedDirective); ok && directive.Name == askBlockType {
		return true
	}
	for child := block.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Type() == ast.TypeBlock && holdsAnAsk(child) {
			return true
		}
	}
	return false
}

// askOpenersRespelled is source with every line the browser reader opens an `ask` container on
// (askOpener) written as this parser's grammar reads it, and whether any line changed. It keeps
// the lines and their order, and rewrites nothing else: the line endings, every other line and
// every other directive name stand as written, so the block the card is finally found in stands
// where it stands in the body, and a `:::note` the reader opens no `ask` on stays the unknown
// container it is.
func askOpenersRespelled(source []byte) ([]byte, bool) {
	var respelled []byte
	kept := 0
	for offset := 0; offset < len(source); {
		end := bytes.IndexByte(source[offset:], '\n')
		if end < 0 {
			end = len(source)
		} else {
			end += offset
		}
		line := source[offset:end]
		if opener, ok := askOpener(line); ok && !bytes.Equal(opener, line) {
			respelled = append(respelled, source[kept:offset]...)
			respelled = append(respelled, opener...)
			kept = end
		}
		offset = end + 1
	}
	if respelled == nil {
		return nil, false
	}
	return append(respelled, source[kept:]...), true
}

// askOpener is line written as the opening line of an `ask` this parser reads, and whether the
// browser reader opens an `ask` container on line at all.
//
// The reader's opener is micromark's container directive (micromark-extension-directive's
// directive-container.js): a run of three or more colons, a name, an optional label (`[…]`,
// factory-label.js), optional attributes (`{…}`, factory-attributes.js, which a container reads
// on one line alone), and then spaces or tabs to the line's end. This parser reads the run, the
// name and attributes ending the line (ParseTypedDirectiveOpen), so the line is written back
// with its own colon run, `ask` and the attributes it carried - or `{}` where it carried none,
// and where this parser's narrower attribute grammar refuses the ones it carried, since the
// refusal reads the opener and never what it carries. The line keeps the indentation and the
// quote and list item markers it was written inside, which both parsers read off a line before
// they read what it opens, so the block opens where the reader opens it.
func askOpener(line []byte) ([]byte, bool) {
	// A carriage return ending the line is the line's ending to the reader and a character of
	// the line to goldmark, so the opener is read, and written back, without it.
	line = bytes.TrimRight(line, "\r")
	prefix := containerPrefixWidth(line)
	colons := colonRun(line[prefix:])
	if colons < 3 {
		return nil, false
	}
	tail := line[prefix+colons:]
	if len(tail) < len(askBlockType) || string(tail[:len(askBlockType)]) != askBlockType {
		return nil, false
	}
	tail = tail[len(askBlockType):]
	// The name ends where a label, attributes or the line's trailing spaces begin; anywhere else
	// it runs on into another name (`asks`, `ask-me`) or ends on a character no opener may carry
	// there, and the reader opens no `ask` either way.
	if len(tail) > 0 && tail[0] != '[' && tail[0] != '{' && tail[0] != ' ' && tail[0] != '\t' {
		return nil, false
	}
	if len(tail) > 0 && tail[0] == '[' {
		width, ok := directiveLabelWidth(tail)
		if !ok {
			return nil, false
		}
		tail = tail[width:]
	}
	var attributes []byte
	if len(tail) > 0 && tail[0] == '{' {
		width, ok := directiveAttributesWidth(tail)
		if !ok {
			return nil, false
		}
		attributes, tail = tail[:width], tail[width:]
	}
	// The spaces and tabs the reader ignores before the line's ending.
	if len(bytes.TrimRight(tail, " \t")) > 0 {
		return nil, false
	}
	opener := make([]byte, 0, prefix+colons+len(askBlockType)+max(len(attributes), len("{}")))
	opener = append(opener, line[:prefix+colons]...)
	opener = append(opener, askBlockType...)
	named := len(opener)
	opener = append(opener, attributes...)
	if _, _, ok := ParseTypedDirectiveOpen(string(opener[prefix:])); !ok {
		opener = append(opener[:named], "{}"...)
	}
	return opener, true
}

// containerPrefixWidth is how much of line stands before the block it opens: its indentation and
// the markers of the quotes and list items it is written inside, which both parsers read off the
// line first. A line this reads too much of opens no `ask` all the same, since the body is read
// again by goldmark, which holds every rule of its own about where a directive may stand - how
// far a line may be indented, what a fenced code block swallows, what a lazy line continues.
func containerPrefixWidth(line []byte) int {
	width := 0
	for width < len(line) {
		if char := line[width]; char == ' ' || char == '\t' || char == '>' {
			width++
			continue
		}
		marker := listMarkerWidth(line[width:])
		if marker == 0 {
			break
		}
		width += marker
	}
	return width
}

// listMarkerWidth is the width of the list item marker line opens with, the space or tab that
// ends it counted in, and 0 where line opens none: a bullet (`-`, `+`, `*`), or up to nine digits
// and `.` or `)`.
func listMarkerWidth(line []byte) int {
	digits := 0
	for digits < len(line) && digits < 9 && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	width := 0
	switch {
	case digits == 0 && (line[0] == '-' || line[0] == '+' || line[0] == '*'):
		width = 1
	case digits > 0 && digits < len(line) && (line[digits] == '.' || line[digits] == ')'):
		width = digits + 1
	default:
		return 0
	}
	if width == len(line) || line[width] != ' ' && line[width] != '\t' {
		return 0
	}
	return width + 1
}

// directiveLabelWidth is the width of the directive label text opens with, and whether it opens
// one at all (factory-label.js): brackets nest up to 32 deep, a backslash escapes `[`, `\` and
// `]`, and a label the line ends inside is no label, since a container directive reads its label
// on one line alone.
func directiveLabelWidth(text []byte) (int, bool) {
	balance := 0
	for offset := len("["); offset < len(text); offset++ {
		switch text[offset] {
		case '\\':
			offset++
		case '[':
			balance++
			if balance > 32 {
				return 0, false
			}
		case ']':
			if balance == 0 {
				return offset + 1, true
			}
			balance--
		case '\r':
			return 0, false
		}
	}
	return 0, false
}

// directiveAttributesWidth is the width of the attributes text opens with, and whether it opens
// any at all (factory-attributes.js, which a container directive reads with no line ending in
// it): `{`, then id (`#id`) and class (`.class`) shortcuts, names, and names carrying a value,
// each parted by spaces and tabs, and `}`.
func directiveAttributesWidth(text []byte) (int, bool) {
	offset := len("{")
	for {
		for offset < len(text) && (text[offset] == ' ' || text[offset] == '\t') {
			offset++
		}
		if offset == len(text) {
			return 0, false
		}
		char := text[offset]
		if char == '}' {
			return offset + 1, true
		}
		var width int
		var ok bool
		if char == '#' || char == '.' {
			width, ok = directiveShortcutWidth(text[offset+1:])
			width += len("#")
		} else {
			width, ok = directiveAttributeWidth(text[offset:])
		}
		if !ok {
			return 0, false
		}
		offset += width
	}
}

// directiveShortcutWidth is the width of the id or class standing after an attribute shortcut's
// marker, and whether one stands there at all: at least one character, ending at the next
// marker, a space, a tab or `}`, and holding no character that voids the shortcut.
func directiveShortcutWidth(text []byte) (int, bool) {
	if len(text) == 0 || strings.IndexByte("\"#'.<=>`} \t", text[0]) >= 0 {
		return 0, false
	}
	for width := 1; width < len(text); width++ {
		if strings.IndexByte("\"'<=>`", text[width]) >= 0 {
			return 0, false
		}
		if strings.IndexByte("#.} \t", text[width]) >= 0 {
			return width, true
		}
	}
	return 0, false
}

// directiveAttributeWidth is the width of the named attribute text opens with, and whether it
// opens one at all: a name, and, after `=`, a value.
func directiveAttributeWidth(text []byte) (int, bool) {
	width := 0
	for width < len(text) {
		char, size := utf8.DecodeRune(text[width:])
		if !attributeNameRune(char) {
			break
		}
		width += size
	}
	if width == 0 {
		// The attributes hold a character no name of theirs may open with, where the reader is
		// looking for the `}` that ends them.
		return 0, false
	}
	after := width
	for after < len(text) && (text[after] == ' ' || text[after] == '\t') {
		after++
	}
	if after == len(text) || text[after] != '=' {
		// A name alone is an attribute of its own, and what follows it is read as the next one.
		return width, true
	}
	value, ok := directiveValueWidth(text[after+1:])
	if !ok {
		return 0, false
	}
	return after + len("=") + value, true
}

// directiveValueWidth is the width of the attribute value text opens with, and whether it opens
// one at all: a `"` or `'` quoted value runs to its own quote, which `}`, a space or a tab must
// follow; a bare one runs to a space, a tab or `}` and holds no quote, `<`, `=`, `>` or backtick.
func directiveValueWidth(text []byte) (int, bool) {
	width := 0
	for width < len(text) && (text[width] == ' ' || text[width] == '\t') {
		width++
	}
	if width == len(text) || strings.IndexByte("<=>`}", text[width]) >= 0 {
		return 0, false
	}
	if quote := text[width]; quote == '"' || quote == '\'' {
		closing := bytes.IndexByte(text[width+1:], quote)
		if closing < 0 {
			return 0, false
		}
		width += len(`"`) + closing + len(`"`)
		if width == len(text) || strings.IndexByte("} \t", text[width]) < 0 {
			return 0, false
		}
		return width, true
	}
	for ; width < len(text); width++ {
		if strings.IndexByte("\"'<=>`", text[width]) >= 0 {
			return 0, false
		}
		if strings.IndexByte("} \t", text[width]) >= 0 {
			return width, true
		}
	}
	return 0, false
}

// attributeNameRune reports whether char stands in an attribute's name, as factory-attributes.js
// reads one: anything but whitespace and punctuation, with `-`, `.`, `:` and `_` as well. Its
// whitespace and punctuation are JavaScript's `\s` and Unicode's `P` and `S` categories, which
// Go's own classes answer for alike.
func attributeNameRune(char rune) bool {
	if unicode.IsSpace(char) {
		return false
	}
	if !unicode.IsPunct(char) && !unicode.IsSymbol(char) {
		return true
	}
	return char == '-' || char == '.' || char == ':' || char == '_'
}
