package specform

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Extracted is the texts of The Spec in one document, its term lines, and the count of each
// Kind it read, so that "no finding" and "nothing read" are different answers.
type Extracted struct {
	Texts  []Text
	Terms  []Term
	Counts map[Kind]int
}

var (
	// entryTail is what follows the bold statement of an entry line, the form of the how-to of The
	// Spec ("- **<statement>** `<id>` · under: …"): the id in code font, then " · under:".
	entryTail = regexp.MustCompile("^ `([^`]+)` · under:")
	// componentHeading is the text of a heading that names a component or an interface by its id.
	componentHeading = regexp.MustCompile("^(.+?) \\((?:component|interface) `([^`]+)`\\)$")
	purposeLine      = regexp.MustCompile(`^Purpose of ([^\s:]+): (.+)$`)
	// definitionTail is what follows the bold term of a term line ("- **<term>**: <definition>"):
	// a colon, one space and the definition, which can stand wholly in its footnote.
	definitionTail = regexp.MustCompile(`^: (.*)$`)
)

// extraction is ExtractDocument's state: what it read, the bodies each node and kind had, the
// level of the heading Terms whose part the walk is in (0 outside one), and the footnotes of the
// document.
type extraction struct {
	where      string
	out        Extracted
	bodies     map[[2]string][]string
	termsLevel int
	// footnotes is each footnote definition of the document by the key that a reference finds it
	// by (pmdoc.FootnoteLabelKey), the first one where two share a key, as the parser resolves a
	// reference; notes is the reading of each one that a text refers to, made once.
	footnotes map[string]*pmdoc.Node
	notes     map[string]reading
}

// add keeps a text once for each body and what it holds: the same node, kind, body and held
// again is the same text, and the same node and kind with another body or held is another text,
// named "<node> (2)", "<node> (3)".
func (e *extraction) add(node string, kind Kind, body string, held Held) {
	key := [2]string{node, string(kind)}
	seen := fmt.Sprintf("%q %q", body, held)
	if slices.Contains(e.bodies[key], seen) {
		return
	}
	e.bodies[key] = append(e.bodies[key], seen)
	if n := len(e.bodies[key]); n > 1 {
		node = fmt.Sprintf("%s (%d)", node, n)
	}
	e.out.Texts = append(e.out.Texts, Text{Node: node, Kind: kind, Body: body, Where: e.where, Held: held})
	e.out.Counts[kind]++
}

// UnreadableError is ExtractDocument's answer for a version that the parser refuses, which no
// reader of the application reads either.
type UnreadableError struct {
	Document string
	Version  int
	Err      error
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("read %s version %d: %v", e.Document, e.Version, e.Err)
}

func (e *UnreadableError) Unwrap() error { return e.Err }

// Finding is the finding of a version that the parser refuses, which a judgment that names the
// version refuses: Dispatch cannot judge a text that it cannot read, and the way out is a new
// version of the document.
func (e *UnreadableError) Finding() Finding {
	return Finding{
		Node: e.Document, Kind: KindDocument, Where: fmt.Sprintf("%s version %d", e.Document, e.Version), Rule: ruleUnreadable,
		Detail: fmt.Sprintf("Dispatch cannot read this version (%v), so it cannot judge its texts. Write a new version of the document", e.Err),
	}
}

