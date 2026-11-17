package specform

import (
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// quotedName is the first quoted string of a finding's detail: the form that the rule read.
var quotedName = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// referenceNames is "<rule> <form>" for each finding of the reference rules on body under c, in
// order. The text is a name, so no rule of a sentence reads it.
func referenceNames(t *testing.T, c Context, body string) []string {
	t.Helper()
	var names []string
	for _, finding := range Check(c, Input{Texts: []Text{{Node: "n", Kind: KindName, Body: body, Where: where}}}) {
		name, err := strconv.Unquote(quotedName.FindString(finding.Detail))
		if err != nil {
			t.Fatalf("the detail %q quotes no form: %v", finding.Detail, err)
		}
		names = append(names, string(finding.Rule)+" "+name)
	}
	return names
}

// Each form of a reference rule is a whole word: a character of a word (a letter, a mark, a decimal
// digit or connector punctuation such as "_", of any script) right before it or right after it
// joins it to a longer word, which names nothing, and so do the characters that continue the form
// (wordForm). For each form, the form alone, joined to a word on its left and joined to a word on
// its right, with an ASCII letter and with a letter of another script, and a letter of another
// script inside it. Where a letter is part of the form itself (an email address, a handle, the key
// of an incident), the joined text is a longer form of the same kind. A form whose alphabet is ASCII
// by the rule of its system (a key of Dispatch, an id, the owner and the repository of GitHub, a
// number) holds no letter of another script, so such a letter inside it makes it no form.
func TestEachReferenceFormIsAWholeWord(t *testing.T) {
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	for _, test := range []struct {
		form, body string
		want       []string
	}{
		{"a key", "ACME-12", []string{"dispatch-link ACME-12"}},
		{"a key", "xACME-12", nil},
		{"a key", "ACME-12x", nil},
		{"a key", "ACME-12.", []string{"dispatch-link ACME-12"}},
		{"an id", id, []string{"dispatch-link " + id}},
		{"an id", "x" + id, nil},
		{"an id", id + "x", nil},
		{"incident number", "incident number 12", []string{"incident incident number 12"}},
		{"incident number", "xincident number 12", nil},
		{"incident number", "incident number 12x", nil},
		{"incident #", "incident#12", []string{"incident incident#12"}},
		{"incident #", "incident #12x", nil},
		{"an incident's date", "incident 2026-11-17", []string{"incident incident 2026-11-17"}},
		{"an incident's date", "incident 2026-11-17x", nil},
		{"an incident's key", "incident sev-12", []string{"incident incident sev-12"}},
		{"an incident's key", "incident sev-12x", nil},
		{"INC-<n>", "INC-12", []string{"incident INC-12"}},
		{"INC-<n>", "xINC-12", nil},
		{"INC-<n>", "INC-12x", nil},
		{"pull request number", "PR number 12", []string{"pull-request PR number 12"}},
		{"pull request number", "xPR number 12", nil},
		{"pull request number", "PR number 12x", nil},
		{"pull request #", "PR#12", []string{"pull-request PR#12"}},
		{"pull request #", "pull request #12", []string{"pull-request pull request #12"}},
		{"pull request #", "PR #12x", nil},
		{"<owner>/<repo>#<n>", "acme/widgets#12", []string{"pull-request acme/widgets#12"}},
		{"<owner>/<repo>#<n>", "docs/acme/widgets#12", nil},
		{"<owner>/<repo>#<n>", "acme/widgets#12x", nil},
		{"#<n>", "#12", []string{"pull-request #12"}},
		{"#<n>", "(#12).", []string{"pull-request #12"}},
		{"#<n>", "x#12", nil},
		{"#<n>", "#12x", nil},
		{"#<n>, a colour of six digits and letters", "#12abcd", nil},
		{"#<n>, a colour of six digits and letters", "#123abc", nil},
		{"#<n>, the stated limit: a colour of three digits", "#123", []string{"pull-request #123"}},
		{"the address of a pull request", "github.com/o/r/pull/12", []string{"pull-request github.com/o/r/pull/12"}},
		{"the address of a pull request", "xgithub.com/o/r/pull/12", nil},
		{"the address of a pull request", "github.com/o/r/pull/12draft", nil},
		{"the address of a pull request", "github.com/o/r/pull/12/files", []string{"pull-request github.com/o/r/pull/12"}},
		{"an email address", "ada@example.com", []string{"person ada@example.com"}},
		{"an email address", "xada@example.com", []string{"person xada@example.com"}},
		{"an email address", "ada@example.comx", []string{"person ada@example.comx"}},
		{"an email address", "ada@example.com5", nil},
		{"a handle", "@legion", []string{"person @legion"}},
		{"a handle", "x@legion", nil},
		{"a handle", "@legion/core", nil},
		{"a handle, an npm scope", "@types/node", nil},
		{"a key, ASCII by Dispatch's rule", "ACMÉ-12", nil},
		{"a key, ASCII by Dispatch's rule", "éACME-12", nil},
		{"a key, ASCII by Dispatch's rule", "ACME-12é", nil},
		{"an id, hexadecimal", "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0é37", nil},
		{"an id, hexadecimal", "é" + id, nil},
		{"an id, hexadecimal", id + "é", nil},
		{"an incident's key", "incident sév-12", []string{"incident incident sév-12"}},
		{"an incident's key", "éincident sev-12", nil},
		{"an incident's key", "incident sev-12é", nil},
		{"incident number", "incident number 12é", nil},
		{"INC-<n>", "éINC-12", nil},
		{"INC-<n>", "INC-12é", nil},
		{"pull request number", "éPR number 12", nil},
		{"pull request #", "PR #12é", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "acmé/widgets#12", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "éacme/widgets#12", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "acme/widgets#12é", nil},
		{"#<n>", "é#12", nil},
		{"#<n>", "#12é", nil},
		{"#<n>, the stated limit: a script with no space between words", "见#12", nil},
		{"#<n>, the stated limit: a colour of digits only", "#000000", []string{"pull-request #000000"}},
		{"the address of a pull request, ASCII by GitHub's rule", "github.com/ö/r/pull/12", nil},
		{"the address of a pull request", "égithub.com/o/r/pull/12", nil},
		{"the address of a pull request", "github.com/o/r/pull/12é", nil},
		{"the address of a pull request, the stated limit: a script with no space between words",
			"参见github.com/acme/widgets/pull/12", nil},
		{"an email address", "josé.garcia@example.com", []string{"person josé.garcia@example.com"}},
		{"an email address", "ada@exämple.com", []string{"person ada@exämple.com"}},
		{"an email address", "éada@example.com", []string{"person éada@example.com"}},
		{"an email address", "ada@example.comé", []string{"person ada@example.comé"}},
		{"an email address, a letter with a combining mark", "jose\u0301@example.com", []string{"person jose\u0301@example.com"}},
		{"an email address, then a colon and a space", "ada@example.com: she", []string{"person ada@example.com"}},
		{"an email address, then a colon at the end", "ada@example.com:", []string{"person ada@example.com"}},
		{"an SSH address: a colon that no white space follows continues it", "git@github.com:owner/repo.git", nil},
		{"a handle", "@josé", []string{"person @josé"}},
		{"a handle", "é@legion", nil},
		{"a handle, a letter with a combining mark", "@jose\u0301", []string{"person @jose\u0301"}},
		{"a handle, the stated limit: a word that starts with @", "@media", []string{"person @media"}},
	} {
		if got := referenceNames(t, planted(), test.body); !slices.Equal(got, test.want) {
			t.Errorf("%s: %q gives %q, want %q", test.form, test.body, got, test.want)
		}
	}
}

// Each part of a person's name is a whole word of wordClass, as each form of a name is: a character
// of a word right before it or right after it makes it part of a longer word, which is no part of
// the name. A part of a person's email that holds a digit or a mark is one word too, and so is a
// word of an approved component name. The stated limit: Dispatch compares no normal forms, so a
// letter written with a combining mark in the text names no part that the email writes as one
// composed letter.
func TestEachPartOfAPersonsNameIsAWholeWord(t *testing.T) {
	const ada = "ada.lovelace@example.com"
	for _, test := range []struct {
		name, person, system, body string
		want                       []string
	}{
		{"the control: the name", ada, "", "Dispatch asks Ada Lovelace.", []string{"person Ada Lovelace"}},
		{"the control: a comma and a digit apart", ada, "", "Dispatch asks Ada Lovelace, 2 times.", []string{"person Ada Lovelace"}},
		{"a digit after a part", ada, "", "Dispatch asks Ada Lovelace2.", nil},
		{"a digit before a part", ada, "", "Dispatch asks 2Ada Lovelace.", nil},
		{"_ after a part", ada, "", "Dispatch asks Ada Lovelace_x.", nil},
		{"_ before a part", ada, "", "Dispatch asks x_Ada Lovelace.", nil},
		{"a combining mark after a part", ada, "", "Dispatch asks Ada Lovelace\u0301.", nil},
		{"a part of the email with a digit", "ada2.lovelace@example.com", "", "Dispatch asks Ada2 Lovelace.", []string{"person Ada2 Lovelace"}},
		{"a combining mark in the email and in the text", "jose\u0301.garcia@example.com", "", "Dispatch asks Jose\u0301 Garcia.",
			[]string{"person Jose\u0301 Garcia"}},
		{"a composed letter in the email and in the text", "josé.garcia@example.com", "", "Dispatch asks José Garcia.", []string{"person José Garcia"}},
		{"the stated limit: a composed letter in the email, a combining mark in the text", "josé.garcia@example.com", "",
			"Dispatch asks Jose\u0301 Garcia.", nil},
		{"a component name that _ joins is one word", "dispatch.admin@example.com", "Dispatch_Admin", "Dispatch asks Dispatch Admin.",
			[]string{"person Dispatch Admin"}},
		{"the control: a component name of two words", "dispatch.admin@example.com", "Dispatch Admin", "Dispatch asks Dispatch Admin.", nil},
	} {
		context := planted(test.person)
		if test.system != "" {
			context.SystemNames = []string{test.system}
		}
		if got := referenceNames(t, context, test.body); !slices.Equal(got, test.want) {
			t.Errorf("%s: %q with %s gives %q, want %q", test.name, test.body, test.person, got, test.want)
		}
	}
}

// No form can read a word otherwise than wordClass does: newWordForm refuses a form, a left or a
// right that holds a notion of a word of the regular expressions, each of which reads ASCII only,
// and so does wordRegexp, which also builds the words of a person's name (nameWord). The control:
// the same form with wordClass is taken.
func TestNoFormHoldsAnotherNotionOfAWord(t *testing.T) {
	refuses := func(form, left, right string) (refused bool) {
		defer func() { refused = recover() != nil }()
		newWordForm(form, left, right)
		return false
	}
	for _, test := range []struct{ form, left, right string }{
		{`[\w.+-]+@example\.com`, "", ""},
		{`\bACME-[0-9]+`, "", ""},
		{`#[0-9]+\B`, "", ""},
		{`@[^\W]+`, "", ""},
		{`@[[:word:]]+`, "", ""},
		{`#[0-9]+`, `\w`, ""},
		{`#[0-9]+`, "", `\w`},
	} {
		if !refuses(test.form, test.left, test.right) {
			t.Errorf("newWordForm(%q, %q, %q) is taken, want a refusal", test.form, test.left, test.right)
		}
	}
	for _, expr := range []string{`\w+`, `[[:word:]]+`} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("wordRegexp(%q) is taken, want a refusal", expr)
				}
			}()
			wordRegexp(expr)
		}()
	}
	if refuses(`[`+wordClass+`.+\-]+@example\.com`, "", "") {
		t.Error("the control: a form built on wordClass is refused")
	}
}

// A bare key or id names a record of Dispatch and links to none: the finding says so. A reference
// links to the record.
func TestABareKeyOrIdNamesARecordAndLinksToNone(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, test := range []struct{ body, detail string }{
		{"Dispatch stores the value `" + id + "`.", `it names a record of Dispatch by its id: "` + id + `"`},
		{"Dispatch keeps ACME-3 for each reader.", `it names a record of Dispatch by its key: "ACME-3"`},
		{"Dispatch keeps dispatch://ACME-4 for each reader.", `it links to a record of Dispatch: "dispatch://ACME-4"`},
	} {
		text := statement(test.body)
		if got, want := Check(planted(), Input{Texts: []Text{text}}), []Finding{finding(text, "dispatch-link", test.detail)}; !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}
