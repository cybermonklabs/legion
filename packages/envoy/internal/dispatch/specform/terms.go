package specform

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The rules of R50, by the ids of the refusal, beside term-line (rules.go), the rule of a line
// under a heading Terms that is no term line: terms-heading is the rule of a heading that reads as
// Terms in another form, a text of Kind heading, and term-name the rule of a name of a term that
// holds a character other than the letters a to z, the digits, a space and a hyphen.
const (
	ruleDefinitionSentences  Rule = "definition-sentences"
	ruleTermSecondDefinition Rule = "term-second-definition"
	ruleTermsHeading         Rule = "terms-heading"
	ruleTermName             Rule = "term-name"
)

// termsHeadingDetail is the detail of a heading that reads as Terms in another form: it shows the
// one form of the heading Terms.
const termsHeadingDetail = `it is no Terms heading, and a Terms heading has the form "## Terms" at any level`

// anyHeading is what the detail of a second definition outside Terms says first: the form of the
// line makes it a definition, so a writer of a list item that is no definition knows to change it.
const anyHeading = `A line of the form "- **<term>**: <definition>" is a definition under each heading.`

// termNameDetail is the detail of the rule term-name: the characters of the name outside the
// alphabet of a term, each as the writer typed it and with its code point.
func termNameDetail(held []rune) string {
	named := make([]string, len(held))
	for index, r := range held {
		named[index] = fmt.Sprintf("%q (U+%04X)", string(r), r)
	}
	list := named[0]
	if last := len(named) - 1; last > 0 {
		list = strings.Join(named[:last], ", ") + " and " + named[last]
	}
	return "the name holds " + list + ". A term name holds only the letters a to z, the digits 0 to 9, spaces and hyphens"
}

// outsideAlphabet is each character of name, once and in the order of the name, whose own
// caseless form (TermKey) holds a character other than the letters a to z, the digits 0 to 9, a
// space and a hyphen. A statement of The Spec is English text (R49) and a term is a noun of it, so
// a name of a term holds only those characters after NFKC_Casefold: a full-width letter or a
// ligature folds to them, and a letter of another script, a letter with a mark, an apostrophe or
// an angle bracket does not. That closes the class of two names that read as one and compare as
// two, such as "İnbox" and "inbox", or a Cyrillic а in place of a Latin a, which no comparison of
// all of Unicode closes. A name is in the alphabet exactly when each of its characters is: NFKC
// composes a mark with the letter before it into no letter from a to z.
func outsideAlphabet(name string) []rune {
	var held []rune
	for _, r := range name {
		if !slices.Contains(held, r) && strings.ContainsFunc(TermKey(string(r)), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != ' ' && r != '-'
		}) {
			held = append(held, r)
		}
	}
	return held
}

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

// termIndex is what the rules of R50 compare a definition with: the first term of each name that
// the list holds, the index of each proposed definition of each name, and the index among the
// texts of each line of each name in the form of a term line outside Terms (Kind term_line).
type termIndex struct {
	listed   map[string]Term
	proposed map[string][]int
	lines    map[string][]int
}

func indexTerms(in Input) termIndex {
	index := termIndex{listed: map[string]Term{}, proposed: map[string][]int{}, lines: map[string][]int{}}
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
	for at, text := range in.Texts {
		if text.Kind == KindTermLine {
			key := TermKey(text.Node)
			index.lines[key] = append(index.lines[key], at)
		}
	}
	return index
}

// definitions is how many definitions of the term of key the document holds: its term lines
// under Terms, then its lines of that form elsewhere. The second of them in that order is the one
// whose finding counts them.
func (index termIndex) definitions(key string) int {
	return len(index.proposed[key]) + len(index.lines[key])
}

// breaks is each rule of R50 that the proposed definition at of term breaks: a name with a
// character outside the alphabet of a term (outsideAlphabet); a definition of other than one
// sentence, counted as the rules of R49 count one (sentencesOf), so a definition with no word that
// has a letter has no sentence; a second definition of a term the list holds, which names where
// the list defines it; and the second definition of a term the document defines more than once,
// which counts the definitions.
func (index termIndex) breaks(at int, term Term) []broken {
	var found []broken
	if held := outsideAlphabet(term.Name); len(held) > 0 {
		found = append(found, broken{ruleTermName, termNameDetail(held)})
	}
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
		found = append(found, broken{ruleTermSecondDefinition, fmt.Sprintf("this document defines this term %d times. Keep one definition", index.definitions(key))})
	}
	return found
}

// lineBreaks is each rule of R50 that the text at, a line in the form of a term line outside
// Terms, breaks: it defines a term that the list holds, or it is the second definition of a term
// that the document defines more than once. It adds no term, so no other rule judges it. Its
// detail says that the form of the line makes it a definition under each heading.
func (index termIndex) lineBreaks(at int, line Text) []broken {
	var found []broken
	key := TermKey(line.Node)
	if listed, ok := index.listed[key]; ok {
		found = append(found, broken{ruleTermSecondDefinition, fmt.Sprintf(
			"%s already defines this term. %s If this line is no definition, change its form. If it is a definition, use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first",
			listed.Where, anyHeading)})
	}
	// The second definition in the order of definitions: the first line of this form when one
	// term line under Terms comes before it, the second when none does, and none when two do.
	if second, lines := 1-len(index.proposed[key]), index.lines[key]; second >= 0 && second < len(lines) && lines[second] == at {
		found = append(found, broken{ruleTermSecondDefinition, fmt.Sprintf(
			"this document defines this term %d times. %s Keep one definition, and change the form of each line that is no definition",
			index.definitions(key), anyHeading)})
	}
	return found
}
