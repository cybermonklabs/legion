package specform

import (
	"strings"
	"testing"
)

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

// R50: a term "has one definition of one sentence". A footnote that a definition refers to is
// part of it, as of every text, so its sentences count, and the finding names the footnote and the
// way out: what the footnote says goes into the one sentence, or the footnote goes. The rows are
// the acceptance tester's and the second reader's, each a definition of one sentence with a source
// in a footnote, and one with two footnotes. The control: a definition of two sentences with no
// footnote gets the detail that counts its sentences alone.
func TestTheFindingOfADefinitionWithAFootnoteNamesTheFootnote(t *testing.T) {
	const oneNote = "the definition has 2 sentences with its footnote, and a definition has one. " +
		"A footnote is part of the definition that refers to it. " +
		"Put what the footnote says in the one sentence of the definition, or remove the footnote"
	for _, row := range []struct{ name, markdown, want string }{
		{"a footnote that gives a source",
			"## Terms\n\n- **mooring**: The rope that ties one boat to a dock.[^1]\n\n[^1]: The harbour manual of 2024 gives this meaning.\n",
			oneNote},
		{"a footnote that links to a source",
			"## Terms\n\n- **zephyr**: One warm wind.[^source]\n\n[^source]: See [source](https://example.com/source).\n",
			oneNote},
		{"two footnotes",
			"## Terms\n\n- **lease**: One record of a claim.[^a][^b]\n\n[^a]: The design note names it.\n\n[^b]: The manual names it too.\n",
			"the definition has 3 sentences with its footnotes, and a definition has one. " +
				"A footnote is part of the definition that refers to it. " +
				"Put what the footnotes say in the one sentence of the definition, or remove the footnotes"},
		{"the control: two sentences and no footnote",
			"## Terms\n\n- **heartbeat**: The registration that a session sends again. It comes every 90 seconds.\n",
			"the definition has 2 sentences, and a definition has one"},
	} {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, row.markdown))
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		var details []string
		for _, finding := range Check(planted(), Input{Proposed: extracted.Terms}) {
			if finding.Rule == ruleDefinitionSentences {
				details = append(details, finding.Detail)
			}
		}
		if len(details) != 1 || details[0] != row.want {
			t.Errorf("%s: the details of %s are %q, want only %q", row.name, ruleDefinitionSentences, details, row.want)
		}
	}
}

