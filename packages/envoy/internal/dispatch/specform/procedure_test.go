package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The procedure of a check is in ASD-STE100 (R49): each instruction gives one instruction and has
// at most 20 words, and a sentence that describes has at most 25, as a statement, a claim, a
// purpose and a definition do. A sentence is an instruction when it starts with a verb of the list
// (instruction-verbs.txt), with no comma after it, and the first word of its object; after a
// condition that a comma ends, it starts after the comma. A second instruction is the same pair
// after "and", "or", "then" or a comma, where the listed verb starts a clause of its own. Two
// verbs of one object, as in "Push and hold the switch", are one instruction, as ASD-STE100 lets
// two actions at the same time be. A word that the list does not name is no verb. The same words
// in a claim break neither rule. The stated limits: a verb that the list does not name, as "Seed"
// in "Seed the database and run the test", names no instruction; a second instruction after a
// relative clause, as in "Read each request that Dispatch opens and record its version", is the
// reviewer's to name; a condition ends at its first comma, so a list of verbs in it reads as two
// instructions; a listed verb that is a noun of the object with no clause of its own after it, as
// `name` in "the step, name `lint`", reads as a second instruction, and so does one before a
// clause whose verb has no -s or -ed, as `output` in "the input and output the tools write"; and
// a listed verb before a word that starts no object, as in "Run tests against the server", starts
// no instruction, so its sentence keeps 25 words. The finding quotes the first words of the
// second instruction.
func TestAProcedureGivesOneInstructionInEachSentenceOfTwentyWordsAtMost(t *testing.T) {
	procedure := func(body string) Text { return Text{Node: "v-run", Kind: KindProcedure, Body: body, Where: where} }
	// twoInstructions is the detail of a sentence that gives two instructions: it quotes the first
	// words of the second one.
	twoInstructions := func(second string) string {
		return `a sentence gives two instructions, and a sentence of a procedure gives one: "` + second + `"`
	}
	const twenty = "Run the merge test against a fixture server that holds two approved specs of the same issue and three requests."
	const twentyOne = "Run the merge test against a fixture server that holds two approved specs of the same issue and three open requests."
	const named = "Run the test of the step, name `lint`, with the race detector."
	for _, test := range []struct {
		name string
		text Text
		want []Finding
	}{
		{"a procedure of 21 words", procedure(twentyOne), []Finding{finding(procedure(twentyOne), ruleSentenceWords,
			`a sentence has 21 words, and an instruction has at most 20: "Run the merge test against a fixture server"`)}},
		{"a procedure of two instructions", procedure("Read each open approval request and record its version."),
			[]Finding{finding(procedure("Read each open approval request and record its version."), ruleInstructions,
				twoInstructions("record its version"))}},
		{"the review's procedure of two instructions in 22 words",
			procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
			[]Finding{
				finding(procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
					ruleSentenceWords, `a sentence has 22 words, and an instruction has at most 20: "Read each open approval request and record the"`),
				finding(procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
					ruleInstructions, twoInstructions("record the document, its version, its author and")),
			}},
		{"two instructions in 20 words",
			procedure("Read each open approval request of the root and record its version in the table of the review for today."),
			[]Finding{finding(procedure("Read each open approval request of the root and record its version in the table of the review for today."), ruleInstructions,
				twoInstructions("record its version in the table of the"))}},
		{"a second instruction after a comma", procedure("Read the log, record its size."),
			[]Finding{finding(procedure("Read the log, record its size."), ruleInstructions,
				twoInstructions("record its size"))}},
		{"a second instruction after then", procedure("Run the test, then read the log."),
			[]Finding{finding(procedure("Run the test, then read the log."), ruleInstructions,
				twoInstructions("read the log"))}},
		{"a second instruction after a condition", procedure("If the run fails, read the log and send the report."),
			[]Finding{finding(procedure("If the run fails, read the log and send the report."), ruleInstructions,
				twoInstructions("send the report"))}},
		{"a condition before the second instruction alone", procedure("Read the log and, if it fails, send the report."),
			[]Finding{finding(procedure("Read the log and, if it fails, send the report."), ruleInstructions,
				twoInstructions("send the report"))}},
		// A condition after the first instruction can govern each instruction before it, with or
		// without a comma before the condition, and the finding quotes it with the instruction.
		{"a condition after the second instruction", procedure("Read the log and send the report if the run fails."),
			[]Finding{finding(procedure("Read the log and send the report if the run fails."), ruleInstructions,
				twoInstructions("send the report if the run fails"))}},
		{"the same condition after a comma", procedure("Read the log and send the report, if the run fails."),
			[]Finding{finding(procedure("Read the log and send the report, if the run fails."), ruleInstructions,
				twoInstructions("send the report, if the run fails"))}},
		{"a condition of another word after the second instruction", procedure("Run the tests and send the report when the merge ends."),
			[]Finding{finding(procedure("Run the tests and send the report when the merge ends."), ruleInstructions,
				twoInstructions("send the report when the merge ends"))}},
		{"a choice of two instructions", procedure("Read the diff or run the tests."),
			[]Finding{finding(procedure("Read the diff or run the tests."), ruleInstructions,
				twoInstructions("run the tests"))}},
		{"the control: a condition word inside the first instruction", procedure("Read the log before the run ends and send the report."),
			[]Finding{finding(procedure("Read the log before the run ends and send the report."), ruleInstructions,
				twoInstructions("send the report"))}},
		{"the control: three instructions, each in a sentence of its own", procedure("Read the log, record the time and send the report."),
			[]Finding{finding(procedure("Read the log, record the time and send the report."), ruleInstructions,
				twoInstructions("record the time and send the report"))}},
		{"an object in code font", procedure("Run `go test` and read the log."),
			[]Finding{finding(procedure("Run `go test` and read the log."), ruleInstructions,
				twoInstructions("read the log"))}},
		{"a second instruction of an object in code font", procedure("Read the log and run `go test ./...` on it."),
			[]Finding{finding(procedure("Read the log and run `go test ./...` on it."), ruleInstructions,
				twoInstructions("run `go test ./...` on it"))}},
		{"the control: a procedure of 20 words and one instruction", procedure(twenty), nil},
		{"the control: two sentences of one instruction each", procedure("Read each open approval request. Record its version."), nil},
		{"the control: two verbs of one object", procedure("Push and hold the switch."), nil},
		{"the control: two objects of one verb", procedure("Read the log and the report of each run."), nil},
		{"the control: a procedure that describes a program", procedure("A test against a Dispatch server from the head of the pull request."), nil},
		{"the control: a relative clause whose verbs a plural noun joins", procedure("Count the requests that open and close the same day."), nil},
		{"the control: an adverb after a comma", procedure("Open an ask on the root issue, never a child."), nil},
		{"the control: a question word after and", procedure("Check the label, the status and whether the document is the spec."), nil},
		{"the control: a list of nouns after a word and a comma", procedure("Instead, this skill reads each word and keeps it."), nil},
		{"the control: a word and a comma at the start", procedure("Locally, the tests read each file and record the time."), nil},
		{"the stated limit: a list of verbs in a condition", procedure("Before you plan, file an issue or post a finding, run the search."),
			[]Finding{finding(procedure("Before you plan, file an issue or post a finding, run the search."), ruleInstructions,
				twoInstructions("post a finding, run the search"))}},
		{"the stated limit: a listed verb that is a noun of the object", procedure(named),
			[]Finding{finding(procedure(named), ruleInstructions,
				twoInstructions("name `lint`, with the race detector"))}},
		{"the control: a claim of two instructions", Text{Node: "v-run", Kind: KindClaim, Body: "Read each open approval request and record its version.", Where: where}, nil},
		{"the control: a claim of 21 words", Text{Node: "v-run", Kind: KindClaim, Body: twentyOne, Where: where}, nil},
		{"the stated limit: a verb that the list does not name", procedure("Seed the database and run the test."), nil},
		{"the stated limit: a second instruction after a relative clause", procedure("Read each request that Dispatch opens and record its version."), nil},
		{"the stated limit: a listed verb before a clause whose verb has no -s or -ed", procedure("Compare the input and output the tools write."),
			[]Finding{finding(procedure("Compare the input and output the tools write."), ruleInstructions,
				twoInstructions("output the tools write"))}},
		{"the control: the same clause after that", procedure("Compare the input and output that the tools write."), nil},
		{"the stated limit: a listed verb before a word that starts no object, in 21 words",
			procedure("Run tests against the server with the race detector on and with a timeout of ten minutes for each run here."), nil},
		{"the stated limit: a listed verb before a name, with a second listed verb after a comma",
			procedure("Start Dispatch with a root of The Spec and a fixture issue, then send each fixture text through each route."), nil},
		{"the control: the same instruction with an object start",
			procedure("Run the tests against the server with the race detector on and with a timeout of ten minutes for each run here."),
			[]Finding{finding(procedure("Run the tests against the server with the race detector on and with a timeout of ten minutes for each run here."),
				ruleSentenceWords, `a sentence has 22 words, and an instruction has at most 20: "Run the tests against the server with the"`)}},
	} {
		if got := check(planted(), Input{Texts: []Text{test.text}}); !sameFindings(got, test.want) {
			t.Errorf("%s: Check = %#v, want %#v", test.name, got, test.want)
		}
	}
	if words := len(strings.Fields(twenty)); words != 20 {
		t.Fatalf("the control has %d words, want 20", words)
	}
}

