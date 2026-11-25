// Package ste is a Go port of the seven hard rules of ste-lint, the ASD-STE100 linter of
// github.com/danyuchn/asd-ste100-skill at commit 32511c6992ecb5f1971e46a2943f2e6adceedafe
// (scripts/ste-lint.py, kept verbatim as testdata/ste-lint.py). It is under that program's MIT
// License, in LICENSE beside this file. The word lists are data, in rules.json.
//
// The hard ("advisory-free") rules are semicolon, phrasal-verb, marketing-adjective,
// nominalization, long-sentence, synonym-rotation and dangling-conjunction. The two advisory
// rules, passive-voice and present-perfect, never fail ste-lint and are not here.
//
// Python's re and str differ from Go's regexp and strings in four places, and this port
// follows Python in each by hand:
//   - str.splitlines ends a line at \n, \r, \r\n, \v, \f, \x1c, \x1d, \x1e, \x85, U+2028 and
//     U+2029, and not only at \n (splitLines).
//   - Python's whitespace (str.split, str.strip and \s) is Go's unicode.IsSpace plus \x1c to
//     \x1f (isSpace, spaceClass). RE2 has no lookbehind, so the sentence split is a scan.
//   - Python's \w and \b read Unicode: a word character is a letter, a number or "_" (isWord),
//     where Go's are ASCII. Every pattern here starts and ends with a word character, and a
//     match counts only with a word boundary on each side (boundedMatches).
//   - Python's re.IGNORECASE also matches "ı" (U+0131) and "İ" (U+0130) to "i", which Go's
//     case folding does not (pythonFold). Both match "ſ" to "s" and "K" (U+212A) to "k".
//
// TestPortMatchesSteLintOnTheCorpus holds the port to the Python program on testdata/corpus.jsonl,
// through the program's findings recorded as testdata/corpus-findings.jsonl.
package ste

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/bits"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Finding is one hard finding of ste-lint: the rule's name as ste-lint writes it, and the text
// it matched ("30 words" for long-sentence).
type Finding struct {
	Rule  string
	Match string
}

//go:embed rules.json
var rulesJSON []byte

// ruleData is rules.json: ste-lint's word lists, each a Python regular expression fragment.
type ruleData struct {
	MaxWords               int        `json:"max_words"`
	PhrasalVerbs           []string   `json:"phrasal_verbs"`
	MarketingAdjectives    []string   `json:"marketing_adjectives"`
	NominalizationVerbs    []string   `json:"nominalization_verbs"`
	NominalizationSuffixes []string   `json:"nominalization_suffixes"`
	SynonymGroups          [][]string `json:"synonym_groups"`
}

// spaceClass is Python's whitespace as a character class of a Go pattern.
const spaceClass = `[\t\n\v\f\r \x{85}\x{a0}\x{1c}-\x{1f}\p{Z}]`

// wordClass is Python's \w for str patterns: a letter, a number or "_".
const wordClass = `[\p{L}\p{N}_]`

var (
	inlineCode    = regexp.MustCompile("`[^`]*`")
	listItemStart = regexp.MustCompile(`^( {0,3})([-*+]|[0-9]+[.)])( +)(.*)$`)
	tableRule     = regexp.MustCompile(`^:?-{3,}:?$`)
	rules         = compileRules(rulesJSON)
)

type compiled struct {
	maxWords       int
	phrasal        *regexp.Regexp
	marketing      *regexp.Regexp
	nominalization *regexp.Regexp
	// synonyms holds, for each group, one pattern per member: the member and its inflections.
	synonyms [][]*regexp.Regexp
	// members is the word of each member of each group, as rules.json writes it, first the bit of
	// the first member of each group in Verbs, and groups the bits of all members of each group.
	members [][]string
	first   []int
	groups  []Verbs
}

// Verbs is the members of ste-lint's synonym groups that a text uses, one bit for each member of
// each group in the order of rules.json, so that the verbs of two texts join in one step.
type Verbs uint64

