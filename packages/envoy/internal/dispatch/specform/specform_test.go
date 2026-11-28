package specform

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const where = "spec.md version 12"

// planted is the context of the planted texts: project ACME exists, and the server's address is
// dispatch.example.com.
func planted(people ...string) Context {
	return Context{
		IsProjectKey: func(key string) bool { return key == "ACME" },
		PersonNames:  people,
		ServerURL:    "https://dispatch.example.com",
	}
}

func statement(body string) Text {
	return Text{Node: "planted", Kind: KindStatement, Body: body, Where: where}
}

func finding(text Text, rule Rule, detail string) Finding {
	return Finding{Node: text.Node, Kind: text.Kind, Where: text.Where, Rule: rule, Detail: detail, Excerpt: text.Body}
}

// brokenFinding is the finding of Check for found, a break of text.
func brokenFinding(text Text, found broken) Finding {
	return finding(text, found.rule, found.detail)
}

func sameFindings(got, want []Finding) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

// TestCheckRefusesEachFormFault holds Check to the seven planted texts of the plan, each with the
// rule and the detail it breaks. A sentence of 26 words also breaks ste-lint's long-sentence,
// which is the second finding of its text.
func TestCheckRefusesEachFormFault(t *testing.T) {
	claim := Text{Node: "planted", Kind: KindClaim, Body: "Ada Lovelace approves each change before it lands.", Where: where}
	for _, test := range []struct {
		context Context
		text    Text
		rule    Rule
		detail  string
		steLong string
	}{
		{planted(), statement("Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment."),
			"sentences", "it has 3 sentences, and a statement has one or two", ""},
		{planted(), statement("Dispatch refuses a write of the whole text of a document when that text breaks the document schema, and it answers with one named error code."),
			"sentence-words", `a sentence has 26 words, and a sentence has at most 25: "Dispatch refuses a write of the whole text"`,
			`ASD-STE100 does not permit a sentence of more than 25 words: "26 words"`},
		{planted(), statement("Dispatch performs a validation of each upload."),
			"ste100-nominalization", `ASD-STE100 does not permit an action frozen into a noun: "performs a validation"`, ""},
		{planted(), statement("Dispatch keeps each answer, as dispatch://ACME-12 asks."),
			"dispatch-link", `it links to a record of Dispatch: "dispatch://ACME-12"`, ""},
		{planted(), statement("Envoy restarts its listener after incident INC-2041."),
			"incident", `it names an incident: "incident INC-2041". Dispatch reads these forms as the name of an incident: #<n>, number <n>, a key such as INC-<n> and a date`, ""},
		{planted(), statement("Envoy stores each message, as pull request #4031 makes it do."),
			"pull-request", `it names a pull request: "pull request #4031". Dispatch reads these forms as the name of a pull request: #<n>, number <n>, <owner>/<repo>#<n> and its address`, ""},
		{planted("ada.lovelace@example.com"), claim,
			"person", `it names a person: "Ada Lovelace"`, ""},
	} {
		got := check(test.context, Input{Texts: []Text{test.text}})
		want := []Finding{finding(test.text, test.rule, test.detail)}
		if test.steLong != "" {
			want = append(want, finding(test.text, "ste100-long-sentence", test.steLong))
		}
		if !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.text.Body, got, want)
		}
	}
}

// TestCheckTakesTwoSentencesOfTwentyFiveWords is the control of the planted texts: two sentences
// of 25 words each, which ste-lint passes, break no rule.
func TestCheckTakesTwoSentencesOfTwentyFiveWords(t *testing.T) {
	text := statement("Dispatch shows the person each question of each issue that the person follows, with the time of the question and the name of its project. Dispatch shows the person each approval request that waits on the person, with the document, its version and the short summary that its agent wrote.")
	if got := check(planted("ada.lovelace@example.com"), Input{Texts: []Text{text}}); len(got) != 0 {
		t.Fatalf("Check of two sentences of 25 words = %#v, want none", got)
	}
}