// A definition with no word is no sentence: R50 says a term has "one definition of one sentence",
// and "42", "...", "—" and "12:30." hold no word with a letter. The control: a definition of one
// sentence breaks no rule.
func TestADefinitionWithNoWordIsRefused(t *testing.T) {
	for _, body := range []string{"42", "...", "\u2014", "12:30."} {
		term := Term{Name: "mark", Definition: body, Where: where}
		got := Check(planted(), Input{Proposed: []Term{term}})
		refused := false
		for _, f := range got {
			if f.Rule == ruleDefinitionSentences {
				refused = true
			}
		}
		if !refused {
			t.Errorf("Check of the definition %q = %#v, want a finding of rule %s", body, got, ruleDefinitionSentences)
		}
	}
	one := Term{Name: "mark", Definition: "The sign that a reviewer puts on one line.", Where: where}
	if got := Check(planted(), Input{Proposed: []Term{one}}); len(got) != 0 {
		t.Fatalf("control, one sentence: Check = %#v, want no finding", got)
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
// second definition. Each name is also refused, because é is no letter from a to z.
func TestTermNamesCompareInOneUnicodeForm(t *testing.T) {
	listed := Term{Name: "caf\u00e9", Definition: "The room where the agents rest.", Where: "spec.md version 1 of ACME-35"}
	detail := "spec.md version 1 of ACME-35 already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"
	again := Term{Name: "caf\u00e9", Definition: "The list of the menus of one day.", Where: where}
	if got, want := Check(planted(), Input{Proposed: []Term{again}, List: []Term{listed}}), []Finding{
		finding(definition(again), "term-name", nameDetail("\"\u00e9\" (U+00E9)")),
		finding(definition(again), "term-second-definition", detail),
	}; !sameFindings(got, want) {
		t.Fatalf("control, the same form: Check = %#v, want %#v", got, want)
	}
	proposed := Term{Name: "cafe\u0301", Definition: "The list of the menus of one day.", Where: where}
	if got, want := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{listed}}), []Finding{
		finding(definition(proposed), "term-name", nameDetail("\"\u0301\" (U+0301)")),
		finding(definition(proposed), "term-second-definition", detail),
	}; !sameFindings(got, want) {
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

// Two names that a person reads as one name are one term. A zero-width space, a soft hyphen or a
// word joiner inside a name shows nothing, and Greek in capitals ends a word with Σ where lower
// case writes ς. The control: a plural is another term.
func TestTermNamesThatReadAsOneNameAreOneTerm(t *testing.T) {
	cases := []struct{ listed, proposed string }{
		{"heartbeat", "heart\u200bbeat"},
		{"heartbeat", "heart\u00adbeat"},
		{"heartbeat", "heart\u2060beat"},
		{"\u03bb\u03cc\u03b3\u03bf\u03c2", "\u039b\u038c\u0393\u039f\u03a3"},
	}
	for _, c := range cases {
		listed := Term{Name: c.listed, Definition: "The first meaning of this name.", Where: "spec.md version 1 of ACME-61"}
		proposed := Term{Name: c.proposed, Definition: "Another meaning of this name.", Where: where}
		got := Check(planted(), Input{Proposed: []Term{proposed}, List: []Term{listed}})
		refused := false
		for _, f := range got {
			if f.Rule == ruleTermSecondDefinition && strings.HasPrefix(f.Detail, "spec.md version 1 of ACME-61 already defines") {
				refused = true
			}
		}
		if !refused {
			t.Errorf("Check of %q (%+q) against the listed %q = %#v, want term-second-definition", c.proposed, c.proposed, c.listed, got)
		}
	}
	plural := Term{Name: "heartbeats", Definition: "The signals of many sessions.", Where: where}
	listed := Term{Name: "heartbeat", Definition: "The first meaning of this name.", Where: "spec.md version 1 of ACME-61"}
	if got := Check(planted(), Input{Proposed: []Term{plural}, List: []Term{listed}}); len(got) != 0 {
		t.Fatalf("control, a plural: Check = %#v, want no finding", got)
	}
}

// TermKey is the caseless form of an identifier (NFKC_Casefold): the compatibility form, full case
// folding, no default-ignorable character, and each run of spaces as one space. The controls: a
// plural, and a space where the other name has none, are other names.
func TestTermKeyIsTheCaselessFormOfAnIdentifier(t *testing.T) {
	for _, same := range [][2]string{
		{"heartbeat", "heart\u034fbeat"},                                     // a combining grapheme joiner
		{"heartbeat", "heart\ufe0fbeat"},                                     // a variation selector
		{"heartbeat", "heart\u200dbeat"},                                     // a zero-width joiner
		{"stra\u00dfe", "STRASSE"},                                           // full folding makes ß two letters
		{"\u03c3\u03bf\u03c6\u03af\u03b1", "\u03a3\u039f\u03a6\u038a\u0391"}, // capitals and an accent
		{" Work  Unit ", "work unit"},
	} {
		if got, want := TermKey(same[0]), TermKey(same[1]); got != want {
			t.Errorf("TermKey(%+q) = %+q and TermKey(%+q) = %+q, want one name", same[0], got, same[1], want)
		}
	}
	for _, other := range [][2]string{{"heartbeat", "heartbeats"}, {"heartbeat", "heart beat"}} {
		if TermKey(other[0]) == TermKey(other[1]) {
			t.Errorf("TermKey(%q) = TermKey(%q), want two names", other[0], other[1])
		}
	}
}

// nameDetail is the detail of the rule term-name for a name that holds the characters held.
func nameDetail(held string) string {
	return "the name holds " + held + ". A term name holds only the letters a to z, the digits 0 to 9, spaces and hyphens"
}

// refusedOf reports whether Check refuses proposed as a second definition of listed, and the
// detail of its term-name finding ("" for none). It is the helper of the tester's third run
// (secondDefinitionOf), which also reads the name rule.
func refusedOf(proposed, listed string) (second bool, name string) {
	list := []Term{{Name: listed, Definition: "The first meaning of this name.", Where: "terms.md version 1 of ACME-1"}}
	in := Input{Proposed: []Term{{Name: proposed, Definition: "Another meaning of this name.", Where: where}}, List: list}
	for _, f := range Check(planted(), in) {
		switch {
		case f.Rule == "term-second-definition" && strings.HasPrefix(f.Detail, "terms.md version 1 of ACME-1 already defines"):
			second = true
		case f.Rule == "term-name":
			name = f.Detail
		}
	}
	return second, name
}

// Names that read as one name are one term, or a name of a term is refused (the tester's
// TestNamesThatReadAsOneAreOneTermAccept3, from the third acceptance run). Its ten rows that
// fold to the letters a to z are one term with the listed name, as the tester wrote them. Its
// two rows of a dot above on i ("İnbox", U+0130, folds to "i\u0307nbox") asked for a second
// definition of inbox. The decision on that run closes the class by the alphabet: such a name is
// refused by the rule term-name, which names the character, so those two rows ask for that. The
// controls are the tester's: a dotless ı, a plural and a hyphen are other terms.
func TestNamesThatReadAsOneAreOneTermOrARefusedName(t *testing.T) {
	one := []struct{ proposed, listed, why string }{
		{"\uff48\uff45\uff41\uff52\uff54\uff42\uff45\uff41\uff54", "heartbeat", "full-width letters"},
		{"\ufb01le", "file", "the ligature fi"},
		{"caf\u00e9", "cafe\u0301", "an accent in one code point or two"},
		{"a\u0323\u0302", "a\u0302\u0323", "two combining marks in either order"},
		{"\u1ea1\u0302", "a\u0323\u0302", "a precomposed letter and its marks"},
		{"heart\u200dbeat", "heartbeat", "a zero-width joiner"},
		{"heart\u034fbeat", "heartbeat", "a combining grapheme joiner"},
		{"heartbeat\ufe0f", "heartbeat", "a variation selector"},
		{"INBOX", "inbox", "capitals"},
		{"STRASSE", "stra\u00dfe", "the sharp s"},
	}
	for _, c := range one {
		if second, _ := refusedOf(c.proposed, c.listed); !second {
			t.Errorf("%s: %+q against the listed %+q gives no second definition, want one term (TermKey %+q and %+q)",
				c.why, c.proposed, c.listed, TermKey(c.proposed), TermKey(c.listed))
		}
	}
	for _, c := range []struct{ proposed, held, why string }{
		{"\u0130nbox", "\"\u0130\" (U+0130)", "a capital I with a dot above (Turkish)"},
		{"i\u0307nbox", "\"\u0307\" (U+0307)", "an i with a combining dot above"},
	} {
		if _, name := refusedOf(c.proposed, "inbox"); name != nameDetail(c.held) {
			t.Errorf("%s: %+q against the listed inbox gives the term-name detail %q, want %q", c.why, c.proposed, name, nameDetail(c.held))
		}
	}
	two := []struct{ proposed, listed, why string }{
		{"\u0131nbox", "inbox", "a dotless ı is another letter"},
		{"heartbeats", "heartbeat", "a plural"},
		{"work-unit", "work unit", "a hyphen is no space"},
	}
	for _, c := range two {
		if second, _ := refusedOf(c.proposed, c.listed); second {
			t.Errorf("control, %s: %+q against %+q gives a second definition, want two terms", c.why, c.proposed, c.listed)
		}
	}
}

// A name that shows as a listed name is not a new term in silence (the tester's
// TestANameThatShowsAsAListedNameIsNotANewTermInSilenceAccept3). "Heаrtbeat" with a Cyrillic а
// (U+0430) shows as "Heartbeat", and its finding names that letter. The tester's control was a
// name wholly in Cyrillic ("сессия") with no finding; by the decision on that run a term name
// holds only the letters a to z, so that name is refused too, and the control here is a name in
// those letters that is no listed name.
func TestANameThatShowsAsAListedNameIsRefused(t *testing.T) {
	list := []Term{{Name: "heartbeat", Definition: "The signal that a session sends.", Where: "terms.md version 1 of ACME-1"}}
	check := func(name string) []Finding {
		return Check(planted(), Input{Proposed: []Term{{Name: name, Definition: "Another meaning of this name.", Where: where}}, List: list})
	}
	if got := check("heartbeats"); len(got) != 0 {
		t.Fatalf("control, a name of the letters a to z: Check = %#v, want no finding", got)
	}
	proposed := Term{Name: "He\u0430rtbeat", Definition: "Another meaning of this name.", Where: where}
	if got, want := check(proposed.Name), []Finding{finding(definition(proposed), "term-name", nameDetail("\"\u0430\" (U+0430)"))}; !sameFindings(got, want) {
		t.Errorf("%+q shows as Heartbeat: Check = %#v, want %#v", proposed.Name, got, want)
	}
	whole := Term{Name: "\u0441\u0435\u0441\u0441\u0438\u044f", Definition: "Another meaning of this name.", Where: where}
	if got, want := check(whole.Name), []Finding{finding(definition(whole), "term-name",
		nameDetail("\"\u0441\" (U+0441), \"\u0435\" (U+0435), \"\u0438\" (U+0438) and \"\u044f\" (U+044F)"))}; !sameFindings(got, want) {
		t.Errorf("a name wholly in Cyrillic: Check = %#v, want %#v", got, want)
	}
}

// After NFKC_Casefold, a term name holds only the letters a to z, the digits 0 to 9, spaces and
// hyphens (R50 and R49: a term is an English noun). A name with another character breaks the rule
// term-name, whose finding names each such character once, as the writer typed it, with its code
// point. The controls: full-width letters, a ligature, capitals, the sharp s, a character that
// shows nothing, a digit and a hyphen fold to those characters, and break no rule.
func TestATermNameHoldsOnlyTheLettersAToZDigitsSpacesAndHyphens(t *testing.T) {
	for _, c := range []struct{ name, held string }{
		{"\u0130nbox", "\"\u0130\" (U+0130)"},
		{"\u0131l\u0131k", "\"\u0131\" (U+0131)"},
		{"<statement>", `"<" (U+003C) and ">" (U+003E)`},
		{"limit's wait", `"'" (U+0027)`},
		{"limit\u2019s wait", "\"\u2019\" (U+2019)"},
		{"work_unit", `"_" (U+005F)`},
		{"\u03bb\u03cc\u03b3\u03bf\u03c2", "\"\u03bb\" (U+03BB), \"\u03cc\" (U+03CC), \"\u03b3\" (U+03B3), \"\u03bf\" (U+03BF) and \"\u03c2\" (U+03C2)"},
		{"work\u2010unit", "\"\u2010\" (U+2010)"},
	} {
		term := Term{Name: c.name, Definition: "One meaning of this name.", Where: where}
		if got, want := Check(planted(), Input{Proposed: []Term{term}}), []Finding{finding(definition(term), "term-name", nameDetail(c.held))}; !sameFindings(got, want) {
			t.Errorf("Check of the name %+q = %#v, want %#v", c.name, got, want)
		}
	}
	for _, name := range []string{
		"heartbeat", "work unit", "work-unit", "r49 check", "Work  Unit", "STRASSE", "stra\u00dfe",
		"\uff48\uff45\uff41\uff52\uff54\uff42\uff45\uff41\uff54", "\ufb01le", "heart\u200dbeat", "\uff11\uff12 hours",
	} {
		if got := Check(planted(), Input{Proposed: []Term{{Name: name, Definition: "One meaning of this name.", Where: where}}}); len(got) != 0 {
			t.Errorf("control, the name %+q (TermKey %+q): Check = %#v, want no finding", name, TermKey(name), got)
		}
	}
}

// A term name holds at least one letter from a to z or one digit: a term line under Terms whose
// bold name is only a character that shows nothing, or only a hyphen, breaks the rule term-name.
// Bold marks around white space alone mark no bold text, so a person reads such a line as no term
// line, and it breaks the rule term-line. The controls: a name of one letter, of a letter and a
// digit, and with a hyphen.
func TestATermNameWithNoLetterAndNoDigitIsRefused(t *testing.T) {
	const detail = "the name holds no letter from a to z and no digit. A term name holds at least one letter or digit"
	check := func(name string) (Extracted, []Finding) {
		extracted, err := ExtractDocument("spec.md", 1, "# Spec\n\n## Terms\n\n- **"+name+"**: One thing of the system.\n")
		if err != nil {
			t.Fatalf("ExtractDocument of the name %+q: %v", name, err)
		}
		return extracted, Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms})
	}
	for _, name := range []string{"\u200b", "-", "\u00ad"} {
		extracted, got := check(name)
		if len(extracted.Terms) != 1 {
			t.Fatalf("the name %+q: Terms = %#v, want one term", name, extracted.Terms)
		}
		if want := []Finding{finding(definition(extracted.Terms[0]), "term-name", detail)}; !sameFindings(got, want) {
			t.Errorf("the name %+q: Check = %#v, want %#v", name, got, want)
		}
	}
	for _, name := range []string{" ", " - "} {
		extracted, got := check(name)
		if len(extracted.Terms) != 0 || len(got) != 1 || got[0].Rule != ruleTermLine || got[0].Kind != KindLine {
			t.Errorf("the name %+q: Terms = %#v and Check = %#v, want no term and one term-line finding", name, extracted.Terms, got)
		}
	}
	for _, name := range []string{"x", "r2", "x-ray"} {
		if _, got := check(name); len(got) != 0 {
			t.Errorf("control, the name %+q: Check = %#v, want no finding", name, got)
		}
	}
}