func compileRules(raw []byte) compiled {
	var data ruleData
	if err := json.Unmarshal(raw, &data); err != nil {
		panic(fmt.Sprintf("ste: rules.json: %v", err))
	}
	alternation := func(fragments []string) string {
		folded := make([]string, len(fragments))
		for index, fragment := range fragments {
			folded[index] = pythonFold(fragment)
		}
		return "(?:" + strings.Join(folded, "|") + ")"
	}
	rule := compiled{
		maxWords:  data.MaxWords,
		phrasal:   regexp.MustCompile("(?i)" + alternation(data.PhrasalVerbs)),
		marketing: regexp.MustCompile("(?i)" + alternation(data.MarketingAdjectives)),
		nominalization: regexp.MustCompile("(?i)" + alternation(data.NominalizationVerbs) +
			spaceClass + "+" + alternation([]string{"a", "an", "the"}) + spaceClass + "+" +
			wordClass + "+" + alternation(data.NominalizationSuffixes)),
	}
	for _, group := range data.SynonymGroups {
		patterns := make([]*regexp.Regexp, len(group))
		for index, base := range group {
			patterns[index] = regexp.MustCompile("(?i)" + pythonFold(regexp.QuoteMeta(base)) +
				alternation([]string{"s", "es", "ed", "d", "ing"}) + "?")
		}
		rule.first = append(rule.first, verbCount(rule.members))
		rule.synonyms = append(rule.synonyms, patterns)
		rule.members = append(rule.members, group)
		rule.groups = append(rule.groups, (Verbs(1)<<len(group)-1)<<rule.first[len(rule.first)-1])
	}
	if count := verbCount(rule.members); count > 64 {
		panic(fmt.Sprintf("ste: rules.json has %d members of synonym groups, and Verbs holds 64", count))
	}
	return rule
}

// verbCount is how many members the groups hold.
func verbCount(groups [][]string) int {
	count := 0
	for _, group := range groups {
		count += len(group)
	}
	return count
}

// pythonFold writes each "i" of a pattern outside a character class as a class that also holds
// the two letters that Python's re.IGNORECASE matches to it and Go's (?i) does not.
func pythonFold(pattern string) string {
	var out strings.Builder
	inClass := false
	for index := 0; index < len(pattern); index++ {
		char := pattern[index]
		switch {
		case char == '\\' && index+1 < len(pattern):
			out.WriteByte(char)
			index++
			out.WriteByte(pattern[index])
			continue
		case char == '[':
			inClass = true
		case char == ']':
			inClass = false
		case char == 'i' && !inClass:
			out.WriteString(`[i\x{131}\x{130}]`)
			continue
		}
		out.WriteByte(char)
	}
	return out.String()
}

// isSpace is Python's str.isspace for one character.
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

// spacePattern matches one character of spaceClass, for the test that holds it to isSpace.
func spacePattern() *regexp.Regexp {
	return regexp.MustCompile("^" + spaceClass + "$")
}

// isWord is Python's \w for one character of a str.
func isWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_'
}

// Strip is Python's str.strip with no argument: s without the white space that ste-lint reads
// (isSpace) at its two ends. A caller that cuts a line into sentences names each by it, so a
// sentence starts at its first word and ends at its last.
func Strip(s string) string {
	return strings.TrimFunc(s, isSpace)
}

// fields is Python's str.split with no argument.
func fields(s string) []string {
	return strings.FieldsFunc(s, isSpace)
}

// runes is the number of characters of s, which is how Python counts a string's length and
// indexes it. It is the one count of characters of the port, and a variable, so a test can count
// the characters that Lint counts (TestEachRuleThatWalksMatchesCountsEachCharacterOfALineOnce).
var runes = utf8.RuneCountInString

// lineBreak reports whether Python's str.splitlines ends a line at r.
func lineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// splitLines is Python's str.splitlines: no line ending is kept, "\r\n" is one ending, and a
// last ending makes no empty last line. starts is the byte of text where each line starts.
func splitLines(text string) (lines []string, starts []int) {
	start := 0
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if !lineBreak(r) {
			index += size
			continue
		}
		lines, starts = append(lines, text[start:index]), append(starts, start)
		index += size
		if r == '\r' && index < len(text) && text[index] == '\n' {
			index++
		}
		start = index
	}
	if start < len(text) {
		lines, starts = append(lines, text[start:]), append(starts, start)
	}
	return lines, starts
}

// wordBoundary is Python's \b at byte offset at of s.
func wordBoundary(s string, at int) bool {
	before, after := false, false
	if at > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:at])
		before = isWord(r)
	}
	if at < len(s) {
		r, _ := utf8.DecodeRuneInString(s[at:])
		after = isWord(r)
	}
	return before != after
}

