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

// ruleUnreadable is the rule of a version that the parser refuses (UnreadableError.Finding): a
// judgment that names the version cannot read its texts, so it refuses the version.
const ruleUnreadable Rule = "unreadable"

// termLineDetail is the detail of a line under Terms that is no term line: it shows the one form
// of a term line, and the two ways out. A line that defines no term, such as an example or a note,
// moves out from under the heading. A decision block, whose question line is such a line, stands
// above the heading, and the chosen definition goes under it after the answer.
const termLineDetail = `it is no term line, and a term line has the form "- **<term>**: <definition>". ` +
	`Move a line that defines no term, such as an example or a note, out from under the heading Terms. ` +
	`Put a decision block above the heading Terms, and after the answer, write the chosen definition under the heading`

// maxWords is R49's bound on a sentence, which is also ste-lint's.
const maxWords = 25

// steRules is the words of each hard rule of ste-lint that the refusal names, by ste-lint's own
// rule name: all seven, so each hard finding of ste-lint is a finding of the form (R49). Its
// long-sentence counts the words between two stops that white space follows, as ste-lint reads a
// sentence, so it also refuses a text whose sentences sentence-words counts as short, such as two
// sentences that a stop inside a quote ends.
var steRules = map[string]string{
	"semicolon":            "a semicolon",
	"phrasal-verb":         "the phrasal verb",
	"marketing-adjective":  "the marketing adjective",
	"nominalization":       "an action frozen into a noun",
	"long-sentence":        "a sentence of more than 25 words",
	"synonym-rotation":     "two verbs for one action",
	"dangling-conjunction": "a list item that ends with and or or",
}

// wordClass is the one class of a character of a word, as the content of a bracket expression: a
// letter, a mark, a decimal digit or connector punctuation such as "_", of any script, which are
// Unicode's characters of a word. Each form of a name is built on it (newWordForm), its boundaries
// and the parts of it that are words, and so is each word of a person's name and of an approved
// name (nameWord), so the rules read one alphabet.
const wordClass = `\p{L}\p{M}\p{Nd}\p{Pc}`

// otherWordNotions are the notions of a word of the regular expressions themselves, which read
// ASCII only. No regular expression of this file holds one (wordRegexp).
var otherWordNotions = []string{`\w`, `\W`, `\b`, `\B`, `[:word:]`}

// wordRegexp is the regular expression expr, which holds no notion of a word of its own
// (otherWordNotions), so it reads a word only as wordClass does: wordRegexp panics on one.
func wordRegexp(expr string) *regexp.Regexp {
	for _, notion := range otherWordNotions {
		if strings.Contains(expr, notion) {
			panic(fmt.Sprintf("specform: %q holds %s, a notion of a word other than wordClass", expr, notion))
		}
	}
	return regexp.MustCompile(expr)
}

// nameWord is a word of a person's name and of an approved name (personNames, peopleNames).
var nameWord = wordRegexp(`[` + wordClass + `]+`)

// The forms of a name that the reference rules read, each a whole word (newWordForm), so "#12abcd"
// is a colour, "@types/node" a package and "xACME-12" no key. A part of a form that the rule of its
// own system writes in ASCII is ASCII: a key of Dispatch (text.issueKeyPattern), an id (hexadecimal
// digits), the owner and the repository of GitHub (letters, digits, ".", "_" and "-"), a number
// and a date. So a letter of another script in such a part makes it no form. A part that a person
// names, the key of an incident, an email address and a handle, is of wordClass.
var (
	// bareKey is an issue key standing as a word: the server's own reference grammar finds
	// only dispatch:// and http(s):// references, so this one is this package's.
	bareKey  = newWordForm(`[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*`, "", "")
	recordID = newWordForm(`(?i:(?:message:)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`, "", "")
	// incident, pullRequestName and pullRequestRepo name a record only by a form that only a name
	// has: "#" and digits, the word "number" and digits, an incident's key or date, and a pull
	// request's <owner>/<repo>#<digits>, which no "/", "." or "-" comes before, since that is a
	// longer path. A bare number after the words is a count, such as "each pull request 10 minutes
	// after", and names nothing. pullRequestAddress is the address of a pull request, the one form
	// of a name that an address holds: its host is github.com or www.github.com in any case, as a
	// host has no case, which no "." or "-" comes before, so github.com at the end of another host
	// names nothing.
	incident = newWordForm(`(?i:incident\s*#[0-9]+|incident\s+(?:number\s+#?[0-9]+|[0-9]{4}-[0-9]{2}-[0-9]{2}|\p{L}[`+
		wordClass+`]*-[0-9]+))|INC-[0-9]+`, "", "")
	pullRequestName    = newWordForm(`(?i:(?:pr|pull request)(?:\s*#[0-9]+|\s+number\s+#?[0-9]+))|#[0-9]+`, "", "")
	pullRequestRepo    = newWordForm(`[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+#[0-9]+`, `./\-`, "")
	pullRequestAddress = newWordForm(`(?i:(?:www\.)?github\.com)/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+/pull/[0-9]+`, `.\-`, "")
	// email is an e-mail address: its domain's last label is letters, so a version after an at
	// sign, as in "checker@v1.2.3", is no address. Its local part and its domain take the letters
	// beside them, so a letter there makes a longer address. A ":" that no white space follows
	// continues it, as the host and the path of an SSH address, "git@github.com:owner/repo.git".
	email = newWordForm(`[`+wordClass+`.+\-]+@(?:[`+wordClass+`\-]+\.)+\p{L}[\p{L}\p{M}]+`, "", `:[^\s\p{Z}]`)
	// handle is an @handle, which no "@" comes before and no "/" or "@" comes after, so the scope
	// of a package such as "@types/node" is no handle.
	handle = newWordForm(`@\p{L}[`+wordClass+`\-]*`, "@", `[/@]`)
)

