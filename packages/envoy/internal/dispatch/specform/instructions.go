package specform

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// maxProcedureWords is R49's bound on an instruction, a sentence of the procedure of a check that
// starts with a verb of instructionVerbs: ASD-STE100 gives an instruction at most 20 words, where
// a description, a sentence of a procedure that describes a program among them, has 25.
const maxProcedureWords = 20

// instructionQuote is how many words of the second instruction a finding of the rule instructions
// quotes, as the rule sentence-words quotes the first words of its sentence.
const instructionQuote = 8

// codeWord starts and ends the word that stands for a span in code font among the words that the
// rule instructions reads, so that a command in code font is the object of an instruction, as in
// "Run `go test` and read the log" (codeAsWords).
const codeWord = "\uE000"

// instructionVerbsFile is the list of the verbs of an instruction, reviewed data: a change of the
// list is a change of the rule, which a person reviews (its header says where its verbs came
// from). The rule instructions reads a verb only from it, so a word that it does not list starts no
// instruction.
//
//go:embed instruction-verbs.txt
var instructionVerbsFile string

// wordSet is a set of words in lower case.
func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

var (
	// instructionVerbs is each verb of instructionVerbsFile, in its base form and in lower case.
	instructionVerbs = func() map[string]bool {
		verbs := map[string]bool{}
		for line := range strings.Lines(instructionVerbsFile) {
			if word := strings.TrimSpace(line); word != "" && !strings.HasPrefix(word, "#") {
				verbs[word] = true
			}
		}
		return verbs
	}()
	// conditions open a condition. At the start of a sentence a comma ends it, and the instruction
	// starts after that comma.
	conditions = wordSet("if", "when", "after", "before", "once", "unless", "until", "while")
	// objectStarts start the object of an instruction: an article, a word of quantity, a
	// possessive, a demonstrative, a pronoun, or a span in code font.
	objectStarts = wordSet("the", "a", "an", "each", "every", "all", "any", "both", "no", "its", "their",
		"this", "these", "those", "it", "them", codeWord)
	// wholePhrases are the object starts that are a whole noun phrase alone: a pronoun, and a span
	// in code font.
	wholePhrases = wordSet("it", "them", codeWord)
	// auxiliaries are the verbs that, after a noun phrase, make it the subject of a clause.
	auxiliaries = wordSet("is", "are", "was", "were", "has", "have", "had", "does", "do", "did", "can", "could",
		"may", "might", "must", "shall", "should", "will", "would")
	// relatives open a relative clause, whose verbs give no instruction.
	relatives = wordSet("that", "which", "who", "whom", "whose")
	// joiners join a second instruction to the first, as a comma does.
	joiners = wordSet("and", "or", "then")
)

// instructionBreaks is a finding of the rule instructions for each sentence of a procedure that
// gives two instructions (instructionStarts). A span in code font counts as one word of the
// sentence, where the rules that count words read none. The finding quotes the first words of the
// second instruction.
func instructionBreaks(line string) []broken {
	var found []broken
	coded, spans := codeAsWords(line)
	for _, each := range sentencesOf(coded) {
		sentence := each.words
		words, commas := sentenceWords(sentence)
		starts := instructionStarts(words, commas)
		if len(starts) < 2 {
			continue
		}
		second := starts[1]
		quoted := strings.TrimRight(spans.Replace(strings.Join(sentence[second:min(len(sentence), second+instructionQuote)], " ")), ".!?")
		found = append(found, broken{rule: ruleInstructions,
			detail: fmt.Sprintf("a sentence gives two instructions, and a sentence of a procedure gives one: %q", quoted)})
	}
	return found
}

// codeAsWords is line with each span in code font, as ste-lint cuts one ("`" to the next "`"),
// written as one word that codeWord starts and ends, and the replacer that writes each such word
// back as its span.
func codeAsWords(line string) (string, *strings.Replacer) {
	var written strings.Builder
	var spans []string
	for {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			break
		}
		end := strings.IndexByte(line[start+1:], '`')
		if end < 0 {
			break
		}
		word := fmt.Sprintf("%s%d%s", codeWord, len(spans)/2, codeWord)
		spans = append(spans, word, line[start:start+1+end+1])
		written.WriteString(line[:start])
		written.WriteString(word)
		line = line[start+1+end+1:]
	}
	written.WriteString(line)
	return written.String(), strings.NewReplacer(spans...)
}

// sentenceWords is each word of sentence as wordCore reads it, and whether a comma ends it.
func sentenceWords(sentence []string) ([]string, []bool) {
	words := make([]string, len(sentence))
	commas := make([]bool, len(sentence))
	for index, word := range sentence {
		words[index], commas[index] = wordCore(word)
	}
	return words, commas
}

