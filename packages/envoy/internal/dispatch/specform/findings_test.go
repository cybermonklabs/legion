package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// R49: "The ASD-STE100 program (ste-lint) reports no hard finding on them." Each hard finding of
// the port is a finding of the form, by ste-lint's own rule name. In the first two rows ste-lint
// reads no end of a sentence at a stop before a quote, so it counts one long sentence where the
// form counts two short ones: the text is refused by the rule of ste-lint alone.
func TestEachHardFindingOfSteLintIsAFindingOfTheForm(t *testing.T) {
	long := func(words string) string {
		return `ASD-STE100 does not permit a sentence of more than 25 words: "` + words + `"`
	}
	for _, test := range []struct {
		body string
		want []Finding
	}{
		{`Dispatch shows the person each question of each issue with the label "Open." Dispatch shows the person each answer of each issue with the label "Done."`,
			[]Finding{{Rule: "ste100-long-sentence", Detail: long("26 words")}}},
		{`Dispatch answers "done." Then it closes the request, writes the review of each document that the request names, and sends the event to every follower of it.`,
			[]Finding{{Rule: "ste100-long-sentence", Detail: long("27 words")}}},
		{"Dispatch refuses a write of the whole text of a document when that text breaks the document schema, and it answers with one named error code.",
			[]Finding{
				{Rule: "sentence-words", Detail: `a sentence has 26 words, and a sentence has at most 25: "Dispatch refuses a write of the whole text"`},
				{Rule: "ste100-long-sentence", Detail: long("26 words")},
			}},
		{"Dispatch shows the person each question of each issue with the label `\"Open.\"` Dispatch shows the person each answer of each issue with the label `\"Done.\"`", nil},
	} {
		text := statement(test.body)
		var want []Finding
		for _, each := range test.want {
			want = append(want, finding(text, each.Rule, each.Detail))
		}
		if got := Check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("Check(%q) =\n%#v\nwant\n%#v", test.body, got, want)
		}
	}
}

// Each of the seven hard rules of ste-lint is a rule of the form: none is left out.
func TestEachHardRuleOfSteLintIsARuleOfTheForm(t *testing.T) {
	for _, test := range []struct{ body, rule string }{
		{"Dispatch keeps it; it waits.", "ste100-semicolon"},
		{"Legion will spin up the job.", "ste100-phrasal-verb"},
		{"Dispatch keeps a seamless log.", "ste100-marketing-adjective"},
		{"Dispatch performs a validation of each upload.", "ste100-nominalization"},
		{strings.Repeat("word ", 25) + "words.", "ste100-long-sentence"},
		{"Check the config file. Then verify the output.", "ste100-synonym-rotation"},
		{"- Dispatch keeps the record and", "ste100-dangling-conjunction"},
	} {
		got := Check(planted(), Input{Texts: []Text{statement(test.body)}})
		if !slices.ContainsFunc(got, func(finding Finding) bool { return string(finding.Rule) == test.rule }) {
			t.Errorf("Check(%q) = %#v, want a finding of %s", test.body, got, test.rule)
		}
	}
}

