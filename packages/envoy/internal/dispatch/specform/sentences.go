package specform

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// ruleStopNoSpace is the rule of a stop that an upper-case letter follows with no space, as in
// "Web.Config". The text shows no end of a sentence there, so the rules that count sentences and
// words cannot read where the sentence ends.
const ruleStopNoSpace Rule = "stop-no-space"

// closingMarks close a quote or a bracket: a run of them after a stop belongs to the sentence
// that the stop ends.
const closingMarks = "\"'\u201d\u2019)]"

// openingMarks open a quote or a bracket.
const openingMarks = "\"'\u201c\u2018(["

// initialism is a word of single letters, each with its stop, such as "U.S." or "e.g.".
var initialism = wordRegexp(`^(?:\pL\.){2,}$`)

// addressStart is the start of an address that the words show: a scheme and "//", or "www.", as
// in "https://example.com/Web.Config" or "www.example.com/Web.Config".
var addressStart = wordRegexp(`(?i)^(?:[a-z][a-z0-9+.-]*://|www\.)`)

// sentence is one sentence of a line as the rules of the form read it (sentencesOf): its words,
// and the bytes of the line that it takes (start, end).
type sentence struct {
	words      []string
	start, end int
}

// in is the sentence's text in line, the line that sentencesOf read it from, as the bytes that it
// takes without the white space around them: its words with what code font holds among them.
func (s sentence) in(line string) string {
	return strings.TrimSpace(line[s.start:s.end])
}

// sentencesOf is each sentence of line, as its words, that a rule of the form counts. It is the
// one place that says where a sentence ends: after a word that ends in ".", "!" or "?" and then
// any closing marks (endsSentence), since white space or the end of the line follows each word.
// An initialism of single letters such as "U.S." is one word, and none of its stops ends a
// sentence, also the last one before a space: "in the U.S. Dispatch keeps it" is one sentence.
// The end of the line still ends one. A stop that a letter follows with no space ends none
// (stopsWithNoSpace). The words are ste-lint's (ste.Words), so nothing in code font counts. A
// piece with no word that holds a letter, such as "42", "...", "—" or "12:30.", is no sentence,
// so a text of such pieces alone has no sentence. The rule reads English text, and a text in
// another language is the reviewer's to name. Each sentence takes the bytes of the line from the
// end of the sentence before it, or the start of the line, to the end of its last word, and the
// last one to the end of the line: so the sentences take each byte of the line once, code font
// and white space among them, and the judge of the ways out reads a place of the line in the
// sentence that the rules read (wayJudge).
func sentencesOf(line string) []sentence {
	var sentences []sentence
	from := 0
	keep := func(words []ste.Word) {
		if !slices.ContainsFunc(words, func(word ste.Word) bool { return hasLetter(word.Text) }) {
			return
		}
		texts := make([]string, len(words))
		for index, word := range words {
			texts[index] = word.Text
		}
		end := words[len(words)-1].End
		sentences = append(sentences, sentence{words: texts, start: from, end: end})
		from = end
	}
	for _, words := range ste.Words(line) {
		start := 0
		for index, word := range words {
			if endsSentence(word.Text) {
				keep(words[start : index+1])
				start = index + 1
			}
		}
		if start < len(words) {
			keep(words[start:])
		}
	}
	if len(sentences) > 0 {
		sentences[len(sentences)-1].end = len(line)
	}
	return sentences
}

// stopsWithNoSpace is each word of the sentence that holds a ".", "!" or "?" after a letter or a
// digit, which an upper-case letter follows with no space, after any closing marks, such as
// "Web.Config" or "comment.Dispatch". Such a stop ends no sentence: the writer puts a space after
// it, or puts the name in code font. An initialism holds none, and nor does ".NET". An address that
// the words show holds none either: it is one word, as the address of a link is no text of a
// sentence, and only its last stop can end the sentence (endsSentence). Such a word holds an
// upper-case letter, so it stands in a sentence that the rules count, and each stands in one.
func (s sentence) stopsWithNoSpace() []string {
	var found []string
	for _, word := range s.words {
		if !isAddress(word) && joinedStop(word) {
			found = append(found, word)
		}
	}
	return found
}

