package pmdoc

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// notSpaceOrParen is the browser's `[^()\s]`, the characters a link destination the cut left
// open may hold: Go's own `\s` is the five ASCII blanks alone, so the Unicode separators
// JavaScript's `\s` holds stand beside it.
const notSpaceOrParen = `[^()\s\v\p{Zs}\x{2028}\x{2029}\x{feff}]`

// The three shapes a cut leaves a link in, each the browser's own pattern: a link whose
// destination the cut left open, a link whose text it left open, and a link whose opening the cut
// at the fragment's start took. Each keeps the link's words and drops its syntax.
var (
	cutOpenLinkDestination = regexp.MustCompile(`\[([^\[\]]*)\]\(` + notSpaceOrParen + `*$`)
	cutOpenLinkText        = regexp.MustCompile(`\[([^\[\]]*)$`)
	cutClosedLinkTail      = regexp.MustCompile(`^([^\[\]]*)\]\(` + notSpaceOrParen + `*\)`)
)

// A fragment cut out of a Markdown body: one the server cut before anybody read it, which today
// is a search hit's ts_headline snippet (the 24 words around the match), widened back to the
// source's own constructs (SearchSnippet). A cut can still land inside a mark, and the half it
// leaves renders as its characters. The balancer below drops exactly the delimiters that
// CommonMark would have paired had the cut not happened and nothing else: a run the parser would
// read as literal text anyway (`Use ** for bold`, `2 * 3`, `snake_case`) is the author's and
// stays, and nothing inside a code span is ever a delimiter.
//
// This began as the port of the browser's own balancer, which the palette ran on a snippet
// before the server read one. That reading is gone from the browser with the rest of its
// markdown parsing (the server sends the snippet's tree now), so this is the one balancer left
// and the shapes it must hold are the ones fragment_test.go pins. The browser's markdownHead
// was never ported, since nothing on the server cuts a body's head.

// SearchSnippetWords is how many words ts_headline's window holds (headlineOptions in
// internal/dispatch/api/search.go: MaxWords=24). A source of no more words than that is one the
// window held whole, so its snippet is the whole source; and a headline SearchSnippet cannot find
// in its source is read as a cut only when it is that wide.
const SearchSnippetWords = 24

// searchSnippetLink is an inline link `[text](dest)` written on one line, its text holding no
// bracket: the link BalanceCutMarkdown's three patterns mend when a cut lands inside one, so a
// snippet widened past every such link leaves the balancer no link of that shape to mend.
var searchSnippetLink = regexp.MustCompile(`\[[^\[\]\n]*\]\(` + notSpaceOrParen + `*\)`)