// A covered document that names a listed term again, in the one term-line form, under a heading
// that is not Terms, is refused (the tester's TestASecondDefinitionUnderAnotherHeadingIsRefusedAccept3,
// as the tester wrote it): such a line is no definition, but a reader would take it for a second
// definition of that term. R50: "Dispatch refuses an approval request … that holds a second
// definition of a term, and it names the term". The controls: under "## Terms" the line is refused
// once, and under "## Glossary" a line that names no listed term gives no finding.
func TestASecondDefinitionUnderAnotherHeadingIsRefused(t *testing.T) {
	list := []Term{{Name: "heartbeat", Definition: "The signal that a session sends to Dispatch at a fixed interval.", Where: "terms.md version 1 of ACME-1"}}
	findingsUnder := func(heading, line string) []Finding {
		extracted, err := ExtractDocument("spec.md", 1, "# Spec\n\n"+heading+"\n\n"+line+"\n")
		if err != nil {
			t.Fatalf("ExtractDocument under %q: %v", heading, err)
		}
		return Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms, List: list})
	}
	const second = "- **heartbeat**: A pulse of a pump."
	if got := findingsUnder("## Terms", second); len(got) != 1 || got[0].Rule != ruleTermSecondDefinition {
		t.Fatalf("control, ## Terms: findings %#v, want one term-second-definition", got)
	}
	if got := findingsUnder("## Glossary", "- **Note**: Each record keeps its date."); len(got) != 0 {
		t.Fatalf("control, ## Glossary with no listed term: findings %#v, want none", got)
	}
	for _, heading := range []string{"## Terms.", "## Terms (draft)", "## Terms::", "## \uff34\uff45\uff52\uff4d\uff53", "## Glossary", "## Definitions"} {
		if got := findingsUnder(heading, second); len(got) == 0 {
			t.Errorf("under %q a second definition of the listed heartbeat gives no finding", heading)
		}
	}
}