// boundedMatches is Python's finditer of `\b(?:pattern)\b` over line, as byte spans: the
// leftmost match of pattern, kept when a word boundary stands on each side of it, and the search
// goes on one character later when not. Each pattern here has one end for each start (its
// alternatives differ in their text, and an optional suffix is made of word characters), so the
// match RE2 prefers at a start is the one that Python's backtracking would keep there. With first
// it stops at the first kept match, as Python's search does.
func boundedMatches(pattern *regexp.Regexp, line string, first bool) [][2]int {
	var spans [][2]int
	for at := 0; at < len(line); {
		found := pattern.FindStringIndex(line[at:])
		if found == nil {
			break
		}
		start, end := at+found[0], at+found[1]
		if end > start && wordBoundary(line, start) && wordBoundary(line, end) {
			spans = append(spans, [2]int{start, end})
			if first {
				break
			}
			at = end
			continue
		}
		_, size := utf8.DecodeRuneInString(line[start:])
		at = start + max(size, 1)
	}
	return spans
}

// sentencePieces is Python's re.split(r"(?<=[.!?])\s+", line): the line split at each run of
// whitespace that follows ".", "!" or "?".
func sentencePieces(line string) []string {
	var pieces []string
	start := 0
	previous := rune(-1)
	for index := 0; index < len(line); {
		r, size := utf8.DecodeRuneInString(line[index:])
		if isSpace(r) && (previous == '.' || previous == '!' || previous == '?') {
			pieces = append(pieces, line[start:index])
			for index < len(line) {
				r, size = utf8.DecodeRuneInString(line[index:])
				if !isSpace(r) {
					break
				}
				index += size
			}
			start, previous = index, ' '
			continue
		}
		previous = r
		index += size
	}
	return append(pieces, line[start:])
}

// segment is a piece of a line that ste-lint lints on its own: the whole line, or one cell of a
// markdown table row. column is where it starts in the line, in characters, and start where it
// starts in bytes: in its line, and in the text once eachSegment visits it. cuts is each span of
// text, in bytes, that inline code takes, which the text that the rules lint leaves out.
type segment struct {
	text   string
	column int
	start  int
	cuts   [][]int
}

// splitTableRow is ste-lint's _split_table_row: the trimmed cells of a row and their columns, or
// false for a line that is no table row (it holds no "|" that separates two cells).
func splitTableRow(line string) ([]segment, bool) {
	leftTrimmed := strings.TrimLeftFunc(line, isSpace)
	leftByte, right := len(line)-len(leftTrimmed), len(strings.TrimRightFunc(line, isSpace))
	if leftByte >= right {
		return nil, false
	}
	content, left, at := line[leftByte:right], runes(line[:leftByte]), leftByte
	if !strings.Contains(content, "|") {
		return nil, false
	}
	if strings.HasPrefix(content, "|") {
		content = content[1:]
		left++
		at++
	}
	content = strings.TrimSuffix(content, "|")
	var raw []string
	from := 0
	for index := range len(content) {
		if content[index] == '|' && (index == 0 || content[index-1] != '\\') {
			raw = append(raw, content[from:index])
			from = index + 1
		}
	}
	raw = append(raw, content[from:])
	if len(raw) < 2 {
		return nil, false
	}
	cells := make([]segment, len(raw))
	column := left
	for index, cell := range raw {
		trimmed := strings.TrimLeftFunc(cell, isSpace)
		leading := runes(cell) - runes(trimmed)
		cells[index] = segment{text: Strip(cell), column: column + leading, start: at + len(cell) - len(trimmed)}
		column += runes(cell) + 1
		at += len(cell) + 1
	}
	return cells, true
}

// tableCells is ste-lint's _markdown_table_cells: for each line of a table, by index, the cells
// that are linted in place of the line. A separator row maps to no cell.
func tableCells(lines []string) map[int][]segment {
	cells := map[int][]segment{}
	for index := 1; index < len(lines); {
		separator, isRow := splitTableRow(lines[index])
		header, headerIsRow := splitTableRow(lines[index-1])
		if !isRow || !headerIsRow || len(separator) != len(header) || !allRules(separator) {
			index++
			continue
		}
		cells[index-1] = header
		cells[index] = []segment{}
		for index++; index < len(lines); index++ {
			row, ok := splitTableRow(lines[index])
			if !ok || len(row) != len(separator) {
				break
			}
			cells[index] = row
		}
	}
	return cells
}

