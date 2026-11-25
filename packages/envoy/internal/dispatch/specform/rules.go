package specform

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The rules of R49, by the ids of the refusal.
const (
	ruleSentences     Rule = "sentences"
	ruleSentenceWords Rule = "sentence-words"
	ruleInstructions  Rule = "instructions"
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
// of a term line. termLineWays is its two ways out. A line that defines no term, such as an
// example or a note, moves out from under the heading. A decision block, whose question line is
// such a line, stands above the heading, and the chosen definition goes under it after the answer.
const (
	termLineDetail = `it is no term line, and a term line has the form "- **<term>**: <definition>"`
	termLineWays   = `Move a line that defines no term, such as an example or a note, out from under the heading Terms. ` +
		`Put a decision block above the heading Terms, and after the answer, write the chosen definition under the heading`
)

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
// ASCII only. No regular expression of this package holds one (wordRegexp).
var otherWordNotions = []string{`\w`, `\W`, `\b`, `\B`, `[:word:]`}

// wordRegexp is the regular expression expr, which holds no notion of a word of its own
// (otherWordNotions), so it reads a word only as wordClass does: wordRegexp panics on one. Each
// regular expression of the package is built through it
// (TestEveryPatternOfTheRulesIsBuiltThroughTheGuard).
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
	// longer path, and whose owner starts with a letter or a digit, as GitHub's owner does. A bare
	// number after the words is a count, such as "each pull request 10 minutes after", and names
	// nothing. pullRequestAddress is the address of a pull request, the one form of a name that an
	// address holds: its host is github.com or www.github.com in any case, as a host has no case,
	// which no "." or "-" comes before, so github.com at the end of another host names nothing.
	incident = newWordForm(`(?i:incident\s*#[0-9]+|incident\s+(?:number\s+#?[0-9]+|[0-9]{4}-[0-9]{2}-[0-9]{2}|\p{L}[`+
		wordClass+`]*-[0-9]+))|INC-[0-9]+`, "", "")
	pullRequestName    = newWordForm(`(?i:(?:pr|pull request)(?:\s*#[0-9]+|\s+number\s+#?[0-9]+))|#[0-9]+`, "", "")
	pullRequestRepo    = newWordForm(`[A-Za-z0-9][A-Za-z0-9_.\-]*/[A-Za-z0-9_.\-]+#[0-9]+`, `./\-`, "")
	pullRequestAddress = newWordForm(`(?i:(?:www\.)?github\.com)/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+/pull/[0-9]+`, `.\-`, "")
	// email is an e-mail address: its domain's last label is letters, so a version after an at
	// sign, as in "checker@v1.2.3", is no address. Its local part and its domain take the letters
	// beside them, so a letter there makes a longer address. An email address is a whole word, and
	// no email address where it is part of a longer address: a "/" before it, as the user of a URL
	// ("ssh://git@github.com/owner/repo.git"), or a ":" after it, as the user and host of a remote
	// path ("git@github.com:owner/repo.git", "deploy@build.example.com:'/srv/my reports/'"),
	// unless white space, a closing mark or one of ",", ";", "!" and "?" follows the ":", or the
	// text ends there. A closing mark is a closing bracket (\p{Pe}), a closing curly quote (\p{Pf}),
	// or a straight quote that no character of a path follows: the quote ends the text, or white
	// space, a closing mark or one of ",", ";", "!" and "?" follows it. So "(ada@example.com:)",
	// "ada@example.com: she" and "the field reads "ada@example.com:" each day" name her.
	email = newWordForm(`[`+wordClass+`.+\-]+@(?:[`+wordClass+`\-]+\.)+\p{L}[\p{L}\p{M}]+`, "/",
		`:(?:[^`+addressEnd+`"']|["'][^`+addressEnd+`"'])`)
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

// addressEnd is the content of a bracket expression of the characters after the colon of an email
// address that end the address, so that it names a person (email): white space, a closing
// bracket, a closing curly quote and ",", ";", "!" and "?".
const addressEnd = `\s\v\x{85}\p{Z}\p{Pe}\p{Pf},;!?`

// broken is one rule a text breaks, the detail of the break, and for a break of the rule
// dispatch-link the record of Dispatch that it names and how the text names it (named), so that
// one link gives one finding (heldBreaks) and that finding's detail says each form from its parts.
// The detail is the rule's words at the place of the break, and no more: ways is each way out
// that the finding gives, and waysSaid the words that give them, which are written from them and
// are no part of the finding (Finding.Ways).
type broken struct {
	rule     Rule
	detail   string
	record   text.Ref
	named    naming
	ways     [maxWays]wayOut
	waysSaid string
}

// maxWays is the most ways out that one finding gives: the rule person gives three.
const maxWays = 3

// wayOut is one way out that a finding gives, as a change of the text: the words that it changes
// (from) and what it writes in their place (to). A finding gives a way out only where its judge
// passes it (wayJudge): the text that it writes gives no finding that the text does not give. A
// test applies each way out of each finding and holds the text that results to every rule
// (TestEachWayOutOfARefusalPasses). A zero wayOut is none.
type wayOut struct {
	from, to string
}

// naming is how a text names a record of Dispatch: by a link to it, or by its key or its id
// (form), and the characters that do so (value).
type naming struct {
	form, value string
}

// The forms in which a text names a record of Dispatch (naming).
const (
	byLink = "link"
	byKey  = "key"
	byID   = "id"
)

// recordBreak is the break of the rule dispatch-link of a text that names record in form, by
// value.
func recordBreak(form, value string, record text.Ref) broken {
	named := naming{form: form, value: value}
	return broken{rule: ruleDispatchLink, detail: named.said("it"), record: record, named: named}
}

// said is the naming as the detail of the rule dispatch-link says it of subject: "it", the text,
// or "its words", the words of the page of a text whose link names the same record. It is the one
// place that writes those words, so the detail of one finding that holds both forms is made from
// the parts of each, never from the words of another detail.
func (n naming) said(subject string) string {
	links, names := "links", "names"
	if subject != "it" {
		links, names = "link", "name"
	}
	if n.form == byLink {
		return fmt.Sprintf("%s %s to a record of Dispatch: %q", subject, links, n.value)
	}
	return fmt.Sprintf("%s %s a record of Dispatch by its %s: %q", subject, names, n.form, n.value)
}

// kindFootnote is the kind that a footnote is judged as (footnoteJudge.judgment): prose, which is
// no statement, so that its sentences count in each statement that holds it, and not in itself.
const kindFootnote Kind = "footnote"

// prose reports whether the form rules judge a text of kind: each kind but the failing example of
// a check, the name of a component and the name of a term, which the reference rules alone judge.
func prose(kind Kind) bool {
	return kind != KindFailingExample && kind != KindName && kind != KindTerm
}

// breaks is each rule of R49 that text breaks, once for each detail: the form rules for the kinds
// that are prose of The Spec (prose), on the words with code in code font (Body), with the
// sentences of its footnotes in the count of a statement and their verbs beside its own, then the
// four reference rules for every kind, on what it holds (heldBreaks), then the findings of each
// footnote that it is the first text to hold (footnoteJudge.owned). A line under Terms that is no
// term line (Kind line) breaks the one rule term-line, and no other. people is the name of each
// signed-in person (peopleNames). Each finding gives only the ways out that its judge passes
// (wayJudge): one judge for the text, for the form rules and the reference rules, which first takes
// each place of each finding of the text (survey).
func breaks(c Context, people [][]string, text Text, notes *footnoteJudge) []broken {
	if text.Kind == KindLine {
		return []broken{{rule: ruleTermLine, detail: termLineDetail, waysSaid: termLineWays}}
	}
	var found []broken
	judge := newWayJudge(c, people, text.Kind, text.Held.Shown != "")
	held := notes.held(text)
	find := func() {
		found = found[:0]
		if prose(text.Kind) {
			form, _, _ := formBreaks(text.Kind, oneLine(text.Body), held, judge)
			found = append(found, form...)
		}
		found = append(found, heldBreaks(c, people, text.Body, text.Held, judge)...)
	}
	judge.survey(find)
	find()
	found = append(found, notes.owned(text)...)
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

// heldBreaks is each break of the four reference rules in what a text or a footnote holds: the
// words exactly as the page shows them (held.Shown, or body for a text that a caller writes with
// no Shown), each title as words, and each address as an address (addressBreaks). A record of
// Dispatch that the address of a link names, and the words of the page name too, gives one
// finding, the address's: so one link gives one finding, and two links to two records give two,
// also when one record is a part of the other, such as the spec of an issue and the issue. The
// words name the record of an address also by a part of what the address names (namedBy): the
// key of its issue, or its id. Where the words name it in another form, the address's finding
// names that form too, said from the parts of each naming. judge judges the ways out of each
// finding (wayJudge), a finding of the shown words in the sentence of body that holds it, as the
// rules of the form read body, code font and all (sentencePlaces).
func heldBreaks(c Context, people [][]string, body string, held Held, judge *wayJudge) []broken {
	var addressed []broken
	// gave is each record whose finding an address gave, and linked the finding of the address of
	// each record that an address links to, by each form in which the words can name that record.
	gave := map[text.Ref]bool{}
	linked := map[text.Ref]int{}
	for _, address := range held.Addresses {
		for _, each := range addressBreaks(c, people, address, judge) {
			if each.rule == ruleDispatchLink {
				if gave[each.record] {
					continue
				}
				gave[each.record] = true
				for _, name := range namedBy(each.record) {
					if _, ok := linked[name]; !ok {
						linked[name] = len(addressed)
					}
				}
			}
			addressed = append(addressed, each)
		}
	}
	shown := held.Shown
	if shown == "" {
		shown = body
	}
	line := oneLine(shown)
	places := judge.placesIn(oneLine(body), line, false)
	var found []broken
	type also struct {
		finding int
		named   naming
	}
	var named map[also]bool
	for _, each := range referenceBreaks(c, people, line, false, judge, places) {
		index, ok := linked[each.record]
		if each.rule != ruleDispatchLink || !ok {
			found = append(found, each)
			continue
		}
		if key := (also{index, each.named}); each.named != addressed[index].named && !named[key] {
			if named == nil {
				named = map[also]bool{}
			}
			named[key] = true
			addressed[index].detail += ", and " + each.named.said("its words")
		}
	}
	for _, title := range held.Titles {
		line := oneLine(title)
		found = append(found, referenceBreaks(c, people, line, false, judge, judge.placesIn(line, line, true))...)
	}
	return append(found, addressed...)
}

// namedBy is each form in which words name record, a record that an address links to: the record
// itself, the issue whose key the record holds, and the id of an ask, a comment or a message, or
// of the item whose block it is, as the rule dispatch-link reads a bare key and a bare id
// (referenceBreaks).
func namedBy(record text.Ref) []text.Ref {
	names := []text.Ref{record}
	if record.IssueKey != "" {
		names = append(names, text.Ref{Kind: "issue", IssueKey: record.IssueKey, ID: record.IssueKey})
	}
	switch {
	case record.Kind == "ask" || record.Kind == "comment" || record.Kind == "message",
		record.Kind == "block" && record.Item != "":
		names = append(names, text.Ref{Kind: "id", ID: strings.ToLower(record.ID)})
	}
	return names
}

// addressBreaks is each break of the reference rules in address, the address of a link or an
// image. An address with no scheme opens a page of the server, so it links to a record when that
// page is the page of a record (serverPage), as the whole address of the page does, and that one
// finding holds each key and id that the path writes.
func addressBreaks(c Context, people [][]string, address string, judge *wayJudge) []broken {
	record, page := serverPage(c, address)
	if !page {
		return referenceBreaks(c, people, address, true, judge, nil)
	}
	found := []broken{recordBreak(byLink, address, record)}
	for _, each := range referenceBreaks(c, people, address, true, judge, nil) {
		if each.rule != ruleDispatchLink {
			found = append(found, each)
		}
	}
	return found
}

// standInServer is the address of the server that a path is read against when the context names
// none: a path names the same page on each server, so one host stands for each.
const standInServer = "https://dispatch.invalid"

// serverPage is the record of Dispatch whose page address opens, an address with no scheme (a
// path, a fragment, or "//" and a host), by the server's one reading of the address of a page
// (text.ParseRef, which reads the pages the dashboard's route table reads): an issue and each of
// its tabs, its documents, its asks, comments and messages, a document of a project or of an
// agent, and a block of each. It reads address with each "\" as "/", as a browser reads an
// address of http, and in each way that the dashboard reads it. A component's prose on a page of
// its own reads it against the origin of the server (nodeRouteFromHref), and each page that draws
// the document that holds it reads it against that page (c.Pages), as the editor and the browser
// do. The address links to a record when any reading opens the page of one. So
// "/projects/ACME/documents/terms-md", "projects/ACME/documents/terms-md" and
// "../../projects/ACME/documents/terms-md" name the same document; from the spec of ACME-1,
// "artifacts/notes-md", "decisions", "verification" and "#b-x1" name a document of ACME-1, its
// decisions, its Verification tab and a block of the spec, and from its Artifacts page
// "notes-md" and "../decisions" do too. A path that names no such page from any base, and an
// address of another host, name none.
func serverPage(c Context, address string) (text.Ref, bool) {
	if written, err := url.Parse(address); err != nil || written.Scheme != "" {
		return text.Ref{}, false
	}
	serverURL := c.ServerURL
	if serverURL == "" {
		serverURL = standInServer
	}
	base, err := url.Parse(serverURL)
	if err != nil {
		return text.Ref{}, false
	}
	relative, err := url.Parse(strings.ReplaceAll(address, `\`, "/"))
	if err != nil {
		return text.Ref{}, false
	}
	origin := &url.URL{Scheme: base.Scheme, Host: base.Host}
	if record, ok := text.ParseRef(origin.ResolveReference(relative).String(), serverURL); ok {
		return record, true
	}
	for _, path := range c.Pages {
		page, err := url.Parse(strings.TrimSuffix(serverURL, "/") + path)
		if err != nil {
			continue
		}
		if record, ok := text.ParseRef(page.ResolveReference(relative).String(), serverURL); ok {
			return record, true
		}
	}
	return text.Ref{}, false
}

// formBreaks is each word that holds a stop with no space after it (sentence.stopsWithNoSpace),
// the sentence count of a statement, with the sentences of its footnotes (noted), and each sentence
// over maxWords, or, for an instruction of a procedure (isInstruction), over maxProcedureWords, as
// sentencesOf reads the line, each sentence of a procedure that gives two instructions
// (instructionBreaks), and each hard finding of ste-lint, by its own rule name. The verbs of the
// footnotes that a text holds stand beside the
// line's in the rule synonym-rotation where no one footnote that it refers to holds them all
// (ste.Rotations): two verbs for one action that the words of a footnote hold, or that it holds
// with the footnotes that it refers to, are the footnote's one finding (footnoteJudge.rotations),
// however many texts refer to it. It also answers how many sentences the line has, and the verbs
// of the synonym groups that it uses. judge judges the ways out of each finding (wayJudge): it is
// told the line's count of sentences and of its stops with no space after them, and the line
// itself with the verbs of its footnotes for the rules that read more than one sentence
// (wholeText), before any way out is judged, and each way out of a stop with no space after it is
// judged in the sentence that holds the word, as sentencesOf reads it.
func formBreaks(kind Kind, line string, noted footnotesHeld, judge *wayJudge) ([]broken, int, ste.Verbs) {
	sentences := sentencesOf(line)
	lint, verbs := ste.LintVerbs(line)
	judge.told(len(sentences)+noted.count, joinedIn(sentences), line, noted.verbs)
	var found []broken
	for index, sentence := range sentences {
		for _, word := range sentence.stopsWithNoSpace() {
			found = append(found, stopNoSpaceBreak(word, judge.offered(judge.sentence(index), ruleStopNoSpace, word, stopNoSpaceWays(word))))
		}
	}
	if count := len(sentences) + noted.count; kind == KindStatement && (count < 1 || count > maxSentences) {
		found = append(found, broken{rule: ruleSentences, detail: sentencesDetail(count, noted)})
	}
	for _, sentence := range sentences {
		bound, of := maxWords, "a sentence"
		if kind == KindProcedure && sentence.isInstruction(line) {
			bound, of = maxProcedureWords, "an instruction"
		}
		if words := sentence.words; len(words) > bound {
			found = append(found, broken{rule: ruleSentenceWords, detail: fmt.Sprintf("a sentence has %d words, and %s has at most %d: %q",
				len(words), of, bound, strings.Join(words[:8], " "))})
		}
	}
	if kind == KindProcedure {
		found = append(found, instructionBreaks(line, judge)...)
	}
	for _, finding := range lint {
		found = append(found, steBreak(finding))
	}
	found = append(found, rotationBreaks(ste.Rotations(verbs, noted.verbs), "its footnotes")...)
	return found, len(sentences), verbs
}

// steBreak is the finding of the form for a hard finding of ste-lint, by the rule's own name.
func steBreak(finding ste.Finding) broken {
	return broken{rule: Rule("ste100-" + finding.Rule), detail: fmt.Sprintf("ASD-STE100 does not permit %s: %q", steRules[finding.Rule], finding.Match)}
}

// rotationBreaks is a finding of the rule synonym-rotation for each synonym group of rotated, the
// verbs that a text or a footnote uses together with the words of what (ste.Rotations).
func rotationBreaks(rotated ste.Verbs, what string) []broken {
	var found []broken
	for _, words := range rotated.Words() {
		found = append(found, broken{rule: "ste100-" + steSynonymRotation, detail: fmt.Sprintf("with the words of %s: ASD-STE100 does not permit %s: %s",
			what, steRules[steSynonymRotation], quotedList(words))})
	}
	return found
}

// steSynonymRotation is ste-lint's name of the rule of two verbs for one action.
const steSynonymRotation = "synonym-rotation"

// sentencesDetail is the detail of the rule sentences for a statement of count sentences, with
// those that its footnotes add (noted): each footnote by its label, with how many it adds, and
// where Dispatch stops the count.
func sentencesDetail(count int, noted footnotesHeld) string {
	if len(noted.adds) == 0 {
		return fmt.Sprintf("it has %d sentences, and a statement has one or two", count)
	}
	total := fmt.Sprint(count)
	if noted.atLeast {
		total = "at least " + total
	}
	adds := make([]string, len(noted.adds))
	for index, each := range noted.adds {
		adds[index] = fmt.Sprintf("[^%s] adds %d", each.label, each.sentences)
	}
	detail := fmt.Sprintf("it has %s sentences with those of its footnotes, and a statement has one or two: %s", total, joinList(adds))
	if noted.atLeast {
		detail += ", and Dispatch stops the count there"
	}
	return detail
}

// quotedList is each word quoted, in a list of words (joinList).
func quotedList(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = fmt.Sprintf("%q", word)
	}
	return joinList(quoted)
}

// joinList is items as a list of words: "a", "a and b", "a, b and c".
func joinList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
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

// covered is which bytes of a line the spans that a rule found so far hold, so that the rule reads
// in one step whether a form starts inside one (holds), and marks each span once (mark): its work
// is the length of the line and of its spans, never their count times the count of its forms.
type covered []bool

func newCovered(line string) covered {
	return make(covered, len(line))
}

func (c covered) mark(spans ...span) {
	for _, each := range spans {
		for at := each.start; at < each.end; at++ {
			c[at] = true
		}
	}
}

func (c covered) holds(at int) bool {
	return c[at]
}

// referenceBreaks is each link to a record of Dispatch, incident, pull request and person that
// line names, rule by rule, each rule's in the order of the line. A line that is an address (the
// address of a link or an image) names a record only by the forms that an address holds: a
// reference to a record of Dispatch, the address of a pull request and an email address. A form
// of a name that only words have, such as "#12" or "Ada Lovelace", names nothing in an address,
// nor in a reference that words hold, such as "https://example.com/guide#12". A "#12" in the name
// of an incident names no pull request. A line of words that holds a format character (formatForms)
// names each form that either reading of it finds, and the finding quotes the form with its format
// characters. judge judges the ways out of each person (wayJudge): in the whole address, or in the
// sentence that places finds for the place of line that names the person (sentencePlaces), which a
// caller with a judge gives for a line that is no address.
func referenceBreaks(c Context, people [][]string, line string, address bool, judge *wayJudge, places *sentencePlaces) []broken {
	var forms []span
	if !address && strings.ContainsFunc(line, isFormat) {
		forms = formatForms(c, people, line)
	} else {
		forms = referenceForms(c, people, line, address)
	}
	// The judge names an address once, not once for each name in it.
	var addressed charge.Unit
	if address && slices.ContainsFunc(forms, func(each span) bool { return each.rule == rulePerson }) {
		addressed = judge.named(line, true)
	}
	found := make([]broken, 0, len(forms))
	for _, each := range forms {
		words := line[each.start:each.end]
		switch each.rule {
		case ruleDispatchLink:
			found = append(found, recordBreak(each.named.form, words, each.record))
		case ruleIncident:
			found = append(found, broken{rule: ruleIncident, detail: fmt.Sprintf("it names an incident: %q. %s", words, incidentForms)})
		case rulePullRequest:
			found = append(found, broken{rule: rulePullRequest, detail: fmt.Sprintf("it names a pull request: %q. %s", words, pullRequestForms)})
		case rulePerson:
			unit := addressed
			if judge != nil && !address {
				unit = places.of(each.start, words)
			}
			found = append(found, personBreak(words, judge.offered(unit, rulePerson, words, personWays(words))))
		}
	}
	return found
}

// referenceForms is the span of each form that referenceBreaks reads in line, with its rule, and
// for a record of Dispatch the record and the form in which the words name it (broken.named):
// first each record, in the order of the line, then each incident, each pull request and each
// person, each in the order of the line.
func referenceForms(c Context, people [][]string, line string, address bool) []span {
	var links []span
	references := newCovered(line)
	for _, located := range text.ExtractAt(line, c.ServerURL) {
		raw := referenceAt(line, located.Offset)
		references.mark(span{start: located.Offset, end: located.Offset + len(raw)})
		if located.Kind != "url" {
			links = append(links, span{located.Offset, located.Offset + len(raw), recordBreak(byLink, raw, located.Ref)})
		}
	}
	for _, each := range bareKey.find(line) {
		key := line[each.start:each.end]
		project := key[:strings.LastIndexByte(key, '-')]
		ref, ok := text.ParseRef("dispatch://"+key, "")
		if !references.holds(each.start) && ok && ref.Kind == "issue" && c.IsProjectKey != nil && c.IsProjectKey(project) {
			links = append(links, span{each.start, each.end, recordBreak(byKey, key, ref)})
		}
	}
	for _, each := range recordID.find(line) {
		if !references.holds(each.start) {
			id := line[each.start:each.end]
			links = append(links, span{each.start, each.end, recordBreak(byID, id, text.Ref{Kind: "id", ID: strings.ToLower(id)})})
		}
	}
	sort.SliceStable(links, func(a, b int) bool { return links[a].start < links[b].start })
	found := links
	pullRequests := pullRequestAddress.find(line)
	if !address {
		// A form of a pull request inside an address of one, an incident or a form found before
		// it is part of that name.
		named := newCovered(line)
		named.mark(pullRequests...)
		for _, each := range incident.find(line) {
			named.mark(each)
			found = append(found, span{each.start, each.end, broken{rule: ruleIncident}})
		}
		for _, form := range []wordForm{pullRequestRepo, pullRequestName} {
			for _, each := range form.find(line) {
				if !references.holds(each.start) && !named.holds(each.start) {
					named.mark(each)
					pullRequests = append(pullRequests, each)
				}
			}
		}
	}
	sort.SliceStable(pullRequests, func(a, b int) bool { return pullRequests[a].start < pullRequests[b].start })
	for _, each := range pullRequests {
		found = append(found, span{each.start, each.end, broken{rule: rulePullRequest}})
	}
	var names []span
	if address {
		for _, each := range email.find(line) {
			if !references.holds(each.start) {
				names = append(names, each)
			}
		}
	} else {
		names = personNames(people, line, references)
	}
	for _, each := range names {
		found = append(found, span{each.start, each.end, broken{rule: rulePerson}})
	}
	return found
}

// isFormat reports whether r is a format character (\p{Cf}), such as a soft hyphen, a zero-width
// space or a word joiner, which the page shows as nothing.
func isFormat(r rune) bool {
	return unicode.Is(unicode.Cf, r)
}

// formRank is the place of the forms of rule among the forms that referenceForms answers.
var formRank = map[Rule]int{ruleDispatchLink: 0, ruleIncident: 1, rulePullRequest: 2, rulePerson: 3}

// formatForms is the forms of line, a line of words that holds a format character, that either of
// its two readings finds (referenceForms), so that no format character hides a form: the words
// with each format character taken out, which joins the characters on its two sides, as in
// "Ada Love\u00adlace", and the words with each format character read as white space, which parts
// them, as in "Ada\u2060Lovelace" and "x\u200bACME-12". A form that the first reading finds stands
// for each form of its rule that the second finds over any of the same characters, so one place
// gives one finding. Each span is a span of line, so a finding quotes the form with its format
// characters.
func formatForms(c Context, people [][]string, line string) []span {
	var joined strings.Builder
	spaced := []byte(line)
	// at is the offset in line of each byte of joined.
	at := make([]int, 0, len(line))
	for offset := 0; offset < len(line); {
		r, size := utf8.DecodeRuneInString(line[offset:])
		if isFormat(r) {
			for index := offset; index < offset+size; index++ {
				spaced[index] = ' '
			}
		} else {
			joined.WriteString(line[offset : offset+size])
			for index := offset; index < offset+size; index++ {
				at = append(at, index)
			}
		}
		offset += size
	}
	forms := referenceForms(c, people, joined.String(), false)
	for index := range forms {
		forms[index].start, forms[index].end = at[forms[index].start], at[forms[index].end-1]+1
	}
	held := map[Rule]covered{}
	for _, each := range forms {
		if held[each.rule] == nil {
			held[each.rule] = newCovered(line)
		}
		held[each.rule].mark(each)
	}
	for _, each := range referenceForms(c, people, string(spaced), false) {
		taken := held[each.rule]
		if taken == nil || !slices.ContainsFunc(taken[each.start:each.end], func(held bool) bool { return held }) {
			forms = append(forms, each)
		}
	}
	sort.SliceStable(forms, func(a, b int) bool {
		if rankA, rankB := formRank[forms[a].rule], formRank[forms[b].rule]; rankA != rankB {
			return rankA < rankB
		}
		return forms[a].start < forms[b].start
	})
	return forms
}

// personWays is the ways out of the rule person for name, an email address: a path after the
// colon, as the user and host of a remote path take one, or the user and the host apart. A name
// that is no email address has none.
func personWays(name string) [maxWays]wayOut {
	user, host, found := strings.Cut(name, "@")
	if !found || user == "" {
		return [maxWays]wayOut{}
	}
	return [maxWays]wayOut{{name, name + ":~/"}, {name, name + ":./?.log"}, {name, "ssh -l " + user + " " + host}}
}

// personBreak is the break of the rule person for name. An email address is the one form of both
// a person and the user and host of a remote command with no path, such as "scp report.txt
// deploy@build.example.com:", so its detail says so, and the words of its ways out give the ways
// out of a remote path of the offer that it is given (personWays, wayJudge.offered), written from
// them: a path after the colon, or the user and the host apart.
func personBreak(name string, given offer) broken {
	user, _, found := strings.Cut(name, "@")
	if !found || user == "" {
		return broken{rule: rulePerson, detail: fmt.Sprintf("it names a person: %q", name)}
	}
	detail := fmt.Sprintf("it names a person: %q. Dispatch reads an address user@host with no path after it as the address of a person", name)
	var paths, options []string
	for _, way := range given.ways[:2] {
		if way != (wayOut{}) {
			paths = append(paths, way.to)
		}
	}
	if len(paths) > 0 {
		options = append(options, "write the path after the colon, such as "+strings.Join(paths, " or "))
	}
	if given.ways[2] != (wayOut{}) {
		options = append(options, "write the user and the host apart, such as "+given.ways[2].to)
	}
	ways := ""
	if len(options) > 0 {
		ways = "For a remote path, " + strings.Join(options, ", or ")
	}
	return broken{rule: rulePerson, ways: given.ways, detail: detail, waysSaid: given.said(ways)}
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
	// The parentheses are counted once, and the walk goes back from the end, so a run of closing
	// ones costs one pass.
	opened, closed := strings.Count(raw, "("), strings.Count(raw, ")")
	end := len(raw)
	for ; end > 0; end-- {
		switch raw[end-1] {
		case '.', ',', ';', ':', '!', '?':
		case ')':
			if opened >= closed {
				return raw[:end]
			}
			closed--
		default:
			return raw[:end]
		}
	}
	return raw[:end]
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

// personNames is the span of each person line names, in the order of the line: an email address,
// an @handle, and the name of one signed-in person (peopleNames), its parts in order as whole words
// (nameWord) that only white space parts, each with a capital first letter as a name is written.
// A reference holds none, and a name holds no other.
func personNames(people [][]string, line string, references covered) []span {
	var found []span
	named := newCovered(line)
	keep := func(each span) {
		named.mark(each)
		found = append(found, each)
	}
	for _, each := range email.find(line) {
		if !references.holds(each.start) {
			keep(each)
		}
	}
	for _, each := range handle.find(line) {
		if !references.holds(each.start) && !named.holds(each.start) {
			keep(each)
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
			if !unicode.IsUpper(first) || strings.ToLower(line[word.start:word.end]) != part || references.holds(word.start) {
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
			if !named.holds(words[at].start) && writes(at, name) {
				keep(span{start: words[at].start, end: words[at+len(name)-1].end})
			}
		}
	}
	sort.SliceStable(found, func(a, b int) bool { return found[a].start < found[b].start })
	return found
}
