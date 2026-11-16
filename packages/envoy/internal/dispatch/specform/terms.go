package specform

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The rules of R50, by the ids of the refusal. term-line is the rule of a line under a heading
// Terms that is no term line, which the extractor reads as a text of Kind line, and terms-heading
// the rule of a heading that reads as Terms in another form, a text of Kind heading.
const (
	ruleDefinitionSentences  Rule = "definition-sentences"
	ruleTermSecondDefinition Rule = "term-second-definition"
	ruleTermLine             Rule = "term-line"
	ruleTermsHeading         Rule = "terms-heading"
)

// termLineDetail is the detail of a line under Terms that is no term line: it shows the one form
// of a term line.
const termLineDetail = `it is no term line, and a term line has the form "- **<term>**: <definition>"`

// termsHeadingDetail is the detail of a heading that reads as Terms in another form: it shows the
// one form of the heading Terms.
const termsHeadingDetail = `it is no Terms heading, and a Terms heading has the form "## Terms" at any level`

// TermKey is a term's name as two names of one term compare: the caseless form that Unicode
// defines for identifiers (NFKC_Casefold, UAX #31), with each run of whitespace collapsed to one
// space. That is the compatibility form (NFKC), so that a letter with its accent in one code
// point or in two, a ligature and a full-width letter read as the letters they show; full case
// folding, so that a capital Σ and a final ς read as one letter; and no default-ignorable
// character, such as a zero-width space, a soft hyphen or a word joiner, which shows nothing. The
// list of terms of a root holds one definition of each key.
func TermKey(name string) string {
	folded := cases.Fold().String(norm.NFKC.String(name))
	shown := strings.Map(func(r rune) rune {
		if defaultIgnorable(r) {
			return -1
		}
		return r
	}, folded)
	return strings.Join(strings.Fields(norm.NFKC.String(shown)), " ")
}

// defaultIgnorable reports whether r is a Default_Ignorable_Code_Point of Unicode's derived core
// properties: Other_Default_Ignorable_Code_Point, a format character (Cf) or a variation selector,
// less white space, the interlinear annotation characters, the Egyptian hieroglyph format
// controls and the prepended concatenation marks.
func defaultIgnorable(r rune) bool {
	switch {
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r), unicode.Is(unicode.Variation_Selector, r):
		return true
	case !unicode.Is(unicode.Cf, r), unicode.Is(unicode.White_Space, r), unicode.Is(unicode.Prepended_Concatenation_Mark, r):
		return false
	}
	return !(r >= 0xfff9 && r <= 0xfffb) && !(r >= 0x13430 && r <= 0x1343f)
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
// other than one sentence, counted as the rules of R49 count one (sentencesOf), so a definition
// with no word that has a letter has no sentence; a second definition of a term the list holds,
// which names where the list defines it; and the second definition of a term the proposal
// defines more than once, which counts the definitions.
func (index termIndex) breaks(at int, term Term) []broken {
	var found []broken
	if sentences := len(sentencesOf(oneLine(term.Definition))); sentences != 1 {
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
