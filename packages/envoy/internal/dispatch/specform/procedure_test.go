package specform

import (
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
// no instruction, so its sentence keeps 25 words. A finding writes the instructions as sentences of
// their own only where that keeps what the sentence means: no "or" joins two of them, and no
// condition stands outside the first instruction, before it or after it, with or without a comma;
// elsewhere it names what the sentences must keep. Each finding's ways out are the sentences that
// TestEachWayOutOfARefusalPasses applies.
func TestAProcedureGivesOneInstructionInEachSentenceOfTwentyWordsAtMost(t *testing.T) {
	procedure := func(body string) Text { return Text{Node: "v-run", Kind: KindProcedure, Body: body, Where: where} }
	twoInstructions := func(second, split, verb string) string {
		return `a sentence gives two instructions, and a sentence of a procedure gives one: "` + second +
			`". Write each instruction in a sentence of its own, as in "` + split + `", or, if "` + verb +
			`" is no verb here, write "that" before the clause after it`
	}
	// kept is the detail of a sentence whose split would change what it means: it names what each
	// sentence must keep, and gives no split.
	kept := func(second, keep, verb string) string {
		return `a sentence gives two instructions, and a sentence of a procedure gives one: "` + second +
			`". Write each instruction in a sentence of its own and keep ` + keep + `, or, if "` + verb +
			`" is no verb here, write "that" before the clause after it`
	}
	// split is the detail of a sentence whose way out "that" another rule refuses: in an instruction
	// of 20 words or more, "that" makes one word more than the word bound.
	split := func(second, split string) string {
		return `a sentence gives two instructions, and a sentence of a procedure gives one: "` + second +
			`". Write each instruction in a sentence of its own, as in "` + split + `"`
	}
	const condition, choice = "the condition with each instruction that it governs", "the choice between them"
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
				twoInstructions("record its version", "Read each open approval request. Record its version.", "record"))}},
		{"the review's procedure of two instructions in 22 words",
			procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
			[]Finding{
				finding(procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
					ruleSentenceWords, `a sentence has 22 words, and an instruction has at most 20: "Read each open approval request and record the"`),
				finding(procedure("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval."),
					ruleInstructions, split("record the document, its version, its author and",
						"Read each open approval request. Record the document, its version, its author and the reason that the request gives for approval.")),
			}},
		{"two instructions in 20 words, whose way out that would make 21",
			procedure("Read each open approval request of the root and record its version in the table of the review for today."),
			[]Finding{finding(procedure("Read each open approval request of the root and record its version in the table of the review for today."), ruleInstructions,
				split("record its version in the table of the", "Read each open approval request of the root. Record its version in the table of the review for today."))}},
		{"a second instruction after a comma", procedure("Read the log, record its size."),
			[]Finding{finding(procedure("Read the log, record its size."), ruleInstructions,
				twoInstructions("record its size", "Read the log. Record its size.", "record"))}},
		{"a second instruction after then", procedure("Run the test, then read the log."),
			[]Finding{finding(procedure("Run the test, then read the log."), ruleInstructions,
				twoInstructions("read the log", "Run the test. Read the log.", "read"))}},
		// After "If so," the second sentence still gives two instructions, so the finding offers no
		// condition of its own; and the split would cut the second instruction off from the condition
		// that governs it, so the finding gives none.
		{"a second instruction after a condition", procedure("If the run fails, read the log and send the report."),
			[]Finding{finding(procedure("If the run fails, read the log and send the report."), ruleInstructions,
				kept("send the report", condition, "send"))}},
		{"a condition before the second instruction alone", procedure("Read the log and, if it fails, send the report."),
			[]Finding{finding(procedure("Read the log and, if it fails, send the report."), ruleInstructions,
				kept("send the report", condition, "send"))}},
		// A condition after the first instruction can govern each instruction before it, and a
		// split cannot keep its scope, with or without a comma before the condition.
		{"a condition after the second instruction", procedure("Read the log and send the report if the run fails."),
			[]Finding{finding(procedure("Read the log and send the report if the run fails."), ruleInstructions,
				kept("send the report if the run fails", condition, "send"))}},
		{"the same condition after a comma", procedure("Read the log and send the report, if the run fails."),
			[]Finding{finding(procedure("Read the log and send the report, if the run fails."), ruleInstructions,
				kept("send the report, if the run fails", condition, "send"))}},
		{"a condition of another word after the second instruction", procedure("Run the tests and send the report when the merge ends."),
			[]Finding{finding(procedure("Run the tests and send the report when the merge ends."), ruleInstructions,
				kept("send the report when the merge ends", condition, "send"))}},
		{"a choice of two instructions", procedure("Read the diff or run the tests."),
			[]Finding{finding(procedure("Read the diff or run the tests."), ruleInstructions,
				kept("run the tests", choice, "run"))}},
		{"the control: a condition word inside the first instruction", procedure("Read the log before the run ends and send the report."),
			[]Finding{finding(procedure("Read the log before the run ends and send the report."), ruleInstructions,
				twoInstructions("send the report", "Read the log before the run ends. Send the report.", "send"))}},
		{"the control: three instructions, each in a sentence of its own", procedure("Read the log, record the time and send the report."),
			[]Finding{finding(procedure("Read the log, record the time and send the report."), ruleInstructions,
				twoInstructions("record the time and send the report", "Read the log. Record the time. Send the report.", "record"))}},
		{"an object in code font", procedure("Run `go test` and read the log."),
			[]Finding{finding(procedure("Run `go test` and read the log."), ruleInstructions,
				twoInstructions("read the log", "Run `go test`. Read the log.", "read"))}},
		{"a second instruction of an object in code font", procedure("Read the log and run `go test ./...` on it."),
			[]Finding{finding(procedure("Read the log and run `go test ./...` on it."), ruleInstructions,
				twoInstructions("run `go test ./...` on it", "Read the log. Run `go test ./...` on it.", "run"))}},
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
				kept("post a finding, run the search", choice+" and "+condition, "post")+
					`, or, if the words before it are part of the condition, write the condition as a sentence of its own, as in `+
					`"You plan, file an issue or post a finding. Before that, run the search."`)}},
		{"the stated limit: a listed verb that is a noun of the object", procedure(named),
			[]Finding{finding(procedure(named), ruleInstructions,
				twoInstructions("name `lint`, with the race detector", "Run the test of the step. Name `lint`, with the race detector.", "name"))}},
		{"the control: a claim of two instructions", Text{Node: "v-run", Kind: KindClaim, Body: "Read each open approval request and record its version.", Where: where}, nil},
		{"the control: a claim of 21 words", Text{Node: "v-run", Kind: KindClaim, Body: twentyOne, Where: where}, nil},
		{"the stated limit: a verb that the list does not name", procedure("Seed the database and run the test."), nil},
		{"the stated limit: a second instruction after a relative clause", procedure("Read each request that Dispatch opens and record its version."), nil},
		{"the stated limit: a listed verb before a clause whose verb has no -s or -ed", procedure("Compare the input and output the tools write."),
			[]Finding{finding(procedure("Compare the input and output the tools write."), ruleInstructions,
				twoInstructions("output the tools write", "Compare the input. Output the tools write.", "output"))}},
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
		if got := saidFindings(Check(planted(), Input{Texts: []Text{test.text}})); !sameFindings(got, test.want) {
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
		if got := Check(planted(), Input{Texts: []Text{text}}); len(got) != 0 {
			t.Errorf("%q gives one instruction: Check = %#v, want no finding", body, got)
		}
	}
	two := Text{Node: "v-run", Kind: KindProcedure, Body: "Read the log and record the date.", Where: where}
	if got := Check(planted(), Input{Texts: []Text{two}}); !sameFindings(got, []Finding{findingWays(two, ruleInstructions,
		`a sentence gives two instructions, and a sentence of a procedure gives one: "record the date"`,
		`Write each instruction in a sentence of its own, as in "Read the log. Record the date.", `+
			`or, if "record" is no verb here, write "that" before the clause after it`)}) {
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
		for _, finding := range Check(planted(), Input{Texts: []Text{text}}) {
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
