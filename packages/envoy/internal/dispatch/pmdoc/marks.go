package pmdoc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// Range is where the span's text stands in the document.
	Range Range
}

// Crossing is why a quote crosses a marked span of its textblock (TextblockAt.CrossedSpan), or
// CrossesNoSpan where it crosses none.
type Crossing uint8

const (
	// CrossesNoSpan is a quote that crosses no span.
	CrossesNoSpan Crossing = iota
	// CrossesEdge is a quote text is written over that covers text inside a span and text
	// outside it, and spells neither the span's own markup over all of the span's text it covers,
	// nor a whole link with another URL or title, nor another URL or title for a link the edit
	// removes whole. Only a quote text is written over, a replace's find, crosses a span so.
	CrossesEdge
	// LeavesEdgeOnSpace is a quote text is written over that spells a span's own markup over the
	// part of the span it covers, across its edge, where the part of the span it leaves would meet
	// the written text at a space, which markdown cannot write that span's delimiters beside
	// (TextblockAt.KeptOnSpace). Only a replace's find crosses a span so.
	LeavesEdgeOnSpace
	// SpellsAnotherLink is a quote that covers a link's whole text and text outside it, and spells
	// the link with another URL or title than the link has.
	SpellsAnotherLink
	// SpellsAnotherLinkAlone is a quote that covers a link's whole text and nothing outside it,
	// and spells the link with another URL or title than the link has.
	SpellsAnotherLinkAlone
	// RemovesAnotherLink is a quote over part of a link, whose edit removes more than the quote
	// matched and the whole link with it, that spells the link with another URL or title than the
	// link has: a delete whose list marker selected the item holding the link, which it removes
	// whole, or a replace whose with is blocks, which drops the whitespace beside the quote where
	// that is the rest of the link.
	RemovesAnotherLink
)