// TestAPieceWithNoLetterIsNoSentence holds a statement to sentences that hold a word with a
// letter: a text of numbers or marks alone has no sentence, so it breaks the rule of one or two
// sentences, and a piece with no letter beside a real sentence does not count. A number inside a
// sentence is a word of it.
func TestAPieceWithNoLetterIsNoSentence(t *testing.T) {
	const none = "it has 0 sentences, and a statement has one or two"
	for _, test := range []struct {
		body   string
		detail string
	}{
		{"42", none},
		{"...", none},
		{"—", none},
		{"12:30.", none},
		{"Dispatch keeps each message for 72 hours.", ""},
		{"Dispatch keeps each message for 72 hours. 42.", ""},
	} {
		text := statement(test.body)
		var want []Finding
		if test.detail != "" {
			want = []Finding{finding(text, "sentences", test.detail)}
		}
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestCheckLetsTheWordIncidentStand holds the incident rule to an incident's id: level one says
// "incident" with none.
func TestCheckLetsTheWordIncidentStand(t *testing.T) {
	text := statement("The role prompts and skills that Legion and Envoy ship name no deployment, project, private repository or person, and quote no measurement or incident.")
	if got := check(planted(), Input{Texts: []Text{text}}); len(got) != 0 {
		t.Fatalf("Check = %#v, want none", got)
	}
}

// pullRequestNamed and incidentNamed are the details of the two rules for the text they matched.
func pullRequestNamed(match string) string {
	return fmt.Sprintf("it names a pull request: %q. Dispatch reads these forms as the name of a pull request: #<n>, number <n>, <owner>/<repo>#<n> and its address", match)
}

func incidentNamed(match string) string {
	return fmt.Sprintf("it names an incident: %q. Dispatch reads these forms as the name of an incident: #<n>, number <n>, a key such as INC-<n> and a date", match)
}

// TestACountAfterPullRequestOrIncidentNamesNoRecord holds the two rules to the forms that only a
// name has. A bare number after the words is a count, such as "10 minutes", and names nothing.
// The rows that name a record are the control: each form of a name keeps its finding.
func TestACountAfterPullRequestOrIncidentNamesNoRecord(t *testing.T) {
	for _, body := range []string{
		"Legion merges each pull request 10 minutes after its checks pass.",
		"Legion lands each pull request 2 minutes after its last check passes.",
		"Legion opens each PR 3 times at most.",
		"Dispatch closes each incident 24 hours after its last event.",
		"Dispatch keeps each incident 30 days after it closes.",
		"Legion merges each change, as pull request 4179 shows.",
	} {
		if _, got := checkStatement(body); len(got) != 0 {
			t.Errorf("Check(%q) = %#v, want none: the number is a count", body, got)
		}
	}
	for _, test := range []struct{ body, rule, detail string }{
		{"Legion reads each thread, as PR #3920 requires.", "pull-request", pullRequestNamed("PR #3920")},
		{"Legion reads each thread, as pull request number 12 requires.", "pull-request", pullRequestNamed("pull request number 12")},
		{"Legion reads each thread, as acme/widgets#12 requires.", "pull-request", pullRequestNamed("acme/widgets#12")},
		{"Legion reads each thread, as https://github.com/acme/widgets/pull/12 requires.", "pull-request", pullRequestNamed("github.com/acme/widgets/pull/12")},
		{"Legion reads each thread, as #12 requires.", "pull-request", pullRequestNamed("#12")},
		{"Envoy starts again within 10 seconds, as incident number 4471 showed.", "incident", incidentNamed("incident number 4471")},
		{"Envoy starts again within 10 seconds, as incident SEV-12 showed.", "incident", incidentNamed("incident SEV-12")},
		{"Envoy starts again within 10 seconds, as incident 2026-11-01 showed.", "incident", incidentNamed("incident 2026-11-01")},
	} {
		text, got := checkStatement(test.body)
		if want := []Finding{finding(text, Rule(test.rule), test.detail)}; !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestFailingExampleAndNameTakeOnlyReferenceRules holds the two kinds that are no prose of The
// Spec to the four reference rules.
func TestFailingExampleAndNameTakeOnlyReferenceRules(t *testing.T) {
	example := Text{Node: "v-one", Kind: KindFailingExample, Where: where,
		Body: strings.Repeat("word ", 39) + "and; more."}
	if got := check(planted(), Input{Texts: []Text{example}}); len(got) != 0 {
		t.Errorf("a failing example of 40 words with a semicolon: Check = %#v, want none", got)
	}
	name := Text{Node: "queue", Kind: KindName, Body: "PR #12 queue", Where: where}
	if got, want := check(planted(), Input{Texts: []Text{name}}), []Finding{finding(name, "pull-request", pullRequestNamed("PR #12"))}; !sameFindings(got, want) {
		t.Errorf("the name %q: Check = %#v, want %#v", name.Body, got, want)
	}
}

// TestReferenceRuleReadsInsideCodeSpans holds the reference rules to the raw text: ste-lint
// drops a code span, and a link in one is still a link.
func TestReferenceRuleReadsInsideCodeSpans(t *testing.T) {
	for _, test := range []struct{ body, reference string }{
		{"Dispatch answers `dispatch://ACME-1` here.", "dispatch://ACME-1"},
		{"Dispatch keeps it, as [the ask](https://dispatch.example.com/issues/ACME-3) says.", "https://dispatch.example.com/issues/ACME-3"},
	} {
		text := statement(test.body)
		want := []Finding{finding(text, "dispatch-link", fmt.Sprintf("it links to a record of Dispatch: %q", test.reference))}
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestCheckFindsEachLinkToARecordOfDispatch holds the dispatch-link rule to what names a record:
// a reference, which links to it, and a bare key of a project that exists, a message id and a bare
// id, which name it.
func TestCheckFindsEachLinkToARecordOfDispatch(t *testing.T) {
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	for _, test := range []struct {
		body   string
		detail string
	}{
		{"Dispatch keeps ACME-3 for each reader.", `it names a record of Dispatch by its key: "ACME-3"`},
		{"Dispatch keeps XYZ-3 for each reader.", ""},
		{"Dispatch keeps message:" + id + " for each reader.", `it names a record of Dispatch by its id: "message:` + id + `"`},
		{"Dispatch keeps " + id + " for each reader.", `it names a record of Dispatch by its id: "` + id + `"`},
		{"Dispatch keeps dispatch://ACME-1/ask/" + id + " for each reader.", `it links to a record of Dispatch: "dispatch://ACME-1/ask/` + id + `"`},
		{"Dispatch keeps https://example.org/ACME-3 for each reader.", ""},
		{"Dispatch keeps it (dispatch://ACME-4).", `it links to a record of Dispatch: "dispatch://ACME-4"`},
	} {
		text := statement(test.body)
		var want []Finding
		if test.detail != "" {
			want = []Finding{finding(text, "dispatch-link", test.detail)}
		}
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestTheReferenceRulesCutAReferenceAsTextCutsIt holds the reference rules to the one reading of
// where a reference ends, the text package's, which the reference graph reads by: it drops a `*`,
// `_` or `~` after a reference, it reads on past a vertical tab and U+0085, and it keeps a
// bracketed IPv6 host. A key inside a reference is part of that reference, so it names no record.
func TestTheReferenceRulesCutAReferenceAsTextCutsIt(t *testing.T) {
	type form struct {
		rule  Rule
		words string
	}
	link := func(words string) []form { return []form{{ruleDispatchLink, words}} }
	for _, test := range []struct {
		name, server, line string
		want               []form
	}{
		{"a star after it", "", "Dispatch keeps *dispatch://ACME-1* here.", link("dispatch://ACME-1")},
		{"an underscore after it", "", "Dispatch keeps _dispatch://ACME-1_ here.", link("dispatch://ACME-1")},
		{"a tilde after it", "", "Dispatch keeps ~~dispatch://ACME-1~~ here.", link("dispatch://ACME-1")},
		{"a vertical tab in it", "", "Dispatch opens https://example.org/a\vACME-3 here.", nil},
		{"a next line in it", "", "Dispatch opens https://example.org/a\u0085ACME-3 here.", nil},
		{"a bracketed IPv6 host", "http://[::1]:8766", "Dispatch opens http://[::1]:8766/issues/ACME-1 here.", link("http://[::1]:8766/issues/ACME-1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := planted()
			if test.server != "" {
				c.ServerURL = test.server
			}
			var got []form
			for _, each := range referenceForms(c, nil, test.line, false) {
				got = append(got, form{each.rule, test.line[each.start:each.end]})
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("referenceForms(%q) = %q, want %q", test.line, got, test.want)
			}
		})
	}
}

// TestCheckFindsAnAddressAndAHandle holds the person rule to an email address and an @handle.
func TestCheckFindsAnAddressAndAHandle(t *testing.T) {
	for _, test := range []struct{ body, name string }{
		{"Dispatch mails ada@example.com about each change.", "ada@example.com"},
		{"Dispatch asks @ada about each change.", "@ada"},
	} {
		text := statement(test.body)
		want := []Finding{brokenFinding(text, personBreak(test.name))}
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestCheckJudgesEachProposedDefinition holds Check to a definition it is given as a term: one
// text of Kind definition, named by its term, at the term's place.
func TestCheckJudgesEachProposedDefinition(t *testing.T) {
	term := Term{Name: "session", Definition: "One conversation of an agent; it holds the work.", Where: "terms.md version 3"}
	text := Text{Node: "session", Kind: KindDefinition, Body: term.Definition, Where: term.Where}
	want := []Finding{finding(text, "ste100-semicolon", `ASD-STE100 does not permit a semicolon: ";"`)}
	if got := check(planted(), Input{Proposed: []Term{term}}); !sameFindings(got, want) {
		t.Fatalf("Check = %#v, want %#v", got, want)
	}
}

// TestCheckJoinsTheLinesOfAText holds Check to one line of a text, as ste-lint reads a line, and
// to one finding where a text repeats a fault.
func TestCheckJoinsTheLinesOfAText(t *testing.T) {
	text := statement("Legion lands a change\r\nat the head it read; it lands no other head; it waits.\n")
	want := []Finding{finding(text, "ste100-semicolon", `ASD-STE100 does not permit a semicolon: ";"`)}
	if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
		t.Fatalf("Check = %#v, want %#v", got, want)
	}
	empty := statement("  ")
	want = []Finding{finding(empty, "sentences", "it has 0 sentences, and a statement has one or two")}
	if got := check(planted(), Input{Texts: []Text{empty}}); !sameFindings(got, want) {
		t.Fatalf("Check of an empty statement = %#v, want %#v", got, want)
	}
}

// TestCheckCutsTheExcerpt holds the excerpt to the first 160 characters of the text.
func TestCheckCutsTheExcerpt(t *testing.T) {
	text := statement(strings.Repeat("é", 150) + "; " + strings.Repeat("x", 40))
	got := check(planted(), Input{Texts: []Text{text}})
	if len(got) != 1 || got[0].Excerpt != string([]rune(text.Body)[:160]) {
		t.Fatalf("Check = %#v, want one finding whose excerpt is the first 160 characters", got)
	}
}

func breaking(count int) []Finding {
	findings := make([]Finding, count)
	for index := range findings {
		findings[index] = Finding{Node: fmt.Sprintf("s-%d", index+1), Kind: KindStatement, Where: where,
			Rule: "sentences", Detail: "it has 3 sentences, and a statement has one or two"}
	}
	return findings
}

const more = "dispatch spec-form --issue ACME-2 --artifact spec.md lists them all."

// TestMessageListsTwentyAndCountsTheRest holds the refusal to 20 finding lines and a count of
// the rest, before the next step.
func TestMessageListsTwentyAndCountsTheRest(t *testing.T) {
	lines := strings.Split(Message(ActRequest, breaking(23), 23, more).String(), "\n")
	listed := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "- ") {
			listed++
		}
	}
	if listed != 20 || len(lines) != 23 {
		t.Fatalf("the refusal has %d lines and lists %d findings, want 23 and 20:\n%s", len(lines), listed, strings.Join(lines, "\n"))
	}
	if got, want := lines[21], "And 3 more: "+more; got != want {
		t.Errorf("line 22 = %q, want %q", got, want)
	}
	if got, want := lines[1], "- statement s-1 in spec.md version 12: it has 3 sentences, and a statement has one or two."; got != want {
		t.Errorf("line 2 = %q, want %q", got, want)
	}
}

// TestMessageEndsWithTheNextStep holds each act's head and last line, and the count's grammar.
// The next step is the refusal's second part, and the whole refusal is the first part, then the
// next step on a line of its own.
func TestMessageEndsWithTheNextStep(t *testing.T) {
	two := breaking(2)
	for _, test := range []struct {
		name       string
		words      Words
		head, last string
	}{
		{"request", Message(ActRequest, two, 2, more), "Dispatch refused this approval request, because 2 texts break the form of The Spec.", "Fix each text, then request approval again."},
		{"approval", Message(ActApproval, two, 2, more), "Dispatch refused this approval, because 2 texts break the form of The Spec.", "To send these findings to the author, choose Request changes."},
		{"restore", Message(ActRestore, two, 2, more), "Dispatch did not open this approval request again, because 2 texts break the form of The Spec.", "Fix each text, then request approval again."},
		{"one", Message(ActRequest, two[:1], 1, more), "Dispatch refused this approval request, because 1 text breaks the form of The Spec.", "Fix each text, then request approval again."},
	} {
		lines := strings.Split(test.words.String(), "\n")
		if lines[0] != test.head || lines[len(lines)-1] != test.last {
			t.Errorf("%s: head %q and last line %q, want %q and %q", test.name, lines[0], lines[len(lines)-1], test.head, test.last)
		}
		if test.words.NextStep != test.last || test.words.String() != test.words.Refusal+"\n"+test.last || strings.Contains(test.words.Refusal, test.last) {
			t.Errorf("%s: refusal %q and next step %q, want the next step %q apart from the refusal", test.name, test.words.Refusal, test.words.NextStep, test.last)
		}
	}
	reason := strings.Split(ChangesReason(two, 2, more), "\n")
	if head, last := reason[0], reason[len(reason)-1]; head != "Dispatch refused my approval, because 2 texts break the form of The Spec." || last != "Fix each text, then request approval again." {
		t.Errorf("changes reason: head %q and last line %q", head, last)
	}
}

// TestMessageIsTheRefusalOfThePlan holds the request's refusal to the copy of the plan.
func TestMessageIsTheRefusalOfThePlan(t *testing.T) {
	findings := []Finding{
		{Node: "ship-at-head", Kind: KindStatement, Where: where, Rule: "sentences", Detail: "it has 3 sentences, and a statement has one or two"},
		{Node: "session", Kind: KindDefinition, Where: where, Rule: "term-second-definition", Detail: "terms.md version 3 of ACME-1 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"},
	}
	want := "Dispatch refused this approval request, because 2 texts break the form of The Spec.\n" +
		"- statement ship-at-head in spec.md version 12: it has 3 sentences, and a statement has one or two.\n" +
		"- definition session in spec.md version 12: terms.md version 3 of ACME-1 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first.\n" +
		"Fix each text, then request approval again."
	if got := Message(ActRequest, findings, 2, more).String(); got != want {
		t.Fatalf("Message =\n%s\nwant\n%s", got, want)
	}
}

// TestTheRefusalHeadCountsTheTextsThatBreak holds the first line of each refusal to the count of
// the texts that hold a finding: one statement with several faults is one text, with one line for
// each fault. Control: a second text with its own faults makes the head say 2.
func TestTheRefusalHeadCountsTheTextsThatBreak(t *testing.T) {
	text := statement("Dispatch spins up a seamless lease; it starts. It ends.")
	findings := check(planted(), Input{Texts: []Text{text}})
	if len(findings) < 3 {
		t.Fatalf("setup: Check = %#v, want at least three findings of the one text", findings)
	}
	other := Text{Node: "second", Kind: KindStatement, Body: "Legion spins up each lease; it waits.", Where: where}
	both := append(slices.Clone(findings), check(planted(), Input{Texts: []Text{other}})...)
	for _, test := range []struct {
		name, text, head string
		lines            int
	}{
		{"request", Message(ActRequest, findings, len(findings), more).String(), "Dispatch refused this approval request, because 1 text breaks the form of The Spec.", len(findings)},
		{"approval", Message(ActApproval, findings, len(findings), more).String(), "Dispatch refused this approval, because 1 text breaks the form of The Spec.", len(findings)},
		{"changes reason", ChangesReason(findings, len(findings), more), "Dispatch refused my approval, because 1 text breaks the form of The Spec.", len(findings)},
		{"restore", Message(ActRestore, findings, len(findings), more).String(), "Dispatch did not open this approval request again, because 1 text breaks the form of The Spec.", len(findings)},
		{"two texts", Message(ActRequest, both, len(both), more).String(), "Dispatch refused this approval request, because 2 texts break the form of The Spec.", len(both)},
	} {
		lines := strings.Split(test.text, "\n")
		if lines[0] != test.head || len(lines) != test.lines+2 {
			t.Errorf("%s: head %q and %d lines, want %q and %d finding lines between the head and the next step:\n%s",
				test.name, lines[0], len(lines), test.head, test.lines, test.text)
		}
	}
}

// TestADefinitionLineInAnotherFormIsNotPassedInSilence holds Check to each line under Terms: a
// line in another form than "- **<term>**: <definition>" gets a finding that names it, so a
// definition that breaks the form cannot pass because its line has another form. Control: the
// same definition in the term line's form gets findings of its own rules.
func TestADefinitionLineInAnotherFormIsNotPassedInSilence(t *testing.T) {
	control, err := ExtractDocument("terms.md", 1, "# Terms\n\n- **beacon**: A seamless signal that each session spins up; it repeats. It stops. It starts.\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := check(planted(), Input{Texts: control.Texts, Proposed: control.Terms}); len(got) == 0 {
		t.Fatalf("control: the same definition in the form of a term line gives no finding")
	}
	extracted, err := ExtractDocument("terms.md", 1, "# Terms\n\n- **lease**: The time for which a session holds an issue.\n"+
		"- **beacon** — A seamless signal that each session spins up; it repeats. It stops. It starts.\n"+
		"- **drift:** The slow change of the clock of one machine; it is robust.\n")
	if err != nil {
		t.Fatal(err)
	}
	got := check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms})
	for _, name := range []string{"beacon", "drift"} {
		if !slices.ContainsFunc(got, func(finding Finding) bool { return strings.Contains(finding.Node, name) }) {
			t.Errorf("%s: no finding names its line, and the extractor read terms %#v: a definition that breaks the form passes in silence", name, extracted.Terms)
		}
	}
}