// instructionStart is where the instruction of a sentence starts: its first word, or the word
// after the comma that ends a condition at its start (conditions). It is false for a condition
// that no comma ends, and for a sentence whose words there are no instruction (opensInstruction).
func instructionStart(words []string, commas []bool) (int, bool) {
	start := 0
	if len(words) > 0 && conditions[words[0]] {
		start = slices.Index(commas, true) + 1
		if start == 0 {
			return 0, false
		}
	}
	return start, opensInstruction(words, commas, start)
}

// opensInstruction reports whether an instruction starts at at: a verb of instructionVerbs with
// no comma after it, then the first word of its object (objectStarts), which is no clause of its
// own (contactClause). A word that the list does not name is no verb.
func opensInstruction(words []string, commas []bool, at int) bool {
	return at+1 < len(words) && instructionVerbs[words[at]] && !commas[at] && objectStarts[words[at+1]] &&
		!contactClause(words, commas, at)
}

// contactClause reports whether the words after the word at at are a clause of their own: a noun
// phrase (a pronoun or a span in code font alone, else a determiner and one noun), no comma after
// it, and then a verb of its own, an auxiliary or a form in -s or -ed. That is a relative clause
// with no "that", as in "the memory and time it uses" or "the input and output the tool writes",
// so the word at at is a noun of the object that the clause tells of, and no verb.
func contactClause(words []string, commas []bool, at int) bool {
	phrase := at + 1
	if !wholePhrases[words[phrase]] {
		phrase++
	}
	if phrase+1 >= len(words) || commas[phrase] {
		return false
	}
	next := words[phrase+1]
	return auxiliaries[next] || inflected(next)
}

// inflected reports whether word, in lower case and of letters alone, ends in -ed or in an -s that
// is no part of -ss, -us or -is.
func inflected(word string) bool {
	if len(word) < 3 || strings.ContainsFunc(word, func(r rune) bool { return !unicode.IsLetter(r) }) {
		return false
	}
	switch {
	case strings.HasSuffix(word, "ed"):
		return true
	case strings.HasSuffix(word, "s"):
		return !strings.HasSuffix(word, "ss") && !strings.HasSuffix(word, "us") && !strings.HasSuffix(word, "is")
	}
	return false
}

// eachInstruction visits where each instruction of a sentence starts, in the order of the
// sentence, until visit answers false: none when its words open no instruction at
// instructionStart. A later instruction is a listed verb that opens one after a word that joins it
// (joiners) or a comma, once the object of the instruction before stands between them. So two
// verbs of one object, as in "Push and hold the switch", are one instruction, as ASD-STE100 lets
// two actions at the same time be. A verb after a relative clause is no instruction, and nor is any
// after it: in "the requests that open and close the same day" the verbs are the clause's. It is
// one pass over the sentence, whatever the count of its instructions.
func eachInstruction(words []string, commas []bool, visit func(at int) bool) {
	start, ok := instructionStart(words, commas)
	if !ok || !visit(start) {
		return
	}
	for at := start + 2; at+1 < len(words); at++ {
		if relatives[words[at-1]] {
			return
		}
		if (commas[at-1] || joiners[words[at-1]]) && opensInstruction(words, commas, at) && !visit(at) {
			return
		}
	}
}

// instructionStarts is where each instruction of a sentence starts (eachInstruction).
func instructionStarts(words []string, commas []bool) []int {
	var starts []int
	eachInstruction(words, commas, func(at int) bool {
		starts = append(starts, at)
		return true
	})
	return starts
}

// isInstruction reports whether the sentence, which sentencesOf read from line, is an instruction:
// it opens one at instructionStart. It reads the sentence as instructionBreaks does, with each span
// in code font one word (codeAsWords), so a span in code font stands for the object, as in
// "Run `go test` with the flag". So the 20-word bound of an instruction is its, though nothing in
// code font counts among its words, and a sentence of a procedure that describes keeps 25.
func (s sentence) isInstruction(line string) bool {
	coded, _ := codeAsWords(line[s.start:s.end])
	var sentence []string
	for _, piece := range ste.Words(coded) {
		for _, word := range piece {
			sentence = append(sentence, word.Text)
		}
	}
	words, commas := sentenceWords(sentence)
	_, ok := instructionStart(words, commas)
	return ok
}

// wordCore is word in lower case without the marks that open or close it and the stops and
// commas after it, and whether a comma ends it. A span in code font is codeWord, as codeAsWords
// writes it or as a word of the line that opens with "`".
func wordCore(word string) (string, bool) {
	word = strings.TrimLeft(word, openingMarks)
	word = strings.TrimRight(word, closingMarks+".!?:;")
	comma := strings.HasSuffix(word, ",")
	word = strings.TrimRight(word, closingMarks+",")
	if strings.HasPrefix(word, codeWord) || strings.HasPrefix(word, "`") {
		return codeWord, comma
	}
	return strings.ToLower(word), comma
}
