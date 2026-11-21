// Package prose reads the prose of a Markdown body: the sentences a writing check reads, and
// nothing a reader takes for code, a quotation, an address or markup.
//
// The body is read with pmdoc.Parse, the parser itemblocks.Check reads a body with, so a block is
// prose here exactly where the Dispatch reader draws it as prose. Prose is the text of:
//
//   - a paragraph (Kind "paragraph"), a footnote's paragraphs included;
//   - a heading (Kind "heading");
//   - a list item (Kind "list_item"): the item's own paragraphs make one segment, so the item's
//     last sentence is the last sentence of its last paragraph. A list nested in the item is an
//     item of its own;
//   - a table cell, header cells included (Kind "table_cell");
//   - a paragraph that stands directly in a typed block, a directive container such as
//     `:::callout` (Kind "directive"). A heading, list or table inside the container keeps its own
//     kind.
//
// Not prose, and left out with all its text:
//
//   - fenced and indented code (code_block);
//   - a block quote and everything in it: quoted words are someone else's;
//   - an HTML block, which the parser refuses, so it is read by the fallback below;
//   - inline HTML, an image (its alt text is not read), a footnote reference;
//   - a link's target: a link's text is prose, its href is not;
//   - directive attributes, which the tree keeps as attributes and never as text;
//   - front matter.
//
// Inline code and bare addresses stand inside a sentence as operands, so they are not dropped
// but replaced: each code span by CodeToken, and each bare address, a link whose text is its own
// URL, by URLToken. Each token is one word with no punctuation, so it keeps a sentence's word
// count and its grammar ("Combine CODE and CODE" ends in no conjunction), and it cannot end a
// sentence. A sentence that holds nothing but tokens and punctuation holds no prose and is left
// out.
//
// Each segment is split into sentences by the English Punkt model of
// github.com/jdkato/prose/v3/segment.
//
// A body the parser refuses (block HTML, an unknown directive, a shape the browser reader reads
// otherwise) is still read, by a line scan: blank lines split it into paragraphs, and the scan
// leaves out the lines of a fenced code block (from a line opening with ``` or ~~~ to the next
// such line), a paragraph whose first line opens with `<` (an HTML block), each line opening with
// `>` (a block quote) and each line opening with `:::` (a directive's fence and attributes). Each
// remaining paragraph is one "paragraph" segment, its lines joined by spaces, with each code span
// replaced by CodeToken. The scan does not know lists, tables or links.
package prose

import (
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/jdkato/prose/v3/segment"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The kinds of construct a sentence stands in (Sentence.Kind).
const (
	KindParagraph = "paragraph"
	KindHeading   = "heading"
	KindListItem  = "list_item"
	KindTableCell = "table_cell"
	KindDirective = "directive"
)

// CodeToken stands in a sentence where a code span stood, and URLToken where a bare address stood.
const (
	CodeToken = "CODE"
	URLToken  = "URL"
)

// Sentence is one sentence of a body's prose. Field is the field the body arrived in. Segment
// counts the body's prose segments from 0, in document order: one segment for each paragraph,
// heading, table cell and directive paragraph, and one for each list item's own paragraphs. Kind
// names the segment's construct (KindParagraph and the rest). Text is the sentence, and Start
// and End are its byte offsets in the segment's plain text, where a list item's paragraphs are
// joined by a blank line. Last reports whether the sentence is the last of its segment.
type Sentence struct {
	Field   string
	Segment int
	Kind    string
	Text    string
	Start   int
	End     int
	Last    bool
}

// ProseSentences is the sentences of body's prose, in document order, each carrying field.
func ProseSentences(field, body string) []Sentence {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	reader := &reader{field: field}
	doc, err := pmdoc.Parse(body)
	if err != nil {
		reader.lines(body)
	} else {
		reader.blocks(doc.Children, KindParagraph)
	}
	return reader.sentences
}

// reader gathers the sentences of one body, numbering its segments as it reads them.
type reader struct {
	field     string
	segments  int
	sentences []Sentence
}

// blocks reads nodes, where a paragraph is of kind paragraphKind: KindParagraph at the top, in a
// footnote and in a list item's nested blocks, and KindDirective directly inside a typed block.
func (r *reader) blocks(nodes []*pmdoc.Node, paragraphKind string) {
	for _, node := range nodes {
		switch node.Type {
		case "paragraph":
			r.segment(paragraphKind, []string{inlineText(node.Children)})
		case "heading":
			r.segment(KindHeading, []string{inlineText(node.Children)})
		case "list_item":
			r.listItem(node)
		case "table_cell", "table_header":
			texts := make([]string, 0, len(node.Children))
			for _, child := range node.Children {
				texts = append(texts, inlineText(child.Children))
			}
			r.segment(KindTableCell, texts)
		case "code_block", "blockquote", "frontmatter", "hr":
			// Not prose. Block HTML never reaches the tree: the parser refuses it.
		default:
			if pmdoc.IsTypedBlock(node.Type) {
				r.blocks(node.Children, KindDirective)
				continue
			}
			// doc, lists, tables, rows and footnotes hold blocks.
			r.blocks(node.Children, KindParagraph)
		}
	}
}

// listItem reads item's own paragraphs as one segment, and its other blocks (a nested list, code,
// a quote, a directive) as they are read anywhere.
func (r *reader) listItem(item *pmdoc.Node) {
	var paragraphs []string
	var rest []*pmdoc.Node
	for _, child := range item.Children {
		if child.Type == "paragraph" {
			paragraphs = append(paragraphs, inlineText(child.Children))
			continue
		}
		rest = append(rest, child)
	}
	r.segment(KindListItem, paragraphs)
	r.blocks(rest, KindParagraph)
}

// segment adds the sentences of one segment of kind, whose paragraphs are texts. A sentence with
// no prose (holdsProse), such as a lone code span, is left out, and a segment left with no
// sentence takes no number.
func (r *reader) segment(kind string, texts []string) {
	first := len(r.sentences)
	offset := 0
	for _, text := range texts {
		for _, found := range segmenter().Segment(text) {
			if !holdsProse(found.Text) {
				continue
			}
			r.sentences = append(r.sentences, Sentence{
				Field:   r.field,
				Segment: r.segments,
				Kind:    kind,
				Text:    found.Text,
				Start:   offset + found.Start,
				End:     offset + found.End(),
			})
		}
		offset += len(text) + len(paragraphBreak)
	}
	if len(r.sentences) == first {
		return
	}
	r.sentences[len(r.sentences)-1].Last = true
	r.segments++
}

// paragraphBreak joins a list item's paragraphs in the plain text its offsets count.
const paragraphBreak = "\n\n"

// holdsProse reports whether text holds a letter or digit outside CodeToken and URLToken.
func holdsProse(text string) bool {
	return strings.IndexFunc(withoutTokens.Replace(text), func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}) >= 0
}

