package specform

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// The rules of R50, by the ids of the refusal.
const (
	ruleDefinitionSentences  Rule = "definition-sentences"
	ruleTermSecondDefinition Rule = "term-second-definition"
)

// termKey is a term's name as two names of one term compare: in lower case, with each run of
// whitespace collapsed to one space.
func termKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
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
		key := termKey(term.Name)
		if _, ok := index.listed[key]; !ok {
			index.listed[key] = term
		}
	}
	for at, term := range in.Proposed {
		key := termKey(term.Name)
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
	key := termKey(term.Name)
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
