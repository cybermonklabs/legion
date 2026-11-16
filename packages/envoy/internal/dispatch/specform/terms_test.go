package specform

import "testing"

// listedSession is the definition of session that the list of the root holds.
var listedSession = Term{
	Name:       "session",
	Definition: "One conversation of an agent, with its id, in which the agent does its work.",
	Where:      "terms.md version 3 of ACME-1",
}

// definition is the text of Kind definition that Check judges for term.
func definition(term Term) Text {
	return Text{Node: term.Name, Kind: KindDefinition, Body: term.Definition, Where: term.Where}
}

// A proposed definition of a term that the list holds is a second definition, and the refusal
// names the document that defines the term. The control: the same definition with no list, or
// with a list that holds another term, breaks no rule.
func TestSecondDefinitionInTheListIsRefused(t *testing.T) {
	proposed := Term{Name: "session", Definition: "The record of one agent process in Envoy.", Where: where}
	want := []Finding{finding(definition(proposed), "term-second-definition",
		"terms.md version 3 of ACME-1 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first")}
	if got := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{listedSession}}); !sameFindings(got, want) {
		t.Fatalf("Check = %#v, want %#v", got, want)
	}
	heartbeat := Term{Name: "heartbeat", Definition: "The registration that a session sends again every 90 seconds.", Where: "terms.md version 3 of ACME-1"}
	for _, list := range [][]Term{nil, {heartbeat}} {
		if got := Check(planted(), Input{Proposed: []Term{proposed}, List: list}); len(got) != 0 {
			t.Fatalf("Check with the list %#v = %#v, want no finding", list, got)
		}
	}
}

// A document that defines one term twice holds a second definition: the second one is refused,
// with the count. The control: one definition of each of two terms breaks no rule.
func TestSecondDefinitionInOneDocumentIsRefused(t *testing.T) {
	first := Term{Name: "Session", Definition: "One conversation of an agent, in which the agent does its work.", Where: where}
	second := Term{Name: "session", Definition: "The record of one agent process in Envoy.", Where: where}
	want := []Finding{finding(definition(second), "term-second-definition", "this document defines this term 2 times. Keep one definition")}
	if got := Check(planted(), Input{Proposed: []Term{first, second}}); !sameFindings(got, want) {
		t.Fatalf("Check = %#v, want %#v", got, want)
	}
	lease := Term{Name: "lease", Definition: "The right of one session to one work unit for a time.", Where: where}
	if got := Check(planted(), Input{Proposed: []Term{first, lease}}); len(got) != 0 {
		t.Fatalf("Check of two terms = %#v, want no finding", got)
	}
}

// A definition has one sentence. The control: the same definition in one sentence breaks no rule.
func TestDefinitionOfTwoSentencesIsRefused(t *testing.T) {
	heartbeat := Term{Name: "heartbeat", Definition: "The registration that a session sends again. It comes every 90 seconds.", Where: where}
	want := []Finding{finding(definition(heartbeat), "definition-sentences", "the definition has 2 sentences, and a definition has one")}
	if got := Check(planted(), Input{Proposed: []Term{heartbeat}}); !sameFindings(got, want) {
		t.Fatalf("Check = %#v, want %#v", got, want)
	}
	heartbeat.Definition = "The registration that a session sends again every 90 seconds."
	if got := Check(planted(), Input{Proposed: []Term{heartbeat}}); len(got) != 0 {
		t.Fatalf("Check of one sentence = %#v, want no finding", got)
	}
}

// Two term names are one term when they are the same in lower case with each run of spaces
// collapsed to one. The control: a plural is another term.
func TestTermNamesCompareLowercaseWithSpacesCollapsed(t *testing.T) {
	listed := Term{Name: "work unit", Definition: "A piece of work that one agent does.", Where: "terms.md version 3 of ACME-1"}
	proposed := Term{Name: "Work  Unit", Definition: "One task of an agent.", Where: where}
	want := []Finding{finding(definition(proposed), "term-second-definition",
		"terms.md version 3 of ACME-1 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first")}
	if got := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{listed}}); !sameFindings(got, want) {
		t.Fatalf("Check against the list = %#v, want %#v", got, want)
	}
	again := Term{Name: " work unit ", Definition: "A piece of work that one agent does.", Where: where}
	want = []Finding{finding(definition(again), "term-second-definition", "this document defines this term 2 times. Keep one definition")}
	if got := Check(planted(), Input{Proposed: []Term{proposed, again}}); !sameFindings(got, want) {
		t.Fatalf("Check of one document = %#v, want %#v", got, want)
	}
	plural := Term{Name: "work units", Definition: "The pieces of work that the agents do.", Where: where}
	if got := Check(planted(), Input{Proposed: []Term{plural}, List: []Term{listed}}); len(got) != 0 {
		t.Fatalf("Check of a plural = %#v, want no finding", got)
	}
}

// One name in two Unicode forms is one term: "café" with U+00E9 (NFC) and "café" with e and
// U+0301 (NFD) read as one word to a person. The control: the same name in the same form is a
// second definition.
func TestTermNamesCompareInOneUnicodeForm(t *testing.T) {
	listed := Term{Name: "caf\u00e9", Definition: "The room where the agents rest.", Where: "spec.md version 1 of ACME-35"}
	detail := "spec.md version 1 of ACME-35 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"
	again := Term{Name: "caf\u00e9", Definition: "The list of the menus of one day.", Where: where}
	if got, want := Check(planted(), Input{Proposed: []Term{again}, List: []Term{listed}}), []Finding{finding(definition(again), "term-second-definition", detail)}; !sameFindings(got, want) {
		t.Fatalf("control, the same form: Check = %#v, want %#v", got, want)
	}
	proposed := Term{Name: "cafe\u0301", Definition: "The list of the menus of one day.", Where: where}
	if got, want := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{listed}}), []Finding{finding(definition(proposed), "term-second-definition", detail)}; !sameFindings(got, want) {
		t.Fatalf("Check of the NFD name against the NFC one = %#v, want %#v", got, want)
	}
}

// Names compare in the compatibility form: a ligature and a full-width letter read as the letters
// they stand for, in the list and in one document. The control: a plural is still another term.
func TestTermNamesCompareInTheCompatibilityForm(t *testing.T) {
	file := Term{Name: "file", Definition: "The bytes of one document that Dispatch stores.", Where: "terms.md version 3 of ACME-1"}
	detail := "terms.md version 3 of ACME-1 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"
	for _, name := range []string{"\ufb01le", "\uff46\uff49\uff4c\uff45", "\uff26ile"} {
		proposed := Term{Name: name, Definition: "One upload of a person.", Where: where}
		if got, want := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{file}}), []Finding{finding(definition(proposed), "term-second-definition", detail)}; !sameFindings(got, want) {
			t.Errorf("Check of %q against the list's file = %#v, want %#v", name, got, want)
		}
	}
	ligature := Term{Name: "\ufb01le", Definition: "One upload of a person.", Where: where}
	want := []Finding{finding(definition(ligature), "term-second-definition", "this document defines this term 2 times. Keep one definition")}
	if got := Check(planted(), Input{Proposed: []Term{{Name: "file", Definition: "The bytes of one document.", Where: where}, ligature}}); !sameFindings(got, want) {
		t.Errorf("Check of one document = %#v, want %#v", got, want)
	}
	plural := Term{Name: "\ufb01les", Definition: "The uploads of a person.", Where: where}
	if got := Check(planted(), Input{Proposed: []Term{plural}, List: []Term{file}}); len(got) != 0 {
		t.Fatalf("Check of a plural = %#v, want no finding", got)
	}
}