var withoutTokens = strings.NewReplacer(CodeToken, "", URLToken, "")

// inlineText is the plain text of a block's inline nodes: text as written, a code span as
// CodeToken, a bare address as URLToken, a hard break as a space, and nothing for an image, inline
// HTML or a footnote reference. A link's text is kept and its href is not read.
func inlineText(nodes []*pmdoc.Node) string {
	var text strings.Builder
	for index := 0; index < len(nodes); {
		node := nodes[index]
		switch node.Type {
		case "text":
			if href, linked := linkHref(node); linked {
				end := index + 1
				for end < len(nodes) && nodes[end].Type == "text" {
					if next, ok := linkHref(nodes[end]); !ok || next != href {
						break
					}
					end++
				}
				if isBareAddress(nodes[index:end], href) {
					text.WriteString(URLToken)
				} else {
					writeTexts(&text, nodes[index:end])
				}
				index = end
				continue
			}
			end := index + 1
			for end < len(nodes) && nodes[end].Type == "text" {
				if _, ok := linkHref(nodes[end]); ok {
					break
				}
				end++
			}
			writeTexts(&text, nodes[index:end])
			index = end
			continue
		case "hardbreak":
			text.WriteByte(' ')
		}
		index++
	}
	return text.String()
}

// writeTexts writes the text nodes of one run, each run of code spans in it as one CodeToken.
func writeTexts(text *strings.Builder, nodes []*pmdoc.Node) {
	inCode := false
	for _, node := range nodes {
		code := hasMark(node, "inlineCode")
		switch {
		case code && !inCode:
			text.WriteString(CodeToken)
		case !code:
			text.WriteString(node.Text)
		}
		inCode = code
	}
}

// isBareAddress reports whether the text of run, which links to href, is its own address: what
// an autolink and a bare URL or email address show (pmdoc's autolinkText), where the text is the
// href, the href with a scheme before it (`http://` before a www address), or `mailto:` before it.
func isBareAddress(run []*pmdoc.Node, href string) bool {
	var label strings.Builder
	for _, node := range run {
		label.WriteString(node.Text)
	}
	text := label.String()
	return text == href || "mailto:"+text == href || strings.HasSuffix(href, "://"+text)
}

func linkHref(node *pmdoc.Node) (string, bool) {
	for _, mark := range node.Marks {
		if mark.Type == "link" {
			href, _ := mark.Attrs["href"].(string)
			return href, true
		}
	}
	return "", false
}

func hasMark(node *pmdoc.Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

// lines reads a body the parser refused as the package comment describes.
func (r *reader) lines(body string) {
	var paragraph []string
	skipping := false
	flush := func() {
		if len(paragraph) > 0 && !skipping {
			r.segment(KindParagraph, []string{inlineCode.ReplaceAllString(strings.Join(paragraph, " "), CodeToken)})
		}
		paragraph, skipping = nil, false
	}
	inFence := false
	for _, line := range strings.Split(pmdoc.LineFeeds(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			flush()
			inFence = !inFence
			continue
		}
		switch {
		case inFence:
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, ">"), strings.HasPrefix(trimmed, ":::"):
		default:
			if len(paragraph) == 0 && !skipping && strings.HasPrefix(trimmed, "<") {
				skipping = true
			}
			paragraph = append(paragraph, trimmed)
		}
	}
	flush()
}

// inlineCode is a code span as the line scan finds one.
var inlineCode = regexp.MustCompile("`[^`]*`")

// segmenter is the English sentence segmenter, loaded once. Its model is built into the binary,
// so a failure to load it is a broken build, not a body this package can read otherwise.
var segmenter = sync.OnceValue(func() *segment.Segmenter {
	segmenter, err := segment.New()
	if err != nil {
		panic("prose: the built-in English sentence model does not load: " + err.Error())
	}
	return segmenter
})
