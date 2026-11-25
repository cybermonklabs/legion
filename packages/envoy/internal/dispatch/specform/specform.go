// Package specform decides the form of the texts of The Spec (R49): the sentences and words of
// each statement, check, component purpose and definition of a term, the hard findings of the
// ASD-STE100 program, and the links, incidents, pull requests and people that no text may name.
// It works on plain texts and reads no store: ExtractDocument reads the texts of one document's
// markdown, Check judges them, and Message and ChangesReason write the refusal.
package specform

import (
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
	KindTerm           Kind = "term"     // the name of a term; reference rules only
	KindLine           Kind = "line"     // a line under a heading Terms that is no term line; the rule term-line only
	KindDocument       Kind = "document" // a version that the parser refuses, named by its document; the rule unreadable only
)

// kinds is every Kind that ExtractDocument counts in each document, in the order a count of them
// is written. A line under Terms that is no term line (KindLine) is a break of the form, not a text
// to read, so its count is written only for a document that holds one.
var kinds = []Kind{KindStatement, KindClaim, KindProcedure, KindFailingExample, KindName, KindPurpose, KindDefinition}

// Rule is the stable id of a rule of the form, such as "sentences" or "ste100-semicolon".
type Rule string

// Text is one text of The Spec: the words that a person reads, with code in code font (Body), what
// its node holds beside them (Held), and each footnote that it refers to (Notes). The rules of a
// sentence and the port of ste-lint read the Body, since ste-lint reads code font. Where is
// "<file>:<line>" or "<document> version <n>".
type Text struct {
	Node  string
	Kind  Kind
	Body  string
	Where string
	Held  Held
	// Notes is each footnote that the text refers to, once and in the order of the first
	// reference. Each one, and each footnote that it refers to in turn, is part of the text.
	Notes []*Footnote
}

// Footnote is one footnote of a document, which ExtractDocument reads once for each document: its
// label as the markdown writes it, its words, with code in code font (Body), what they hold beside
// them (Held), and each footnote that it refers to (Notes), once and in the order of the first
// reference, and then the next footnote of its label up to case, where the document has another
// (readFootnotes). A footnote is part of each text that refers to it, directly or through another
// footnote: Check judges it once, gives its findings once, and counts its sentences in each
// statement that holds it.
type Footnote struct {
	Label string
	Body  string
	Held  Held
	Notes []*Footnote
	// words is whether the footnote, or a footnote that it refers to, directly or in turn, holds
	// words, so that a definition can stand wholly in its footnotes (holdsWords).
	words bool
}

// Held is what the reference rules read of a text: the words exactly as the page shows them, with
// no mark of code font (Shown), and the address and the title of each of its links and images. The
// reference rules read the shown words and each title as words, and each address as an address. A
// text that a caller writes with no Shown is shown as its Body.
type Held struct {
	Addresses, Titles []string
	Shown             string
}

// with is held and each address and title of more after its own. A reading gathers them so, in
// the order that it reads them, and keeps the first of each once it is whole (unique), so its work
// is in proportion to its links and images, not their count times their count. A caller keeps the
// Held that with, withAddress and withTitle answer, and no longer the one it gave them.
func (held Held) with(more Held) Held {
	held.Addresses = append(held.Addresses, more.Addresses...)
	held.Titles = append(held.Titles, more.Titles...)
	return held
}

func (held Held) withAddress(address string) Held {
	if address != "" {
		held.Addresses = append(held.Addresses, address)
	}
	return held
}

func (held Held) withTitle(title string) Held {
	if title != "" {
		held.Titles = append(held.Titles, title)
	}
	return held
}

// unique is held with each address and each title once, the first of each, in their order.
func (held Held) unique() Held {
	held.Addresses, held.Titles = firstOfEach(held.Addresses), firstOfEach(held.Titles)
	return held
}

func firstOfEach(values []string) []string {
	var kept []string
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			kept = append(kept, value)
		}
	}
	return kept
}

// Term is a term and its definition, with what each holds beside its words and each footnote that
// its line refers to, which is part of the definition. Where of a listed term is "<document>
// version <n> of <issue key>" or "<file>:<line>".
type Term struct {
	Name, Definition, Where  string
	NameHeld, DefinitionHeld Held
	DefinitionNotes          []*Footnote
}

// Context is what Check knows of the system: which project keys exist, the email of each person
// who signed in, the names of the system's components and interfaces that a person approved, the
// server's address, and the pages of the server that show the document whose texts Check judges.
type Context struct {
	IsProjectKey func(key string) bool
	PersonNames  []string // emails of signed-in people (people.email)
	SystemNames  []string // approved component and interface names, which are nouns of the system
	ServerURL    string
	// Pages is each path of the dashboard that draws the document, under ServerURL, such as
	// "/issues/ACME-1/spec" and "/issues/ACME-1/artifacts/spec" for a spec (text.DocumentPages):
	// each page reads an address with no scheme against itself. It is empty for texts of no one
	// document of the server, such as the files of a tree.
	Pages []string
}

// Input is the texts to judge, the definitions a request proposes, and the approved list.
type Input struct {
	Texts    []Text
	Proposed []Term
	List     []Term
}

// Finding is one break of the form: the text, by its node, kind and place, the rule it breaks,
// the detail of the break, the ways out that it gives and the start of the text. The detail is
// the rule's words at the place of the break, and Ways the words that give its ways out, which
// change with what the other findings of a sentence offer and are no part of the finding (Said).
type Finding struct {
	Node    string `json:"node"`
	Kind    Kind   `json:"kind"`
	Where   string `json:"where"`
	Rule    Rule   `json:"rule"`
	Detail  string `json:"detail"`
	Ways    string `json:"ways"`
	Excerpt string `json:"excerpt"`
}

// Said is the finding as a refusal writes it: its detail, then the ways out that it gives.
func (f Finding) Said() string {
	if f.Ways == "" {
		return f.Detail
	}
	return f.Detail + ". " + f.Ways
}

// excerptRunes is how many characters of a text a finding carries.
const excerptRunes = 160

// Check judges each text of in, and each proposed term as two texts named by the term: its name,
// of Kind term, and its definition, of Kind definition. It judges them by the rules of R49, in the
// order of the texts. It judges each footnote that the texts refer to once (footnoteJudge), so its
// work is in proportion to the texts and their footnotes, whatever the count of texts that share
// one footnote.
func Check(c Context, in Input) []Finding {
	texts := make([]Text, 0, len(in.Texts)+2*len(in.Proposed))
	texts = append(texts, in.Texts...)
	for _, term := range in.Proposed {
		texts = append(texts,
			Text{Node: term.Name, Kind: KindTerm, Body: term.Name, Where: term.Where, Held: term.NameHeld},
			Text{Node: term.Name, Kind: KindDefinition, Body: term.Definition, Where: term.Where, Held: term.DefinitionHeld, Notes: term.DefinitionNotes})
	}
	people := peopleNames(c, in)
	notes := newFootnoteJudge(c, people, texts)
	var findings []Finding
	for _, text := range texts {
		for _, broken := range breaks(c, people, text, notes) {
			findings = append(findings, Finding{
				Node: text.Node, Kind: text.Kind, Where: text.Where,
				Rule: broken.rule, Detail: broken.detail, Ways: broken.waysSaid, Excerpt: excerpt(text.Body),
			})
		}
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