// SearchSnippet is the Markdown to read for a search hit: headline, the text ts_headline answered
// with the start and stop sentinels around what the query matched, set back into source, the
// text ts_headline read, and widened to the constructs of source it cut into. The sentinels stay
// in the answer for the display read and the HTML snippet to mark.
//
// The source is read because the headline alone does not say what its words are. ts_headline
// chooses a window of words and drops the punctuation it does not count as words, so the cut
// lands inside a construct, and the cut text reads as something the stored text never wrote
// (measured on PostgreSQL 18 with headlineOptions, INFRA-1702). "Run `CORE-1` locally", searched
// for CORE-1, comes back as "CORE-1` locally": the opening backtick is lost, so code reads as
// prose and the key as a reference. "See [the gyroscope](dispatch://CORE-1) now", searched for
// CORE-1 too, comes back as "gyroscope](dispatch://CORE-1", a link's broken tail. "Ask
// dispatch://person/alice first", searched for first, comes back as "person/alice first", an
// address without its scheme. And "The release is blocked on CORE-99 until it lands" comes back
// without its first word, so a headline is rarely the whole body even when the body is short.
//
// So the headline is found in the source, and the range [from, to) it stands at is widened until
// nothing moves: to whitespace-delimited token edges, and over every code span and every inline
// link it starts or ends strictly inside. A source of no more than SearchSnippetWords words is the
// whole source, since ts_headline's window held it all and what it left out is the edge words it
// trims. A headline that starts inside a fenced or indented code block is widened to whole lines,
// with the fence's opening line in front of it when the cut left that line out, so it reads as
// the code it is; the balancer never runs there, since it would take characters out of code.
// Anywhere else the result is balanced when it is a cut of the source, and answered as it is when
// it is the whole of it.
func SearchSnippet(headline, source, start, stop string) string {
	plain := strings.NewReplacer(start, "", stop, "").Replace(headline)
	at := -1
	if plain != "" {
		at = strings.Index(source, plain)
	}
	if at < 0 {
		// A fallback for a source ts_headline did not read verbatim: with nothing to set the
		// headline back into, it is read alone, whole when it is shorter than the window and
		// balanced as a cut when it is the window wide.
		if countWords(headline) < SearchSnippetWords {
			return headline
		}
		return BalanceCutMarkdown(headline)
	}
	end := at + len(plain)
	if countWords(source) <= SearchSnippetWords {
		return source[:at] + headline + source[end:]
	}
	code := text.CodeRanges(source)
	if block, ok := codeBlockHolding(code, at); ok {
		from := strings.LastIndexByte(source[:at], '\n') + 1
		fence := ""
		if opener := source[block.Start:snippetLineEnd(source, block.Start)]; from > block.Start && opensFence(opener) {
			fence = opener + "\n"
		}
		return fence + source[from:at] + headline + source[end:snippetLineEnd(source, end)]
	}
	from, to := widenSnippet(source, at, end, code)
	snippet := source[from:at] + headline + source[end:to]
	if from == 0 && to == len(source) {
		return snippet
	}
	return BalanceCutMarkdown(snippet)
}

// widenSnippet widens [from, to) within source until nothing moves: to the edges of the
// whitespace-delimited tokens it cuts, and over every code span (a range of code that is no
// block) and every inline link (searchSnippetLink) that holds from or to strictly inside it. A
// span or a link may hold whitespace of its own, and the token past its edge may start another,
// so the steps repeat until one pass moves neither edge.
func widenSnippet(source string, from, to int, code []text.CodeRange) (int, int) {
	links := searchSnippetLink.FindAllStringIndex(source, -1)
	for {
		moved := false
		for from > 0 && !snippetSpace(source[from-1]) {
			from--
			moved = true
		}
		for to < len(source) && !snippetSpace(source[to]) {
			to++
			moved = true
		}
		widen := func(start, end int) {
			if start < from && from < end {
				from, moved = start, true
			}
			if start < to && to < end {
				to, moved = end, true
			}
		}
		for _, span := range code {
			if !span.Block {
				widen(span.Start, span.End)
			}
		}
		for _, link := range links {
			widen(link[0], link[1])
		}
		if !moved {
			return from, to
		}
	}
}

