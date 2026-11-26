package ste

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// sameFindings reports whether got and want hold the same findings in the same order, a nil
// list and an empty one alike.
func sameFindings(got, want []Finding) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

// TestLintFindsEachHardRule holds Lint to ste-lint's own hard findings, in ste-lint's order: each
// input string of `ste-lint --selftest` (the findings ste-lint.py at the pinned commit gives
// them, advisory ones left out), then the cases of the plan.
func TestLintFindsEachHardRule(t *testing.T) {
	for _, test := range []struct {
		text string
		want []Finding
	}{
		{"The panel is removed; spin up the job. Perform an analysis of the seamless log. We have received the report.", []Finding{{"semicolon", ";"}, {"phrasal-verb", "spin up"}, {"nominalization", "Perform an analysis"}, {"marketing-adjective", "seamless"}}},
		{"The request may have failed. It could be a timeout. The disk might have filled.", []Finding{}},
		{"The task has run. The job has set the flag. We have begun.", []Finding{}},
		{"The job may have run.", []Finding{}},
		{"The task may have run.", []Finding{}},
		{"The task may  have run.", []Finding{}},
		{"The task may\thave run.", []Finding{}},
		{"The task may not have run.", []Finding{}},
		{"The task might have run.", []Finding{}},
		{"The task might  have run.", []Finding{}},
		{"The task might\thave run.", []Finding{}},
		{"The task might not have run.", []Finding{}},
		{"The task could have run.", []Finding{}},
		{"The task could  have run.", []Finding{}},
		{"The task could\thave run.", []Finding{}},
		{"The task could not have run.", []Finding{}},
		{"The task should have run.", []Finding{}},
		{"The task should  have run.", []Finding{}},
		{"The task should\thave run.", []Finding{}},
		{"The task should not have run.", []Finding{}},
		{"The task would have run.", []Finding{}},
		{"The task would  have run.", []Finding{}},
		{"The task would\thave run.", []Finding{}},
		{"The task would not have run.", []Finding{}},
		{"The task must have run.", []Finding{}},
		{"The task must  have run.", []Finding{}},
		{"The task must\thave run.", []Finding{}},
		{"The task must not have run.", []Finding{}},
		{"The task couldn't have run. The task MAY NOT HAVE RUN.", []Finding{}},
		{"The task has run. We have begun. The flag is set.", []Finding{}},
		{"The task is gone.", []Finding{}},
		{"```\nx = a; y = b\n```", []Finding{}},
		{"- Confirm the target and\n* Record the result OR  \n+ Close the panel\n1. Start the task and\n2) Stop the task OR", []Finding{{"dangling-conjunction", "Confirm the target and"}, {"dangling-conjunction", "Record the result OR"}, {"dangling-conjunction", "Start the task and"}, {"dangling-conjunction", "Stop the task OR"}}},
		{"  - Confirm the target and\n    record the result.", []Finding{}},
		{"- Confirm the target\n  and", []Finding{{"dangling-conjunction", "and"}}},
		{"    - code and", []Finding{}},
		{"> - Confirm the target and\n> - Record the result or", []Finding{}},
		{"- Do this and\n~~~\ncode and\n~~~", []Finding{{"dangling-conjunction", "Do this and"}}},
		{"```text\n- code and\n```", []Finding{}},
		{" - Start the task and\n   record the result.", []Finding{}},
		{"-  Start the task and\n   record the result.", []Finding{}},
		{"-\tStart the task and", []Finding{}},
		{"   - Start the task and", []Finding{{"dangling-conjunction", "Start the task and"}}},
		{"100. Start the task and\n  unrelated text", []Finding{{"dangling-conjunction", "Start the task and"}}},
		{"- Start the task and.\n- Stop the task or,", []Finding{}},
		{"- Start the task and\n\n  record the result.", []Finding{}},
		{"- Parent item and\n  - Nested item or", []Finding{{"dangling-conjunction", "Parent item and"}, {"dangling-conjunction", "Nested item or"}}},
		{"The process may include steps and", []Finding{}},
		{"- Use `and` as a label", []Finding{}},
		{"- Combine `left` and `right`", []Finding{}},
		{"~~~\n- code and\n~~~", []Finding{}},
		{"word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word.", []Finding{{"long-sentence", "30 words"}}},
		{"| Label | Detail |\n| --- | --- |\n| Clear | term1 term2 term3 term4 term5 term6 term7 term8 term9 term10 term11 term12 term13 term14 term15 term16 term17 term18 term19 term20 term21 term22 term23 term24. |", []Finding{}},
		{"Label | Detail\n--- | ---\nClear | term1 term2 term3 term4 term5 term6 term7 term8 term9 term10 term11 term12 term13 term14 term15 term16 term17 term18 term19 term20 term21 term22 term23 term24.", []Finding{}},
		{"| Label | Detail |\n| --- | --- |\n| Clear | term1 term2 term3 term4 term5 term6 term7 term8 term9 term10 term11 term12 term13 term14 term15 term16 term17 term18 term19 term20 term21 term22 term23 term24 term25 term26. |", []Finding{{"long-sentence", "26 words"}}},
		{"Check the config file. Then verify the output. Verify twice.", []Finding{{"synonym-rotation", "verify"}}},
		{"Check the config. Check the output.", []Finding{}},
		{"a; b", []Finding{{"semicolon", ";"}}},
		// The cases of the plan.
		{"Perform an analysis of the seamless log; spin up the job.", []Finding{{"nominalization", "Perform an analysis"}, {"marketing-adjective", "seamless"}, {"semicolon", ";"}, {"phrasal-verb", "spin up"}}},
		{strings.TrimSpace(strings.Repeat("word ", 30)) + ".", []Finding{{"long-sentence", "30 words"}}},
		{"Check the config file. Then verify the output.", []Finding{{"synonym-rotation", "verify"}}},
	} {
		if got := Lint(test.text); !sameFindings(got, test.want) {
			t.Errorf("Lint(%q) = %v, want %v", test.text, got, test.want)
		}
	}
}