func allRules(cells []segment) bool {
	for _, cell := range cells {
		if !tableRule.MatchString(cell.text) {
			return false
		}
	}
	return true
}

func isFence(stripped string) bool {
	return strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~")
}

// NoFences is text with each fence marker that ste-lint reads written as white space, byte for
// byte, for a text that is no markdown document, as a text of Dispatch is: on each line
// (splitLines) that starts with "```" or "~~~" after its white space (isFence), the run of
// backticks or tildes that starts it, the characters right after the run up to white space or the
// run's own character (a language, such as "sh" in "```sh"), and the next run of as many of that
// character on the line, which closes it; and so again while the line still starts with such a
// run. So Lint reads each line of the text that it returns, and the words after a marker as
// words, where ste-lint reads no word from the marker to the next one; and each byte stays where
// it was, so an offset of the one text is the offset of the same character of the other.
func NoFences(text string) string {
	var written []byte
	lines, starts := splitLines(text)
	for index, line := range lines {
		start := starts[index]
		for {
			if written != nil {
				line = string(written[start : start+len(line)])
			}
			rest := strings.TrimLeftFunc(line, isSpace)
			if !isFence(rest) {
				break
			}
			if written == nil {
				written = []byte(text)
			}
			mark := rest[0]
			at := len(line) - len(rest)
			run := len(rest) - len(strings.TrimLeft(rest, rest[:1]))
			end := at + run
			if after := strings.IndexFunc(line[end:], func(r rune) bool { return isSpace(r) || r == rune(mark) }); after >= 0 {
				end += after
			} else {
				end = len(line)
			}
			blank(written[start+at : start+end])
			for search := end; search < len(line); {
				next := strings.IndexByte(line[search:], mark)
				if next < 0 {
					break
				}
				search += next
				closing := len(line[search:]) - len(strings.TrimLeft(line[search:], rest[:1]))
				if closing == run {
					blank(written[start+search : start+search+closing])
					break
				}
				search += closing
			}
		}
	}
	if written == nil {
		return text
	}
	return string(written)
}

// blank writes each byte of b as a space.
func blank(b []byte) {
	for index := range b {
		b[index] = ' '
	}
}

// eachSegment visits the text that ste-lint's line loop lints, in its order: each segment of
// each line, with its inline code removed (from.cuts), outside a code fence where fences is set,
// as ste-lint reads a document. line is the 1-based line number, and starts the byte where each
// line starts in the text (splitLines), so from.start is where the segment starts in the text.
func eachSegment(lines []string, starts []int, fences bool, visit func(line int, from segment, text string)) {
	tables := tableCells(lines)
	inFence := false
	for index, raw := range lines {
		if fences && isFence(Strip(raw)) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		segments, ok := tables[index]
		if !ok {
			segments = []segment{{text: raw}}
		}
		for _, from := range segments {
			from.start += starts[index]
			from.cuts = inlineCode.FindAllStringIndex(from.text, -1)
			visit(index+1, from, withoutCuts(from.text, from.cuts))
		}
	}
}

// withoutCuts is text without the spans that cuts names, in order: the text that a rule lints,
// as Python's re.sub of inline code with "" writes it.
func withoutCuts(text string, cuts [][]int) string {
	if len(cuts) == 0 {
		return text
	}
	var kept strings.Builder
	from := 0
	for _, cut := range cuts {
		kept.WriteString(text[from:cut[0]])
		from = cut[1]
	}
	kept.WriteString(text[from:])
	return kept.String()
}

// Word is one word of Words: its characters, with inline code removed, and the bytes of the text
// that Words reads that it takes, from its first character to the end of its last (Start, End),
// so inline code inside the word stands between them.
type Word struct {
	Text       string
	Start, End int
}

