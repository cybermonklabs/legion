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
// TestPortMatchesSteLintOnTheCorpus holds the port to the Python program on testdata/corpus.jsonl.
package ste

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
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
}

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
		rule.synonyms = append(rule.synonyms, patterns)
	}
	return rule
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

// strip is Python's str.strip with no argument.
func strip(s string) string {
	return strings.TrimFunc(s, isSpace)
}

// fields is Python's str.split with no argument.
func fields(s string) []string {
	return strings.FieldsFunc(s, isSpace)
}

// runes is the number of characters of s, which is how Python counts a string's length and
// indexes it.
func runes(s string) int {
	return utf8.RuneCountInString(s)
}

// lineBreak reports whether Python's str.splitlines ends a line at r.
func lineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// splitLines is Python's str.splitlines: no line ending is kept, "\r\n" is one ending, and a
// last ending makes no empty last line.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if !lineBreak(r) {
			index += size
			continue
		}
		lines = append(lines, text[start:index])
		index += size
		if r == '\r' && index < len(text) && text[index] == '\n' {
			index++
		}
		start = index
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
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
// markdown table row. column is where it starts in the line, in characters.
type segment struct {
	text   string
	column int
}

// splitTableRow is ste-lint's _split_table_row: the trimmed cells of a row and their columns, or
// false for a line that is no table row (it holds no "|" that separates two cells).
func splitTableRow(line string) ([]segment, bool) {
	leftTrimmed := strings.TrimLeftFunc(line, isSpace)
	leftByte, right := len(line)-len(leftTrimmed), len(strings.TrimRightFunc(line, isSpace))
	if leftByte >= right {
		return nil, false
	}
	content, left := line[leftByte:right], runes(line[:leftByte])
	if !strings.Contains(content, "|") {
		return nil, false
	}
	if strings.HasPrefix(content, "|") {
		content = content[1:]
		left++
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
		leading := runes(cell) - runes(strings.TrimLeftFunc(cell, isSpace))
		cells[index] = segment{text: strip(cell), column: column + leading}
		column += runes(cell) + 1
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

// eachSegment visits the text that ste-lint's line loop lints, in its order: each segment of
// each line outside a code fence, with its inline code removed. line is the 1-based line number.
func eachSegment(lines []string, visit func(line int, from segment, text string)) {
	tables := tableCells(lines)
	inFence := false
	for index, raw := range lines {
		if isFence(strip(raw)) {
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
			visit(index+1, from, inlineCode.ReplaceAllString(from.text, ""))
		}
	}
}

// Words is each piece of text that ste-lint lints, as its words: outside code fences, a table's
// cells one by one, with inline code removed, split at Python's white space. A piece with no word
// is left out. Where a sentence ends is the caller's to decide.
func Words(text string) [][]string {
	var pieces [][]string
	eachSegment(splitLines(text), func(_ int, _ segment, line string) {
		if words := fields(line); len(words) > 0 {
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

// synonymHit is where a member of a synonym group first stands, and its text there.
type synonymHit struct {
	line, column int
	match        string
}

// Lint is ste-lint's hard findings for text, in ste-lint's order: by line and column, and in
// the order ste-lint found them where those are equal.
func Lint(text string) []Finding {
	lines := splitLines(text)
	var found []located
	add := func(line, column int, rule, match string) {
		found = append(found, located{line: line, column: column, Finding: Finding{Rule: rule, Match: match}})
	}
	seen := map[[2]int]synonymHit{}
	eachSegment(lines, func(line int, from segment, text string) {
		for index, char := range text {
			if char == ';' {
				add(line, from.column+runes(text[:index])+1, "semicolon", ";")
			}
		}
		for _, rule := range []struct {
			name    string
			pattern *regexp.Regexp
		}{{"phrasal-verb", rules.phrasal}, {"marketing-adjective", rules.marketing}, {"nominalization", rules.nominalization}} {
			for _, span := range boundedMatches(rule.pattern, text, false) {
				add(line, from.column+runes(text[:span[0]])+1, rule.name, text[span[0]:span[1]])
			}
		}
		for group, members := range rules.synonyms {
			for member, pattern := range members {
				if _, ok := seen[[2]int{group, member}]; ok {
					continue
				}
				if spans := boundedMatches(pattern, text, true); len(spans) > 0 {
					seen[[2]int{group, member}] = synonymHit{
						line: line, column: from.column + runes(text[:spans[0][0]]) + 1,
						match: text[spans[0][0]:spans[0][1]],
					}
				}
			}
		}
		for _, piece := range sentencePieces(text) {
			if words := len(fields(piece)); words > rules.maxWords {
				add(line, from.column+1, "long-sentence", fmt.Sprintf("%d words", words))
			}
		}
	})
	// A synonym rotation is each member of a group after the first one to stand in the text, at
	// its first place, ordered as Python orders its (line, column, match) tuples.
	for group, members := range rules.synonyms {
		var present []synonymHit
		for member := range members {
			if hit, ok := seen[[2]int{group, member}]; ok {
				present = append(present, hit)
			}
		}
		sort.Slice(present, func(a, b int) bool {
			x, y := present[a], present[b]
			if x.line != y.line {
				return x.line < y.line
			}
			if x.column != y.column {
				return x.column < y.column
			}
			return x.match < y.match
		})
		for index := 1; index < len(present); index++ {
			add(present[index].line, present[index].column, "synonym-rotation", present[index].match)
		}
	}
	found = append(found, danglingConjunctions(lines)...)
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
	return findings
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
	if strip(line) == "" {
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
// fence, whose last line with text ends in "and" or "or", inline code read as a neutral word.
func danglingConjunctions(lines []string) []located {
	var found []located
	inFence := false
	for index := 0; index < len(lines); {
		line := lines[index]
		if isFence(strip(line)) {
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
			if isFence(strip(candidate)) {
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
			if cleaned := strip(inlineCode.ReplaceAllString(item.text, " CODE ")); cleaned != "" {
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