// ExtractDocument reads the texts of The Spec in version of the markdown of document. It reads the
// tree that every reader of a document's typed blocks reads (pmdoc.ParseRendering), so a block is
// a text of The Spec wherever the application reads one, also in a block quote, a list item or a
// callout, and nothing in code is. Each text is the text that a person reads (inline): a link is
// its words, an image its alternative text, a character reference its character and a hard break
// a space. Its Body keeps code in code font, for the rules of a sentence and ste-lint, and its
// Held.Shown is the same words exactly as the page shows them, for the reference rules. Each
// address and title of its links and images is held beside the words (Held), and the footnote that
// it refers to is part of it. The run and the negative_control of a check are attributes, which a
// text holds as they are written.
//
// The texts are each requirement block (a statement, of its paragraphs), each entry line of the
// how-to (a statement, of its bold text), each check block (a claim of its paragraphs, a procedure
// of its run and a failing example of its negative_control), each heading of a component or an
// interface (its name, and a purpose of the paragraph under it), each "Purpose of <id>:"
// paragraph, and each row of a table whose head starts Id, Kind, Name and Purpose (a name and a
// purpose). A block is named by its id. The term lines under a heading Terms are Terms, counted as
// Kind definition. Each other paragraph, list item or table row under that heading is a text of
// Kind line, named by its text in quotes: a term line has one form, "- **<term>**: <definition>",
// and a line in any other form is a break of the form, not a term. A heading whose text reads as
// Terms in another form (readsAsTerms) is a text of Kind heading, named by its text in quotes, and
// no Terms part: the heading Terms has one form too. A term line outside a Terms part is a text of
// Kind term_line (termLineElsewhere): it is no definition and adds no term, and Check refuses it
// when it names a defined term (R50). A heading opens and closes a part of Terms only among the
// blocks of its own container (container). Each text's Where is "<document> version <n>". A
// document that the parser refuses is an *UnreadableError: the application reads no block of it
// either.
func ExtractDocument(document string, version int, markdown string) (Extracted, error) {
	if !utf8.ValidString(markdown) {
		return Extracted{}, &UnreadableError{document, version, errors.New("it is not valid UTF-8")}
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return Extracted{}, &UnreadableError{document, version, err}
	}
	e := &extraction{
		where:     fmt.Sprintf("%s version %d", document, version),
		out:       Extracted{Counts: map[Kind]int{}},
		bodies:    map[[2]string][]string{},
		footnotes: map[string]*pmdoc.Node{},
		notes:     map[string]reading{},
	}
	for _, kind := range kinds {
		e.out.Counts[kind] = 0
	}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "footnote_definition" {
			label, _ := node.Attrs["label"].(string)
			if key := pmdoc.FootnoteLabelKey(label); e.footnotes[key] == nil {
				e.footnotes[key] = node
			}
		}
		return true
	})
	e.blocks(tree.Children, "")
	return e.out, nil
}

// blocks reads each block of one container in document order. container is the container's node
// type, so a list item knows its list.
func (e *extraction) blocks(nodes []*pmdoc.Node, container string) {
	for index, node := range nodes {
		switch node.Type {
		case "heading":
			e.heading(node, nodes[index+1:])
		case "requirement":
			body, held := e.text(e.paragraphs(node)...)
			e.add(blockID(node), KindStatement, body, held)
		case "check":
			id := blockID(node)
			body, held := e.text(e.paragraphs(node)...)
			e.add(id, KindClaim, body, held)
			if run, _ := node.Attrs["run"].(string); run != "" {
				e.add(id, KindProcedure, run, Held{})
			}
			if example, _ := node.Attrs["negative_control"].(string); example != "" {
				e.add(id, KindFailingExample, example, Held{})
			}
		case "paragraph":
			e.paragraph(node)
		case "list_item":
			e.listItem(node, container)
		case "table":
			e.table(node)
		case "footnote_definition":
			// A footnote is part of each text that refers to it (note), so it is no line of the part
			// of Terms where it is written.
			e.container(node.Children, node.Type, 0)
		case "code_block", "frontmatter", "hr", "html":
		default:
			e.container(node.Children, node.Type, e.termsLevel)
		}
	}
}

// container reads the blocks of a container that starts in the part of Terms of the given level
// (0 for none). A heading in it opens or closes a part only up to the container's end, and the
// blocks after the container are in the part that they were in before it.
func (e *extraction) container(nodes []*pmdoc.Node, container string, termsLevel int) {
	outer := e.termsLevel
	e.termsLevel = termsLevel
	e.blocks(nodes, container)
	e.termsLevel = outer
}

// heading reads a heading, with the blocks after it in its container: the heading Terms opens a
// Terms part, which the next heading of its level or above closes, a heading that reads as Terms
// in another form is a text of Kind heading, and a heading of a component or an interface is a
// name, with the paragraph right under it as its purpose.
func (e *extraction) heading(node *pmdoc.Node, after []*pmdoc.Node) {
	level := headingLevel(node)
	if e.termsLevel > 0 && level <= e.termsLevel {
		e.termsLevel = 0
	}
	heading := e.read(node.Children)
	if heading.words == "Terms" {
		e.termsLevel = level
		return
	}
	if readsAsTerms(heading.words) {
		e.add(fmt.Sprintf("%q", heading.words), KindHeading, heading.words, Held{})
		return
	}
	component := componentHeading.FindStringSubmatch(heading.words)
	if component == nil {
		return
	}
	heading = heading.part(0, len(heading.words)-len(component[1]))
	name, held := e.text(heading)
	e.add(component[2], KindName, name, held)
	if len(after) > 0 && after[0].Type == "paragraph" {
		if purpose := e.read(after[0].Children); purpose.words != "" {
			body, held := e.text(purpose)
			e.add(component[2], KindPurpose, body, held)
		}
	}
}

