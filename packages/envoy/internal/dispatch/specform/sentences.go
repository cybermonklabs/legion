package specform

import (
	"fmt"
	"regexp"
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
var initialism = regexp.MustCompile(`^(?:\pL\.){2,}$`)

// addressStart is the start of an address that the words show: a scheme and "//", or "www.", as
// in "https://example.com/Web.Config" or "www.example.com/Web.Config".
var addressStart = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://|www\.)`)

// sentencesOf is each sentence of line, as its words, that a rule of the form counts. It is the
// one place that says where a sentence ends: after a word that ends in ".", "!" or "?" and then
// any closing marks (endsSentence), since white space or the end of the line follows each word.
// An initialism of single letters such as "U.S." is one word, and none of its stops ends a
// sentence, also the last one before a space: "in the U.S. Dispatch keeps it" is one sentence.
// The end of the line still ends one. A stop that a letter follows with no space ends none
// (stopsWithNoSpace). The words are ste-lint's (ste.Words), so nothing in code font counts. A
// piece with no word that holds a letter, such as "42", "...", "—" or "12:30.", is no sentence,
// so a text of such pieces alone has no sentence. The rule reads English text, and a text in
// another language is the reviewer's to name.
func sentencesOf(line string) [][]string {
	var sentences [][]string
	keep := func(sentence []string) {
		if slices.ContainsFunc(sentence, hasLetter) {
			sentences = append(sentences, sentence)
		}
	}
	for _, words := range ste.Words(line) {
		start := 0
		for index, word := range words {
			if endsSentence(word) {
				keep(words[start : index+1])
				start = index + 1
			}
		}
		keep(words[start:])
	}
	return sentences
}

// stopsWithNoSpace is each word of line, outside code font, that holds a ".", "!" or "?" after a
// letter or a digit, which an upper-case letter follows with no space, after any closing marks,
// such as "Web.Config" or "comment.Dispatch". Such a stop ends no sentence: the writer puts a
// space after it, or puts the name in code font. An initialism holds none, and nor does ".NET".
// An address that the words show holds none either: it is one word, as the address of a link is
// no text of a sentence, and only its last stop can end the sentence (endsSentence).
func stopsWithNoSpace(line string) []string {
	var found []string
	for _, words := range ste.Words(line) {
		for _, word := range words {
			if !isAddress(word) && joinedStop(word) {
				found = append(found, word)
			}
		}
	}
	return found
}

// isAddress reports whether word, without the marks that open it, is an address (addressStart).
func isAddress(word string) bool {
	return addressStart.MatchString(strings.TrimLeft(word, openingMarks))
}

// stopNoSpaceDetail is the detail of the rule stop-no-space for word: the place, and the two ways
// out.
func stopNoSpaceDetail(word string) string {
	return fmt.Sprintf("a stop in %q has no space after it, so Dispatch cannot find the end of the sentence. Put a space after the stop, or put the name in code font", word)
}

// endsSentence reports whether word ends in a stop and then any closing marks, and is no
// initialism.
func endsSentence(word string) bool {
	last, _ := utf8.DecodeLastRuneInString(strings.TrimRight(word, closingMarks))
	return isStop(last) && !isInitialism(word)
}

// joinedStop reports whether word, which is no initialism, holds a stop after a letter or a digit
// that an upper-case letter follows directly, after any closing marks.
func joinedStop(word string) bool {
	if isInitialism(word) {
		return false
	}
	for index, letter := range word {
		if !isStop(letter) || !strings.ContainsFunc(word[:index], isLetterOrDigit) {
			continue
		}
		if next, _ := utf8.DecodeRuneInString(strings.TrimLeft(word[index+1:], closingMarks)); unicode.IsUpper(next) {
			return true
		}
	}
	return false
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