// snippetSpace reports whether a byte of the source separates two tokens. Every byte of a
// multi-byte character is above ASCII, so the token edges widenSnippet moves to never split one.
func snippetSpace(char byte) bool {
	switch char {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// codeBlockHolding is the fenced or indented code block of code that holds the byte at offset.
func codeBlockHolding(code []text.CodeRange, offset int) (text.CodeRange, bool) {
	for _, candidate := range code {
		if candidate.Start > offset {
			break
		}
		if candidate.Block && offset < candidate.End {
			return candidate, true
		}
	}
	return text.CodeRange{}, false
}

// snippetLineEnd is where the line holding offset ends, before its line feed, or the end of
// source.
func snippetLineEnd(source string, offset int) int {
	if next := strings.IndexByte(source[offset:], '\n'); next >= 0 {
		return offset + next
	}
	return len(source)
}

// opensFence reports whether line, a code block's first line, opens a fenced block: past its
// indentation and its containers' markers - a block quote's `>`, a list item's bullet or number -
// it starts with three or more backticks or tildes. An indented block's first line is code
// itself and starts with neither.
func opensFence(line string) bool {
	for {
		line = strings.TrimLeft(line, " \t")
		marker := containerMarkerWidth(line)
		if marker == 0 {
			break
		}
		line = line[marker:]
	}
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// containerMarkerWidth is the width of the container marker line starts with - a block quote's
// `>`, or a list item's `-`, `+`, `*`, or one to nine digits and a `.` or `)`, followed by a
// space or a tab - or 0 when it starts with none.
func containerMarkerWidth(line string) int {
	if strings.HasPrefix(line, ">") {
		return 1
	}
	width := 0
	switch {
	case line != "" && strings.IndexByte("-+*", line[0]) >= 0:
		width = 1
	default:
		for width < len(line) && width < 9 && line[width] >= '0' && line[width] <= '9' {
			width++
		}
		if width == 0 || width == len(line) || (line[width] != '.' && line[width] != ')') {
			return 0
		}
		width++
	}
	if width < len(line) && (line[width] == ' ' || line[width] == '\t') {
		return width
	}
	return 0
}

// countWords is how many whitespace-separated words text holds, which is what the browser's
// `snippet.split(/\s+/u).filter(word => word !== "")` counts.
func countWords(text string) int {
	words, inWord := 0, false
	for _, char := range text {
		if literalEnd(char) {
			inWord = false
			continue
		}
		if !inWord {
			words++
			inWord = true
		}
	}
	return words
}

// BalanceCutMarkdown is fragment with every delimiter the cut left unmatched removed. Runs are
// paired as the parser would pair them (a closer with the nearest earlier opener of the same
// character, the run lengths consumed in order); whatever run, or part of a run, is left over on
// either side could only have been matched across the cut, so it goes. A link cut inside its text
// or URL at either edge keeps its words.
func BalanceCutMarkdown(fragment string) string {
	text := cutOpenLinkDestination.ReplaceAllString(fragment, "$1")
	text = cutOpenLinkText.ReplaceAllString(text, "$1")
	text = cutClosedLinkTail.ReplaceAllString(text, "$1")

	runs := cutDelimiterRuns(text, codeSpans(text))
	// Remaining unmatched length per run, consumed as pairs are made.
	remaining := make([]int, len(runs))
	for index := range runs {
		remaining[index] = runs[index].end - runs[index].start
	}
	openers := make([]int, 0, len(runs))
	for index := range runs {
		run := runs[index]
		if run.canClose {
			for o := len(openers) - 1; o >= 0 && remaining[index] > 0; o-- {
				opener := runs[openers[o]]
				if opener.char != run.char || remaining[openers[o]] == 0 {
					continue
				}
				// GFM pairs a tilde run only with a closer of its own length (`~~a~` is plain
				// text); CommonMark's emphasis pairs one or two characters of a `*` or `_` run
				// at a time.
				take := 0
				if run.char == '~' {
					if remaining[openers[o]] == remaining[index] {
						take = remaining[index]
					}
				} else {
					take = min(remaining[openers[o]], remaining[index], 2)
				}
				if take == 0 {
					continue
				}
				remaining[openers[o]] -= take
				remaining[index] -= take
				if remaining[openers[o]] == 0 {
					openers = append(openers[:o], openers[o+1:]...)
				}
			}
		}
		if run.canOpen && remaining[index] > 0 {
			openers = append(openers, index)
		}
	}
	text = withoutLeftovers(text, runs, remaining)

	// A backtick run with no closer is a code span the cut opened when the span it opened would
	// run to the fragment's end: the cut took its closer. The text after it is the code the cut
	// left - a path, an identifier - with no space-separated prose in it (`\`dispatch-deploy.y`).
	// A lone tick followed by ordinary words (`press the \` key then`) is the author's, as
	// CommonMark reads it.
	start, stop := lastBacktickRun(text)
	if start < 0 || withinSpan(codeSpans(text), start) || proseFollows(text[stop:]) {
		return text
	}
	return text[:start] + text[stop:]
}

// withoutLeftovers is text without the characters of every run the pairing left over. The
// unmatched characters go from the side nearer the cut: an opener keeps its tail, a closer its
// head. Either way the run's leftover is gone. The runs are in text order and their leftovers
// never overlap, so one pass writes what is kept.
func withoutLeftovers(text string, runs []cutDelimiterRun, remaining []int) string {
	var kept strings.Builder
	written := 0
	for index := range runs {
		if remaining[index] == 0 {
			continue
		}
		run := runs[index]
		start, stop := run.end-remaining[index], run.end
		if run.canOpen && !run.canClose {
			start, stop = run.start, run.start+remaining[index]
		}
		if kept.Len() == 0 {
			kept.Grow(len(text))
		}
		kept.WriteString(text[written:start])
		written = stop
	}
	if written == 0 {
		return text
	}
	kept.WriteString(text[written:])
	return kept.String()
}

// cutDelimiterRun is one run of `*`, `_` or `~` the parser could pair: where it stands in the text,
// and whether CommonMark's flanking rules let it open or close emphasis.
type cutDelimiterRun struct {
	char       byte
	start, end int
	canOpen    bool
	canClose   bool
}

// cutDelimiterRuns is CommonMark's delimiter runs of `*`, `_` and `~` outside code spans, each with
// whether it can open or close emphasis (the left- and right-flanking rules, with `_`'s
// word-internal restriction). A run that can do neither is literal text and is never a candidate.
func cutDelimiterRuns(text string, code [][2]int) []cutDelimiterRun {
	var runs []cutDelimiterRun
	for index := 0; index < len(text); {
		if span := spanEnd(code, index); span > 0 {
			index = span
			continue
		}
		char := text[index]
		if char != '*' && char != '_' && char != '~' {
			index++
			continue
		}
		end := index
		for end < len(text) && text[end] == char {
			end++
		}
		// GFM ends the strikethrough sequence at a third marker, so `~~~` is literal text rather
		// than a delimiter of any length.
		if char == '~' && end-index > 2 {
			index = end
			continue
		}
		before, after := classifyBefore(text, index), classifyAt(text, end)
		leftFlanking := after != fragmentSpace &&
			(after != fragmentPunctuation || before == fragmentSpace || before == fragmentPunctuation)
		rightFlanking := before != fragmentSpace &&
			(before != fragmentPunctuation || after == fragmentSpace || after == fragmentPunctuation)
		canOpen, canClose := leftFlanking, rightFlanking
		if char == '_' {
			canOpen = leftFlanking && (!rightFlanking || before == fragmentPunctuation)
			canClose = rightFlanking && (!leftFlanking || after == fragmentPunctuation)
		}
		if canOpen || canClose {
			runs = append(runs, cutDelimiterRun{char: char, start: index, end: end, canOpen: canOpen, canClose: canClose})
		}
		index = end
	}
	return runs
}

// codeSpans is the spans of text that are code spans, as CommonMark reads them: a backtick run
// and the next run of the same length close it; a run with no closer is literal text.
func codeSpans(text string) [][2]int {
	var spans [][2]int
	for index := 0; index < len(text); {
		if text[index] != '`' {
			index++
			continue
		}
		runEnd := index
		for runEnd < len(text) && text[runEnd] == '`' {
			runEnd++
		}
		fence := text[index:runEnd]
		closer := indexFrom(text, fence, runEnd)
		for closer >= 0 && closer+len(fence) < len(text) && text[closer+len(fence)] == '`' {
			// A longer run is not this span's closer; skip past it.
			past := closer
			for past < len(text) && text[past] == '`' {
				past++
			}
			closer = indexFrom(text, fence, past)
		}
		if closer < 0 {
			index = runEnd
			continue
		}
		spans = append(spans, [2]int{index, closer + len(fence)})
		index = closer + len(fence)
	}
	return spans
}

// indexFrom is where needle next stands in text at or after from, or -1 when it does not.
func indexFrom(text, needle string, from int) int {
	if from >= len(text) {
		return -1
	}
	at := strings.Index(text[from:], needle)
	if at < 0 {
		return -1
	}
	return from + at
}

// spanEnd is where the code span holding index ends, or 0 when index stands in none.
func spanEnd(spans [][2]int, index int) int {
	for _, span := range spans {
		if index >= span[0] && index < span[1] {
			return span[1]
		}
	}
	return 0
}

// withinSpan reports whether index stands inside one of the code spans.
func withinSpan(spans [][2]int, index int) bool {
	return spanEnd(spans, index) > 0
}

// lastBacktickRun is where the last run of backticks in text starts and stops, or -1 when text
// holds none: the browser's /`+(?!.*`)/su, which matches the final run whole.
func lastBacktickRun(text string) (start, stop int) {
	stop = strings.LastIndexByte(text, '`') + 1
	if stop == 0 {
		return -1, 0
	}
	start = stop - 1
	for start > 0 && text[start-1] == '`' {
		start--
	}
	return start, stop
}

// proseFollows reports whether text holds space-separated prose: the browser's /\s\S+\s\S/u, a
// whitespace, a word, a whitespace and the start of another word. Code the cut left - a path, an
// identifier - holds none.
func proseFollows(text string) bool {
	for index, char := range text {
		if !literalEnd(char) {
			continue
		}
		rest := text[index+utf8.RuneLen(char):]
		word := 0
		for word < len(rest) {
			next, size := utf8.DecodeRuneInString(rest[word:])
			if literalEnd(next) {
				break
			}
			word += size
		}
		if word == 0 || word >= len(rest) {
			continue
		}
		_, size := utf8.DecodeRuneInString(rest[word:])
		if tail := rest[word+size:]; tail != "" {
			if next, _ := utf8.DecodeRuneInString(tail); !literalEnd(next) {
				return true
			}
		}
	}
	return false
}

// fragmentGroup is the group micromark's classifyCharacter puts a character in: whitespace,
// punctuation, or neither, which is what the flanking rules read.
type fragmentGroup int

const (
	fragmentWord fragmentGroup = iota
	fragmentSpace
	fragmentPunctuation
)

// classifyBefore is the group of the code point ending at index, or whitespace at the fragment's
// start, which is what the tokenizer classifies there. The code point is taken whole, so an
// astral character is the character CommonMark reads rather than a trailing surrogate of it: a
// `**` after an emoji follows punctuation (every emoji is `\p{S}`), which decides whether that
// run can open emphasis.
func classifyBefore(text string, index int) fragmentGroup {
	if index <= 0 {
		return fragmentSpace
	}
	char, _ := utf8.DecodeLastRuneInString(text[:index])
	return classifyFragmentCharacter(char)
}

// classifyAt is the group of the code point standing at index, or whitespace at the fragment's
// end, where the tokenizer classifies the absent character.
func classifyAt(text string, index int) fragmentGroup {
	if index >= len(text) {
		return fragmentSpace
	}
	char, _ := utf8.DecodeRuneInString(text[index:])
	return classifyFragmentCharacter(char)
}

// classifyFragmentCharacter is micromark's classifyCharacter (micromark-util-classify-character):
// whitespace first, then punctuation, which it reads as `\p{P}|\p{S}` exactly as the directive
// name reader does (directivePunctuation).
func classifyFragmentCharacter(char rune) fragmentGroup {
	switch {
	case literalEnd(char):
		return fragmentSpace
	case directivePunctuation(char):
		return fragmentPunctuation
	default:
		return fragmentWord
	}
}