// TestLintLeavesAdvisoryRulesOut holds the port to the seven hard rules: passive voice and the
// present perfect are ste-lint's advisory findings, which never fail a run.
func TestLintLeavesAdvisoryRulesOut(t *testing.T) {
	if got := Lint("The panel is removed. We have received the report."); len(got) != 0 {
		t.Fatalf("Lint gave %v for two advisory findings, want none", got)
	}
}

// TestLintSplitsLinesAsPythonDoes holds the two splits whose Go defaults differ from Python's: a
// line ends at every separator of str.splitlines, and a sentence ends at a run of Python's \s,
// which takes \x1f where Go's \s does not.
func TestLintSplitsLinesAsPythonDoes(t *testing.T) {
	// "\u0085" is NEL, the character; a Go "\x85" is one byte that is no UTF-8.
	if got, want := Lint("Check the file.\u0085Then verify it."), []Finding{{"synonym-rotation", "verify"}}; !sameFindings(got, want) {
		t.Errorf("two lines split at NEL: Lint = %v, want %v", got, want)
	}
	// A rotation spans lines, so it holds one document of two lines; a list item ends at its
	// line, so it shows the split itself.
	for _, separator := range []string{"\v", "\f", "\x1c", "\x1d", "\x1e", "\u0085", "\u2028", "\u2029", "\r"} {
		if got, want := Lint("- Read the file and"+separator+"- Write the file."), []Finding{{"dangling-conjunction", "Read the file and"}}; !sameFindings(got, want) {
			t.Errorf("two list items split at %q: Lint = %v, want %v", separator, got, want)
		}
	}
	half := strings.Repeat("word ", 20) + "word."
	if got := Lint(half + "\x1f" + half); len(got) != 0 {
		t.Errorf("two sentences of 21 words split at \\x1f: Lint = %v, want none", got)
	}
	// The control: with no space between them the two are one sentence.
	if got, want := Lint(half+"_"+half), []Finding{{"long-sentence", "41 words"}}; !sameFindings(got, want) {
		t.Errorf("one sentence of 41 words: Lint = %v, want %v", got, want)
	}
}