// The rule reads a verb only from its list (instruction-verbs.txt), never a word that no list
// names, so a word after a comma or "and" that is no listed verb starts no instruction: an adverb,
// "plus", "minus", "though", a noun before a span in code font or before "each". A listed verb
// starts a second instruction only where it starts a clause of its own: a noun phrase after it that
// a verb of its own follows, as in "time it uses", is a clause about the object (a relative clause
// with no "that"), so the word is a noun there. Each of these procedures gives one instruction.
func TestAProcedureOfOneInstructionIsTakenWhateverWordStandsAfterACommaOrAnd(t *testing.T) {
	for _, body := range []string{
		// The deep audit's ten.
		"Read the log, especially the last line.",
		"Check the output, particularly the last row.",
		"Read the log, mainly the errors.",
		"Run the slow tests, notably the merge test.",
		"Run the test, exactly the way the README says.",
		"Read each file, plus the README.",
		"Count the rows, minus the header.",
		"Record the date and time the run ends.",
		"Note the version and size the server reports.",
		"Read the log, though the trace has more detail.",
		// The review's.
		"Read the log, especially the last lines.",
		"Run the test on a clean machine, ideally a new one.",
		"Check the output, particularly the first line.",
		"Run each test alone, preferably the slow ones first.",
		"Open the dashboard, mainly the Inbox.",
		"Run the test with the race detector, typically the slowest run.",
		"Check the logs, notably the errors.",
		"Run the tests, primarily the API tests.",
		"Send the request to the server, exactly the one that the test starts.",
		"Run the check against the main branch, plus the release branch.",
		"Count the requests, minus the ones that failed.",
		"Read the README, section `Install`.",
		"Read the logs of the client and server each day.",
		"Measure the time and memory each run takes.",
		"Read the input and output the tool writes.",
		// The acceptance's four, each a relative clause with no "that".
		"Record the date and time each request opens.",
		"Read the name and version this request holds.",
		"Measure the memory and time it uses.",
		"Compare the size and hash this build writes.",
		// And the same four with "that" before the clause, the acceptance's control.
		"Record the date and time that each request opens.",
		"Read the name and version that this request holds.",
		"Measure the memory and time that it uses.",
		"Compare the size and hash that this build writes.",
	} {
		text := Text{Node: "v-run", Kind: KindProcedure, Body: body, Where: where}
		if got := check(planted(), Input{Texts: []Text{text}}); len(got) != 0 {
			t.Errorf("%q gives one instruction: Check = %#v, want no finding", body, got)
		}
	}
	two := Text{Node: "v-run", Kind: KindProcedure, Body: "Read the log and record the date.", Where: where}
	if got := check(planted(), Input{Texts: []Text{two}}); !sameFindings(got, []Finding{finding(two, ruleInstructions,
		`a sentence gives two instructions, and a sentence of a procedure gives one: "record the date"`)}) {
		t.Errorf("a procedure of two instructions: Check = %#v, want the one finding of the rule instructions", got)
	}
}