// paragraph reads a paragraph: a purpose line, or under a heading Terms a line.
func (e *extraction) paragraph(node *pmdoc.Node) {
	paragraph := e.read(node.Children)
	if match := purposeLine.FindStringSubmatch(paragraph.words); match != nil {
		paragraph = paragraph.part(len(paragraph.words)-len(match[2]), 0)
		body, held := e.text(paragraph)
		e.add(match[1], KindPurpose, body, held)
		return
	}
	e.line(paragraph.words)
}

// line keeps body, a block under a heading Terms that is no term line, as a text of Kind line
// named by its body in quotes. Outside a Terms part, or for a block with no text, it keeps nothing.
func (e *extraction) line(body string) {
	if e.termsLevel > 0 && body != "" {
		e.add(fmt.Sprintf("%q", body), KindLine, body, Held{})
	}
}

// listItem reads a list item of list, the type of its list: its first paragraph as the line of
// the item (itemLine), then its other blocks, as a container.
func (e *extraction) listItem(node *pmdoc.Node, list string) {
	children := node.Children
	if len(children) > 0 && children[0].Type == "paragraph" {
		e.itemLine(children[0], list)
		children = children[1:]
	}
	e.container(children, node.Type, e.termsLevel)
}

// itemLine reads the first paragraph of a list item of list: bold text, its id in code font and
// " · under:" are an entry line, a statement of the bold text. Bold text, ": " and a definition in
// an item of a bullet list are a term line: under a heading Terms a term, and elsewhere a text of
// Kind term_line (termLineElsewhere). Under a heading Terms any other item is a line. The name of a
// term is its bold text as the page shows it, so code font makes no second name of a term (R50). A
// footnote of a term line is part of its definition before the line is read, so a definition can
// stand wholly in its footnote.
func (e *extraction) itemLine(paragraph *pmdoc.Node, list string) {
	lead, rest := strongLead(paragraph.Children)
	strong, after := e.read(lead), e.inline(rest)
	if strong.words != "" {
		if match := entryTail.FindStringSubmatch(after.words); match != nil {
			body, held := e.text(strong)
			e.add(match[1], KindStatement, body, held)
			return
		}
	}
	if strong.words != "" && list == "bullet_list" {
		if match := definitionTail.FindStringSubmatch(after.words); match != nil {
			tail := after.part(len(after.words)-len(match[1]), 0)
			tail.words, tail.shown = strings.TrimSpace(tail.words), strings.TrimSpace(tail.shown)
			tail.notes = append(slices.Clip(strong.notes), after.notes...)
			if definition, held, footnotes := e.noted(tail); definition != "" {
				if e.termsLevel == 0 {
					e.termLineElsewhere(strong.shown, definition, held)
					return
				}
				nameHeld := strong.held
				nameHeld.Shown = strong.shown
				e.out.Terms = append(e.out.Terms, Term{Name: strong.shown, Definition: definition, Where: e.where, NameHeld: nameHeld, DefinitionHeld: held, Footnotes: footnotes})
				e.out.Counts[KindDefinition]++
				return
			}
		}
	}
	e.line(e.read(paragraph.Children).words)
}

// termLineElsewhere keeps a term line outside a Terms part, of its term name, its definition and
// what the definition holds, as a text of Kind term_line named by the name, with the definition as
// its body. It is no definition and adds no term. Check refuses it when it names a defined term,
// since a reader would take it for a second definition. Two such lines of one name are two texts.
func (e *extraction) termLineElsewhere(name, definition string, held Held) {
	e.out.Texts = append(e.out.Texts, Text{Node: name, Kind: KindTermLine, Body: definition, Where: e.where, Held: held})
}

// readsAsTerms reports whether heading, the text of a heading as a person reads it and not
// "Terms", reads as Terms: the word terms in any case, after one last colon is taken away. Another
// word, such as Glossary, or Terms in a longer text, does not.
func readsAsTerms(heading string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(heading, ":")), "terms")
}

