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

// Term is a term and its definition, with what each holds beside its words. Where of a listed
// term is "<document> version <n> of <issue key>" or "<file>:<line>".
type Term struct {
	Name, Definition, Where  string
	NameHeld, DefinitionHeld Held
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

// Input is the texts to judge, the definitions a request proposes, and the approved list.
type Input struct {
	Texts    []Text
	Proposed []Term
	List     []Term
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
// order of the texts.
func Check(c Context, in Input) []Finding {
	texts := make([]Text, 0, len(in.Texts)+2*len(in.Proposed))
	texts = append(texts, in.Texts...)
	for _, term := range in.Proposed {
		texts = append(texts,
			Text{Node: term.Name, Kind: KindTerm, Body: term.Name, Where: term.Where, Held: term.NameHeld},
			Text{Node: term.Name, Kind: KindDefinition, Body: term.Definition, Where: term.Where, Held: term.DefinitionHeld})
	}
	people := peopleNames(c, in)
	var findings []Finding
	for _, text := range texts {
		for _, broken := range breaks(c, people, text) {
			findings = append(findings, Finding{
				Node: text.Node, Kind: text.Kind, Where: text.Where,
				Rule: broken.rule, Detail: broken.detail, Excerpt: excerpt(text.Body),
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