// wordForm is a form of a name that a rule finds only as a whole word (newWordForm).
type wordForm struct {
	// pattern is the form, its group 1, after the start of the line or a character that does not
	// continue it; continues matches at the start of what follows the form when that continues it.
	pattern, continues *regexp.Regexp
}

// newWordForm is the form of a name whose regular expression is form. Its match is a whole word
// when the character before it is the start of the line, or no character of a word (wordClass) and
// none of left, and what follows it is the end of the line, or no character of a word and no match
// of right. left is the content of a bracket expression of the characters that continue the form
// on its left, and right a regular expression of what continues it on its right. Both regular
// expressions are wordRegexp's, so none of the three holds a notion of a word of its own.
func newWordForm(form, left, right string) wordForm {
	continues := `^[` + wordClass + `]`
	if right != "" {
		continues = `^(?:[` + wordClass + `]|` + right + `)`
	}
	return wordForm{
		pattern:   wordRegexp(`(?:^|[^` + wordClass + left + `])(` + form + `)`),
		continues: wordRegexp(continues),
	}
}

// find is the span of each whole word of the form in line, in the order of the line.
func (form wordForm) find(line string) []span {
	var found []span
	for _, match := range form.pattern.FindAllStringSubmatchIndex(line, -1) {
		if !form.continues.MatchString(line[match[3]:]) {
			found = append(found, span{start: match[2], end: match[3]})
		}
	}
	return found
}

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
// kinds that are prose of The Spec, on the words with code in code font (Body), then the four
// reference rules for every kind, on the words exactly as the page shows them (Held.Shown), on
// each title it holds, as words, and on each address it holds, as an address. A line under Terms
// that is no term line (Kind line) breaks the one rule term-line, and a heading that reads as
// Terms in another form (Kind heading) the one rule terms-heading, and no other. people is the
// name of each signed-in person (peopleNames).
func breaks(c Context, people [][]string, text Text) []broken {
	switch text.Kind {
	case KindLine:
		return []broken{{ruleTermLine, termLineDetail}}
	case KindHeading:
		return []broken{{ruleTermsHeading, termsHeadingDetail}}
	}
	var found []broken
	if text.Kind != KindFailingExample && text.Kind != KindName && text.Kind != KindTerm {
		found = append(found, formBreaks(text.Kind, oneLine(text.Body))...)
	}
	shown := text.Held.Shown
	if shown == "" {
		shown = text.Body
	}
	found = append(found, referenceBreaks(c, people, oneLine(shown), false)...)
	for _, title := range text.Held.Titles {
		found = append(found, referenceBreaks(c, people, oneLine(title), false)...)
	}
	for _, address := range text.Held.Addresses {
		found = append(found, referenceBreaks(c, people, address, true)...)
	}
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
// and each hard finding of ste-lint, by its own rule name.
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
		found = append(found, broken{Rule("ste100-" + finding.Rule), fmt.Sprintf("ASD-STE100 does not permit %s: %q", steRules[finding.Rule], finding.Match)})
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
// line names, rule by rule, each rule's in the order of the line. A line that is an address (the
// address of a link or an image) names a record only by the forms that an address holds: a
// reference to a record of Dispatch, the address of a pull request and an email address. A form
// of a name that only words have, such as "#12" or "Ada Lovelace", names nothing in an address,
// nor in a reference that words hold, such as "https://example.com/guide#12". A "#12" in the name
// of an incident names no pull request.
func referenceBreaks(c Context, people [][]string, line string, address bool) []broken {
	var references, links []span
	for _, located := range text.ExtractAt(line, c.ServerURL) {
		raw := referenceAt(line, located.Offset)
		references = append(references, span{start: located.Offset, end: located.Offset + len(raw)})
		if located.Kind != "url" {
			links = append(links, span{located.Offset, located.Offset + len(raw), broken{ruleDispatchLink, linkDetail(raw)}})
		}
	}
	inReference := func(at int) bool {
		return inSpans(references, at)
	}
	for _, each := range bareKey.find(line) {
		key := line[each.start:each.end]
		project := key[:strings.LastIndexByte(key, '-')]
		ref, ok := text.ParseRef("dispatch://"+key, "")
		if !inReference(each.start) && ok && ref.Kind == "issue" && c.IsProjectKey != nil && c.IsProjectKey(project) {
			links = append(links, span{each.start, each.end, broken{ruleDispatchLink, recordNameDetail("key", key)}})
		}
	}
	for _, each := range recordID.find(line) {
		if !inReference(each.start) {
			links = append(links, span{each.start, each.end, broken{ruleDispatchLink, recordNameDetail("id", line[each.start:each.end])}})
		}
	}
	sort.SliceStable(links, func(a, b int) bool { return links[a].start < links[b].start })
	var found []broken
	for _, link := range links {
		found = append(found, link.broken)
	}
	var incidents []span
	if !address {
		incidents = incident.find(line)
		for _, each := range incidents {
			found = append(found, broken{ruleIncident, fmt.Sprintf("it names an incident: %q. %s", line[each.start:each.end], incidentForms)})
		}
	}
	pullRequests := pullRequestAddress.find(line)
	if !address {
		for _, form := range []wordForm{pullRequestRepo, pullRequestName} {
			for _, each := range form.find(line) {
				if !inReference(each.start) && !inSpans(pullRequests, each.start) && !inSpans(incidents, each.start) {
					pullRequests = append(pullRequests, each)
				}
			}
		}
	}
	sort.SliceStable(pullRequests, func(a, b int) bool { return pullRequests[a].start < pullRequests[b].start })
	for _, each := range pullRequests {
		found = append(found, broken{rulePullRequest, fmt.Sprintf("it names a pull request: %q. %s", line[each.start:each.end], pullRequestForms)})
	}
	var names []string
	if address {
		for _, each := range email.find(line) {
			if !inReference(each.start) {
				names = append(names, line[each.start:each.end])
			}
		}
	} else {
		names = personNames(people, line, inReference)
	}
	for _, name := range names {
		found = append(found, broken{rulePerson, fmt.Sprintf("it names a person: %q", name)})
	}
	return found
}

func linkDetail(reference string) string {
	return fmt.Sprintf("it links to a record of Dispatch: %q", reference)
}

// recordNameDetail is the detail of a bare key or id of a record of Dispatch, which names the
// record by its key or its id and links to none.
func recordNameDetail(by, name string) string {
	return fmt.Sprintf("it names a record of Dispatch by its %s: %q", by, name)
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

// peopleNames is the name of each signed-in person: the parts of the local part of the person's
// email, split at ".", "_" or "-", that hold at least three letters, in order and in lower case.
// A part that is a word (nameWord) of a name that a person approved, one of the context's system
// names or a term of the approved list, is no part of a name: such a word is a noun of the
// system. A name that the judged text writes itself, a component's or a term's, is no such name,
// so it cannot hide a person. One part alone is no name.
func peopleNames(c Context, in Input) [][]string {
	system := map[string]bool{}
	addWords := func(name string) {
		for _, word := range nameWord.FindAllString(name, -1) {
			system[strings.ToLower(word)] = true
		}
	}
	for _, name := range c.SystemNames {
		addWords(name)
	}
	for _, term := range in.List {
		addWords(term.Name)
	}
	var names [][]string
	for _, address := range c.PersonNames {
		local, _, _ := strings.Cut(address, "@")
		var name []string
		for _, part := range strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
			letters := 0
			for _, r := range part {
				if unicode.IsLetter(r) {
					letters++
				}
			}
			if part = strings.ToLower(part); letters >= 3 && !system[part] {
				name = append(name, part)
			}
		}
		if len(name) > 1 {
			names = append(names, name)
		}
	}
	return names
}

// personNames is each person line names, in the order of the line: an email address, an
// @handle, and the name of one signed-in person (peopleNames), its parts in order as whole words
// (nameWord) that only white space parts, each with a capital first letter as a name is written.
// A reference holds none.
func personNames(people [][]string, line string, inReference func(int) bool) []string {
	var found []span
	for _, each := range email.find(line) {
		if !inReference(each.start) {
			found = append(found, each)
		}
	}
	for _, each := range handle.find(line) {
		if !inReference(each.start) && !inSpans(found, each.start) {
			found = append(found, each)
		}
	}
	var words []span
	for _, each := range nameWord.FindAllStringIndex(line, -1) {
		words = append(words, span{start: each[0], end: each[1]})
	}
	writes := func(at int, name []string) bool {
		for index, part := range name {
			word := words[at+index]
			first, _ := utf8.DecodeRuneInString(line[word.start:])
			if !unicode.IsUpper(first) || strings.ToLower(line[word.start:word.end]) != part || inReference(word.start) {
				return false
			}
			if index > 0 && strings.TrimFunc(line[words[at+index-1].end:word.start], unicode.IsSpace) != "" {
				return false
			}
		}
		return true
	}
	for _, name := range people {
		for at := 0; at+len(name) <= len(words); at++ {
			if !inSpans(found, words[at].start) && writes(at, name) {
				found = append(found, span{start: words[at].start, end: words[at+len(name)-1].end})
			}
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