// Words is each piece of text that ste-lint lints, as its words, as LintVerbs reads them: a
// table's cells one by one, with inline code removed, split at Python's white space, each word with
// the bytes of text that it takes, and no line a fence. A piece with no word is left out. Where a
// sentence ends is the caller's to decide.
func Words(text string) [][]Word {
	var pieces [][]Word
	lines, starts := splitLines(text)
	eachSegment(lines, starts, false, func(_ int, from segment, kept string) {
		var words []Word
		// cut is the next cut of from.cuts, and removed the bytes of the cuts before it: a byte
		// of kept stands at from.start, its offset and removed, and the words go in the order of
		// the text, so the cuts are read once.
		cut, removed := 0, 0
		at := func(offset int) int {
			for cut < len(from.cuts) && from.cuts[cut][0] <= offset+removed {
				removed += from.cuts[cut][1] - from.cuts[cut][0]
				cut++
			}
			return from.start + offset + removed
		}
		start := -1
		for index, char := range kept {
			switch space := isSpace(char); {
			case !space && start < 0:
				start = index
			case space && start >= 0:
				words = append(words, Word{Text: kept[start:index], Start: at(start), End: at(index-1) + 1})
				start = -1
			}
		}
		if start >= 0 {
			words = append(words, Word{Text: kept[start:], Start: at(start), End: at(len(kept)-1) + 1})
		}
		if len(words) > 0 {
			pieces = append(pieces, words)
		}
	})
	return pieces
}

// located is a finding with the line and column that ste-lint orders findings by.
type located struct {
	line, column int
	Finding
}

// columns is the column ste-lint gives a byte offset of a segment's text: the 1-based character
// index in the segment's line. It counts the characters from the offset it last answered, so a
// walk over offsets in increasing order counts each character of the text once, and an offset
// before the last one counts again from the segment's start.
type columns struct {
	text           string
	start          int
	offset, counts int
}

func (c *columns) at(offset int) int {
	if offset < c.offset {
		c.offset, c.counts = 0, 0
	}
	c.counts += runes(c.text[c.offset:offset])
	c.offset = offset
	return c.start + c.counts + 1
}

// Lint is ste-lint's hard findings for text, in ste-lint's order: by line and column, and in
// the order ste-lint found them where those are equal. It reads text as ste-lint reads a
// document, so a line that starts with "```" or "~~~" opens or closes a fence, and it lints no line
// of a fence.
func Lint(text string) []Finding {
	findings, _ := lint(text, true)
	return findings
}

// LintVerbs is Lint of a text that is no markdown document, such as the one line of a text of
// Dispatch or a sentence of it, and the members of the synonym groups that text uses (Verbs), each
// as it reads them for the rule synonym-rotation: so the verbs of one text can stand beside the
// words of another (Rotations). It reads no line as a fence: a caller hands it a text whose fence
// markers NoFences wrote as white space, or a piece of one, which can start with three backticks of
// code font where no line starts.
func LintVerbs(text string) ([]Finding, Verbs) {
	return lint(text, false)
}

// lint is Lint, and the verbs of LintVerbs, of text, with a line that starts with a fence marker
// opening or closing a fence where fences is set.
func lint(text string, fences bool) ([]Finding, Verbs) {
	lines, starts := splitLines(text)
	var found []located
	add := func(line, column int, rule, match string) {
		found = append(found, located{line: line, column: column, Finding: Finding{Rule: rule, Match: match}})
	}
	seen := map[[2]int]bool{}
	var hits []Hit
	eachSegment(lines, starts, fences, func(line int, from segment, text string) {
		column := columns{text: text, start: from.column}
		for index, char := range text {
			if char == ';' {
				add(line, column.at(index), "semicolon", ";")
			}
		}
		for _, rule := range []struct {
			name    string
			pattern *regexp.Regexp
		}{{"phrasal-verb", rules.phrasal}, {"marketing-adjective", rules.marketing}, {"nominalization", rules.nominalization}} {
			for _, span := range boundedMatches(rule.pattern, text, false) {
				add(line, column.at(span[0]), rule.name, text[span[0]:span[1]])
			}
		}
		hits = append(hits, firstHits(text, line, &column, seen)...)
		for _, piece := range sentencePieces(text) {
			if words := len(fields(piece)); words > rules.maxWords {
				add(line, from.column+1, "long-sentence", fmt.Sprintf("%d words", words))
			}
		}
	})
	// A synonym rotation is each member of a group after the first one to stand in the text, at
	// its first place, in ste-lint's order of the places (Hit.Less). Each member that stands in the
	// text is one of its verbs.
	var verbs Verbs
	present := make([][]Hit, len(rules.synonyms))
	for _, hit := range hits {
		present[hit.Group] = append(present[hit.Group], hit)
		verbs |= hit.Verb()
	}
	for _, each := range present {
		sort.Slice(each, func(a, b int) bool { return each[a].Less(each[b]) })
		for index := 1; index < len(each); index++ {
			add(each[index].Line, each[index].Column, "synonym-rotation", each[index].Match)
		}
	}
	found = append(found, danglingConjunctions(lines, fences)...)
	sort.SliceStable(found, func(a, b int) bool {
		if found[a].line != found[b].line {
			return found[a].line < found[b].line
		}
		return found[a].column < found[b].column
	})
	findings := make([]Finding, len(found))
	for index, finding := range found {
		findings[index] = finding.Finding
	}
	return findings, verbs
}