// ste-lint reads a line that starts with three backticks or three tildes as a fence, and lints no
// line from there to the next such line. A text of The Spec is no document: Dispatch joins its
// lines into one, and the page shows the procedure of a check as it is written. So a fence marker
// hides no sentence of a text from the rules: the sentence count, the bounds of 25 and 20 words,
// the rule instructions and the rules of ste-lint read the text as they read the same text without
// the marker. Each row is a text with a fence marker and the same text without it, which give the
// same findings, and the text without it gives one at least, so the row can fail.
func TestAFenceMarkerHidesNoSentenceFromTheRules(t *testing.T) {
	const claim = "The team spins up the tool; it performs an installation of each package that the team builds in the main tree of the repository " +
		"for the users who read the guide and the notes that the team writes for them each week of the year and each day of the month at home."
	const run = "Run the tests and read the log. Open the file and record its size. Read the report and send it."
	const three = "Dispatch keeps each record. Dispatch shows each record. Dispatch sends each record."
	said := func(findings []Finding) []string {
		var lines []string
		for _, each := range findings {
			lines = append(lines, fmt.Sprintf("%s %s %s: %s | %s", each.Kind, each.Node, each.Rule, each.Detail, each.Ways))
		}
		return lines
	}
	for _, test := range []struct {
		name          string
		kind          Kind
		fenced, plain string
	}{
		{"the review's claim of 53 words after three backticks", KindClaim, "``` " + claim, claim},
		{"the claim after three tildes", KindClaim, "~~~ " + claim, claim},
		{"the claim in a fence of three lines", KindClaim, "```\n" + claim + "\n```", claim},
		{"the review's procedure in a fence of one line", KindProcedure, "```sh " + run + " ```", run},
		{"the procedure in a fence of three lines", KindProcedure, "```sh\n" + run + "\n```", run},
		{"the procedure in a fence of tildes", KindProcedure, "~~~sh " + run + " ~~~", run},
		{"the procedure after a line break that ste-lint reads", KindProcedure, "Read the guide.\u2028```sh " + run + " ```", "Read the guide.\u2028" + run},
		{"a statement of three sentences after three backticks", KindStatement, "``` " + three, three},
	} {
		fenced := Text{Node: "fence", Kind: test.kind, Body: test.fenced, Where: where}
		plain := Text{Node: "fence", Kind: test.kind, Body: test.plain, Where: where}
		want := said(Check(planted(), Input{Texts: []Text{plain}}))
		if len(want) == 0 {
			t.Fatalf("%s: the text without the marker gives no finding, so the row cannot fail", test.name)
		}
		if got := said(Check(planted(), Input{Texts: []Text{fenced}})); !slices.Equal(got, want) {
			t.Errorf("%s: Check(%q) =\n%q\nwant the findings of the text without the marker\n%q", test.name, test.fenced, got, want)
		}
	}
	if words := len(strings.Fields(claim)); words != 53 {
		t.Fatalf("the claim has %d words, want the review's 53", words)
	}
	// The stated limit: code font that starts a line and holds two backticks in a row, which three
	// backticks open, counts its words there, so a claim of 25 words after it has 26. The control:
	// the same code font inside the line is code font, which counts as no word.
	twentyFive := strings.Repeat("word ", 24) + "words."
	for _, test := range []struct {
		body string
		want []string
	}{
		{"``` a``b ``` " + twentyFive, []string{
			`claim fence sentence-words: a sentence has 26 words, and a sentence has at most 25: "ab word word word word word word word" | `,
			`claim fence ste100-long-sentence: ASD-STE100 does not permit a sentence of more than 25 words: "26 words" | `,
		}},
		{twentyFive + " ``` a``b ```", nil},
	} {
		if got := said(Check(planted(), Input{Texts: []Text{{Node: "fence", Kind: KindClaim, Body: test.body, Where: where}}})); !slices.Equal(got, test.want) {
			t.Errorf("Check(%q) =\n%q\nwant\n%q", test.body, got, test.want)
		}
	}
}

// R49: "No text names ... a person by name." A person's name is the name of one signed-in person:
// the pieces of that person's email, in order, each with a capital letter as a name is written. A
// single piece, and pieces of two people, are no name. An e-mail address has a domain whose last
// label is letters, so a version after an at sign is no address.
func TestAPersonsNameIsTheNameOfOnePerson(t *testing.T) {
	procedure := func(body string) Text { return Text{Node: "go-run", Kind: KindProcedure, Body: body, Where: where} }
	for _, test := range []struct {
		name   string
		people []string
		text   Text
		want   string
	}{
		{"two name pieces of two people", []string{"will.hart@example.com", "sam.page@example.com"},
			statement("Envoy will page the owner when the lease ends."), ""},
		{"the pieces of one person in lower case", []string{"will.page@example.com"},
			statement("Envoy will page the owner when the lease ends."), ""},
		{"one piece of one person", []string{"ada.lovelace@example.com"}, statement("Each change waits for Ada before it lands."), ""},
		{"the pieces of one person in another order", []string{"ada.lovelace@example.com"},
			statement("Each change waits for Lovelace Ada before it lands."), ""},
		{"the pieces of two people with capital letters", []string{"will.hart@example.com", "sam.page@example.com"},
			statement("Each change waits for Will Page before it lands."), ""},
		{"a version of a Go module after an at sign", nil, procedure("Run `go run example.com/checker@v1.2.3`."), ""},
		{"the control: the name of one person", []string{"ada.lovelace@example.com"},
			statement("Ada Lovelace approves each change before it lands."), "Ada Lovelace"},
		{"the control: the name of one person of two", []string{"will.hart@example.com", "sam.page@example.com"},
			statement("Each change waits for Will Hart before it lands."), "Will Hart"},
		{"the control: the address of one person", []string{"ada.lovelace@example.com"},
			statement("Dispatch mails ada.lovelace@example.com about each change."), "ada.lovelace@example.com"},
		{"the control: an address in a procedure", nil, procedure("Run `mail ada@example.org`."), "ada@example.org"},
	} {
		var want []Finding
		if test.want != "" {
			want = []Finding{brokenFinding(test.text, personBreak(test.want, offer{ways: personWays(test.want)}))}
		}
		if got := Check(planted(test.people...), Input{Texts: []Text{test.text}}); !sameFindings(got, want) {
			t.Errorf("%s: Check(%q) =\n%#v\nwant\n%#v", test.name, test.text.Body, got, want)
		}
	}
}

