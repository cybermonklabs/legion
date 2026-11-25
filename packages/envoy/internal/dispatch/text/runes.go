package text

import (
	"bytes"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	markdowntext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// HeadRunes returns value cut to its first limit runes; a value at or under the
// limit is returned unchanged. It walks only as far as the limit, rather than
// converting the whole value, and a cut value reads as a []rune conversion of it
// would: each byte that is no UTF-8 is one rune, written back as U+FFFD.
func HeadRunes(value string, limit int) string {
	count := 0
	for index := range value {
		if count == limit {
			return string([]rune(value[:index]))
		}
		count++
	}
	return value
}

// HeadWords returns value cut to its first limit runes at a word boundary, and whether it cut:
// the cut falls back to the last space inside the limit, so the answer ends at a whole word,
// and a value whose first limit runes hold no space is cut at the limit itself, since there is
// no boundary to fall back to. A value at or under the limit is returned unchanged and uncut.
// A caller that marks a cut (an ellipsis) reads the second answer rather than comparing
// lengths, which a trailing space the cut removed would make wrong.
func HeadWords(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	head := string(runes[:limit])
	if space := strings.LastIndexAny(head, " \t\n"); space > 0 {
		head = head[:space]
	}
	return strings.TrimRight(head, " \t\n"), true
}

// longestWholeReferenceRunes is the longest reference HeadRefs keeps whole, in runes: the text it
// reads past the cut. A reference, or a link around one, that runs further than this past the cut
// is cut by runes, as HeadRunes cuts it. It is far longer than any reference a person writes - an
// address to a block is a few hundred characters, a mention's link a few dozen - and short enough
// that a cut reads the same few kilobytes whatever the length of the text it cuts.
const longestWholeReferenceRunes = 1024

// HeadRefs is value cut to at most limit runes, ellipsis included, at a place no reference
// crosses: the cut of a text a reader is shown with its references drawn (an Inbox subject, a
// candidate's first line, an excerpt), where HeadRunes would leave half of one - `CORE-12` of
// `CORE-1234`, a working link to another issue, or `[@Planner](dispatch://agent/` drawn raw. A
// value at or under the limit is returned unchanged. One over it is cut to its first limit runes
// less the ellipsis's and the ellipsis is appended: `…` for a line, as contracts.OneLineSummary
// marks its cut, and "" for an excerpt, which marks none.
//
// It reads value's first limit and longestWholeReferenceRunes runes, no more, so a cut costs the
// same on a text of any length - which every read of the replies, the backlinks and the id
// prefixes pays for each row it cuts. The references in them are the ones Dispatch holds
// (cutReferences): a `dispatch://` address and a dashboard URL of the deployment at serverURL as
// Extract reads them, a bare issue key and a `#id` as Mentions reads its candidates, each taken
// with the inline link around it whole, as goldmark reads links - a title, an address in angle
// brackets, brackets in the words and words over a line break included. A URL of anywhere else
// names nothing Dispatch holds, so it is cut as text is.
//
// Where a reference starts before the cut and ends after it, the cut moves back to its start - the
// link's `[` where a link wraps it. A code span the cut opens and does not close counts as
// crossing it too, and the cut moves back to before its opening backticks (codeSpans): the head
// would hold those backticks with no closing run, which reads them as text and what the span
// held - an address in it included - as prose with live references. It moves back the same way
// where the head, with the ellipsis after it, does not read the references of the text it holds
// as the text does: a reference it reads that the text does not write (`CORE-12` of `CORE-12x4`),
// or one the text writes whole that the ellipsis joins (`dispatch://agent/s1` and `…` read as the
// session `s1…`). A cut that moved drops the whitespace before it, line breaks included, save
// where the head then ends after a reference: there it keeps the first whitespace character the
// text has after that reference, so the ellipsis joins nothing and the walk stops - a line of
// short addresses separated by spaces is cut after its last whole address and a space, not back
// past every one of them. A cut never empties the head: where moving back would leave nothing
// before the ellipsis - a text that opens with a reference, a link or a code span longer than the
// bound - the cut by length stands, since a head of nothing tells a reader less than half a
// reference does, and it closes what it leaves open (closeCut). Inside an inline link or picture
// around a reference, the head is the link's opening, its words cut by this same rule to leave
// room for the rest of the link, and that rest as the text writes it, from the `]` its words end
// at to its `)`: the head reads that link, where the cut by length would leave its `[` as text and
// half its address as another reference. Inside a code span, the head is as much of the span's
// content as fits with the span's opening backticks written after it, which close it: what the
// span holds reads as code, as the text has it, where the cut by length would leave the opening
// backticks as text and an address in the span live; where no character of the content fits, the
// head is the opening backticks alone. Where the rest of a link does not fit the bound, or the
// closed link would not read as that link, the cut by length stands as it is. A bare reference
// has nothing to close, so the cut by length can leave half of one: an excerpt's head of an
// address whose slug runs past the bound names an artifact of a shorter slug. A cut that moves
// nothing is HeadRunes' own.
//
// A reference or a link that runs to the window's edge, where the window stops short of value,
// may run further, so it is none the cut keeps whole and is cut by length. A code span that runs
// to that edge, or that the window opens and does not close in the paragraph it ends in, may close
// past it, so it counts as crossing the cut (windowSpans).
//
// The dashboard's headRefs (`packages/dispatch/web/src/features/refs/head-refs.ts`) cuts the
// same way, and `testdata/head-refs.json` is the table both copies are held to.
func HeadRefs(value string, limit int, ellipsis, serverURL string) string {
	keep := max(limit-utf8.RuneCountInString(ellipsis), 0)
	cut, window, count := 0, len(value), 0
	over := false
	for index := range value {
		if count == keep {
			cut = index
		}
		if count == limit {
			over = true
		}
		if count == limit+longestWholeReferenceRunes {
			window = index
			break
		}
		count++
	}
	if !over {
		return value
	}
	return headOf(value[:window], window < len(value), cut, 0, ellipsis, serverURL) + ellipsis
}

// headOf is the head of window, value's first runes, cut at cut (HeadRefs), without its ellipsis:
// window up to where the walk back ends (headEnd), or, where that leaves nothing past floor, the
// cut by length with what it leaves open closed (closeCut). Floor is 0 for a whole head, and the
// first byte of a link's words where those words are cut to leave room for the rest of the link.
func headOf(window string, truncated bool, cut, floor int, ellipsis, serverURL string) string {
	written := cutReferences(window, serverURL)
	if truncated {
		written = slices.DeleteFunc(written, func(reference cutReference) bool {
			return reference.end >= len(window)
		})
	}
	spans := windowSpans(window, truncated)
	if end := headEnd(window, written, spans, cut, ellipsis, serverURL); end > floor {
		return window[:end]
	}
	return closeCut(window, written, spans, cut, floor, serverURL)
}

// headEnd is where the head of window cut at cut ends (HeadRefs): cut moved back past every
// reference written and every code span in spans that crosses it, and past every reference the
// head with the ellipsis after it reads otherwise than the text does, less the whitespace before
// it but the one a reference ends at. Each move is back, so it ends; at 0 it leaves nothing.
func headEnd(window string, written []cutReference, spans []codeRange, cut int, ellipsis, serverURL string) int {
	head := cut
	for head > 0 {
		back := head
		for _, reference := range written {
			if reference.start < head && head < reference.end {
				back = min(back, reference.start)
			}
		}
		for _, span := range spans {
			if span.start < head && head < span.end {
				back = min(back, span.start)
			}
		}
		read := cutReferences(window[:head]+ellipsis, serverURL)
		for _, reference := range read {
			if reference.start < head && !slices.Contains(written, reference) {
				back = min(back, reference.start)
			}
		}
		for _, reference := range written {
			if reference.end <= head && !slices.Contains(read, reference) {
				back = min(back, reference.start)
			}
		}
		if back == head {
			break
		}
		head = len(strings.TrimRight(window[:back], " \t\r\n"))
		// A head that now ends after a reference the text follows with whitespace keeps the
		// first of it, so the ellipsis after the head joins nothing: without it the ellipsis would
		// join that reference and the walk step back past it, and on a line of short addresses
		// back past every one of them.
		if head < back && slices.ContainsFunc(written, func(reference cutReference) bool {
			return reference.end == head
		}) {
			head++
		}
	}
	return head
}

// closeCut is the cut by length of window at cut, which a head falls back to where moving the cut
// back leaves nothing past floor (HeadRefs), with what it leaves open closed so the head reads
// what it holds as the text does: the inline link or picture around a reference the cut is in
// (closeLink), or else the code span it is in (closeSpan). Where it is in neither, or in a link it
// cannot close and in no span, it is window[:cut].
func closeCut(window string, written []cutReference, spans []codeRange, cut, floor int, serverURL string) string {
	if head, closed := closeLink(window, written, cut, floor, serverURL); closed {
		return head
	}
	for _, span := range spans {
		if floor <= span.start && span.start < cut && cut < span.end {
			return closeSpan(window, span, cut)
		}
	}
	return window[:cut]
}

// closeLink is the head of window cut at cut inside the inline link or picture around a reference
// written there, closed: the window up to the link's words, those words cut by HeadRefs' own rule
// with the link's `[` as the floor no cut moves back past, so it splits no reference or code span
// in them, and the rest of the link as the text writes it, from the `]` its words end at through
// its `)`, all of it within the runes of window[:cut]. It closes nothing where no such link holds
// the cut, where the rest of the link leaves no room for a word, or where the head would not read
// as that link - a bracket the cut words leave open, or a backslash they end in, reads otherwise.
func closeLink(window string, written []cutReference, cut, floor int, serverURL string) (string, bool) {
	for _, link := range linkRanges(window) {
		if link.open < floor || link.open >= cut || cut >= link.close {
			continue
		}
		if !slices.ContainsFunc(written, func(reference cutReference) bool {
			return reference.start == link.open && reference.end == link.close
		}) {
			return "", false
		}
		words := link.open + 1
		if window[link.open] == '!' {
			words++
		}
		rest := window[link.words:link.close]
		room := utf8.RuneCountInString(window[:cut]) - utf8.RuneCountInString(window[:words]) -
			utf8.RuneCountInString(rest)
		if room <= 0 {
			return "", false
		}
		wordsCut := words
		for range room {
			_, size := utf8.DecodeRuneInString(window[wordsCut:])
			wordsCut += size
		}
		head := headOf(window[:link.words], false, wordsCut, words, "", serverURL) + rest
		if !slices.ContainsFunc(linkRanges(head), func(read linkRange) bool {
			return read.open == link.open && read.close == len(head)
		}) {
			return "", false
		}
		return head, true
	}
	return "", false
}

// closeSpan is the head of window cut at cut inside span, closed: the span's content up to its
// opening backticks' width in runes before cut, less the backticks and whitespace it ends in, then
// a run of that width. The run follows a character of the content on the content's own line, so it
// joins no backtick run of the content and opens no fence, and closes the span where the head ends
// it. Where no character of the content fits with that run, the head is the span's opening
// backticks alone, so what the span holds reads nowhere: cut by length, even a `#q` of `#q12`
// would read as a live reference.
func closeSpan(window string, span codeRange, cut int) string {
	content := backtickRunEnd(window, span.start, len(window))
	opener := window[span.start:content]
	end := cut
	for range len(opener) {
		_, size := utf8.DecodeLastRuneInString(window[:end])
		end -= size
	}
	if end > content {
		if held := strings.TrimRight(window[content:end], "` \t\r\n"); held != "" {
			return window[:content+len(held)] + opener
		}
	}
	return window[:min(cut, content)]
}

// windowSpans is every code span window writes, its blocks left out: a cut inside a span the head
// does not close leaves its opening backticks as text, and what the span held - an address in it
// included - read as prose, where a cut inside a fenced block leaves the rest of the head in the
// block as the text has it. A window with no backtick has no span, and is not walked for one.
//
// A window that stops short of the text (truncated) is read with a line after it that closes a
// span of any width it could open (closingLine): the first span the window leaves open in the
// paragraph it ends in may close past the window, so the line closes it and it runs past the
// window's end, as a span whose closing run ends at that end does, whose run the text may carry on.
// Either crosses every cut after its start. A span the window opens in an earlier paragraph is
// closed by none, and one the window opens after such a span is inside it, so the line closes
// neither.
func windowSpans(window string, truncated bool) []codeRange {
	if !strings.Contains(window, "`") {
		return nil
	}
	read := window
	if truncated {
		read += closingLine(window)
	}
	return slices.DeleteFunc(codeRanges(read), func(code codeRange) bool {
		return code.block || code.start >= len(window)
	})
}

// closingLine is a line to read after window that continues the paragraph window ends in, if one
// is open there, and closes the first span it leaves open: an `x`, so the line opens no block of
// its own and reads as that paragraph's text, then a run of each width a span of window could open
// with, widest last and each after a space, so no two join. A span opens with a run of backticks
// or, after a backslash that takes the first of them, the rest of that run.
func closingLine(window string) string {
	var widths []int
	for at := strings.IndexByte(window, '`'); at >= 0; {
		end := backtickRunEnd(window, at, len(window))
		widths = append(widths, end-at, end-at-1)
		next := strings.IndexByte(window[end:], '`')
		if next < 0 {
			break
		}
		at = end + next
	}
	slices.Sort(widths)
	var line strings.Builder
	if !strings.HasSuffix(window, "\n") {
		line.WriteByte('\n')
	}
	line.WriteByte('x')
	for _, width := range slices.Compact(widths) {
		if width > 0 {
			line.WriteByte(' ')
			line.WriteString(strings.Repeat("`", width))
		}
	}
	return line.String()
}

// cutReference is one reference a text writes, as a cut reads it: the bytes [start, end) a reader
// sees it in - the whole inline link around it, where one is - and what it names.
type cutReference struct {
	start, end int
	ref        Ref
}

// cutReferences is every reference body writes that Dispatch holds, in the reader's own terms:
// each reference span Extract reads as a `dispatch://` address or a page of the dashboard at
// serverURL (locateSpan), a URL of anywhere else left out, and each bare key and `#id` Mentions
// would ask about (bareCandidates), whether or not it names anything, since a cut that splits one
// changes what the text says. Each is widened over the inline link that holds it (linkRanges),
// and a span that is a link's address is that address, as the link reads it, rather than the
// span, which would run on over the `)` into whatever follows it.
func cutReferences(body, serverURL string) []cutReference {
	spans := referencePattern.FindAllStringIndex(body, -1)
	links := linkRanges(body)
	var base *url.URL
	baseParsed := false
	baseOf := func() *url.URL {
		if !baseParsed {
			base, _ = url.Parse(serverURL)
			baseParsed = true
		}
		return base
	}
	var found []cutReference
	add := func(start, end int, ref Ref) {
		for _, link := range links {
			if link.open <= start && start < link.close {
				start, end = link.open, link.close
				break
			}
		}
		found = append(found, cutReference{start: start, end: end, ref: ref})
	}
	for _, span := range spans {
		start, raw := span[0], trimReference(body[span[0]:span[1]])
		for _, link := range links {
			if link.open < start && start < link.close && link.destination != "" &&
				strings.HasPrefix(body[start:], link.destination) {
				raw = link.destination
				break
			}
		}
		if raw == "" {
			continue
		}
		if ref, ok := locateSpan(raw, baseOf, true); ok && ref.Kind != "url" {
			add(start, start+len(raw), ref)
		}
	}
	candidates, _, _ := bareCandidates(body, spans)
	for _, candidate := range candidates {
		if candidate.key != "" {
			add(candidate.offset, candidate.offset+len(candidate.key),
				Ref{Kind: "issue", IssueKey: candidate.key, ID: candidate.key})
			continue
		}
		add(candidate.offset, candidate.offset+1+len(candidate.id), Ref{Kind: "ask", ID: candidate.id})
	}
	return found
}

// linkRange is one inline link or picture a text writes: its `[`, or the `!` before it, the `]`
// its words end at, one past its `)`, and its address as the link reads it.
type linkRange struct {
	open, words, close int
	destination        string
}

// linkRanges is every inline link and picture body writes, in the order they open, as goldmark
// reads CommonMark's links: a title, an address in angle brackets, balanced brackets in the words,
// words over a line break, a backslash escape and no link inside another, all of it the parser's.
// goldmark records where a link opens (ast.Node.Pos) but not where it closes, so its link parser
// is wrapped to note where the reader stands once it has read one (rangedLinkParser).
func linkRanges(body string) []linkRange {
	if !strings.Contains(body, "[") {
		return nil
	}
	var ranges []linkRange
	context := parser.NewContext()
	context.Set(linkRangesKey, &ranges)
	linkReader.Parse(markdowntext.NewReader([]byte(body)), parser.WithContext(context))
	slices.SortFunc(ranges, func(left, right linkRange) int { return left.open - right.open })
	return ranges
}

// linkRangesKey holds, in one parse's context, the links its link parser has read.
var linkRangesKey = parser.NewContextKey()

// linkReader is goldmark's CommonMark parser with its link parser wrapped (rangedLinkParser). It
// holds no state of its own between parses, so one serves every cut.
var linkReader = parser.NewParser(
	parser.WithBlockParsers(parser.DefaultBlockParsers()...),
	parser.WithInlineParsers(rangedInlineParsers()...),
	parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
)

// rangedInlineParsers is goldmark's inline parsers with its link parser wrapped.
func rangedInlineParsers() []util.PrioritizedValue {
	parsers := parser.DefaultInlineParsers()
	link := parser.NewLinkParser()
	for index, prioritized := range parsers {
		if inline, held := prioritized.Value.(parser.InlineParser); held && inline == link {
			parsers[index].Value = rangedLinkParser{InlineParser: inline}
		}
	}
	return parsers
}

// rangedLinkParser is goldmark's link parser, noting the range of each link or picture it reads
// in the parse's context (linkRangesKey): it opens where the node says, its words end at the `]`
// the parser reads it from, and it closes at the `)` the parser has just read past - or the `]`,
// for a link to a reference definition, which has no address of its own written there.
type rangedLinkParser struct {
	parser.InlineParser
}

func (p rangedLinkParser) Parse(parent ast.Node, block markdowntext.Reader, pc parser.Context) ast.Node {
	// The parser reads a link from the `]` its words end at, which is where the reader stands.
	_, at := block.Position()
	node := p.InlineParser.Parse(parent, block, pc)
	var destination []byte
	closer := byte(')')
	switch link := node.(type) {
	case *ast.Link:
		destination = link.Destination
		if link.Reference != nil {
			closer, destination = ']', nil
		}
	case *ast.Image:
		destination = link.Destination
		if link.Reference != nil {
			closer, destination = ']', nil
		}
	default:
		return node
	}
	ranges, held := pc.Get(linkRangesKey).(*[]linkRange)
	if !held {
		return node
	}
	_, after := block.Position()
	closed := bytes.LastIndexByte(block.Source()[:after.Start], closer) + 1
	*ranges = append(*ranges, linkRange{
		open: node.Pos(), words: at.Start, close: closed, destination: string(destination),
	})
	return node
}

// CloseBlock is the link parser's own: it drops the brackets a block left open, so a `[` of one
// paragraph opens no link in the next. A wrapper that did not pass it on would let it.
func (p rangedLinkParser) CloseBlock(parent ast.Node, block markdowntext.Reader, pc parser.Context) {
	if closer, held := p.InlineParser.(parser.CloseBlocker); held {
		closer.CloseBlock(parent, block, pc)
	}
}
