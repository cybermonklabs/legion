package specform

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The rules of R49, by the ids of the refusal.
const (
	ruleSentences     Rule = "sentences"
	ruleSentenceWords Rule = "sentence-words"
	ruleDispatchLink  Rule = "dispatch-link"
	ruleIncident      Rule = "incident"
	rulePullRequest   Rule = "pull-request"
	rulePerson        Rule = "person"
)

// ruleTermLine is the rule of a line under a heading Terms that is no term line, which the
// extractor reads as a text of Kind line. A definition in another form than the term line would
// otherwise pass the rules of R49 unread.
const ruleTermLine Rule = "term-line"

// termLineDetail is the detail of a line under Terms that is no term line: it shows the one form
// of a term line.
const termLineDetail = `it is no term line, and a term line has the form "- **<term>**: <definition>"`

// maxWords is R49's bound on a sentence, which is also ste-lint's.
const maxWords = 25

// steRules is the words of each hard rule of ste-lint that the refusal names, by ste-lint's own
// rule name. Its long-sentence is R49's sentence-words, and is not here.
var steRules = map[string]string{
	"semicolon":            "a semicolon",
	"phrasal-verb":         "the phrasal verb",
	"marketing-adjective":  "the marketing adjective",
	"nominalization":       "an action frozen into a noun",
	"synonym-rotation":     "two verbs for one action",
	"dangling-conjunction": "a list item that ends with and or or",
}

