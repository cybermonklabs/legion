package specform

import (
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// The rules of R50, by the ids of the refusal. term-line is the rule of a line under a heading
// Terms that is no term line, which the extractor reads as a text of Kind line.
const (
	ruleDefinitionSentences  Rule = "definition-sentences"
	ruleTermSecondDefinition Rule = "term-second-definition"
	ruleTermLine             Rule = "term-line"
)

// termLineDetail is the detail of a line under Terms that is no term line: it shows the one form
// of a term line.
const termLineDetail = `it is no term line, and a term line has the form "- **<term>**: <definition>"`

// TermKey is a term's name as two names of one term compare: in the Unicode compatibility form
// (NFKC), so that a letter with its accent in one code point or in two, a ligature and a
// full-width letter read as the letters they show, then in lower case, with each run of
// whitespace collapsed to one space. The list of terms of a root holds one definition of each key.
func TermKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(name))), " ")
}

// termIndex is what the rules of R50 compare a proposed definition with: the first term of each
// name that the list holds, and the index of each proposed definition of each name.
type termIndex struct {
	listed   map[string]Term
	proposed map[string][]int
}

func indexTerms(in Input) termIndex {
	index := termIndex{listed: map[string]Term{}, proposed: map[string][]int{}}
	for _, term := range in.List {
		key := TermKey(term.Name)
		if _, ok := index.listed[key]; !ok {
			index.listed[key] = term
		}
	}
	for at, term := range in.Proposed {
		key := TermKey(term.Name)
		index.proposed[key] = append(index.proposed[key], at)
	}
	return index
}

// breaks is each rule of R50 that the proposed definition at of term breaks: a definition of
// other than one sentence; a second definition of a term the list holds, which names where the
// list defines it; and the second definition of a term the proposal defines more than once, which
// counts the definitions.
func (index termIndex) breaks(at int, term Term) []broken {
	var found []broken
	if sentences := len(ste.Sentences(oneLine(term.Definition))); sentences != 1 {
		found = append(found, broken{ruleDefinitionSentences, fmt.Sprintf("the definition has %d sentences, and a definition has one", sentences)})
	}
	key := TermKey(term.Name)
	if listed, ok := index.listed[key]; ok {
		found = append(found, broken{ruleTermSecondDefinition, fmt.Sprintf(
			"%s already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first",
			listed.Where)})
	}
	if same := index.proposed[key]; len(same) > 1 && same[1] == at {
		found = append(found, broken{ruleTermSecondDefinition, fmt.Sprintf("this document defines this term %d times. Keep one definition", len(same))})
	}
	return found
}