// Hit is the first place of a member of a synonym group in a text, as LintVerbs reads it for the
// rule synonym-rotation: the group and the member, in the order of rules.json, the line and the
// column that ste-lint orders it by, and its words there (Match).
type Hit struct {
	Group, Member, Line, Column int
	Match                       string
}

// Less reports whether h comes before other in ste-lint's order of the first places of the
// members of a synonym group, the order whose first member gives no finding of the rule
// synonym-rotation: by line, then column, then words, as Python orders its (line, column, match)
// tuples. LintVerbs orders the places of one text by it, and a caller that reads a line as
// several texts orders the places of each text by it.
func (h Hit) Less(other Hit) bool {
	if h.Line != other.Line {
		return h.Line < other.Line
	}
	if h.Column != other.Column {
		return h.Column < other.Column
	}
	return h.Match < other.Match
}

// firstHits is the first place in kept, the text of a segment of line without its inline code,
// of each member of a synonym group that seen does not hold yet, in the order of the segment; it
// puts each such member in seen. It is the one scan of the synonym groups: LintVerbs and SpreadOf
// read the members through it alone.
func firstHits(kept string, line int, column *columns, seen map[[2]int]bool) []Hit {
	type first struct {
		member [2]int
		span   [2]int
	}
	var firsts []first
	for group, members := range rules.synonyms {
		for member, pattern := range members {
			if seen[[2]int{group, member}] {
				continue
			}
			if spans := boundedMatches(pattern, kept, true); len(spans) > 0 {
				firsts = append(firsts, first{[2]int{group, member}, spans[0]})
			}
		}
	}
	sort.Slice(firsts, func(a, b int) bool { return firsts[a].span[0] < firsts[b].span[0] })
	hits := make([]Hit, len(firsts))
	for index, hit := range firsts {
		seen[hit.member] = true
		hits[index] = Hit{Group: hit.member[0], Member: hit.member[1], Line: line,
			Column: column.at(hit.span[0]), Match: kept[hit.span[0]:hit.span[1]]}
	}
	return hits
}

// Spread is what LintVerbs reads in a text of one line that the text around it can change, for a
// caller that reads such a line as the sentences that make it, each one text: the first place of
// each member of a synonym group (Hits, in the order of the text), the words of each sentence
// piece as the rule long-sentence counts them (Pieces, one at least), and whether the last piece
// runs on into a text that follows after white space (Open), as it does where the text, without
// its inline code, ends in no ".", "!" or "?". So the rule synonym-rotation over the texts in
// order reads the first Hit of each member, and a piece of the rule long-sentence takes the words
// of the last piece of a text that is Open and of the first piece of the text after it.
type Spread struct {
	Hits   []Hit
	Pieces []int
	Open   bool
}

// SpreadOf is the Spread of text, a text of one line, as LintVerbs reads it: a piece of a line,
// such as a sentence, which no fence switch reads.
func SpreadOf(text string) Spread {
	var spread Spread
	lines, starts := splitLines(text)
	seen := map[[2]int]bool{}
	last := rune(-1)
	eachSegment(lines, starts, false, func(line int, from segment, kept string) {
		column := columns{text: kept, start: from.column}
		spread.Hits = append(spread.Hits, firstHits(kept, line, &column, seen)...)
		for _, piece := range sentencePieces(kept) {
			spread.Pieces = append(spread.Pieces, len(fields(piece)))
		}
		if kept != "" {
			last, _ = utf8.DecodeLastRuneInString(kept)
		}
	})
	if len(spread.Pieces) == 0 {
		spread.Pieces = []int{0}
	}
	spread.Open = last != '.' && last != '!' && last != '?'
	return spread
}