// termLineElsewhere is the detail of a line in the form of a term line outside a heading Terms
// that names a term that where defines. Its way out for a definition is the definition there, never
// a move under Terms, which would be a second definition.
func termLineElsewhere(where string) string {
	return where + " defines this term. This line has the form of a definition, but it is not under a heading Terms. " +
		"If it is no definition, change its form. If it is a definition, use the definition there, or change that definition and request approval of its document"
}

// A definition is a term line under a heading Terms (R50). A term line in another place of a
// covered document is no definition and adds no term: under each heading, under no heading, and in
// each container that the parser reads blocks in (a block quote, a list item, a callout and a
// decision block). A reader would take it for a second definition when it names a defined term, so
// it is a finding then: a term of the list, or a term that the document itself defines under
// Terms. Its finding is of Kind term_line, names where the term is defined, and tells the writer
// to change the form of the line or to use that definition. The controls: the same line in code or
// in a table cell, a line of a name that nothing defines, and two such lines of one name that
// nothing defines, which are prose, give no finding and add no term.
func TestATermLineOutsideTermsIsNoDefinitionAndAddsNoTerm(t *testing.T) {
	list := []Term{{Name: "heartbeat", Definition: "The signal that a session sends.", Where: "terms.md version 1 of ACME-1"}}
	check := func(markdown string) (Extracted, []Finding) {
		extracted, err := ExtractDocument("spec.md", 1, markdown)
		if err != nil {
			t.Fatalf("ExtractDocument(%q): %v", markdown, err)
		}
		return extracted, Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms, List: list})
	}
	line := func(name, body string) Text {
		return Text{Node: name, Kind: "term_line", Body: body, Where: "spec.md version 1"}
	}
	pulse := line("heartbeat", "A pulse of a pump.")
	for _, markdown := range []string{
		"# Spec\n\n## Glossary\n\n- **heartbeat**: A pulse of a pump.\n",
		"- **heartbeat**: A pulse of a pump.\n",
		"# Spec\n\nSome prose.\n\n  * **Heartbeat**: A pulse of a pump.\n",
		"# Spec\n\n> - **heartbeat**: A pulse of a pump.\n",
		"# Spec\n\n- A step.\n\n  - **heartbeat**: A pulse of a pump.\n",
		"# Spec\n\n:::callout{kind=\"note\"}\n- **heartbeat**: A pulse of a pump.\n:::\n",
		"# Spec\n\n:::ask{#which urgency=\"med\" multiple=\"false\"}\nWhich pulse does the pump send?\n\n- **heartbeat**: A pulse of a pump.\n:::\n",
	} {
		extracted, got := check(markdown)
		if len(extracted.Terms) != 0 {
			t.Errorf("%q: Terms = %#v, want no term added", markdown, extracted.Terms)
		}
		if len(got) != 1 || got[0].Rule != ruleTermSecondDefinition || got[0].Kind != "term_line" || TermKey(got[0].Node) != "heartbeat" ||
			got[0].Detail != termLineElsewhere("terms.md version 1 of ACME-1") {
			t.Errorf("%q: Check = %#v, want one term-second-definition of the term_line heartbeat", markdown, got)
		}
	}
	if _, got := check("# Spec\n\n## Glossary\n\n- **heartbeat**: A pulse of a pump.\n"); !sameFindings(got, []Finding{
		finding(pulse, "term-second-definition", termLineElsewhere("terms.md version 1 of ACME-1")),
	}) {
		t.Errorf("the whole finding of a term line under Glossary = %#v", got)
	}
	for _, markdown := range []string{
		"# Spec\n\n```\n- **heartbeat**: A pulse of a pump.\n```\n",
		"# Spec\n\n:::callout{kind=\"note\"}\nThe pump sends a pulse.\n\n~~~\n- **heartbeat**: A pulse of a pump.\n~~~\n:::\n",
		"# Spec\n\n| Term | Note |\n| --- | --- |\n| **heartbeat**: A pulse of a pump. | none |\n",
		"# Spec\n\n## Glossary\n\n- **valve**: A part that opens and closes.\n",
		"# Spec\n\n## Glossary\n\n- **valve**: A part that opens.\n- **Valve**: A part that closes.\n",
		"# Spec\n\n## Steps\n\n- **Note**: Read the plan.\n\n## Checks\n\n- **Note**: Run each check.\n",
	} {
		extracted, got := check(markdown)
		if len(got) != 0 || len(extracted.Terms) != 0 {
			t.Errorf("control %q: Terms %#v and Check = %#v, want neither", markdown, extracted.Terms, got)
		}
	}
	extracted, got := check("# Spec\n\n## Terms\n\n- **valve**: A part that opens.\n\n## Glossary\n\n- **valve**: A part that closes.\n")
	if want := []Finding{finding(line("valve", "A part that closes."), "term-second-definition", termLineElsewhere("spec.md version 1"))}; !sameFindings(got, want) {
		t.Errorf("a term under Terms and again under Glossary: Check = %#v, want %#v", got, want)
	}
	if len(extracted.Terms) != 1 || extracted.Terms[0].Definition != "A part that opens." {
		t.Errorf("a term under Terms and again under Glossary: Terms = %#v, want the one under Terms", extracted.Terms)
	}
	_, got = check("# Spec\n\n## Glossary\n\n- **valve**: A part that closes.\n\n## Terms\n\n- **valve**: A part that opens.\n- **valve**: A part that shuts.\n")
	if want := []Finding{
		finding(line("valve", "A part that closes."), "term-second-definition", termLineElsewhere("spec.md version 1")),
		finding(definition(Term{Name: "valve", Definition: "A part that shuts.", Where: "spec.md version 1"}), "term-second-definition",
			"this document defines this term 2 times. Keep one definition"),
	}; !sameFindings(got, want) {
		t.Errorf("one line under Glossary and two terms under Terms: Check = %#v, want %#v", got, want)
	}
}

