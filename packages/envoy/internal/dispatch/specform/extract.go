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
	// entryLine is a statement as the how-to of The Spec writes one on a line.
	entryLine = regexp.MustCompile("^\\s*[-*+] \\*\\*(.+?)\\*\\* `([^`]+)` · under:")
	// heading is a markdown heading, and componentHeading one that names a component or an
	// interface by its id.
	heading          = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	componentHeading = regexp.MustCompile("^(.+?) \\((?:component|interface) `([^`]+)`\\)$")
	purposeLine      = regexp.MustCompile(`^Purpose of ([^\s:]+): (.+)$`)
	termLine         = regexp.MustCompile(`^\s*[-*+] \*\*(.+?)\*\*: (.+)$`)
)

// extraction is ExtractDocument's state: what it read, and the bodies each node and kind had.
type extraction struct {
	where  string
	out    Extracted
	bodies map[[2]string][]string
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

// ExtractDocument reads the texts of The Spec in version of the markdown of document, outside
// code fences: each requirement block (a statement, of every line of the block); each entry line
// of the how-to (a statement, of its bold text); each check block (a claim of its lines, a
// procedure of its run and a failing example of its negative_control); each heading of a
// component or an interface (its name, and a purpose of the paragraph under it); each
// "Purpose of <id>:" line; and each row of a table whose head starts Id, Kind, Name and Purpose
// (a name and a purpose). A typed block with no id is named "<type> <ordinal>". The term lines
// under a heading Terms are Terms, counted as Kind definition. Each other line under that heading
// that is not blank and not a heading is a text of Kind line, named by the line in quotes: a term
// line has one form, "- **<term>**: <definition>", and a line in any other form is a break of the
// form, not a term. A heading that reads as Terms in another form (readsAsTerms) is a text of Kind
// heading, named by its line in quotes, and no Terms part: the heading Terms has one form too. A
// line in the form of a term line anywhere else, under another heading, under none or in a typed
// block, is a text of Kind term_line (termLineElsewhere): it adds no term, and it is still a
// definition (R50). Each text's Where is "<document> version <n>".
func ExtractDocument(document string, version int, markdown string) (Extracted, error) {
	if !utf8.ValidString(markdown) {
		return Extracted{}, fmt.Errorf("%s version %d is not valid UTF-8", document, version)
	}
	e := &extraction{
		where:  fmt.Sprintf("%s version %d", document, version),
		out:    Extracted{Counts: map[Kind]int{}},
		bodies: map[[2]string][]string{},
	}
	for _, kind := range kinds {
		e.out.Counts[kind] = 0
	}
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	ordinals := map[string]int{}
	fence, termsLevel := "", 0
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if marker := fenceMarker(trimmed); marker != "" {
			fence = marker
			continue
		}
		if strings.HasPrefix(trimmed, ":::") {
			name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(trimmed)
			if !ok || (name != "requirement" && name != "check") {
				continue
			}
			ordinals[name]++
			node, _ := attrs[pmdoc.BlockIDAttr].(string)
			if node == "" {
				node = fmt.Sprintf("%s %d", name, ordinals[name])
			}
			end := blockEnd(lines, index, colonCount(trimmed))
			body := strings.Join(lines[index+1:end], "\n")
			e.termLinesIn(lines[index+1 : end])
			index = end
			if name == "requirement" {
				e.add(node, KindStatement, body)
				continue
			}
			e.add(node, KindClaim, body)
			if run, _ := attrs["run"].(string); run != "" {
				e.add(node, KindProcedure, run)
			}
			if example, _ := attrs["negative_control"].(string); example != "" {
				e.add(node, KindFailingExample, example)
			}
			continue
		}
		if match := heading.FindStringSubmatch(trimmed); match != nil {
			level := len(match[1])
			if termsLevel > 0 && level <= termsLevel {
				termsLevel = 0
			}
			if match[2] == "Terms" {
				termsLevel = level
				continue
			}
			if readsAsTerms(match[2]) {
				e.add(fmt.Sprintf("%q", trimmed), KindHeading, trimmed)
				continue
			}
			if component := componentHeading.FindStringSubmatch(match[2]); component != nil {
				e.add(component[2], KindName, component[1])
				if paragraph := paragraphAfter(lines, index); paragraph != "" {
					e.add(component[2], KindPurpose, paragraph)
				}
			}
			continue
		}
		if match := entryLine.FindStringSubmatch(line); match != nil {
			e.add(match[2], KindStatement, match[1])
			continue
		}
		if match := purposeLine.FindStringSubmatch(trimmed); match != nil {
			e.add(match[1], KindPurpose, match[2])
			continue
		}
		if cells := tableRow(trimmed); len(cells) >= 4 && cells[0] == "Id" && cells[1] == "Kind" && cells[2] == "Name" && cells[3] == "Purpose" {
			index = e.componentTable(lines, index)
			continue
		}
		if termsLevel > 0 && trimmed != "" {
			if match := termLine.FindStringSubmatch(line); match != nil {
				e.out.Terms = append(e.out.Terms, Term{Name: strings.TrimSpace(match[1]), Definition: strings.TrimSpace(match[2]), Where: e.where})
				e.out.Counts[KindDefinition]++
			} else {
				e.add(fmt.Sprintf("%q", trimmed), KindLine, trimmed)
			}
		} else if match := termLine.FindStringSubmatch(line); match != nil {
			e.termLineElsewhere(match)
		}
	}
	return e.out, nil
}