// CrossedSpan is the first span of the textblock, in the order the spans end, that the quote
// matching r crosses, and why it crosses it (crossing); where the quote crosses none, the span is
// zero and crossing is CrossesNoSpan. removed is the range of the textblock the edit removes, which
// holds r: r itself where the edit removes what the quote matched, and more where it removes more,
// as a delete whose list marker selected an item removes all of that item's text, and a replace
// whose with is blocks drops the whitespace beside r (SpaceBeside). spelled is how the quote spells
// r (QuoteFinder.FindSpelling), and overwrites says text is written over r, as a replace writes
// its with over its find.
//
// A link the edit removes whole must be spelled as it is wherever the quote spells it, since the
// edit would remove a link another writer may have changed since the quote was read, its URL and
// title with it, with no word of the change. So a quote that spells a link with another URL or
// title, over any of the link's text, crosses it where the edit removes the whole link: a quote
// over the link's whole text, alone (`[this](https://wrong.example/9)`) or with text outside it,
// however much of the link's text it spells so (`[one](https://b.example/2) two` over
// `[one two](https://a.example/1)`); one that spells a bare link as a link to another URL
// (`[https://a.example/1](https://b.example/2)`); and one over part of a link whose edit removes
// the rest of it too (`- [one](https://b.example/2)` over the item
// `- [one two](https://a.example/1) and more`, which its delete removes whole). An edit that
// removes only part of a link changes only the text the quote names, and the rest of the link keeps
// the document's URL and title, so whatever URL or title the quote spells that link with, it weighs
// it as one spelled with its own (`wrote [one](https://b.example/2)` over
// `wrote [one two](https://a.example/1)` is `wrote [one](https://a.example/1)` there). A quote that
// writes nothing over r, a delete's find, crosses a span only by spelling another link the delete
// removes whole.
//
// Text written over r lands outside every span whose edge r crosses, covering text inside the
// span and text outside it, so a link r covered goes with its URL, and a span it cut to end or
// start on a space is written with character references to keep its delimiters (`**Sami
// said&#32;**loud`). A range inside one span crosses no edge of it. Nor does a quote that spells
// the span's own markup over all of the span's text it covers, the whole span
// (`wrote [this](https://example.com/Sami)`) or the part of it the quote covers (`**old** now` in
// `**Sami said bold** now`, `wrote [one](https://a.example/1)` in
// `wrote [one two](https://a.example/1)`): it replaces that markup too, which is then the text
// written over it to keep or drop (WithoutQuotedMarkup), and the rest of the span keeps its mark,
// cut where the quote's text starts or ends (`**Sami said b**loud`, `that[ two](…)`) - unless that
// rest would meet the written text at a space, which markdown cannot write the delimiters of a
// bold or italic span beside (KeptOnSpace): `**bold** now` in `**Sami said bold** now` leaves
// `Sami said `, and is LeavesEdgeOnSpace. A link written as its own text - a bare URL, or a www
// link the writer leaves bare - has no markup but that text, so a quote that covers the whole of
// it spells it, however the finder read the quote. The test is the span's own: a quote that
// spells one span's markup and covers a second span of the type whose markup it does not spell
// (`wrote [this](…) and that`, where `that` is a second link) crosses the second span's edge, and
// a quote matched as written spells no markup at all.
func (at TextblockAt) CrossedSpan(r, removed Range, spelled Spelling, overwrites bool) (span Span, crossing Crossing) {
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
		for _, o := range opened {
			if child != nil && carriesMark(child, o.mark) {
				kept = append(kept, o)
				continue
			}
			// The span ends at pos.
			if o.from >= r.To || pos <= r.From {
				continue
			}
			inside := o.from <= r.From && r.To <= pos
			covers := r.From <= o.from && pos <= r.To
			// own says the quote writes no unit of the span as a link with another URL or title
			// than the span, a link, has (Spelling.writes). anotherLink says it does, over a link
			// the edit removes whole, which it crosses whatever else holds; a link the edit removes
			// only part of keeps the document's URL and title over the rest, so the quote changes
			// only the text it names, as a quote spelling the link's own does.
			markup, own := spelled.writes(o.mark, o.from, pos)
			anotherLink := !own && removed.From <= o.from && pos <= removed.To
			// A quote that spells no other link the edit removes crosses a span only where text is
			// written over r, and then no span r lies inside, nor one whose own markup it spells
			// over all of the span's text it covers, unless the part of that span it leaves would
			// meet the written text where markdown cannot write the span's delimiters.
			spanRange := Range{From: o.from, To: pos}
			leavesSpace := false
			if !anotherLink && (!overwrites || inside || markup) {
				if !overwrites || at.KeptOnSpace(o.mark, spanRange, r) == "" {
					continue
				}
				leavesSpace = true
			}
			runs := children[o.first:index]
			if !anotherLink && covers && writtenAsItsText(o.mark, runs) {
				continue
			}
			found := Span{Mark: o.mark, Text: TextContent(&Node{Type: "paragraph", Children: runs}), Range: spanRange}
			switch {
			case leavesSpace:
				return found, LeavesEdgeOnSpace
			case !anotherLink:
				return found, CrossesEdge
			case !covers:
				return found, RemovesAnotherLink
			case inside:
				return found, SpellsAnotherLinkAlone
			}
			return found, SpellsAnotherLink
		}
		opened = kept
		if child == nil || (pos >= r.To && len(opened) == 0) {
			break
		}
		// A span opening at or past r's end lies outside it.
		if pos < r.To {
			for _, mark := range child.Marks {
				if renderedMarkTypes[mark.Type] && !slices.ContainsFunc(opened, func(o open) bool { return sameMark(o.mark, mark) }) {
					opened = append(opened, open{mark: mark, first: index, from: pos})
				}
			}
		}
		pos += nodeSize(child)
	}
	return Span{}, CrossesNoSpan
}

// SpanOver is the text of the textblock that r covers as a find spells it to name that very text,
// each run cut at r's edges and written under its own marks (inlineRange), so the part of mark's
// span that r covers is written under mark, a link's own URL and title, and every span inside it
// with its markup: over `wrote one` in `I wrote [one **two**](https://a.example/1)`, with that
// link's mark, it is `wrote [one](https://a.example/1)`, and over `one two`
// `[one **two**](https://a.example/1)`. It is "" for strikethrough, whose markup no find spells.
// A refusal of a quote names it, where it covers the range the quote named. r must end between
// characters, as every range a matcher returns does (slice16).
func (at TextblockAt) SpanOver(mark Mark, r Range) (string, error) {
	if markTypeMarkup(mark.Type) == 0 {
		return "", nil
	}
	runs, err := inlineRange(at.Node, at.Content.From-1, r.From, r.To)
	if err != nil {
		return "", err
	}
	return lineMarkdown(&Node{Type: "paragraph", Children: runs}), nil
}

