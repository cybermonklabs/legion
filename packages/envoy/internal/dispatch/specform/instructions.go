package specform

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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

// instructionVerbsFile is the list of the verbs of an instruction, which its program writes from
// the imperative sentences of the repository's skills and of the live procedures of The Spec
// (instruction_verbs_test.go; its header names the source commit and the command). The rule
// instructions reads a verb only from it, so a word that it does not list starts no instruction.
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
	// starts after that comma; outside the first instruction a split cannot keep its scope
	// (splitKeepsMeaning).
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
// sentence, where the rules that count words read none. The finding gives its ways out, each
// written from a change of the sentence that gives one instruction in each sentence: the
// instructions in sentences of their own (splitInstructions), where that keeps what the sentence
// means (splitKeepsMeaning); "that" before the clause, for a listed verb that is a noun of the
// object there; and, after a condition at the start, the condition as a sentence of its own
// (conditionSentence), for a list of verbs in the condition. Where the split would change what the
// sentence means, the finding says what the sentences must keep, and gives no split. It gives only
// the ways out that judge passes in the sentence (wayJudge).
func instructionBreaks(line string, judge *wayJudge) []broken {
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
		said := func(words []string) string { return spans.Replace(strings.Join(words, " ")) }
		original := said(sentence)
		var ways [maxWays]wayOut
		keep := splitKeepsMeaning(words, commas, starts)
		if len(keep) == 0 {
			ways[0] = wayOut{original, said(splitInstructions(sentence, words, starts))}
		}
		ways[1] = wayOut{original, said(slices.Insert(slices.Clone(sentence), second+1, "that"))}
		if condition, ok := conditionSentence(sentence, words, commas); ok {
			ways[2] = wayOut{original, said(condition)}
		}
		given := judge.offered(original, false, ruleInstructions, original, ways)
		quoted := strings.TrimRight(said(sentence[second:min(len(sentence), second+instructionQuote)]), ".!?")
		found = append(found, broken{rule: ruleInstructions, ways: given.ways,
			detail:   fmt.Sprintf("a sentence gives two instructions, and a sentence of a procedure gives one: %q", quoted),
			waysSaid: given.said(instructionsWays(words[second], keep, given.ways))})
	}
	return found
}

// instructionsWays is the words of the ways out of a finding of the rule instructions whose second
// instruction starts with the word verb, written from the ways out that the finding gives (ways):
// each instruction in a sentence of its own, as in the split, or, where the split would change what
// the sentence means, with the words that each sentence must keep (keep, splitKeepsMeaning); "that"
// before the clause; and the condition as a sentence of its own.
func instructionsWays(verb string, keep []string, ways [maxWays]wayOut) string {
	own := "write each instruction in a sentence of its own"
	switch {
	case ways[0] != (wayOut{}):
		own += fmt.Sprintf(", as in %q", ways[0].to)
	case len(keep) > 0:
		own += " and keep " + joinList(keep)
	}
	options := []string{own}
	if ways[1] != (wayOut{}) {
		options = append(options, fmt.Sprintf(`if %q is no verb here, write "that" before the clause after it`, verb))
	}
	if ways[2] != (wayOut{}) {
		options = append(options, fmt.Sprintf("if the words before it are part of the condition, write the condition as a sentence of its own, as in %q", ways[2].to))
	}
	return capitalized(strings.Join(options, ", or, "))
}