// joinedIn is how many stops with no space after them the sentences hold, each a sentence that the
// space its finding asks for ends (spacedStops), as the judge of the ways out counts them
// (wayJudge).
func joinedIn(sentences []sentence) int {
	count := 0
	for _, each := range sentences {
		for _, word := range each.stopsWithNoSpace() {
			count += len(joinedStops(word))
		}
	}
	return count
}

// isAddress reports whether word, without the marks that open it, is an address (addressStart).
func isAddress(word string) bool {
	return addressStart.MatchString(strings.TrimLeft(word, openingMarks))
}

// stopNoSpaceWays is the two ways out of the rule stop-no-space for word: a space after each such
// stop (spacedStops), or the word in code font.
func stopNoSpaceWays(word string) [maxWays]wayOut {
	return [maxWays]wayOut{{word, spacedStops(word)}, {word, "`" + word + "`"}}
}

// stopNoSpaceBreak is the break of the rule stop-no-space for word: the place, and the words of
// each way out of the offer that it is given (stopNoSpaceWays, wayJudge.offered), written from them.
func stopNoSpaceBreak(word string, given offer) broken {
	var options []string
	if given.ways[0] != (wayOut{}) {
		options = append(options, "put a space after the stop")
	}
	if given.ways[1] != (wayOut{}) {
		options = append(options, "put the name in code font")
	}
	return broken{rule: ruleStopNoSpace, ways: given.ways,
		detail:   fmt.Sprintf("a stop in %q has no space after it, so Dispatch cannot find the end of the sentence", word),
		waysSaid: given.said(capitalized(strings.Join(options, ", or ")))}
}

// spacedStops is word with a space at each place joinedStops names: after each joined stop and
// its closing marks.
func spacedStops(word string) string {
	var spaced strings.Builder
	from := 0
	for _, at := range joinedStops(word) {
		spaced.WriteString(word[from:at])
		spaced.WriteByte(' ')
		from = at
	}
	spaced.WriteString(word[from:])
	return spaced.String()
}

// endsSentence reports whether word ends in a stop and then any closing marks, and is no
// initialism.
func endsSentence(word string) bool {
	last, _ := utf8.DecodeLastRuneInString(strings.TrimRight(word, closingMarks))
	return isStop(last) && !isInitialism(word)
}

// joinedStop reports whether word, which is no initialism, holds a stop that joins two sentences
// (joinedStops).
func joinedStop(word string) bool {
	return !isInitialism(word) && len(joinedStops(word)) > 0
}

// joinedStops is the place of word after each stop that follows a letter or a digit and that an
// upper-case letter follows directly, after any closing marks: the offset of that letter. It is
// the one scan that reads such a stop, so the finding (joinedStop) and its way out (spacedStops)
// read the same stops.
func joinedStops(word string) []int {
	var places []int
	afterLetter := false
	for index, letter := range word {
		if !isStop(letter) || !afterLetter {
			afterLetter = afterLetter || isLetterOrDigit(letter)
			continue
		}
		rest := word[index+utf8.RuneLen(letter):]
		marks := len(rest) - len(strings.TrimLeft(rest, closingMarks))
		if next, _ := utf8.DecodeRuneInString(rest[marks:]); unicode.IsUpper(next) {
			places = append(places, index+utf8.RuneLen(letter)+marks)
		}
	}
	return places
}

// isInitialism reports whether word is an initialism, without the marks that open or close it
// and a comma, a colon or a semicolon after it.
func isInitialism(word string) bool {
	return initialism.MatchString(strings.TrimRight(strings.TrimLeft(word, openingMarks), closingMarks+",:;"))
}

func isStop(letter rune) bool {
	return letter == '.' || letter == '!' || letter == '?'
}

func isLetterOrDigit(letter rune) bool {
	return unicode.IsLetter(letter) || unicode.IsDigit(letter)
}