// KeptOnSpace is the part of span, a bold or italic span mark covers, that text written over r
// keeps, where r crosses one edge of span and that part would meet the written text at a space,
// and "" where it would not or mark is neither bold nor italic. Markdown cannot close the delimiter
// run of a bold or italic span after a space, nor open one before a space (markDelimiter), so the
// renderer writes such a part with character references (`**Sami said&#32;**loud`); a link's text
// and a code span's may start or end on a space, so no part of theirs is kept on one. Nor is a
// struck span's: no find spells its markup, so no find keeps the text of one it crosses, whatever
// that text ends on (TextblockAt.CrossedSpan).
func (at TextblockAt) KeptOnSpace(mark Mark, span, r Range) string {
	if mark.Type != "strong" && mark.Type != "emphasis" {
		return ""
	}
	var kept Range
	switch {
	case span.From < r.From && r.From < span.To && span.To < r.To:
		kept = Range{From: span.From, To: r.From}
	case r.From < span.From && span.From < r.To && r.To < span.To:
		kept = Range{From: r.To, To: span.To}
	default:
		return ""
	}
	text, err := at.textOver(kept)
	if err != nil || text == "" {
		return ""
	}
	edge, _ := utf8.DecodeRuneInString(text)
	if kept.To == r.From {
		edge, _ = utf8.DecodeLastRuneInString(text)
	}
	if !unicode.IsSpace(edge) {
		return ""
	}
	return text
}

// textOver is the text of the textblock that r covers, as a read serves it (TextContent). r must
// end between characters, as every range a matcher returns does (slice16).
func (at TextblockAt) textOver(r Range) (string, error) {
	runs, err := inlineRange(at.Node, at.Content.From-1, r.From, r.To)
	if err != nil {
		return "", err
	}
	return TextContent(&Node{Type: "paragraph", Children: runs}), nil
}

// SpaceBeside is r and the run of the characters of space beside each of its inner edges in a
// text run, measured in UTF-16 units as every position is: the end of the run r cuts at its start,
// where r starts after the textblock's text does, and the start of the run r cuts at its end, where
// r ends before that text does. That is what a write that splits the textblock at r's edges and
// drops the whitespace the split leaves at them removes: a link whose text beyond r is that
// whitespace alone goes whole.
func (at TextblockAt) SpaceBeside(r Range, space string) (Range, error) {
	beside := r
	if r.From > at.Content.From {
		before, err := inlineRange(at.Node, at.Content.From-1, at.Content.From, r.From)
		if err != nil {
			return Range{}, err
		}
		if last := before[len(before)-1]; last.Type == "text" {
			trimmed := strings.TrimRight(last.Text, space)
			beside.From -= len16(last.Text[len(trimmed):])
		}
	}
	if r.To < at.Content.To {
		after, err := inlineRange(at.Node, at.Content.From-1, r.To, at.Content.To)
		if err != nil {
			return Range{}, err
		}
		if first := after[0]; first.Type == "text" {
			trimmed := strings.TrimLeft(first.Text, space)
			beside.To += len16(first.Text[:len(first.Text)-len(trimmed)])
		}
	}
	return beside, nil
}

// writtenAsItsText reports whether a link mark over runs is written as its own text, as a bare
// URL is, or a www link the writer leaves bare: the runs written under that link alone render as
// their text, so the link has no markup but that text.
func writtenAsItsText(mark Mark, runs []*Node) bool {
	if mark.Type != "link" {
		return false
	}
	written := &Node{Type: "paragraph", Children: make([]*Node, len(runs))}
	for index, run := range runs {
		written.Children[index] = &Node{Type: run.Type, Text: run.Text, Attrs: run.Attrs, Marks: []Mark{mark}}
	}
	markdown := lineMarkdown(written)
	return markdown != "" && markdown == TextContent(written)
}

// lineMarkdown is paragraph written at a line's start, where the finder reads a find, and "" where
// the renderer refuses it.
func lineMarkdown(paragraph *Node) string {
	markdown, err := Render(&Node{Type: "doc", Children: []*Node{paragraph}})
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(markdown, "\n")
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