var (
	// bareKey is an issue key standing as a word: the server's own reference grammar finds
	// only dispatch:// and http(s):// references, so this one is this package's.
	bareKey  = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b`)
	recordID = regexp.MustCompile(`(?i)\b(?:message:)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// incident and pullRequest name a record only by a form that only a name has: "#" and digits,
	// the word "number" and digits, an incident's key or date, and a pull request's
	// <owner>/<repo>#<digits> or address. A bare number after the words is a count, such as
	// "each pull request 10 minutes after", and names nothing.
	incident    = regexp.MustCompile(`(?i:\bincident\s+(?:#\d+|number\s+#?\d+|\d{4}-\d{2}-\d{2}|[a-z][a-z0-9]*-\d+))|\bINC-\d+\b`)
	pullRequest = regexp.MustCompile(`(?i:\b(?:pr|pull request)\s+(?:#\d+|number\s+#?\d+))|\b[\w.-]+/[\w.-]+#\d+|github\.com/[\w.-]+/[\w.-]+/pull/\d+|#\d+`)
	email       = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]*[\w-]`)
	handle      = regexp.MustCompile(`(?:^|[^\w@])(@[A-Za-z][\w-]*)`)
)

// incidentForms and pullRequestForms say, in a finding, which forms the two rules read as a name.
const (
	incidentForms    = "Dispatch reads these forms as the name of an incident: #<n>, number <n>, a key such as INC-<n> and a date"
	pullRequestForms = "Dispatch reads these forms as the name of a pull request: #<n>, number <n>, <owner>/<repo>#<n> and its address"
)

// broken is one rule a text breaks, and the detail of the break.
type broken struct {
	rule   Rule
	detail string
}

// breaks is each rule of R49 that text breaks, once for each detail: the form rules for the
// kinds that are prose of The Spec, then the four reference rules for every kind. A line under
// Terms that is no term line (Kind line) breaks the one rule term-line, and a heading that reads
// as Terms in another form (Kind heading) the one rule terms-heading, and no other.
func breaks(c Context, people map[string]bool, text Text) []broken {
	switch text.Kind {
	case KindLine:
		return []broken{{ruleTermLine, termLineDetail}}
	case KindHeading:
		return []broken{{ruleTermsHeading, termsHeadingDetail}}
	}
	line := oneLine(text.Body)
	var found []broken
	if text.Kind != KindFailingExample && text.Kind != KindName {
		found = append(found, formBreaks(text.Kind, line)...)
	}
	found = append(found, referenceBreaks(c, people, line)...)
	seen := map[broken]bool{}
	unique := found[:0]
	for _, each := range found {
		if !seen[each] {
			seen[each] = true
			unique = append(unique, each)
		}
	}
	return unique
}

// formBreaks is each word that holds a stop with no space after it (stopsWithNoSpace), the
// sentence count of a statement and each sentence over maxWords, as sentencesOf reads the line,
// and each hard finding of ste-lint.
func formBreaks(kind Kind, line string) []broken {
	var found []broken
	for _, word := range stopsWithNoSpace(line) {
		found = append(found, broken{ruleStopNoSpace, stopNoSpaceDetail(word)})
	}
	sentences := sentencesOf(line)
	if kind == KindStatement && (len(sentences) < 1 || len(sentences) > 2) {
		found = append(found, broken{ruleSentences, fmt.Sprintf("it has %d sentences, and a statement has one or two", len(sentences))})
	}
	for _, sentence := range sentences {
		if len(sentence) > maxWords {
			found = append(found, broken{ruleSentenceWords, fmt.Sprintf("a sentence has %d words, and a sentence has at most %d: %q",
				len(sentence), maxWords, strings.Join(sentence[:8], " "))})
		}
	}
	for _, finding := range ste.Lint(line) {
		if words, ok := steRules[finding.Rule]; ok {
			found = append(found, broken{Rule("ste100-" + finding.Rule), fmt.Sprintf("ASD-STE100 does not permit %s: %q", words, finding.Match)})
		}
	}
	return found
}

// hasLetter reports whether word holds a letter of any script.
func hasLetter(word string) bool {
	return strings.ContainsFunc(word, unicode.IsLetter)
}

// span is a byte range of a line, with the break found there.
type span struct {
	start, end int
	broken
}

// referenceBreaks is each link to a record of Dispatch, incident, pull request and person that
// line names, rule by rule, each rule's in the order of the line.
func referenceBreaks(c Context, people map[string]bool, line string) []broken {
	var references, links []span
	for _, located := range text.ExtractAt(line, c.ServerURL) {
		raw := referenceAt(line, located.Offset)
		references = append(references, span{start: located.Offset, end: located.Offset + len(raw)})
		if located.Kind != "url" {
			links = append(links, span{located.Offset, located.Offset + len(raw), broken{ruleDispatchLink, linkDetail(raw)}})
		}
	}
	inReference := func(at int) bool {
		for _, reference := range references {
			if at >= reference.start && at < reference.end {
				return true
			}
		}
		return false
	}
	for _, match := range bareKey.FindAllStringIndex(line, -1) {
		key := line[match[0]:match[1]]
		project := key[:strings.LastIndexByte(key, '-')]
		ref, ok := text.ParseRef("dispatch://"+key, "")
		if !inReference(match[0]) && ok && ref.Kind == "issue" && c.IsProjectKey != nil && c.IsProjectKey(project) {
			links = append(links, span{match[0], match[1], broken{ruleDispatchLink, linkDetail(key)}})
		}
	}
	for _, match := range recordID.FindAllStringIndex(line, -1) {
		if !inReference(match[0]) {
			links = append(links, span{match[0], match[1], broken{ruleDispatchLink, linkDetail(line[match[0]:match[1]])}})
		}
	}
	sort.SliceStable(links, func(a, b int) bool { return links[a].start < links[b].start })
	var found []broken
	for _, link := range links {
		found = append(found, link.broken)
	}
	for _, match := range incident.FindAllString(line, -1) {
		found = append(found, broken{ruleIncident, fmt.Sprintf("it names an incident: %q. %s", match, incidentForms)})
	}
	for _, match := range pullRequest.FindAllString(line, -1) {
		found = append(found, broken{rulePullRequest, fmt.Sprintf("it names a pull request: %q. %s", match, pullRequestForms)})
	}
	for _, name := range personNames(people, line, inReference) {
		found = append(found, broken{rulePerson, fmt.Sprintf("it names a person: %q", name)})
	}
	return found
}

func linkDetail(reference string) string {
	return fmt.Sprintf("it links to a record of Dispatch: %q", reference)
}

// referenceAt is the reference that starts at offset of line, as the server's reference grammar
// cuts one: up to a space, a bracket, a quote, a backtick or a pipe, without the punctuation that
// ends a sentence or a closing parenthesis it did not open.
func referenceAt(line string, offset int) string {
	raw := line[offset:]
	if end := strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Z, r) || strings.ContainsRune("<>\"'`[]|", r)
	}); end >= 0 {
		raw = raw[:end]
	}
	for {
		trimmed := strings.TrimRight(raw, ".,;:!?")
		if strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, "(") < strings.Count(trimmed, ")") {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if trimmed == raw {
			return raw
		}
		raw = trimmed
	}
}