// table reads a table: the rows of a table whose head starts Id, Kind, Name and Purpose are each
// a name and a purpose of the component that its Id cell names; under a heading Terms, each row
// of any other table is a line, of its cells.
func (e *extraction) table(node *pmdoc.Node) {
	var rows [][]reading
	for _, row := range node.Children {
		var cells []reading
		for _, cell := range row.Children {
			var parts []reading
			for _, block := range cell.Children {
				parts = append(parts, e.read(block.Children))
			}
			cells = append(cells, joined(" ", parts))
		}
		rows = append(rows, cells)
	}
	words := func(cells []reading) []string {
		texts := make([]string, len(cells))
		for index, cell := range cells {
			texts[index] = cell.words
		}
		return texts
	}
	if len(rows) == 0 || len(rows[0]) < 4 || !slices.Equal(words(rows[0])[:4], []string{"Id", "Kind", "Name", "Purpose"}) {
		for _, cells := range rows {
			e.line(strings.Join(words(cells), " | "))
		}
		return
	}
	for _, cells := range rows[1:] {
		if len(cells) < 4 {
			continue
		}
		node := strings.Trim(cells[0].words, "`")
		if cells[2].words != "" {
			name, held := e.text(cells[2])
			e.add(node, KindName, name, held)
		}
		if cells[3].words != "" {
			purpose, held := e.text(cells[3])
			e.add(node, KindPurpose, purpose, held)
		}
	}
}

// blockID is the id of a typed block, which the parser gives each block that the markdown names
// none for, as it does for every reader.
func blockID(node *pmdoc.Node) string {
	id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	return id
}

// headingLevel is the level of a heading, 1 to 6.
func headingLevel(node *pmdoc.Node) int {
	switch level := node.Attrs["level"].(type) {
	case int:
		return level
	case int64:
		return int(level)
	case float64:
		return int(level)
	}
	return 1
}

// paragraphs is the reading of each paragraph of a typed block.
func (e *extraction) paragraphs(node *pmdoc.Node) []reading {
	var readings []reading
	for _, child := range node.Children {
		readings = append(readings, e.read(child.Children))
	}
	return readings
}

// strongLead splits inline content after the strong text it starts with, if any.
func strongLead(nodes []*pmdoc.Node) (lead, rest []*pmdoc.Node) {
	end := 0
	for end < len(nodes) && hasMark(nodes[end], "strong") {
		end++
	}
	return nodes[:end], nodes[end:]
}

// reading is inline content as a person reads it: its words, with code in code font, the same
// words exactly as the page shows them (shown), the key of each footnote that it refers to, in
// order, and what it holds beside its words.
type reading struct {
	words, shown string
	notes        []string
	held         Held
}

// part is r cut to its words after the first before bytes and before the last after bytes. Its
// shown words lose the same ends without their marks of code font. That is exact for the ends that
// the forms of a text cut, whose code is an id in code font, and an end that it does not fit
// leaves the shown words whole, so the reference rules read more and never less.
func (r reading) part(before, after int) reading {
	cutBefore := strings.ReplaceAll(r.words[:before], "`", "")
	cutAfter := strings.ReplaceAll(r.words[len(r.words)-after:], "`", "")
	r.words = r.words[before : len(r.words)-after]
	r.shown = strings.TrimSuffix(strings.TrimPrefix(r.shown, cutBefore), cutAfter)
	return r
}

// joined is readings read one after the other, their words parted by sep, and so are their shown
// words.
func joined(sep string, readings []reading) reading {
	var out reading
	var words, shown []string
	for _, each := range readings {
		if each.words != "" {
			words, shown = append(words, each.words), append(shown, each.shown)
		}
		out.notes = append(out.notes, each.notes...)
		out.held = out.held.with(each.held)
	}
	out.words, out.shown = strings.Join(words, sep), strings.Join(shown, sep)
	return out
}

// text is the body of a text of readings, the words of each on a line, then the words of each
// footnote that they refer to, once and in the order of the first reference, also a footnote
// that a footnote refers to, and everything they hold, with the same lines as the page shows them
// (Held.Shown).
func (e *extraction) text(readings ...reading) (string, Held) {
	body, held, _ := e.noted(readings...)
	return body, held
}

