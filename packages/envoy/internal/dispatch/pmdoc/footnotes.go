package pmdoc

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// footnoteReferenceParser reads a footnote reference, `[^label]`, or `![^label]` after an image's
// bang, and resolves its label from maps of the parse's definitions built once
// (footnoteDefinitionsOf): a label equal byte for byte to a definition's resolves to the first such
// definition, as goldmark's footnote parser resolves it, and one that matches a definition's only
// up to case (footnoteLabelKey) to the first definition with that key, as the browser editor's
// parser resolves it, where goldmark read `[^A]` as text beside `[^a]: `. Such a reference carries
// its label as written (referenceLabelAttr), which the browser editor keeps on it. It is a parser
// of its own, not goldmark's wrapped: goldmark's found the definition by walking every one of them
// for each reference, so a body of n references and their n definitions cost it n²/2 comparisons,
// and after a bang it read the third byte as the caret without asking whether the second opened
// the label, so `!x^a]` beside `[^a]: ` lost its `x` to a reference and every `!x^` scanned the
// rest of its line for a closer.
type footnoteReferenceParser struct{}

// Trigger is the bytes a reference starts with: the label's opener, or an image's bang before it.
func (footnoteReferenceParser) Trigger() []byte { return []byte{'!', '['} }

// footnoteLabelKey is a footnote label as the browser editor's parser compares it: with full case
// mapping, lowered and then uppercased, so `ß` matches `SS` and `İ` matches `i̇`, which simple case
// folding keeps apart.
func footnoteLabelKey(label string) string {
	return cases.Upper(language.Und).String(cases.Lower(language.Und).String(label))
}

// referenceLabelAttr is the label a reference resolved by footnoteReferenceParser only up to case
// is written with.
var referenceLabelAttr = []byte("pmdoc-reference-label")

func (footnoteReferenceParser) Parse(parent ast.Node, block gmtext.Reader, pc parser.Context) ast.Node {
	line, segment := block.PeekLine()
	opener := 0
	if len(line) > 0 && line[0] == '!' {
		opener++
	}
	// start is the caret's index, after the opener.
	start := opener + 1
	if start+1 >= len(line) || line[opener] != '[' || line[start] != '^' {
		return nil
	}
	closure := util.FindClosure(line[start+1:], '[', ']', false, false) //nolint:staticcheck
	if closure < 0 {
		return nil
	}
	label := line[start+1 : start+1+closure]
	definitions := footnoteDefinitionsOf(pc)
	// Goldmark compares the label as its reader gives the segment's value back.
	written := block.Value(gmtext.NewSegment(segment.Start+start+1, segment.Start+start+1+closure))
	definition, exact := definitions.byLabel[string(written)], true
	if definition == nil {
		definition, exact = definitions.byKey[footnoteLabelKey(string(label))], false
	}
	if definition == nil {
		return nil
	}
	if definition.Index < 0 {
		list := definition.Parent().(*extensionast.FootnoteList)
		list.Count++
		definition.Index = list.Count
	}
	block.Advance(start + 1 + closure + 1)
	link := extensionast.NewFootnoteLink(definition.Index)
	if !exact {
		link.SetAttribute(referenceLabelAttr, string(label))
	}
	if line[0] == '!' {
		parent.AppendChild(parent, ast.NewTextSegment(gmtext.NewSegment(segment.Start, segment.Start+1)))
	}
	return link
}

// footnoteDefinitions is each footnote definition in goldmark's list by its label as written
// (byLabel) and by its label's key (byKey, footnoteLabelKey), the first in the list where two share
// one: goldmark's list holds them in the order they close, which is the order of their slots.
type footnoteDefinitions struct {
	byLabel, byKey map[string]*extensionast.Footnote
}

// footnoteKeysKey holds a parse's footnoteDefinitionsOf.
var footnoteKeysKey = parser.NewContextKey()

// footnoteDefinitionsOf is the parse's footnoteDefinitions. It is built once per parse, on its
// first reference: inline text is parsed after every block has closed, so each definition has its
// slot by then.
func footnoteDefinitionsOf(pc parser.Context) footnoteDefinitions {
	if definitions, ok := pc.Get(footnoteKeysKey).(footnoteDefinitions); ok {
		return definitions
	}
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	definitions := footnoteDefinitions{
		byLabel: make(map[string]*extensionast.Footnote, len(slots)),
		byKey:   make(map[string]*extensionast.Footnote, len(slots)),
	}
	for _, slot := range slots {
		definition, ok := slot.definition.(*extensionast.Footnote)
		if !ok {
			continue
		}
		if _, listed := definition.Parent().(*extensionast.FootnoteList); !listed {
			continue
		}
		if _, seen := definitions.byLabel[string(definition.Ref)]; !seen {
			definitions.byLabel[string(definition.Ref)] = definition
		}
		key := footnoteLabelKey(string(definition.Ref))
		if _, seen := definitions.byKey[key]; !seen {
			definitions.byKey[key] = definition
		}
	}
	pc.Set(footnoteKeysKey, definitions)
	return definitions
}

