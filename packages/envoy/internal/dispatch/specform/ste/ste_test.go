package ste

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
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

// TestWordsAreWhatSteLintLints holds Words to the text that ste-lint's line loop lints, as words:
// inline code is left out, a code fence line holds none, a table's cells are pieces of their own,
// and Python's white space splits the words. A stop ends no piece.
func TestWordsAreWhatSteLintLints(t *testing.T) {
	for _, test := range []struct {
		text string
		want [][]string
	}{
		{"Dispatch stores it. Dispatch shows it! Does it?", [][]string{{"Dispatch", "stores", "it.", "Dispatch", "shows", "it!", "Does", "it?"}}},
		{"Run `a. b` now. Then stop.", [][]string{{"Run", "now.", "Then", "stop."}}},
		{"One.\u00a0Two.\x1fThree", [][]string{{"One.", "Two.", "Three"}}},
		{"First line\nsecond. Third", [][]string{{"First", "line"}, {"second.", "Third"}}},
		{"| A. b | C |\n| --- | --- |\n| d e | f |", [][]string{{"A.", "b"}, {"C"}, {"d", "e"}, {"f"}}},
		{"```\nOne. Two.\n```", nil},
		{"`only code`", nil},
		{"", nil},
		{"Done. ", [][]string{{"Done."}}},
	} {
		if got := Words(test.text); !reflect.DeepEqual(got, test.want) {
			t.Errorf("Words(%q) = %q, want %q", test.text, got, test.want)
		}
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