// TestRotationsJoinTheVerbsOfTexts holds the carry of the rule synonym-rotation from the texts that
// a text holds into it: LintVerbs gives the verbs of the synonym groups that a text uses, and
// Rotations gives each group whose verbs the texts use two of together, with every verb of the
// group that one of them uses, where neither the text nor one text that it holds uses them all. A
// group that one text alone rotates is that text's own finding (Lint, which TestLintFindsEachHardRule
// holds), and the same verb in two texts is no rotation.
func TestRotationsJoinTheVerbsOfTexts(t *testing.T) {
	verbs := func(text string) Verbs {
		_, used := LintVerbs(text)
		return used
	}
	for _, test := range []struct {
		name, own string
		held      []string
		want      [][]string
	}{
		{"a verb and its synonym", "Check the config file.", []string{"Then verify the output."}, [][]string{{"check", "verify"}}},
		{"an inflection of each", "Legion checks each upload.", []string{"It confirmed the size first."}, [][]string{{"check", "confirm"}}},
		{"two groups", "Check the file and delete it.", []string{"Then verify it and remove the copy."}, [][]string{{"check", "verify"}, {"delete", "remove"}}},
		{"three verbs of one group, in the order of the group", "Validate the file.", []string{"Then check it and confirm it."}, [][]string{{"check", "confirm", "validate"}}},
		{"a verb in each of two held texts", "Dispatch keeps the record.", []string{"It checks the size.", "It confirms the type."}, [][]string{{"check", "confirm"}}},
		{"a third verb beside a held text's two", "Validate the file.", []string{"It checks the size and confirms the type."}, [][]string{{"check", "confirm", "validate"}}},
		{"a third verb beside the text's own two", "Check the file and verify it.", []string{"It confirms the type."}, [][]string{{"check", "verify", "confirm"}}},
		{"the control: two verbs in a held text alone", "Dispatch keeps the record.", []string{"It checks the size and confirms the type."}, nil},
		{"the control: a verb of the text that a held text's two hold", "Check the file.", []string{"It checks the size and confirms the type."}, nil},
		{"the control: the same verb in both", "Check the config file.", []string{"Then check the output."}, nil},
		{"the control: two verbs in the text alone", "Check the file and verify it.", []string{"The note says so."}, nil},
		{"the control: the held text adds none of the text's verbs", "Check the file and verify it.", []string{"Then verify the output."}, nil},
		{"the control: a verb in code font", "Check the config file.", []string{"Then run `verify` on it."}, nil},
		{"the control: no held text", "Check the config file.", nil, nil},
	} {
		held := make([]Verbs, len(test.held))
		for index, text := range test.held {
			held[index] = verbs(text)
		}
		if got := Rotations(verbs(test.own), held).Words(); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: Rotations(%q, %q) = %q, want %q", test.name, test.own, test.held, got, test.want)
		}
	}
}

