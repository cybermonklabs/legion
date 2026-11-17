package specform

import (
	"fmt"
	"regexp"
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
	// a colon, one space and the definition.
	definitionTail = regexp.MustCompile(`^: (.+)$`)
)

// extraction is ExtractDocument's state: what it read, the bodies each node and kind had, and the
// level of the heading Terms whose part the walk is in (0 outside one).
type extraction struct {
	where      string
	out        Extracted
	bodies     map[[2]string][]string
	termsLevel int
}

// add keeps text once for each body: the same node, kind and body again is the same text, and the
// same node and kind with another body is another text, named "<node> (2)", "<node> (3)".
func (e *extraction) add(node string, kind Kind, body string) {
	key := [2]string{node, string(kind)}
	for _, seen := range e.bodies[key] {
		if seen == body {
			return
		}
	}
	e.bodies[key] = append(e.bodies[key], body)
	if n := len(e.bodies[key]); n > 1 {
		node = fmt.Sprintf("%s (%d)", node, n)
	}
	e.out.Texts = append(e.out.Texts, Text{Node: node, Kind: kind, Body: body, Where: e.where})
	e.out.Counts[kind]++
}

// ExtractDocument reads the texts of The Spec in version of the markdown of document. It reads the
// tree that every reader of a document's typed blocks reads (pmdoc.ParseRendering), so a block is
// a text of The Spec wherever the application reads one, also in a block quote, a list item or a
// callout, and nothing in code is. Each text is the text that a person reads (read): a character
// reference is its character, a hard break is a space, and code and links keep their marks.
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
// when it names a defined term (R50). Each text's Where is "<document> version <n>". A document
// that the parser refuses is an error: the application reads no block of it either.
func ExtractDocument(document string, version int, markdown string) (Extracted, error) {
	if !utf8.ValidString(markdown) {
		return Extracted{}, fmt.Errorf("%s version %d is not valid UTF-8", document, version)
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return Extracted{}, fmt.Errorf("read %s version %d: %w", document, version, err)
	}
	e := &extraction{
		where:  fmt.Sprintf("%s version %d", document, version),
		out:    Extracted{Counts: map[Kind]int{}},
		bodies: map[[2]string][]string{},
	}
	for _, kind := range kinds {
		e.out.Counts[kind] = 0
	}
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
			e.add(blockID(node), KindStatement, paragraphs(node))
		case "check":
			id := blockID(node)
			e.add(id, KindClaim, paragraphs(node))
			if run, _ := node.Attrs["run"].(string); run != "" {
				e.add(id, KindProcedure, run)
			}
			if example, _ := node.Attrs["negative_control"].(string); example != "" {
				e.add(id, KindFailingExample, example)
			}
		case "paragraph":
			e.paragraph(node)
		case "list_item":
			e.listItem(node, container)
		case "table":
			e.table(node)
		case "code_block", "frontmatter", "hr", "html":
		default:
			e.blocks(node.Children, node.Type)
		}
	}
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
	heading := read(node.Children)
	if heading == "Terms" {
		e.termsLevel = level
		return
	}
	if readsAsTerms(heading) {
		e.add(fmt.Sprintf("%q", heading), KindHeading, heading)
		return
	}
	component := componentHeading.FindStringSubmatch(heading)
	if component == nil {
		return
	}
	e.add(component[2], KindName, component[1])
	if len(after) > 0 && after[0].Type == "paragraph" {
		if purpose := read(after[0].Children); purpose != "" {
			e.add(component[2], KindPurpose, purpose)
		}
	}
}

// paragraph reads a paragraph: a purpose line, or under a heading Terms a line.
func (e *extraction) paragraph(node *pmdoc.Node) {
	paragraph := read(node.Children)
	if match := purposeLine.FindStringSubmatch(paragraph); match != nil {
		e.add(match[1], KindPurpose, match[2])
		return
	}
	e.line(paragraph)
}

// line keeps body, a block under a heading Terms that is no term line, as a text of Kind line
// named by its body in quotes. Outside a Terms part, or for a block with no text, it keeps nothing.
func (e *extraction) line(body string) {
	if e.termsLevel > 0 && body != "" {
		e.add(fmt.Sprintf("%q", body), KindLine, body)
	}
}

// listItem reads a list item of list, the type of its list: its first paragraph as the line of
// the item (itemLine), then its other blocks.
func (e *extraction) listItem(node *pmdoc.Node, list string) {
	children := node.Children
	if len(children) > 0 && children[0].Type == "paragraph" {
		e.itemLine(children[0], list)
		children = children[1:]
	}
	e.blocks(children, node.Type)
}