// termLineElsewhere keeps a line in the form of a term line outside a Terms part, of its termLine
// match, as a text of Kind term_line named by its term, with its definition as its body. It adds
// no term; Check judges it as a second definition. Two such lines of one name are two texts.
func (e *extraction) termLineElsewhere(match []string) {
	e.out.Texts = append(e.out.Texts, Text{Node: strings.TrimSpace(match[1]), Kind: KindTermLine, Body: strings.TrimSpace(match[2]), Where: e.where})
}

// termLinesIn keeps each line of body, the lines of a typed block, that is in the form of a term
// line outside a code fence (termLineElsewhere).
func (e *extraction) termLinesIn(body []string) {
	fence := ""
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		switch marker := fenceMarker(trimmed); {
		case fence != "":
			if closesFence(trimmed, fence) {
				fence = ""
			}
		case marker != "":
			fence = marker
		default:
			if match := termLine.FindStringSubmatch(line); match != nil {
				e.termLineElsewhere(match)
			}
		}
	}
}

// closesFence reports whether trimmed closes the code fence that opened with fence: a run of the
// same character, at least as long.
func closesFence(trimmed, fence string) bool {
	return strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == ""
}

// fenceMarker is the opening run of a code fence line, "```" or "~~~" or longer, or "".
func fenceMarker(trimmed string) string {
	for _, char := range []string{"`", "~"} {
		if strings.HasPrefix(trimmed, char+char+char) {
			return char + strings.Repeat(char, len(trimmed)-len(strings.TrimLeft(trimmed, char))-1)
		}
	}
	return ""
}

// emphasisMarks removes the marks of markdown emphasis from a heading's text.
var emphasisMarks = strings.NewReplacer("*", "", "_", "")

// readsAsTerms reports whether the text of a heading other than "Terms" reads as Terms: the word
// terms in any case, after the marks of emphasis and one last colon are taken away. Another word,
// such as Glossary, or Terms in a longer text, does not.
func readsAsTerms(text string) bool {
	text = strings.TrimSpace(emphasisMarks.Replace(text))
	text = strings.TrimSpace(strings.TrimSuffix(text, ":"))
	return strings.EqualFold(text, "terms")
}

func colonCount(line string) int {
	return len(line) - len(strings.TrimLeft(line, ":"))
}

// blockEnd is the index of the line that closes the typed block opened at open with colons
// colons: a line of colons alone, at least as many. A block that no line closes runs to the end.
func blockEnd(lines []string, open, colons int) int {
	for index := open + 1; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed != "" && strings.Trim(trimmed, ":") == "" && len(trimmed) >= colons {
			return index
		}
	}
	return len(lines)
}

// paragraphAfter is the paragraph under the heading at index: its lines up to a blank line, a
// heading or a typed block, after the blank lines that precede it.
func paragraphAfter(lines []string, index int) string {
	next := index + 1
	for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
		next++
	}
	var paragraph []string
	for ; next < len(lines); next++ {
		trimmed := strings.TrimSpace(lines[next])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ":::") {
			break
		}
		paragraph = append(paragraph, lines[next])
	}
	return strings.Join(paragraph, "\n")
}

// tableRow is the trimmed cells of a markdown table row, split at each "|" that no backslash
// escapes, or nil for a line that is no row.
func tableRow(trimmed string) []string {
	if !strings.HasPrefix(trimmed, "|") {
		return nil
	}
	content := strings.TrimSuffix(trimmed[1:], "|")
	var cells []string
	from := 0
	for index := range len(content) {
		if content[index] == '|' && (index == 0 || content[index-1] != '\\') {
			cells = append(cells, strings.TrimSpace(content[from:index]))
			from = index + 1
		}
	}
	return append(cells, strings.TrimSpace(content[from:]))
}

// componentTable reads the rows of the components table whose head is at index, each a name and
// a purpose of the component its Id cell names, and is the index of its last row.
func (e *extraction) componentTable(lines []string, index int) int {
	for index+1 < len(lines) {
		cells := tableRow(strings.TrimSpace(lines[index+1]))
		if cells == nil {
			break
		}
		index++
		if len(cells) < 4 || strings.Trim(strings.Join(cells, ""), "-: ") == "" {
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
	return index
}