// Held reads the texts that a text holds once (HeldOf), and keeps of them only the sets of verbs of
// each group that they use, so its Rotations reads those sets and no held text again, whatever it
// would keep of one: the held texts followed by more texts that add no set to a group, each held text
// again and texts made of the parts of different held texts, each group's part of one, in an order of
// their own, give the same Held, so its value holds nothing of a text but the sets of its groups.
// HeldOf allocates no more for heldTexts texts followed by heldMixed texts made of their groups'
// parts, at least half of them distinct, than for those texts alone, less than a byte for each
// added text where a copy of one takes 8, so it keeps no copy of the texts, repeated or distinct,
// in its value or outside it. And its Rotations, asked after the caller's slice of held texts is
// written over and other texts are read into another Held, is still the answer for its own, so it
// reads neither the caller's texts again nor texts that another HeldOf read. And
// its Rotations is Rotations as its comment defines it, each held text read for each set of own
// verbs: for 20,000 sets of own verbs and of 0 to 40 held texts, drawn at random from the verbs of
// the synonym groups, the two agree. The cases hold groups that a held text uses each verb of and
// groups it does not (counted), so both answers of the rule are read.
func TestHeldReadsTheHeldTextsOnceAsRotationsReadsThemEach(t *testing.T) {
	definition := func(own Verbs, held []Verbs) Verbs {
		all := own
		for _, each := range held {
			all |= each
		}
		rotated := all.Rotated() & (own ^ all).Groups()
		for _, each := range held {
			rotated &= (each ^ all).Groups()
		}
		return rotated
	}
	var every Verbs
	for _, group := range rules.groups {
		every |= group
	}
	draw := func(random *rand.Rand) Verbs { return Verbs(random.Uint64()&random.Uint64()) & every }
	// mixed is a text that adds no set of verbs to a group of held's: the part of each group of a
	// text of held drawn for that group.
	mixed := func(random *rand.Rand, held []Verbs) Verbs {
		var text Verbs
		for _, group := range rules.groups {
			text |= held[random.IntN(len(held))] & group
		}
		return text
	}
	texts, drawTexts := make([]Verbs, heldTexts), rand.New(rand.NewPCG(4179, 22))
	for index := range texts {
		texts[index] = draw(drawTexts)
	}
	more, distinct := slices.Clone(texts), map[Verbs]bool{}
	for range heldMixed {
		text := mixed(drawTexts, texts)
		more, distinct[text] = append(more, text), true
	}
	if len(distinct) < heldMixed/2 {
		t.Fatalf("the %d texts made of the groups' parts of %d texts hold %d distinct texts, want at least half: the allocation check reads too little", heldMixed, len(texts), len(distinct))
	}
	heldSink = HeldOf(more)
	alone, withMore := heldAllocates(texts), heldAllocates(more)
	if grown := int64(withMore) - int64(alone); grown >= int64(heldMixed) {
		t.Errorf("HeldOf allocates %d bytes for %d texts and %d for them followed by %d texts made of their groups' parts (%d distinct), %d more: want less than a byte for each, as a Held that keeps no copy of its texts allocates",
			alone, len(texts), withMore, heldMixed, len(distinct), grown)
	}
	random := rand.New(rand.NewPCG(4179, 21))
	rotated, kept := 0, 0
	for range 20_000 {
		own := draw(random)
		held := make([]Verbs, random.IntN(41))
		for index := range held {
			held[index] = draw(random)
		}
		want := definition(own, held)
		read := HeldOf(held)
		// more is texts that add no set of verbs to a group: each held text again, and as many texts
		// made of the part of each group of a held text drawn for that group.
		more := slices.Concat(held, held)
		for range held {
			more = append(more, mixed(random, held))
		}
		random.Shuffle(len(more), func(a, b int) { more[a], more[b] = more[b], more[a] })
		if again := HeldOf(slices.Concat(held, more)); !reflect.DeepEqual(again, read) {
			t.Fatalf("held %b: the held texts followed by %b, which add no set of verbs to a group, give the Held %+v, and alone %+v: want the same, a Held that keeps nothing of a text but its groups' sets", held, more, again, read)
		}
		// The caller's slice written over with other texts, and other texts, drawn as the held ones
		// are, read into another Held, before read's Rotations.
		for index := range held {
			held[index] = draw(random)
		}
		others := make([]Verbs, len(held)+1)
		for index := range others {
			others[index] = draw(random)
		}
		HeldOf(others)
		if got := read.Rotations(own); got != want {
			t.Fatalf("own %b, held %b: Held gives %b after the Held of %b was read, the definition %b", own, held, got, others, want)
		}
		if want != 0 {
			rotated++
		} else if (own | read.all).Rotated() != 0 {
			kept++
		}
	}
	t.Logf("HeldOf allocates %d bytes for %d texts and %d for them and %d texts made of their parts (%d distinct); 20,000 cases: %d rotate a group, %d rotate none where the verbs together use two of a group",
		alone, len(texts), withMore, heldMixed, len(distinct), rotated, kept)
	if rotated == 0 || kept == 0 {
		t.Errorf("%d cases rotate a group and %d rotate none beside two verbs of a group: the test reads too little", rotated, kept)
	}
}

// heldTexts is how many texts the allocation check of Held draws, and heldMixed how many texts made
// of their groups' parts follow them: a copy of the distinct ones would take 8 bytes for each, over
// 64 KiB.
const (
	heldTexts = 40
	heldMixed = 16_384
)

// heldSink keeps each Held that heldAllocates reads, so that no allocation of HeldOf is left out.
var heldSink Held