// personPieces is each name piece of a signed-in person's email: each part of its local part,
// split at ".", "_" or "-", that holds at least three letters, in lower case. A piece that is a
// word of a name of the system (the context's, a name text's or a term's) is no person.
func personPieces(c Context, in Input) map[string]bool {
	system := map[string]bool{}
	addWords := func(name string) {
		for _, word := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			system[strings.ToLower(word)] = true
		}
	}
	for _, name := range c.SystemNames {
		addWords(name)
	}
	for _, each := range in.Texts {
		if each.Kind == KindName {
			addWords(each.Body)
		}
	}
	for _, term := range append(append([]Term(nil), in.Proposed...), in.List...) {
		addWords(term.Name)
	}
	pieces := map[string]bool{}
	for _, address := range c.PersonNames {
		local, _, _ := strings.Cut(address, "@")
		for _, piece := range strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
			letters := 0
			for _, r := range piece {
				if unicode.IsLetter(r) {
					letters++
				}
			}
			if piece = strings.ToLower(piece); letters >= 3 && !system[piece] {
				pieces[piece] = true
			}
		}
	}
	return pieces
}

// word is a run of letters of a line, and whether it is the first word of its sentence.
type word struct {
	start, end int
	first      bool
}

// personNames is each person line names, in the order of the line: an email address, an
// @handle, two name pieces in a row, and a name piece that stands as a capitalized word that is
// not the first word of a sentence. A reference holds none.
func personNames(pieces map[string]bool, line string, inReference func(int) bool) []string {
	var found []span
	for _, match := range email.FindAllStringIndex(line, -1) {
		if !inReference(match[0]) {
			found = append(found, span{start: match[0], end: match[1]})
		}
	}
	for _, match := range handle.FindAllStringSubmatchIndex(line, -1) {
		if !inReference(match[2]) && !inSpans(found, match[2]) {
			found = append(found, span{start: match[2], end: match[3]})
		}
	}
	var words []word
	sentenceStart, inWord := true, false
	for index, r := range line {
		if unicode.IsLetter(r) {
			if !inWord {
				words = append(words, word{start: index, first: sentenceStart})
				sentenceStart, inWord = false, true
			}
			words[len(words)-1].end = index + utf8.RuneLen(r)
			continue
		}
		inWord = false
		if r == '.' || r == '!' || r == '?' {
			next, _ := utf8.DecodeRuneInString(line[index+1:])
			if index+1 == len(line) || unicode.IsSpace(next) {
				sentenceStart = true
			}
		}
	}
	isPiece := func(w word) bool {
		return pieces[strings.ToLower(line[w.start:w.end])] && !inReference(w.start) && !inSpans(found, w.start)
	}
	used := map[int]bool{}
	for index := 0; index+1 < len(words); index++ {
		if isPiece(words[index]) && isPiece(words[index+1]) {
			found = append(found, span{start: words[index].start, end: words[index+1].end})
			used[index], used[index+1] = true, true
			index++
		}
	}
	for index, w := range words {
		first, _ := utf8.DecodeRuneInString(line[w.start:])
		if !used[index] && !w.first && unicode.IsUpper(first) && isPiece(w) {
			found = append(found, span{start: w.start, end: w.end})
		}
	}
	sort.SliceStable(found, func(a, b int) bool { return found[a].start < found[b].start })
	names := make([]string, len(found))
	for index, each := range found {
		names[index] = line[each.start:each.end]
	}
	return names
}

func inSpans(spans []span, at int) bool {
	for _, each := range spans {
		if at >= each.start && at < each.end {
			return true
		}
	}
	return false
}
