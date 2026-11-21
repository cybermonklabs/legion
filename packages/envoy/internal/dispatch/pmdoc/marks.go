package pmdoc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
)

// The mark vocabulary the reader and the writer share: which marks a text is written under, in
// what order, with what delimiters, and how two mark sets compare.

// markAttributeKey is the Y.Text attribute key a mark is stored under. A mark whose type excludes
// itself is stored under its type name, as y-prosemirror stores it. A record mark (anchorMarkTypes)
// does not exclude itself, so two of one type may share a character, and y-prosemirror stores such
// a mark under `<type>--<8 characters>` (sync-plugin.js marksToAttributes), one key per mark; the
// writer does the same, or a second record mark of the type would overwrite the first.
//
// The 8 characters are this package's own digest (the first 6 bytes of a SHA-256 over the type and
// the canonical attributes, in standard base64), not y-prosemirror's (lib0's encodeAny over
// mark.toJSON(), which carries every schema default). That is enough: both readers strip the suffix
// by its shape (yattrToMarkName here, hashedMarkNameRegex there) and compare marks by name and
// attributes, never by key, so a mark one side wrote and the other rewrote under a different key on
// an adjacent run reads as one mark across both.
func markAttributeKey(mark Mark) string {
	if !anchorMarkTypes[mark.Type] {
		return mark.Type
	}
	canonical, _ := json.Marshal(tokenMark{Type: mark.Type, Attrs: canonicalAttrs(mark.Attrs)})
	digest := sha256.Sum256(canonical)
	return mark.Type + "--" + base64.StdEncoding.EncodeToString(digest[:6])
}

func containsMark(marks []Mark, markType string) bool {
	for _, mark := range marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

func withoutMark(marks []Mark, markType string) []Mark {
	out := marks[:0]
	for _, mark := range marks {
		if mark.Type != markType {
			out = append(out, mark)
		}
	}
	return out
}

func nodeHasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

// renderedMarkTypes are the marks canonical Markdown writes. Every other mark the schema allows
// (active.marks) is an anchor, invisible to the rendering, and renderedMarks, the one filter the
// rendering and StripAnchorMarks pass marks through, removes exactly the marks that are not here:
// what renders is one list, not two of opposite polarity that a new invisible mark could fall
// between.
var renderedMarkTypes = map[string]bool{
	"link": true, "strong": true, "emphasis": true, "strike_through": true, "inlineCode": true,
}

// anchorMarkTypes are the marks an ask or comment row anchors to: its quote is the text they
// cover (FindMark). They are exactly the marks whose schema declares an empty excludes (the fork's
// proofComment and proofSuggestion, packages/proof-editor's dispatchAsk), so two of one type may
// cover one character and each is stored under its own Y attribute key (markAttributeKey). The
// set moves with those schemas.
var anchorMarkTypes = map[string]bool{"proofComment": true, "proofSuggestion": true, "dispatchAsk": true}

// MarksCovering returns the marks text written over r takes from the text r held, from one walk
// of doc: each mark that every text run inside r carries.
//
// anchors is each anchor mark among them, every anchor r lies wholly inside. Text written over r
// belongs inside those anchors: a replacement spliced in without them ends each one's run where it
// lands, and FindMark then reads only the text before it.
//
// spans is each mark the rendering writes (renderedMarkTypes) among them - bold, italic,
// strikethrough, a link, code - every marked span r lies wholly inside. Text written over r belongs
// inside those spans too: a replacement spliced in without them splits each span where it lands,
// and a split span whose edge is a space is written with character references to keep its
// delimiters (`**Sami&#32;**&#97;nswered`).
//
// A range that runs past an anchor's or a span's edge has a run without its mark, so that anchor
// or span keeps only the text the write leaves alone. A document edit refuses a replace whose
// find runs past a span's edge (TextblockAt.CrossedSpan); an accepted suggestion's text still
// lands outside that span.
func MarksCovering(doc *Node, r Range) (anchors, spans []Mark) {
	var covering []Mark
	first := true
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if pos >= r.To {
			return false
		}
		if node.Type != "text" || end <= r.From {
			return true
		}
		if first {
			first = false
			for _, mark := range node.Marks {
				if anchorMarkTypes[mark.Type] || renderedMarkTypes[mark.Type] {
					covering = append(covering, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
				}
			}
			return len(covering) > 0
		}
		kept := covering[:0]
		for _, mark := range covering {
			if carriesMark(node, mark) {
				kept = append(kept, mark)
			}
		}
		covering = kept
		return len(covering) > 0
	})
	for _, mark := range covering {
		if anchorMarkTypes[mark.Type] {
			anchors = append(anchors, mark)
		} else {
			spans = append(spans, mark)
		}
	}
	return anchors, spans
}