// Rotation is the finding of the rule synonym-rotation at h, as Lint gives it where h is not the
// first member of its group to stand in the text.
func (h Hit) Rotation() Finding {
	return Finding{Rule: "synonym-rotation", Match: h.Match}
}

// Verb is the member of h in Verbs.
func (h Hit) Verb() Verbs {
	return 1 << (rules.first[h.Group] + h.Member)
}

// GroupsEach is each synonym group of which v uses a verb, as all the verbs of the group, in the
// order of rules.json.
func (v Verbs) GroupsEach() []Verbs {
	var each []Verbs
	for _, group := range rules.groups {
		if v&group != 0 {
			each = append(each, group)
		}
	}
	return each
}

// Count is how many verbs v holds.
func (v Verbs) Count() int {
	return bits.OnesCount64(uint64(v))
}

// Lines is how many lines ste-lint reads text as: it ends a line at each line break that
// Python's str.splitlines reads (splitLines), \v, \f and U+2028 among them, and not only at \n.
func Lines(text string) int {
	lines, _ := splitLines(text)
	return len(lines)
}

// LongSentence is the finding of the rule long-sentence for a piece of words, as Lint gives it,
// and whether a piece of so many words breaks the rule.
func LongSentence(words int) (Finding, bool) {
	return Finding{Rule: "long-sentence", Match: fmt.Sprintf("%d words", words)}, words > rules.maxWords
}

// ListItem reports whether line starts a list item as the rule dangling-conjunction reads one.
func ListItem(line string) bool {
	return listItemStart.MatchString(line)
}

// Neutral is text as the rule dangling-conjunction reads it, with each inline code read as a
// neutral word.
func Neutral(text string) string {
	return inlineCode.ReplaceAllString(text, " CODE ")
}

// EndsInConjunction reports whether text, as Neutral writes it, ends in "and" or "or", the end
// of a list item that the rule dangling-conjunction refuses.
func EndsInConjunction(text string) bool {
	return conjunctionEnd(Strip(Neutral(text))) >= 0
}

// Rotations is the verbs of each synonym group that a text breaks the rule synonym-rotation in with
// the texts that it holds, and that no one of them breaks it in alone: own is the verbs of the
// text's own words, and held the verbs of each text that it holds, each with the verbs of the texts
// that that one holds in turn. It is each group of which they use two verbs or more together, where
// neither own nor one of held uses each verb of the group that they use together: a group that
// own uses each of those verbs of is Lint's finding of the text, and one that a text of held uses
// each of is that text's finding.
func Rotations(own Verbs, held []Verbs) Verbs {
	return HeldOf(held).Rotations(own)
}

// Held is the verbs of the texts that a text holds, as Rotations reads them, read once: their verbs
// together (all), and, for each synonym group in the order of rules.json, each set of its verbs that
// one of the texts uses (uses), at most two to the power of the group's size, so at most 16 for the
// groups of rules.json. So Rotations of many sets of own verbs against the same texts reads each
// group's sets, not the texts: its work does not grow with how many texts are held.
type Held struct {
	all  Verbs
	uses [][]Verbs
}

// HeldOf is held, the verbs of each text that a text holds, as Held reads them.
func HeldOf(held []Verbs) Held {
	if len(held) == 0 {
		return Held{}
	}
	read := Held{uses: make([][]Verbs, len(rules.groups))}
	for _, each := range held {
		read.all |= each
		for index, group := range rules.groups {
			if used := each & group; !slices.Contains(read.uses[index], used) {
				read.uses[index] = append(read.uses[index], used)
			}
		}
	}
	return read
}

// Rotations is Rotations(own, held) for the texts of h: each group of which own and the texts use
// two verbs or more together, where own does not use each of those verbs, and no one text uses
// each of them (a set of uses that holds each).
func (h Held) Rotations(own Verbs) Verbs {
	all := own | h.all
	rotated := all.Rotated() & (own ^ all).Groups()
	for index, uses := range h.uses {
		group := rules.groups[index]
		if rotated&group == 0 {
			continue
		}
		for _, used := range uses {
			if all&group&^used == 0 {
				rotated &^= group
				break
			}
		}
	}
	return rotated
}