// ASD-STE100 gives an instruction at most 20 words and a description at most 25. A sentence of a
// procedure is an instruction when it starts with a listed verb (after a condition that a comma
// ends), so a procedure that describes a program keeps 25 words.
func TestADescriptionInAProcedureHasTwentyFiveWordsAndAnInstructionTwenty(t *testing.T) {
	const description = "A test against a Dispatch server from the head of the pull request, with two fixture repositories, one fixture issue and one issue approved before."
	const longDescription = "A test against a Dispatch server from the head of the pull request, with two fixture repositories, one fixture issue and one issue approved before it."
	for _, test := range []struct {
		body string
		want []string
	}{
		{description, nil},
		{longDescription, []string{`a sentence has 26 words, and a sentence has at most 25: "A test against a Dispatch server from the"`,
			`ASD-STE100 does not permit a sentence of more than 25 words: "26 words"`}},
		{"Run the merge test against a fixture server that holds two approved specs of the same issue and three open requests.",
			[]string{`a sentence has 21 words, and an instruction has at most 20: "Run the merge test against a fixture server"`}},
	} {
		text := Text{Node: "v-run", Kind: KindProcedure, Body: test.body, Where: where}
		var got []string
		for _, finding := range check(planted(), Input{Texts: []Text{text}}) {
			got = append(got, finding.Detail)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%q: the details %q, want %q", test.body, got, test.want)
		}
	}
	if words := len(strings.Fields(description)); words != 25 {
		t.Fatalf("the description has %d words, want 25", words)
	}
}