func carriesMark(node *Node, want Mark) bool {
	for _, mark := range node.Marks {
		if mark.Type == want.Type && attrsEqual(mark.Attrs, want.Attrs) {
			return true
		}
	}
	return false
}

// AddMarks adds marks to every text node under node, in place.
func AddMarks(node *Node, marks []Mark) {
	addMarks(node, marks, false)
}

// AddSpanMarks adds each of spans (MarksCovering) to every text node under node that carries no
// mark of its type, in place. A mark of that type the text carries is its own - `**answered**`
// written inside a bold span, a link of its own inside a link - and a span's mark of the type
// beside it would write it twice, or two links over one text.
func AddSpanMarks(node *Node, spans []Mark) {
	addMarks(node, spans, true)
}

// addMarks adds a copy of each of marks to every text node under node, in place, and keeps each
// node's marks in their order (sortMarks). skip passes over a mark of a type the node carries
// already, AddSpanMarks' rule.
func addMarks(node *Node, marks []Mark, skip bool) {
	if len(marks) == 0 {
		return
	}
	if node.Type == "text" {
		added := false
		for _, mark := range marks {
			if skip && containsMark(node.Marks, mark.Type) {
				continue
			}
			node.Marks = append(node.Marks, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
			added = true
		}
		if added {
			sortMarks(node.Marks)
		}
		return
	}
	for _, child := range node.Children {
		addMarks(child, marks, skip)
	}
}

// Span is a marked span of a textblock: the run of its children one bold, italic, strikethrough,
// link or code mark (renderedMarkTypes) covers, which canonical Markdown writes inside one pair of
// delimiters.
type Span struct {
	Mark Mark
	// Text is the span's whole text, as a read serves it (TextContent).
	Text string
	// Markdown is the span written under its own mark alone, as a find spells its markup
	// (`[this](https://example.com/Sami)`). It is empty for strikethrough, whose markup the finder
	// never reads (markTypeMarkup), so that no find spells it.
	Markdown string
}

// CrossedSpan is the first span of the textblock, in the order the spans end, whose edge r
// crosses: r covers text inside the span and text outside it. Text written over such a range lands
// outside the span, so a link it covered goes with its URL, and a span it cut to end or start on a
// space is written with character references to keep its delimiters (`**Sami said&#32;**loud`). A
// range inside one span crosses no edge of it, and neither does one over a span's whole text
// alone, unless its find spells that span, a link, with another URL or title.
//
// spelled is how the find that matched r spells it (QuoteFinder.FindSpelling). A find that spells
// a span's markup over the whole span (`wrote [this](https://example.com/Sami)`) crosses no edge
// of it: it replaces that markup too, which is then the text written over it to keep or drop
// (WithoutQuotedMarkup). A link is spelled only with its own URL and title, so a find that writes
// its text under another URL or another title crosses its edge, and so does one that is that link
// alone (`[this](https://wrong.example/9)`), which was written inside the link under the URL and
// title the document held, where the same spelling in a longer find was refused. A link written
// as its own text - a bare URL, or a www link the writer leaves bare - has no markup but that
// text, so a find that covers the whole of it spells it, however the finder read the find: such a
// link was refused with the find itself as the markup to spell. The test is the span's own: a find
// that spells one span's markup and covers a second span of the type whose markup it does not
// spell (`wrote [this](…) and that`, where `that` is a second link) crosses the second span's
// edge. One that spells only the part of a span it covers (`**bold** now` in `**Sami said bold**
// now`) still crosses that span's edge, and a find matched as written spells no other markup at
// all.
//
// The second answer, alone, says r is the crossed span's whole text alone, a link its find spells
// with another URL or title: r covers no text outside the link, so what refuses it says that, not
// that it covers text outside the span. The third says r crosses a span's edge at all; where it
// does not, the span and alone are zero.
func (at TextblockAt) CrossedSpan(r Range, spelled Spelling) (Span, bool, bool) {
	type open struct {
		mark Mark
		// first is the index of the span's first child, and from its position.
		first, from int
	}
	// At most one span of each rendered mark type is open at a child, since none of those marks
	// overlaps another of its type.
	var openSpans [5]open
	opened := openSpans[:0]
	children := at.Node.Children
	pos := at.Content.From
	for index := 0; index <= len(children); index++ {
		var child *Node
		if index < len(children) {
			child = children[index]
		}
		kept := opened[:0]
		for _, span := range opened {
			if child != nil && carriesMark(child, span.mark) {
				kept = append(kept, span)
				continue
			}
			// The span ends at pos.
			if span.from >= r.To || pos <= r.From {
				continue
			}
			covers := r.From <= span.from && pos <= r.To
			inside := span.from <= r.From && r.To <= pos
			// r inside the span crosses no edge of it, and r over its whole text alone none either,
			// unless the find spells that link with another URL or title.
			alone := inside && covers
			if inside && !(alone && spelled.spellsAnotherLink(span.mark, span.from, pos)) {
				continue
			}
			if covers && spelled.spells(span.mark, span.from, pos) {
				continue
			}
			crossed := spanOf(span.mark, children[span.first:index])
			if covers && crossed.Markdown != "" && crossed.Markdown == crossed.Text {
				continue
			}
			return crossed, alone, true
		}
		opened = kept
		if child == nil || (pos >= r.To && len(opened) == 0) {
			break
		}
		// A span opening at or past r's end lies outside it.
		if pos < r.To {
			for _, mark := range child.Marks {
				if renderedMarkTypes[mark.Type] && !slices.ContainsFunc(opened, func(span open) bool { return sameMark(span.mark, mark) }) {
					opened = append(opened, open{mark: mark, first: index, from: pos})
				}
			}
		}
		pos += nodeSize(child)
	}
	return Span{}, false, false
}

// spanOf is the span mark covers over runs, the textblock's children it covers, with its text and
// the markdown a find spells it with: the runs written under mark alone, at a line's start, where
// the finder reads a find.
func spanOf(mark Mark, runs []*Node) Span {
	written := &Node{Type: "paragraph", Children: make([]*Node, len(runs))}
	for index, run := range runs {
		written.Children[index] = &Node{Type: run.Type, Text: run.Text, Attrs: run.Attrs, Marks: []Mark{mark}}
	}
	span := Span{Mark: mark, Text: TextContent(written)}
	if markTypeMarkup(mark.Type) != 0 {
		if markdown, err := Render(&Node{Type: "doc", Children: []*Node{written}}); err == nil {
			span.Markdown = strings.TrimSuffix(markdown, "\n")
		}
	}
	return span
}

// writtenMarks is the marks a node's text is written under: its visible marks, less a bare URL's
// link, which the parser links again on its own, or an autolink's, which its angle brackets write
// (isAngleURLLink), and less a www link's where the spelling writes it bare (wwwBare,
// isBareWWWLink). A node that is not text is written under none.
func writtenMarks(node *Node, escapePipes, wwwBare bool) []Mark {
	if node.Type != "text" {
		return nil
	}
	marks := visibleMarks(node.Marks)
	if isBareURLLink(node, marks, escapePipes) || isAngleURLLink(node, marks, escapePipes) || wwwBare && isBareWWWLink(node, marks, escapePipes) {
		marks = withoutMark(marks, "link")
	}
	return marks
}

// adjacentDelimiter is the delimiter character of the innermost of marks opened or closed beside a
// text, the one written next to it, or 0 when that mark is not written with a delimiter run.
func adjacentDelimiter(marks []Mark) byte {
	if len(marks) == 0 {
		return 0
	}
	return markDelimiter(marks[len(marks)-1])
}

// markDelimiter is the character a mark's delimiter run is written with, or 0 when the mark is not
// written with one.
func markDelimiter(mark Mark) byte {
	switch mark.Type {
	case "strong", "emphasis":
		return '*'
	case "strike_through":
		return '~'
	}
	return 0
}

// visibleMarks is the rendered marks of marks in writing order: nil when there are none, else a
// fresh slice the caller may filter in place (withoutMark) without touching marks.
func visibleMarks(marks []Mark) []Mark {
	out, kept := renderedMarks(marks)
	if len(out) == 0 {
		return nil
	}
	if kept {
		out = slices.Clone(out)
	}
	sortMarks(out)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if renderMarkRank(out[j].Type) < renderMarkRank(out[i].Type) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sharedMarks(left, right []Mark) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := range limit {
		if !sameMark(left[i], right[i]) {
			return i
		}
	}
	return limit
}

func sameMark(left, right Mark) bool {
	return left.Type == right.Type && attrsEqual(left.Attrs, right.Attrs)
}

func containsSameMark(marks []Mark, mark Mark) bool {
	for _, other := range marks {
		if sameMark(other, mark) {
			return true
		}
	}
	return false
}

func renderMarkRank(markType string) int {
	switch markType {
	case "link":
		return 0
	case "strong":
		return 1
	case "emphasis":
		return 2
	case "strike_through":
		return 3
	case "inlineCode":
		return 4
	default:
		return 5
	}
}

func openMark(mark Mark) string {
	switch mark.Type {
	case "link":
		return "["
	case "strong":
		return "**"
	case "emphasis":
		return "*"
	case "strike_through":
		return "~~"
	case "inlineCode":
		return "`"
	default:
		return ""
	}
}

func closeMark(mark Mark, escapePipes bool) string {
	if mark.Type == "link" {
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return "](" + escapeLinkDestination(href, escapePipes) + titleSuffix(title, escapePipes) + ")"
	}
	return openMark(mark)
}
func withoutSameMark(marks []Mark, mark Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, other := range marks {
		if !sameMark(other, mark) {
			out = append(out, other)
		}
	}
	return out
}