// A line in the form of a term line outside Terms that names a term that another document of the
// request defines under Terms (Input.Defined) is a finding that names that document, whichever
// document of the request comes first. Defined judges no term line under Terms: one request holds
// one definition of a term by the list that the earlier documents add to. The controls: the line
// with no Defined, and a term line under Terms of a name that Defined holds, give no finding.
func TestATermLineOutsideTermsThatAnotherDocumentOfTheRequestDefinesIsRefused(t *testing.T) {
	defined := []Term{{Name: "valve", Definition: "A part that opens.", Where: "verification.md version 2 of ACME-3"}}
	check := func(markdown string, defined []Term) []Finding {
		extracted, err := ExtractDocument("spec.md", 1, markdown)
		if err != nil {
			t.Fatalf("ExtractDocument(%q): %v", markdown, err)
		}
		return Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms, Defined: defined})
	}
	const glossary = "# Spec\n\n## Glossary\n\n- **Valve**: A part that closes.\n"
	want := []Finding{finding(Text{Node: "Valve", Kind: "term_line", Body: "A part that closes.", Where: "spec.md version 1"}, "term-second-definition",
		termLineElsewhere("verification.md version 2 of ACME-3"))}
	if got := check(glossary, defined); !sameFindings(got, want) {
		t.Errorf("a line under Glossary that another document of the request defines: Check = %#v, want %#v", got, want)
	}
	if got := check(glossary, nil); len(got) != 0 {
		t.Errorf("control, no Defined: Check = %#v, want no finding", got)
	}
	if got := check("# Spec\n\n## Terms\n\n- **valve**: A part that closes.\n", defined); len(got) != 0 {
		t.Errorf("control, a term line under Terms: Check = %#v, want no finding", got)
	}
}