// R49: "No text of The Spec holds a link to a record of Dispatch". A term's name is a text of The
// Spec, so the reference rules judge it, as a text of Kind term. The control: a term named by a
// noun gives no finding of its name.
func TestTheReferenceRulesJudgeTheNameOfATerm(t *testing.T) {
	for _, test := range []struct {
		term       Term
		rule, name string
	}{
		{Term{Name: "dispatch://ACME-12", Definition: "One record of a request.", Where: where}, "dispatch-link", `it links to a record of Dispatch: "dispatch://ACME-12"`},
		{Term{Name: "Ada Lovelace", Definition: "One owner of a request.", Where: where}, "person", `it names a person: "Ada Lovelace"`},
		{Term{Name: "session", Definition: "One conversation of an agent.", Where: where}, "", ""},
	} {
		var want []Finding
		if test.rule != "" {
			want = []Finding{{Node: test.term.Name, Kind: Kind("term"), Where: where, Rule: Rule(test.rule), Detail: test.name, Excerpt: test.term.Name}}
		}
		if got := Check(planted("ada.lovelace@example.com"), Input{Proposed: []Term{test.term}}); !sameFindings(got, want) {
			t.Errorf("the term %q: Check =\n%#v\nwant\n%#v", test.term.Name, got, want)
		}
	}
}

// Only a name that a person approved is a noun of the system: the context's system names, which
// the server reads from the approved documents under the root. A name that the judged document
// writes itself, as a component or a term, does not hide a person's name in the same document.
func TestOnlyAnApprovedNameIsANounOfTheSystem(t *testing.T) {
	text := statement("Each issue reaches Dispatch Admin before its owner starts work.")
	context := planted("dispatch.admin@example.com")
	person := []Finding{finding(text, "person", `it names a person: "Dispatch Admin"`)}
	if got := Check(context, Input{Texts: []Text{text}}); !sameFindings(got, person) {
		t.Errorf("with no system name: Check = %#v, want %#v", got, person)
	}
	component := Text{Node: "dispatch", Kind: KindName, Body: "Dispatch", Where: where}
	if got := Check(context, Input{Texts: []Text{text, component}}); !sameFindings(got, person) {
		t.Errorf("with a component Dispatch in the same document: Check = %#v, want %#v", got, person)
	}
	term := Term{Name: "dispatch", Definition: "The server of the work.", Where: where}
	if got := Check(context, Input{Texts: []Text{text}, Proposed: []Term{term}}); !sameFindings(got, person) {
		t.Errorf("with a term dispatch in the same document: Check = %#v, want %#v", got, person)
	}
	context.SystemNames = []string{"Dispatch"}
	if got := Check(context, Input{Texts: []Text{text}}); len(got) != 0 {
		t.Errorf("with the approved system name Dispatch: Check = %#v, want none", got)
	}
}

// The stated limits of the form, each pinned: the README says each, with its way out, and R49
// leaves what a program cannot refuse to the reviewer of R25.
func TestTheStatedLimitsOfTheForm(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       []string
	}{
		{"a word that starts with an at sign is a handle", "Envoy routes each comment that holds @legion to the session of the role.",
			[]string{"statement planted person"}},
		{"a handle in code font is a handle", "Envoy routes each comment that holds `@legion` to the session of the role.",
			[]string{"statement planted person"}},
		{"a bare number after pull request is a count", "Legion merges each change, as pull request 4179 shows.", nil},
		{"a bare number after incident is a count", "Envoy starts again within 10 seconds, as incident 4471 showed.", nil},
		{"an initialism of single letters ends no sentence", "Dispatch stores records in the U.S. Dispatch indexes each record. Dispatch shows each record.", nil},
	} {
		if got := findingNames(Check(planted(), Input{Texts: []Text{statement(test.body)}})); !slices.Equal(got, test.want) {
			t.Errorf("%s: Check(%q) = %q, want %q", test.name, test.body, got, test.want)
		}
	}
}