// heldAllocates is the bytes that the heap gives HeldOf of held, from the process's count of every
// byte allocated (runtime.MemStats.TotalAlloc), read before and after the call.
func heldAllocates(held []Verbs) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	heldSink = HeldOf(held)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestWordsAreWhatSteLintLints holds Words to the text that ste-lint's line loop lints, as words:
// inline code is left out, a table's cells are pieces of their own, and Python's white space splits
// the words. No line is a fence, as a text of Dispatch is no document
// (TestTheReadersOfATextOfDispatchReadNoFence). A stop ends no piece. Each word takes the bytes of
// the text from its first character to its last, so those bytes without their inline code are the
// word, and a word with inline code inside it takes that code too.
func TestWordsAreWhatSteLintLints(t *testing.T) {
	for _, test := range []struct {
		text  string
		want  [][]string
		bytes [][2]int
	}{
		{"Dispatch stores it. Dispatch shows it! Does it?", [][]string{{"Dispatch", "stores", "it.", "Dispatch", "shows", "it!", "Does", "it?"}}, nil},
		{"Run `a. b` now. Then stop.", [][]string{{"Run", "now.", "Then", "stop."}}, [][2]int{{0, 3}, {11, 15}, {16, 20}, {21, 26}}},
		{"Read web`x`.Config now", [][]string{{"Read", "web.Config", "now"}}, [][2]int{{0, 4}, {5, 18}, {19, 22}}},
		{"One.\u00a0Two.\x1fThree", [][]string{{"One.", "Two.", "Three"}}, nil},
		{"First line\nsecond. Third", [][]string{{"First", "line"}, {"second.", "Third"}}, [][2]int{{0, 5}, {6, 10}, {11, 18}, {19, 24}}},
		{"| A. b | C |\n| --- | --- |\n| d e | f |", [][]string{{"A.", "b"}, {"C"}, {"d", "e"}, {"f"}}, [][2]int{{2, 4}, {5, 6}, {9, 10}, {29, 30}, {31, 32}, {35, 36}}},
		{"```\nOne. Two.\n```", [][]string{{"`"}, {"One.", "Two."}, {"`"}}, nil},
		{"`only code`", nil, nil},
		{"", nil, nil},
		{"Done. ", [][]string{{"Done."}}, nil},
	} {
		var got [][]string
		var bytes [][2]int
		for _, piece := range Words(test.text) {
			var words []string
			for _, word := range piece {
				words = append(words, word.Text)
				bytes = append(bytes, [2]int{word.Start, word.End})
				if taken := inlineCode.ReplaceAllString(test.text[word.Start:word.End], ""); taken != word.Text {
					t.Errorf("Words(%q): the word %q takes %q, which is %q without its inline code", test.text, word.Text, test.text[word.Start:word.End], taken)
				}
			}
			got = append(got, words)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("Words(%q) = %q, want %q", test.text, got, test.want)
		}
		if test.bytes != nil && !reflect.DeepEqual(bytes, test.bytes) {
			t.Errorf("Words(%q) take the bytes %v, want %v", test.text, bytes, test.bytes)
		}
	}
}

// A text of Dispatch is no markdown document: Dispatch joins the lines of a text into one and
// reads it with each fence marker written as white space (NoFences,
// TestNoFencesWritesEachFenceMarkerAsWhiteSpace), so a sentence of it can start with three
// backticks of code font. So LintVerbs, Words and SpreadOf read no line as a fence, where Lint,
// the port of ste-lint, still does (the control, and TestPortMatchesSteLintOnTheCorpus).
func TestTheReadersOfATextOfDispatchReadNoFence(t *testing.T) {
	const piece = "```x``` a; b"
	if got := Lint(piece); len(got) != 0 {
		t.Errorf("the control: Lint(%q) = %v, want none, as ste-lint reads the line as a fence", piece, got)
	}
	if got, _ := LintVerbs(piece); !sameFindings(got, []Finding{{"semicolon", ";"}}) {
		t.Errorf("LintVerbs(%q) = %v, want the semicolon after the code font", piece, got)
	}
	var words []string
	for _, each := range Words("```x``` One. Two.") {
		for _, word := range each {
			words = append(words, word.Text)
		}
	}
	if !slices.Equal(words, []string{"One.", "Two."}) {
		t.Errorf("Words: %q, want the two words after the code font", words)
	}
	if got := SpreadOf("```x``` one two three").Pieces; !slices.Equal(got, []int{3}) {
		t.Errorf("SpreadOf: the pieces %v, want one of 3 words", got)
	}
}