// itemLine reads the first paragraph of a list item of list: bold text, its id in code font and
// " · under:" are an entry line, a statement of the bold text. Bold text, ": " and a definition in
// an item of a bullet list are a term line: under a heading Terms a term, and elsewhere a text of
// Kind term_line (termLineElsewhere). Under a heading Terms any other item is a line.
func (e *extraction) itemLine(paragraph *pmdoc.Node, list string) {
	lead, rest := strongLead(paragraph.Children)
	strong, after := read(lead), inline(rest)
	if strong != "" {
		if match := entryTail.FindStringSubmatch(after); match != nil {
			e.add(match[1], KindStatement, strong)
			return
		}
	}
	if strong != "" && list == "bullet_list" {
		if match := definitionTail.FindStringSubmatch(after); match != nil {
			if definition := strings.TrimSpace(match[1]); definition != "" {
				if e.termsLevel == 0 {
					e.termLineElsewhere(strong, definition)
					return
				}
				e.out.Terms = append(e.out.Terms, Term{Name: strong, Definition: definition, Where: e.where})
				e.out.Counts[KindDefinition]++
				return
			}
		}
	}
	e.line(read(paragraph.Children))
}

// termLineElsewhere keeps a term line outside a Terms part, of its term name and its definition,
// as a text of Kind term_line named by the name, with the definition as its body. It is no
// definition and adds no term. Check refuses it when it names a defined term, since a reader would
// take it for a second definition. Two such lines of one name are two texts.
func (e *extraction) termLineElsewhere(name, definition string) {
	e.out.Texts = append(e.out.Texts, Text{Node: name, Kind: KindTermLine, Body: definition, Where: e.where})
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
	var rows [][]string
	for _, row := range node.Children {
		var cells []string
		for _, cell := range row.Children {
			var parts []string
			for _, block := range cell.Children {
				parts = append(parts, read(block.Children))
			}
			cells = append(cells, strings.TrimSpace(strings.Join(parts, " ")))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || len(rows[0]) < 4 || rows[0][0] != "Id" || rows[0][1] != "Kind" || rows[0][2] != "Name" || rows[0][3] != "Purpose" {
		for _, cells := range rows {
			e.line(strings.Join(cells, " | "))
		}
		return
	}
	for _, cells := range rows[1:] {
		if len(cells) < 4 {
			continue
		}
		node := strings.Trim(cells[0], "`")
		if cells[2] != "" {
			e.add(node, KindName, cells[2])
		}
		if cells[3] != "" {
			e.add(node, KindPurpose, cells[3])
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

// paragraphs is the text of each paragraph of a typed block, one paragraph to a line.
func paragraphs(node *pmdoc.Node) string {
	var lines []string
	for _, child := range node.Children {
		if line := read(child.Children); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// strongLead splits inline content after the strong text it starts with, if any.
func strongLead(nodes []*pmdoc.Node) (lead, rest []*pmdoc.Node) {
	end := 0
	for end < len(nodes) && hasMark(nodes[end], "strong") {
		end++
	}
	return nodes[:end], nodes[end:]
}

// read is inline content as a person reads it (inline), without the white space at its ends.
func read(nodes []*pmdoc.Node) string {
	return strings.TrimSpace(inline(nodes))
}

// inline is inline content as a person reads it: each run's characters, which already hold the
// character a reference names and no backslash of an escape, a hard break as a space (a soft one
// is a space in the tree), code in code font, as ste-lint reads code, and each link as its text
// with its address after it, "[<text>](<address>)", so that the reference rules read the link. A
// link whose text is its address is its text.
func inline(nodes []*pmdoc.Node) string {
	var out strings.Builder
	for index := 0; index < len(nodes); {
		href := linkHref(nodes[index])
		end := index + 1
		for end < len(nodes) && linkHref(nodes[end]) == href {
			end++
		}
		if href == "" {
			writeRuns(&out, nodes[index:end])
		} else {
			var link strings.Builder
			writeRuns(&link, nodes[index:end])
			if words := link.String(); words == href || "mailto:"+words == href {
				out.WriteString(words)
			} else {
				out.WriteString("[" + words + "](" + href + ")")
			}
		}
		index = end
	}
	return out.String()
}

// writeRuns writes inline content with no link in it, as inline reads it: an image is its alt text
// and its address, as a link is, and a run of code is one code span.
func writeRuns(out *strings.Builder, nodes []*pmdoc.Node) {
	for index := 0; index < len(nodes); {
		node := nodes[index]
		if node.Type == "text" && hasMark(node, "inlineCode") {
			var code strings.Builder
			for ; index < len(nodes) && nodes[index].Type == "text" && hasMark(nodes[index], "inlineCode"); index++ {
				code.WriteString(nodes[index].Text)
			}
			out.WriteString(codeSpan(code.String()))
			continue
		}
		switch node.Type {
		case "text":
			out.WriteString(node.Text)
		case "hardbreak":
			out.WriteByte(' ')
		case "image":
			alt, _ := node.Attrs["alt"].(string)
			src, _ := node.Attrs["src"].(string)
			out.WriteString("![" + alt + "](" + src + ")")
		case "footnote_reference":
			label, _ := node.Attrs["label"].(string)
			out.WriteString("[^" + label + "]")
		case "html":
			value, _ := node.Attrs["value"].(string)
			out.WriteString(value)
		}
		index++
	}
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

// linkHref is the address of the link that a text run of inline content carries, or "".
func linkHref(node *pmdoc.Node) string {
	for _, mark := range node.Marks {
		if mark.Type == "link" {
			href, _ := mark.Attrs["href"].(string)
			return href
		}
	}
	return ""
}

func hasMark(node *pmdoc.Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}
