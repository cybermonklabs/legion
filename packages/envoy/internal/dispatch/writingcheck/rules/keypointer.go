package rules

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/jdkato/prose/v3/tag"
	"github.com/jdkato/prose/v3/tokenize"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// This file holds key-pointer, the one carried rule that is not a port of ste-lint.py.

// pointer is one pointer at a record: an issue key (BET-34), a pull request of a named repository
// (owner/repo#123), a pull request number (#123), prose.URLToken, or an address that the reader
// keeps as text (dispatch://BET-661/ask/561eb61a), which ends before punctuation.
const pointer = `(?:[A-Z][A-Z0-9]+-[0-9]+|[\w.-]+/[\w.-]+#[0-9]+|#[0-9]+|` + prose.URLToken +
	"|[a-z][a-z0-9+.-]*://[^\\s()<>\"'’*`]*[^\\s()<>\"'’*`.,;:!?])"

var (
	// pointers is each pointer of a text.
	pointers = regexp.MustCompile(pointer)
	// pointerSubject is one pointer, or more joined by commas, "and" or "or", at the start of a text.
	pointerSubject = regexp.MustCompile(`^` + pointer +
		`(?:\s*,\s*(?:(?:and|or)\s+)?` + pointer + `|\s+(?:and|or)\s+` + pointer + `)*`)
	// subjectEnd is what may come right after the subject: the end, white space, a colon, a comma
	// or a parenthesis. So "GPT-6.1", "URLs" and "BET-34's" open with no pointer subject.
	subjectEnd = regexp.MustCompile(`^(?:$|[\s:,(])`)
	// subjectAside is one aside after the subject: a parenthetical, or a relative clause or a
	// prepositional phrase between commas (", which XV6's owner session is working,", ", in
	// lean-tasks,"). An appositive between commas (", the child that builds it,") is no aside: its
	// words say what the record is.
	subjectAside = regexp.MustCompile(`^\s*(?:\([^()]*\)|,\s*(?:which|who|whose|in|on|at|from|for|with|by|after|before|since|until|under)\s[^,]*,)`)
	// labelColon is a colon after the subject: the pointer is the only label of what follows.
	labelColon = regexp.MustCompile(`^\s*:`)
	// possessiveKey is an issue key or a pull request with 's ("BET-1323's", "#3981's").
	possessiveKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-[0-9]+|[\w.-]+/[\w.-]+#[0-9]+)['’]s\b|(?:^|\W)(#[0-9]+)['’]s\b`)

	// The text before a sentence's first word, taken off before the subject is read. Where the
	// parser reads a body these marks are markup and never reach the sentence; the line scan keeps
	// them.
	//
	// leadingMarks is white space, a quote mark, heading marks, a list marker, emphasis and a task
	// box.
	leadingMarks = regexp.MustCompile(`^(?:\s|>|#{1,6}\s|[-*+]\s|[0-9]+[.)]\s|\*+|_+|~~|\[[ xX]\]\s)*`)
	// markdownLink is a link as the line scan keeps it, `[text](href)`: its text is prose and its
	// href is not.
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// openingWord is a linking word before the subject, with its comma.
	openingWord = regexp.MustCompile(`^(?i:so|then|and|but|also)\b,?\s+`)
)

// The patterns of key-pointer, which keyPointerShape answers.
const (
	// keyPointerSubject is a pointer subject followed by a verb ("INFRA-1513 is closed").
	keyPointerSubject = "subject"
	// keyPointerLabel is a pointer subject followed by a colon ("BET-34: URL").
	keyPointerLabel = "label"
	// keyPointerPossessive is a possessive key anywhere in the sentence ("as BET-915's architect
	// wrote").
	keyPointerPossessive = "possessive"
)

// keyPointer finds a sentence that points at a record by its key or link in place of saying in
// words what the record delivers: the owner reported that agents write such sentences, so that a
// text becomes mostly links. It finds, in one sentence, one of two shapes:
//
//   - The sentence opens with a pointer subject: one or more pointers (pointers) joined by commas,
//     "and" or "or", after the leading marks, a link's syntax and an opening word are taken off
//     (leadingMarks, markdownLink, openingWord). After the subject come at most one aside
//     (subjectAside) and then a colon (labelColon) or a verb (verbFirst). A parenthetical aside
//     that names the record in words (namesInWords) makes the sentence no finding of this shape.
//   - A possessive key anywhere in the sentence (possessiveKey).
//
// It finds nothing else. A key after words that name it ("the conformance check (BET-823)"), a
// key as an object or a place ("the three drafts on LINUX-1234", "until INFRA-688 re-lands"), a
// key that a noun, a quoted title, an appositive or a parenthetical that names it follows
// ("INFRA-109 version 149", "INFRA-41 “Connect the VM”", "INFRA-118 (Envoy hub deploy) moves"),
// a key in inline code (prose.CodeToken), a run id, a file name and a line number are no finding.
// A link whose text is words is those words, since prose keeps a link's text and drops its href.
// The sentence gives one finding, which names its subject or its possessive key.
func keyPointer(sentence prose.Sentence) []Finding {
	_, name := keyPointerShape(sentence.Text)
	if name == "" {
		return nil
	}
	named := pointers.ReplaceAllStringFunc(name, func(p string) string {
		if p == prose.URLToken {
			return "the link"
		}
		return p
	})
	rewrite := "Say in words what %s delivers. Put the key or the link after those words."
	if len(pointers.FindAllString(name, -1)) > 1 {
		rewrite = "Say in words what %s deliver. Put the keys or the links after those words."
	}
	return []Finding{finding(sentence, "key-pointer", fmt.Sprintf(rewrite, named))}
}