// Rotated is the verbs of v of each synonym group of which v uses two verbs or more.
func (v Verbs) Rotated() Verbs {
	var rotated Verbs
	for _, group := range rules.groups {
		if used := v & group; bits.OnesCount64(uint64(used)) > 1 {
			rotated |= used
		}
	}
	return rotated
}

// Groups is each verb of each synonym group of which v uses a verb.
func (v Verbs) Groups() Verbs {
	var all Verbs
	for _, group := range rules.groups {
		if v&group != 0 {
			all |= group
		}
	}
	return all
}

// Words is the word of each verb of v, as rules.json writes it: one list for each synonym group of
// which v uses a verb, in the order of rules.json.
func (v Verbs) Words() [][]string {
	var words [][]string
	for group, members := range rules.members {
		var used []string
		for member, word := range members {
			if v&(1<<(rules.first[group]+member)) != 0 {
				used = append(used, word)
			}
		}
		if used != nil {
			words = append(words, used)
		}
	}
	return words
}

// conjunctionEnd is ste-lint's CONJUNCTION_END, `\b(?:and|or)\s*$` with IGNORECASE, searched in
// s: the character index where the closing "and" or "or" starts, or -1.
func conjunctionEnd(s string) int {
	trimmed := strings.TrimRightFunc(s, isSpace)
	for _, word := range []string{"and", "or"} {
		start := len(trimmed) - len(word)
		if start < 0 || !strings.EqualFold(asciiOnly(trimmed[start:]), word) {
			continue
		}
		if !wordBoundary(trimmed, start) {
			continue
		}
		return runes(trimmed[:start])
	}
	return -1
}

// asciiOnly is s when it holds ASCII alone, and "" otherwise, so no non-ASCII letter folds to
// one of the conjunction's letters: Python's IGNORECASE folds none to them.
func asciiOnly(s string) string {
	for index := range len(s) {
		if s[index] >= utf8.RuneSelf {
			return ""
		}
	}
	return s
}

func listContinuation(line string, contentIndent int) bool {
	if Strip(line) == "" {
		return true
	}
	if listItemStart.MatchString(line) {
		return false
	}
	return len(line)-len(strings.TrimLeft(line, " ")) >= contentIndent
}

// itemLine is one line of a list item and its index in the text's lines.
type itemLine struct {
	index int
	text  string
}

// danglingConjunctions is ste-lint's _dangling_conjunction_findings: a list item, outside a code
// fence where fences is set, whose last line with text ends in "and" or "or", inline code read as
// a neutral word.
func danglingConjunctions(lines []string, fences bool) []located {
	var found []located
	inFence := false
	for index := 0; index < len(lines); {
		line := lines[index]
		if fences && isFence(Strip(line)) {
			inFence = !inFence
			index++
			continue
		}
		if inFence {
			index++
			continue
		}
		start := listItemStart.FindStringSubmatchIndex(line)
		if start == nil {
			index++
			continue
		}
		contentIndent := (start[3] - start[2]) + (start[5] - start[4]) + (start[7] - start[6])
		items := []itemLine{{index, line[start[8]:start[9]]}}
		next := index + 1
		itemFence := false
		for ; next < len(lines); next++ {
			candidate := lines[next]
			if fences && isFence(Strip(candidate)) {
				itemFence = !itemFence
				continue
			}
			if itemFence {
				continue
			}
			if !listContinuation(candidate, contentIndent) {
				break
			}
			items = append(items, itemLine{next, candidate})
		}
		var last *itemLine
		for _, item := range items {
			if cleaned := Strip(inlineCode.ReplaceAllString(item.text, " CODE ")); cleaned != "" {
				last = &itemLine{item.index, cleaned}
			}
		}
		if last != nil && conjunctionEnd(last.text) >= 0 {
			finding := located{line: last.index + 1, Finding: Finding{Rule: "dangling-conjunction", Match: last.text}}
			if last.index == index {
				finding.column = start[4] + 1
			} else {
				var raw string
				for _, item := range items {
					if item.index == last.index {
						raw = item.text
						break
					}
				}
				masked := inlineCode.ReplaceAllStringFunc(raw, func(code string) string {
					return strings.Repeat(" ", runes(code))
				})
				finding.column = 1
				if at := conjunctionEnd(masked); at >= 0 {
					finding.column = at + 1
				}
			}
			found = append(found, finding)
		}
		index = next
	}
	return found
}