// TestSpaceIsPythonsWhitespace holds the two definitions of Python's whitespace the port uses,
// the rune test and the class its patterns embed, to each other over every rune.
func TestSpaceIsPythonsWhitespace(t *testing.T) {
	class := spacePattern()
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if in, want := class.MatchString(string(r)), isSpace(r); in != want {
			t.Fatalf("rune %U: the class says %t, isSpace %t", r, in, want)
		}
	}
	for _, r := range []rune{'\x1c', '\x1d', '\x1e', '\x1f', '\u00a0', '\u0085', '\u2028', '\u3000'} {
		if !isSpace(r) {
			t.Errorf("rune %U is Python whitespace, and isSpace says no", r)
		}
	}
	if isSpace('\u200b') {
		t.Errorf("U+200B is no Python whitespace, and isSpace says it is")
	}
}

// walksOfALine is how many times Lint may count each character of a line that is no table row and
// no list item: each walk counts a character at most once (columns), and Lint walks such a line at
// most five times, for the semicolons, for each of the three patterns and for the first places of
// the members of the synonym groups. A walk that counts each finding's column from the start of
// its line again counts about n*n/2 characters for n findings, which no constant times the line
// meets.
const walksOfALine = 5

// TestEachRuleThatWalksMatchesCountsEachCharacterOfALineOnce holds Lint to a bounded count of the
// characters it counts on a line, for each hard rule that walks its matches: each semicolon, each
// phrasal verb, marketing adjective and nominalization, and the first place of each member of a
// synonym group, which stand after words of "é" here. An agent's document can hold such a line,
// and the approval request holds its transaction while Lint reads it. The test counts the
// characters that Lint counts through runes, which no load of the machine changes. All of the
// work of a judgment, a count spelled in another way among it, is the work that
// specform.TestTheWorkOfAJudgmentGrowsInProportionToItsText measures.
func TestEachRuleThatWalksMatchesCountsEachCharacterOfALineOnce(t *testing.T) {
	var data ruleData
	if err := json.Unmarshal(rulesJSON, &data); err != nil {
		t.Fatal(err)
	}
	var members []string
	rotations := 0
	for _, group := range data.SynonymGroups {
		members = append(members, group...)
		rotations += len(group) - 1
	}
	words := strings.Repeat("éé ", 4_000)
	for _, test := range []struct {
		rule, line string
		findings   int
	}{
		{"semicolon", "Dispatch " + strings.Repeat(";", 10_000) + " stores each comment.", 10_000},
		{"phrasal-verb", words + strings.Repeat("spin up ", 2_000), 2_000},
		{"marketing-adjective", words + strings.Repeat("robust ", 2_000), 2_000},
		{"nominalization", words + strings.Repeat("perform a xtion ", 2_000), 2_000},
		{"synonym-rotation", words + strings.Join(members, " "), rotations},
	} {
		t.Run(test.rule, func(t *testing.T) {
			counted := 0
			runes = func(s string) int {
				counted += len(s)
				return utf8.RuneCountInString(s)
			}
			t.Cleanup(func() { runes = utf8.RuneCountInString })
			count := 0
			for _, finding := range Lint(test.line) {
				if finding.Rule == test.rule {
					count++
				}
			}
			t.Logf("%d bytes, %d findings of %s, %d bytes counted", len(test.line), count, test.rule, counted)
			if count != test.findings {
				t.Fatalf("Lint gave %d findings of %s, want %d", count, test.rule, test.findings)
			}
			if counted > walksOfALine*len(test.line) {
				t.Fatalf("Lint counted %d bytes of a line of %d bytes, want at most %d walks of it", counted, len(test.line), walksOfALine)
			}
		})
	}
}