// A span in code font is the object of an instruction, as in "Run `go test` with the flag", for
// the bound of 20 words as for the rule instructions: both rules read the sentence with each span
// in code font as one word. Nothing in code font counts among the 20 words. So an instruction of 21
// to 25 words outside code font breaks the bound, and one of 20 does not. The control: the same
// sentence with its object in words, and a sentence that describes, which keeps 25.
func TestAnInstructionWhoseObjectIsInCodeFontHasTwentyWordsAtMost(t *testing.T) {
	tail := strings.Fields("with the flag that the team uses for each package of the module in the main tree of the repository on each work day")
	// coded is an instruction of words words outside code font: "Run", the span, and the tail.
	coded := func(words int) string { return "Run `go test` " + strings.Join(tail[:words-1], " ") + "." }
	over := func(words int) string {
		return fmt.Sprintf(`a sentence has %d words, and an instruction has at most 20: "Run with the flag that the team uses"`, words)
	}
	procedure := func(body string) Text { return Text{Node: "v-run", Kind: KindProcedure, Body: body, Where: where} }
	const probe = "Run `go test` with the flag that the team uses for each package of the module in the main tree of the repository today."
	const words = "Run the tests with the flag that the team uses for each package of the module in the main tree of the repository today."
	const describes = "The run of `go test` with the flag that the team uses for each package of the module in the main tree of the repository today."
	rows := []struct {
		name, body, want string
	}{
		{"the review's probe, 22 words outside code font", probe, over(22)},
		{"the control: the object in words, 24 words", words, `a sentence has 24 words, and an instruction has at most 20: "Run the tests with the flag that the"`},
		{"the control: a sentence that describes, 24 words outside code font", describes, ""},
		{"the control: an instruction of 20 words outside code font", coded(20), ""},
	}
	for words := 21; words <= 25; words++ {
		rows = append(rows, struct{ name, body, want string }{fmt.Sprintf("an instruction of %d words outside code font", words), coded(words), over(words)})
	}
	for _, test := range rows {
		text := procedure(test.body)
		var want []Finding
		if test.want != "" {
			want = []Finding{finding(text, ruleSentenceWords, test.want)}
		}
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("%s: Check(%q) = %#v, want %#v", test.name, test.body, got, want)
		}
	}
	if got := len(strings.Fields(strings.Replace(coded(25), "`go test` ", "", 1))); got != 25 {
		t.Fatalf("the instruction of 25 words has %d words outside code font", got)
	}
}