// footnotes is goldmark's footnote extension with its definition parser taking every space and
// tab after a definition's `]:` as part of its prefix, as the browser editor's parser does, so the
// definition's first block starts at the column its later lines are measured from: goldmark left
// them to that block, which read a fence opening the definition as indented by one (and dropped a
// space from each code line) and a list's marker one column in (which put its item's later blocks
// outside it).
type footnotes struct{}

func (footnotes) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(footnoteParserOptions()...)
}

// footnoteParserOptions registers footnotes: the definition parser (footnoteDefinitionParser) and
// the reference parser (footnoteReferenceParser), which numbers the definitions it refers to.
// Goldmark's footnote transformer is left out: it only orders the definitions, drops those nothing
// refers to and appends backlinks, and the parser keeps every definition where it is written
// (definitionsInPlace) and writes no backlink.
func footnoteParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(frontmatterAttempt{endsContainers{footnoteDefinitionParser{nestingGuard{BlockParser: extension.NewFootnoteBlockParser()}}}}, 999)),
		parser.WithInlineParsers(util.Prioritized(footnoteReferenceParser{}, 101)),
	}
}

type footnoteDefinitionParser struct{ parser.BlockParser }

// footnoteSlot stands where a footnote definition is written while the document parses:
// goldmark's footnote extension moves each definition it closes into the list it gathers at the
// document's end, and its transformer drops the ones nothing refers to. definitionsInPlace puts
// each back in its slot, where the browser editor's parser keeps it.
type footnoteSlot struct {
	ast.BaseBlock
	definition ast.Node
}

var (
	kindFootnoteSlot = ast.NewNodeKind("FootnoteSlot")
	footnoteSlotsKey = parser.NewContextKey()
)

func (s *footnoteSlot) Kind() ast.NodeKind { return kindFootnoteSlot }

func (s *footnoteSlot) Dump(source []byte, level int) { ast.DumpHelper(s, source, level, nil, nil) }

// Close leaves a slot where the definition is written and hands goldmark the definition from the
// document's level, where goldmark puts the list it gathers definitions in when it closes the first
// one: a list put inside a definition would leave the document with the definition that holds it,
// and goldmark reads no text in blocks outside the document. That list goes ahead of the blocks
// written, where no check of the block before another (interruptsOpenBlock) meets it while the
// document parses: goldmark put it at the document's end, between the blocks written before and
// after the definition closed.
func (p footnoteDefinitionParser) Close(node ast.Node, reader gmtext.Reader, pc parser.Context) {
	slot := &footnoteSlot{definition: node}
	node.Parent().InsertBefore(node.Parent(), node, slot)
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	pc.Set(footnoteSlotsKey, append(slots, slot))
	root := node.Parent()
	for root.Parent() != nil {
		root = root.Parent()
	}
	root.AppendChild(root, node)
	p.BlockParser.Close(node, reader, pc)
	if list := node.Parent(); list != root.FirstChild() {
		root.InsertBefore(root, root.FirstChild(), list)
	}
}

// definitionsInPlace puts each footnote definition back where it is written, in place of its slot,
// and removes the list goldmark gathered them in.
func definitionsInPlace(root ast.Node, context parser.Context) {
	slots, _ := context.Get(footnoteSlotsKey).([]*footnoteSlot)
	for _, slot := range slots {
		if parent := slot.definition.Parent(); parent != nil {
			parent.RemoveChild(parent, slot.definition)
		}
		slot.Parent().ReplaceChild(slot.Parent(), slot, slot.definition)
	}
	if list, ok := root.FirstChild().(*extensionast.FootnoteList); ok {
		root.RemoveChild(root, list)
	}
}

// Open reads the definition's opener without the padding a tab split by the containers' prefix
// leaves ahead of it: goldmark's parser measures where the definition's text starts from the line
// without that padding, so it would take the label's first characters as the text (`]: def`).
func (p footnoteDefinitionParser) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	offset := pc.BlockOffset()
	padded := segment.Padding > 0 && offset >= segment.Padding
	if padded {
		setPadding(reader, 0)
		pc.SetBlockOffset(offset - segment.Padding)
	}
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node == nil {
		if padded {
			setPadding(reader, segment.Padding)
			pc.SetBlockOffset(offset)
		}
		return node, state
	}
	if state&parser.HasChildren != 0 {
		line, _ := reader.PeekLine()
		skip := 0
		for skip < len(line) && (line[skip] == ' ' || line[skip] == '\t') {
			skip++
		}
		if skip < len(line) && !util.IsBlank(line[skip:]) {
			reader.Advance(skip)
		}
	}
	return node, state
}