// keyPointerShape is the pattern by which text points at a record (keyPointerSubject and the
// rest) and the pointers it names, or "" for both where text breaks no pattern. A text that holds
// no pointer breaks none, so most sentences are read no further.
func keyPointerShape(text string) (pattern, name string) {
	if !pointers.MatchString(text) {
		return "", ""
	}
	opening := markdownLink.ReplaceAllString(text, "$1")
	opening = opening[len(leadingMarks.FindString(opening)):]
	opening = opening[len(openingWord.FindString(opening)):]
	if subject := pointerSubject.FindString(opening); subject != "" {
		rest := strings.TrimLeft(opening[len(subject):], "*_~")
		if subjectEnd.MatchString(rest) {
			aside := subjectAside.FindString(rest)
			rest = rest[len(aside):]
			switch {
			case namesInWords(aside):
				// The words in the parenthetical say what the record is.
			case labelColon.MatchString(rest):
				return keyPointerLabel, subject
			case verbFirst(rest, len(pointers.FindAllString(subject, -1)) > 1):
				return keyPointerSubject, subject
			}
		}
	}
	if found := possessiveKey.FindStringSubmatch(text); found != nil {
		return keyPointerPossessive, found[1] + found[2]
	}
	return "", ""
}

// namingWords is the least number of words with which a parenthetical after a pointer subject
// names the record ("INFRA-118 (Envoy hub deploy)"), where one word does not ("(approved: URL)").
const namingWords = 2

var (
	// wholePointer is a word that is one pointer.
	wholePointer = regexp.MustCompile(`^` + pointer + `$`)
	// version is a version number ("v12", "v1.2").
	version = regexp.MustCompile(`^v[0-9]+(?:\.[0-9]+)*$`)
)

// namesInWords reports whether aside is a parenthetical that names the record in words: it holds
// namingWords or more words that are not pointers, numbers or versions ("(the comparator, spec
// v12, which you approved)"). A word holds a letter, and a number opens with a digit ("16:02Z",
// "2026-11-11").
func namesInWords(aside string) bool {
	aside = strings.TrimSpace(aside)
	if !strings.HasPrefix(aside, "(") {
		return false
	}
	words := 0
	for _, field := range strings.Fields(strings.Trim(aside, "()")) {
		word := strings.Trim(field, `,;:.!?"'“”‘’`)
		if strings.IndexFunc(word, unicode.IsLetter) >= 0 && !unicode.IsDigit(rune(word[0])) &&
			!wholePointer.MatchString(word) && !version.MatchString(word) {
			words++
		}
	}
	return words >= namingWords
}

// verbTags are the Penn tags of a verb and a modal; a gerund (VBG) is no finite verb.
var verbTags = map[string]bool{"VB": true, "VBD": true, "VBN": true, "VBP": true, "VBZ": true, "MD": true}

// floatingQuantifiers stand between a plural subject and its verb ("OSSV-343 and OSSV-432 each
// came back").
var floatingQuantifiers = map[string]bool{"each": true, "both": true, "all": true}

// verbFirst reports whether the first word of rest, the text after a pointer subject, is a verb.
// The Penn tagger of github.com/jdkato/prose/v3/tag reads rest after a proper noun that stands
// where the subject stood (subjectStandIn), since it reads a key poorly. The first word past
// adverbs and a floating quantifier is a verb where the tagger tags it as one (verbTags). A
// lowercase word it tags as a plural noun is a verb too: after a name, that is a present verb the
// tagger misreads ("Kafka names 15 shared groups"). A noun ("INFRA-430 architect") is no verb.
func verbFirst(rest string, plural bool) bool {
	standIn := subjectStandIn
	if plural {
		standIn = pluralSubjectStandIn
	}
	tokens := tokenize.ForLanguage("en").Tokenize(standIn + rest)
	verbTagger().TagTokens(tokens)
	for _, token := range tokens {
		switch {
		case token.Start < len(standIn):
			continue
		case strings.HasPrefix(token.Tag, "RB"), floatingQuantifiers[strings.ToLower(token.Text)]:
			continue
		case verbTags[token.Tag]:
			return true
		}
		return token.Tag == "NNS" && token.Text == strings.ToLower(token.Text)
	}
	return false
}

// The proper nouns verbFirst reads in place of one pointer and of several.
const (
	subjectStandIn       = "Kafka"
	pluralSubjectStandIn = "Kafka and Ruby"
)

// verbTagger is the Penn tagger verbFirst shares; it is safe for concurrent use. Its model is
// compiled into the binary, so a failure to load it is a broken build.
var verbTagger = sync.OnceValue(func() *tag.Tagger {
	tagger, err := tag.New()
	if err != nil {
		panic("rules: the built-in tagger model does not load: " + err.Error())
	}
	return tagger
})