// splitKeepsMeaning is what the sentences that splitInstructions writes would lose of what the
// sentence means, none where they keep it: the choice between two instructions that "or" joins, as
// two sentences are two steps where "Read the diff or run the tests" is one of them; and a
// condition that stands outside the first instruction, as the split cannot keep the scope of such a
// condition. A condition before the first instruction governs each instruction, as in "If the run
// fails, read the log and send the report", and one after it can govern each instruction before it,
// as in "Read the log and send the report if the run fails", with or without a comma before it, or
// only the instruction after it, as in "Read the log and, if it fails, send the report": a split
// cuts an instruction off from it in each reading. The first instruction runs from its verb to the
// first comma or word that joins an instruction (joiners), so a condition word inside it, as in
// "Read the log before the run ends and send the report", governs it alone and keeps the split.
func splitKeepsMeaning(words []string, commas []bool, starts []int) []string {
	var keep []string
	for _, start := range starts[1:] {
		if words[start-1] == "or" {
			keep = append(keep, "the choice between them")
			break
		}
	}
	end := starts[0] + 1
	for end < len(words) && !commas[end-1] && !joiners[words[end]] {
		end++
	}
	for at, word := range words {
		if conditions[word] && (at < starts[0] || at >= end) {
			keep = append(keep, "the condition with each instruction that it governs")
			break
		}
	}
	return keep
}

// splitInstructions is sentence with each instruction in a sentence of its own, written in one pass
// from where each instruction starts (starts, instructionStarts): the words before each later
// instruction, without the word that joins it, end with a stop, and that instruction starts with a
// capital.
func splitInstructions(sentence, words []string, starts []int) []string {
	split := make([]string, 0, len(sentence))
	from := 0
	for _, start := range starts[1:] {
		end := start
		if joiners[words[start-1]] {
			end--
		}
		split = append(split, sentence[from:end]...)
		last := len(split) - 1
		split[last] = strings.TrimRight(split[last], ",") + "."
		split = append(split, capitalized(sentence[start]))
		from = start + 1
	}
	return append(split, sentence[from:]...)
}

// conditionThen is how a sentence after a condition of its own goes on from it, for each word
// that opens a condition and reads as one again after it (conditionSentence).
var conditionThen = map[string]string{
	"after": "After that,", "before": "Before that,", "if": "If so,", "once": "Once that is done,",
	"until": "Until then,", "when": "When that happens,", "while": "While that happens,",
}

// conditionSentence is sentence with the condition at its start as a sentence of its own, for a
// list of verbs in the condition: the words from after the condition's first word to the last
// comma that an instruction follows end with a stop, and a sentence that goes on from the
// condition (conditionThen) gives that last instruction, as in "You plan, file an issue or post
// a finding. Before that, run the search." False for a sentence with no condition at its start,
// and where that change still gives a sentence two instructions, which no finding offers.
func conditionSentence(sentence, words []string, commas []bool) ([]string, bool) {
	then, ok := conditionThen[words[0]]
	if !ok {
		return nil, false
	}
	last := 0
	for at := 1; at < len(words); at++ {
		if commas[at-1] && opensInstruction(words, commas, at) {
			last = at
		}
	}
	if last < 2 {
		return nil, false
	}
	condition := slices.Clone(sentence[1:last])
	condition[0] = capitalized(condition[0])
	condition[len(condition)-1] = strings.TrimRight(condition[len(condition)-1], ",") + "."
	changed := append(append(condition, strings.Fields(then)...), sentence[last:]...)
	for _, each := range sentencesOf(strings.Join(changed, " ")) {
		if words, commas := sentenceWords(each.words); words != nil {
			if _, two := secondInstruction(words, commas); two {
				return nil, false
			}
		}
	}
	return changed, true
}

// capitalized is word with its first letter in upper case, and "" for no word; a span in code font
// stays as it is.
func capitalized(word string) string {
	if word == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(word)
	return string(unicode.ToUpper(first)) + word[size:]
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

// secondInstruction is where the second instruction of a sentence starts, when it gives two
// (eachInstruction).
func secondInstruction(words []string, commas []bool) (int, bool) {
	second, seen := 0, 0
	eachInstruction(words, commas, func(at int) bool {
		seen++
		second = at
		return seen < 2
	})
	return second, seen == 2
}

// isInstruction reports whether sentence, as sentencesOf reads a line, is an instruction: it
// opens one at instructionStart, a span in code font standing for the object. So the 20-word bound
// of an instruction is its, and a sentence of a procedure that describes keeps 25.
func isInstruction(sentence []string) bool {
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
