package pmdoc

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// DecisionCard is where the browser reader draws a decision card in a body: Position is the place,
// counted from 1, of the top-level block that holds the first `ask` the reader draws a card for,
// and Type is that block's type, named as the conversion names the Proof node it makes of such a
// block (readerBlockType). The decision block's own type is that block's type only where the
// decision block stands at the top of the body. A card nowhere is the zero DecisionCard.
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
				return DecisionCard{Position: position + blocks, Type: readerBlockType(block)}, nil
			}
		}
		if count == nil || !count.stopped {
			return DecisionCard{}, nil
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
			return DecisionCard{}, nil
		}
		position += blocks - 1
		source = source[next:]
		unclosedFrontmatter = false
	}
}

// holdsAnAsk reports whether block is a decision block or holds one wherever it stands: in a
// quote, in a list item, in another typed block. The decision block is an `ask` typed directive,
// which the parser makes for `:::ask{…}` alone, so a `::::ask` with no attributes is no decision
// block to either parser and draws no card (packages/dispatch/e2e/unknown-directive.stub.e2e.ts).
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

// readerBlockType names one of goldmark's blocks as the conversion names the Proof node it makes
// of it (parseBlock), so a refusal of a body the parse refused names its block the way a refusal of
// a body the parse read names the same block: a typed block is its own type, declared by the schema
// or not, since that is the block its author wrote. A block the conversion refuses rather than
// converts is named by its goldmark kind; no such block holds a decision block, since none of them
// holds a block at all.
func readerBlockType(block ast.Node) string {
	switch current := block.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return "paragraph"
	case *ast.Heading:
		return "heading"
	case *ast.Blockquote:
		return "blockquote"
	case *ast.List:
		if current.IsOrdered() {
			return "ordered_list"
		}
		return "bullet_list"
	case *ast.ListItem:
		return "list_item"
	case *ast.FencedCodeBlock, *ast.CodeBlock:
		return "code_block"
	case *ast.ThematicBreak:
		return "hr"
	case *extensionast.Footnote:
		return "footnote_definition"
	case *extensionast.Table:
		return "table"
	case *typedDirective:
		return current.Name
	}
	return block.Kind().String()
}