// noted is text, and how many footnotes gave words to the body.
func (e *extraction) noted(readings ...reading) (string, Held, int) {
	out := joined("\n", readings)
	var lines, shown []string
	if out.words != "" {
		lines, shown = append(lines, out.words), append(shown, out.shown)
	}
	footnotes := 0
	seen := map[string]bool{}
	// The list grows while it is read: a footnote can refer to another one.
	for index := 0; index < len(out.notes); index++ {
		key := out.notes[index]
		if seen[key] {
			continue
		}
		seen[key] = true
		note := e.note(key)
		if note.words != "" {
			lines, shown = append(lines, note.words), append(shown, note.shown)
			footnotes++
		}
		out.notes = append(out.notes, note.notes...)
		out.held = out.held.with(note.held)
	}
	out.held.Shown = strings.Join(shown, "\n")
	return strings.Join(lines, "\n"), out.held, footnotes
}

// note is the reading of the footnote whose key is key: the words of each paragraph and heading
// in it, one to a line, read once.
func (e *extraction) note(key string) reading {
	if note, read := e.notes[key]; read {
		return note
	}
	var parts []reading
	if definition := e.footnotes[key]; definition != nil {
		pmdoc.Walk(definition, func(node *pmdoc.Node) bool {
			if node.Type == "paragraph" || node.Type == "heading" {
				parts = append(parts, e.read(node.Children))
				return false
			}
			return node.Type != "code_block"
		})
	}
	e.notes[key] = joined("\n", parts)
	return e.notes[key]
}

// read is inline content as a person reads it (inline), without the white space at the ends of
// its words.
func (e *extraction) read(nodes []*pmdoc.Node) reading {
	out := e.inline(nodes)
	out.words, out.shown = strings.TrimSpace(out.words), strings.TrimSpace(out.shown)
	return out
}

// inline is inline content as a person reads it. Its words are each run's characters, which
// already hold the character a reference names and no backslash of an escape, a link's words
// without its address, an image's alternative text, a hard break as a space (a soft one is a
// space in the tree), and code in code font, as ste-lint reads code. Its shown words are the same
// with code as its characters alone, as the page shows them. A footnote reference is no word: the
// footnote is part of the text (note). It holds the address and the title of each link and image.
func (e *extraction) inline(nodes []*pmdoc.Node) reading {
	var out reading
	var words, shown strings.Builder
	write := func(text string) {
		words.WriteString(text)
		shown.WriteString(text)
	}
	for index := 0; index < len(nodes); {
		node := nodes[index]
		if node.Type == "text" && hasMark(node, "inlineCode") {
			var code strings.Builder
			for ; index < len(nodes) && nodes[index].Type == "text" && hasMark(nodes[index], "inlineCode"); index++ {
				code.WriteString(nodes[index].Text)
				out.held = linkHeld(out.held, nodes[index])
			}
			words.WriteString(codeSpan(code.String()))
			shown.WriteString(code.String())
			continue
		}
		out.held = linkHeld(out.held, node)
		switch node.Type {
		case "text":
			write(node.Text)
		case "hardbreak":
			write(" ")
		case "image":
			alt, _ := node.Attrs["alt"].(string)
			src, _ := node.Attrs["src"].(string)
			title, _ := node.Attrs["title"].(string)
			write(alt)
			out.held = out.held.withAddress(src).withTitle(title)
		case "footnote_reference":
			label, _ := node.Attrs["label"].(string)
			out.notes = append(out.notes, pmdoc.FootnoteLabelKey(label))
		case "html":
			value, _ := node.Attrs["value"].(string)
			write(value)
		}
		index++
	}
	out.words, out.shown = words.String(), shown.String()
	return out
}

// linkHeld is held with the address and the title of the link that node carries, if any.
func linkHeld(held Held, node *pmdoc.Node) Held {
	for _, mark := range node.Marks {
		if mark.Type == "link" {
			href, _ := mark.Attrs["href"].(string)
			title, _ := mark.Attrs["title"].(string)
			return held.withAddress(href).withTitle(title)
		}
	}
	return held
}

// codeSpan is code in code font: between runs of backticks longer than any run in it, with a space
// inside each run when the code starts or ends with a backtick.
func codeSpan(code string) string {
	longest, run := 0, 0
	for _, char := range code {
		if char != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(code, "`") || strings.HasSuffix(code, "`") {
		return fence + " " + code + " " + fence
	}
	return fence + code + fence
}

func hasMark(node *pmdoc.Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}
