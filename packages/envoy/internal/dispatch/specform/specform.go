// Package specform decides the form of the texts of The Spec (R49): the sentences and words of
// each statement, check, component purpose and definition of a term, the hard findings of the
// ASD-STE100 program, and the links, incidents, pull requests and people that no text may name.
// It also decides the rules of the terms (R50): a definition is a term line under a heading
// Terms, a term name holds only the letters a to z, the digits, spaces and hyphens, a definition
// has one sentence, and a term has one definition, by its name in one Unicode form (TermKey). A
// line in the form of a term line elsewhere is no definition, and it is refused when it names a
// defined term, since a reader would take it for a second definition. It works on plain texts and
// reads no store: ExtractDocument reads the texts of one document's markdown, Check judges them,
// and Message and ChangesReason write the refusal.
package specform

import (
	"slices"
	"strings"
)

// Kind is the kind of a text of The Spec.
type Kind string

const (
	KindStatement      Kind = "statement"
	KindClaim          Kind = "claim"
	KindProcedure      Kind = "procedure"
	KindFailingExample Kind = "failing_example" // reference rules only
	KindName           Kind = "name"            // a component's or interface's name; reference rules only
	KindPurpose        Kind = "purpose"
	KindDefinition     Kind = "definition"
	KindTerm           Kind = "term"      // the name of a term; reference rules only
	KindLine           Kind = "line"      // a line under a heading Terms that is no term line; the rule term-line only
	KindHeading        Kind = "heading"   // a heading that reads as Terms in another form; the rule terms-heading only
	KindTermLine       Kind = "term_line" // a line in the form of a term line outside Terms; the rule of a second definition only
	KindDocument       Kind = "document"  // a version that the parser refuses, named by its document; the rule unreadable only
)

// kinds is every Kind that ExtractDocument counts in each document, in the order a count of them
// is written. A line under Terms that is no term line (KindLine) is a break of the form, not a text
// to read, so its count is written only for a document that holds one. A line in the form of a
// term line outside Terms (KindTermLine) adds no term, so no count holds it.
var kinds = []Kind{KindStatement, KindClaim, KindProcedure, KindFailingExample, KindName, KindPurpose, KindDefinition}

// Rule is the stable id of a rule of the form, such as "sentences" or "ste100-semicolon".
type Rule string

// Text is one text of The Spec: the words that a person reads, with code in code font (Body), and
// what its node holds beside them (Held). The rules of a sentence and the port of ste-lint read the
// Body, since ste-lint reads code font. Where is "<file>:<line>" or "<document> version <n>".
type Text struct {
	Node  string
	Kind  Kind
	Body  string
	Where string
	Held  Held
}

// Held is what the reference rules read of a text: the words exactly as the page shows them, with
// no mark of code font (Shown), and the address and the title of each of its links and images. The
// reference rules read the shown words and each title as words, and each address as an address. A
// text that a caller writes with no Shown is shown as its Body.
type Held struct {
	Addresses, Titles []string
	Shown             string
}

// with is held and each address and title of more that held does not hold yet.
func (held Held) with(more Held) Held {
	for _, address := range more.Addresses {
		held = held.withAddress(address)
	}
	for _, title := range more.Titles {
		held = held.withTitle(title)
	}
	return held
}

func (held Held) withAddress(address string) Held {
	if address != "" && !slices.Contains(held.Addresses, address) {
		held.Addresses = append(slices.Clip(held.Addresses), address)
	}
	return held
}

func (held Held) withTitle(title string) Held {
	if title != "" && !slices.Contains(held.Titles, title) {
		held.Titles = append(slices.Clip(held.Titles), title)
	}
	return held
}

// Term is a term and its definition, with what each holds beside its words, and how many
// footnotes gave words to the definition: a footnote is part of the definition that refers to it.
// Where of a listed term is "<document> version <n> of <issue key>" or "<file>:<line>".
type Term struct {
	Name, Definition, Where  string
	NameHeld, DefinitionHeld Held
	Footnotes                int
}

// Context is what Check knows of the system: which project keys exist, the email of each person
// who signed in, the names of the system's components and interfaces that a person approved, and
// the server's address.
type Context struct {
	IsProjectKey func(key string) bool
	PersonNames  []string // emails of signed-in people (people.email)
	SystemNames  []string // approved component and interface names, which are nouns of the system
	ServerURL    string
}

// Input is the texts to judge, the definitions a request proposes in the document, the approved
// list, and the definitions under Terms of the other documents of the request (Defined), which
// only a line in the form of a term line outside Terms is judged against.
type Input struct {
	Texts    []Text
	Proposed []Term
	List     []Term
	Defined  []Term
}

// Finding is one break of the form: the text, by its node, kind and place, the rule it breaks,
// the detail of the break and the start of the text.
type Finding struct {
	Node    string `json:"node"`
	Kind    Kind   `json:"kind"`
	Where   string `json:"where"`
	Rule    Rule   `json:"rule"`
	Detail  string `json:"detail"`
	Excerpt string `json:"excerpt"`
}

// excerptRunes is how many characters of a text a finding carries.
const excerptRunes = 160

// Check judges each text of in, and each proposed term as two texts named by the term: its name,
// of Kind term, and its definition, of Kind definition. It judges them by the rules of R49, in the
// order of the texts. A proposed definition first takes the rules of R50: its name holds only the
// letters a to z, the digits, spaces and hyphens, it has one sentence, and it is the one
// definition of its term, in the list and in the proposal. A text of Kind term_line, a line in the
// form of a term line outside Terms, is no definition and takes only the rule of a second
// definition: it is refused when it names a term that the list, the document's own terms or the
// request's other documents (Defined) define.
func Check(c Context, in Input) []Finding {
	people := peopleNames(c, in)
	terms := indexTerms(in)
	var findings []Finding
	keep := func(text Text, found []broken) {
		for _, broken := range found {
			findings = append(findings, Finding{
				Node: text.Node, Kind: text.Kind, Where: text.Where,
				Rule: broken.rule, Detail: broken.detail, Excerpt: excerpt(text.Body),
			})
		}
	}
	for _, text := range in.Texts {
		if text.Kind == KindTermLine {
			keep(text, terms.lineBreaks(text))
			continue
		}
		keep(text, breaks(c, people, text))
	}
	for at, term := range in.Proposed {
		name := Text{Node: term.Name, Kind: KindTerm, Body: term.Name, Where: term.Where, Held: term.NameHeld}
		definition := Text{Node: term.Name, Kind: KindDefinition, Body: term.Definition, Where: term.Where, Held: term.DefinitionHeld}
		keep(name, breaks(c, people, name))
		keep(definition, append(terms.breaks(at, term), breaks(c, people, definition)...))
	}
	return findings
}

// oneLine is a text as ste-lint reads one line: its lines trimmed and joined by one space.
func oneLine(body string) string {
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

// excerpt is the first excerptRunes characters of body.
func excerpt(body string) string {
	count := 0
	for index := range body {
		if count == excerptRunes {
			return body[:index]
		}
		count++
	}
	return body
}
