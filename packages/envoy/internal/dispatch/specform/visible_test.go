package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// judgedRow is one stored document: each finding of its texts as "<kind> <node> <rule>", in
// order, and, when node is set, the body of the text of that node and kind, or of the definition
// of the term of that name (kind definition).
type judgedRow struct {
	name, markdown string
	want           []string
	node           string
	kind           Kind
	body           string
}

// requireJudged stores each row's markdown as Dispatch stores a version, reads it and judges it,
// with the person ada.lovelace@example.com signed in.
func requireJudged(t *testing.T, rows []judgedRow) {
	t.Helper()
	for _, row := range rows {
		stored := storedMarkdown(t, row.markdown)
		extracted, err := ExtractDocument("spec.md", 1, stored)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if row.node != "" {
			var bodies []string
			for _, text := range extracted.Texts {
				if text.Node == row.node && text.Kind == row.kind {
					bodies = append(bodies, text.Body)
				}
			}
			for _, term := range extracted.Terms {
				if term.Name == row.node && row.kind == KindDefinition {
					bodies = append(bodies, term.Definition)
				}
			}
			if !slices.Equal(bodies, []string{row.body}) {
				t.Errorf("%s: stored %q gives the %s %s the bodies %q, want %q", row.name, stored, row.kind, row.node, bodies, row.body)
			}
		}
		got := findingNames(Check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts, Proposed: extracted.Terms}))
		if !slices.Equal(got, row.want) {
			t.Errorf("%s: stored %q gives the findings %q, want %q", row.name, stored, got, row.want)
		}
	}
}

// requirement is a requirement block of id that holds body.
func requirement(id, body string) string {
	return ":::requirement{#" + id + "}\n" + body + "\n:::\n"
}

// The rules of a sentence, the port of ste-lint and the rules of a name judge the words that a
// person reads: a link is its words, and an image is its alternative text. The reference rules
// also read each address and each title that a link or an image holds: an address names a record
// only by a reference to a record of Dispatch, the address of a pull request and an email address,
// and a title is words. A footnote that a text refers to is part of that text.
func TestTheRulesJudgeTheWordsAndTheReferenceRulesTheAddresses(t *testing.T) {
	requireJudged(t, []judgedRow{
		{"a stop in an address", requirement("dot", "Dispatch opens [the record](https://example.com/Web.Config)."),
			nil, "dot", KindStatement, "Dispatch opens the record."},
		{"a stop in another address", requirement("type", "Dispatch reads [the type](https://learn.example.com/api/System.String)."),
			nil, "type", KindStatement, "Dispatch reads the type."},
		{"a number sign in an address", requirement("guide", "Dispatch follows [the guide](https://example.com/guide#12)."),
			nil, "guide", KindStatement, "Dispatch follows the guide."},
		{"a semicolon in an address", requirement("semi", "Dispatch opens [the record](https://example.com/a;b)."),
			nil, "semi", KindStatement, "Dispatch opens the record."},
		{"an anchor of the page", requirement("part", "Dispatch follows [the part](#12)."),
			nil, "part", KindStatement, "Dispatch follows the part."},
		{"a number sign in an address that the words show", requirement("bare", "Dispatch follows https://example.com/guide#12 here."),
			nil, "bare", KindStatement, "Dispatch follows https://example.com/guide#12 here."},
		{"a name of a person in two words, one of them a link", requirement("name", "Dispatch asks [Ada](https://example.com/ada) Lovelace."),
			[]string{"statement name person"}, "name", KindStatement, "Dispatch asks Ada Lovelace."},
		{"a name of a person in two links", requirement("links", "Dispatch asks [Ada](https://example.com/ada) [Lovelace](https://example.com/lovelace)."),
			[]string{"statement links person"}, "links", KindStatement, "Dispatch asks Ada Lovelace."},
		{"the control: a link to an issue", requirement("issue", "Dispatch keeps [the issue](dispatch://ACME-12)."),
			[]string{"statement issue dispatch-link"}, "issue", KindStatement, "Dispatch keeps the issue."},
		{"the control: a link to an ask on the server", requirement("ask", "Dispatch keeps [the ask](https://dispatch.example.com/issues/ACME-3)."),
			[]string{"statement ask dispatch-link"}, "ask", KindStatement, "Dispatch keeps the ask."},
		{"the control: a number sign in the words", requirement("hash", "Legion merges each change, as #12 says."),
			[]string{"statement hash pull-request"}, "", "", ""},
		{"the address of a pull request", requirement("pr", "Legion lands [the change](https://github.com/acme/legion/pull/12)."),
			[]string{"statement pr pull-request"}, "pr", KindStatement, "Legion lands the change."},
		{"the email address of a person", requirement("mail", "Dispatch mails [the owner](mailto:ada.lovelace@example.com)."),
			[]string{"statement mail person"}, "mail", KindStatement, "Dispatch mails the owner."},
		{"an image of an issue", requirement("image", "Dispatch shows ![the issue](dispatch://ACME-12)."),
			[]string{"statement image dispatch-link"}, "image", KindStatement, "Dispatch shows the issue."},
		{"the title of an image", requirement("image-title", "Dispatch shows ![the record](https://example.com/record.png \"Ada Lovelace\")."),
			[]string{"statement image-title person"}, "image-title", KindStatement, "Dispatch shows the record."},
		{"the title of a link", requirement("link-title", "Dispatch shows [the record](https://example.com/record \"Ada Lovelace\")."),
			[]string{"statement link-title person"}, "link-title", KindStatement, "Dispatch shows the record."},
		{"a key in the title of a link", requirement("title-key", "Dispatch shows [the record](https://example.com/record \"See ACME-12\")."),
			[]string{"statement title-key dispatch-link"}, "title-key", KindStatement, "Dispatch shows the record."},
		{"a footnote that links to an issue",
			requirement("footnote", "Dispatch stores each record.[^note]") + "\n[^note]: See [the issue](dispatch://ACME-12).\n",
			[]string{"statement footnote dispatch-link"}, "footnote", KindStatement, "Dispatch stores each record.\nSee the issue."},
		{"a footnote that names a person",
			requirement("noted", "Dispatch stores each record.[^note]") + "\n[^note]: Ada Lovelace wrote it.\n",
			[]string{"statement noted person"}, "noted", KindStatement, "Dispatch stores each record.\nAda Lovelace wrote it."},
		{"the sentences of a footnote count",
			requirement("three", "Dispatch stores each record. Dispatch shows it.[^n]") + "\n[^n]: The design note says so.\n",
			[]string{"statement three sentences"}, "three", KindStatement, "Dispatch stores each record. Dispatch shows it.\nThe design note says so."},
		{"a footnote that two paragraphs refer to is read once",
			requirement("twice", "Dispatch stores each record.[^n]\n\nDispatch shows it.[^n]") + "\n[^n]: The note says so.\n",
			[]string{"statement twice sentences"}, "twice", KindStatement, "Dispatch stores each record.\nDispatch shows it.\nThe note says so."},
		// A footnote of a term is part of its definition, so its sentence counts: R50's definition
		// has one sentence, and a definition with a footnote of one more breaks definition-sentences.
		{"a footnote of a term is part of its definition, and no line of Terms",
			"## Terms\n\n- **lease**: One record of a claim.[^1]\n\n[^1]: The design note names it.\n",
			[]string{"definition lease definition-sentences"}, "lease", KindDefinition, "One record of a claim.\nThe design note names it."},
		{"a footnote of a term that links to an issue",
			"## Terms\n\n- **lease**: One record of a claim.[^1]\n\n[^1]: See [the issue](dispatch://ACME-12).\n",
			[]string{"definition lease definition-sentences", "definition lease dispatch-link"}, "lease", KindDefinition, "One record of a claim.\nSee the issue."},
		{"the name of a term that links to an issue",
			"## Terms\n\n- **[lease](dispatch://ACME-12)**: One record of a claim.\n",
			[]string{"term lease dispatch-link"}, "lease", KindDefinition, "One record of a claim."},
		{"a link in a definition",
			"## Terms\n\n- **lease**: One record of [a claim](https://example.com/Web.Config).\n",
			nil, "lease", KindDefinition, "One record of a claim."},
		{"a link in a line under Terms is named by its words",
			"## Terms\n\nSee [the issue](https://example.com/Web.Config).\n",
			[]string{`line "See the issue." term-line`}, "", "", ""},
	})
}

// The red team's two inputs of a part of Terms and a container, with their controls: a heading in
// a block quote does not close the part of Terms around the quote, and a part of Terms in a block
// quote or a callout ends with the quote or the callout.
func TestAPartOfTermsEndsWithItsContainer(t *testing.T) {
	const bad = "- **bad**: Dispatch performs a validation of each upload.\n"
	const control = ":::requirement{#control}\nDispatch stores each comment.\n:::\n\n"
	requireJudged(t, []judgedRow{
		{"a heading in a block quote in the part",
			"## Terms\n\n- **lease**: One record of a claim.\n\n> ## Example\n>\n> A note.\n\n" + bad,
			[]string{"definition bad ste100-nominalization"}, "bad", KindDefinition, "Dispatch performs a validation of each upload."},
		{"the control: the part with no block quote",
			"## Terms\n\n- **lease**: One record of a claim.\n\n" + bad,
			[]string{"definition bad ste100-nominalization"}, "", "", ""},
		{"the part in a block quote, then a paragraph",
			"> ## Terms\n>\n> - **lease**: One record of a claim.\n\nDispatch stores each record.\n",
			nil, "lease", KindDefinition, "One record of a claim."},
		{"the part in a callout, then a paragraph",
			control + ":::callout{#terms-note title=\"Terms\"}\n## Terms\n\n- **lease**: One record of a claim.\n:::\n\nDispatch stores each record.\n",
			nil, "lease", KindDefinition, "One record of a claim."},
		{"the control: a heading outside the quote closes the part",
			"> ## Terms\n>\n> - **lease**: One record of a claim.\n\n## Outside\n\nDispatch stores each record.\n",
			nil, "", "", ""},
	})
}

// quoted writes prefix before each line of text.
func quoted(prefix, text string) string {
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}

// indented is text with each line after the first indented by indent, as a list item's content.
func indented(indent, text string) string {
	return strings.ReplaceAll(text, "\n", "\n"+indent)
}

func callout(text string) string {
	return "::::callout{kind=\"note\"}\n" + text + "\n::::"
}

// sectionContainers is each container that the parser lets hold a heading, alone and nested.
var sectionContainers = []struct {
	name string
	wrap func(string) string
}{
	{"a block quote", func(text string) string { return quoted("> ", text) }},
	{"a block quote in a block quote", func(text string) string { return quoted("> > ", text) }},
	{"a bullet list item", func(text string) string { return "- " + indented("  ", text) }},
	{"an ordered list item", func(text string) string { return "1. " + indented("   ", text) }},
	{"a callout", callout},
	{"a list item in a block quote", func(text string) string { return quoted("> ", "- "+indented("  ", text)) }},
	{"a block quote in a list item", func(text string) string { return "- " + indented("  ", quoted("> ", text)) }},
	{"a callout in a block quote", func(text string) string { return quoted("> ", callout(text)) }},
	{"a block quote in a callout", func(text string) string { return callout(quoted("> ", text)) }},
}

// A section belongs to its container: a heading opens and closes one only among the blocks of its
// own container. For each kind of section that the form knows (a part of Terms, a component
// heading with its purpose, the components table), in each container that the parser lets hold a
// heading, with a heading in a container before the section, inside it and after it, the form
// reads the same texts in every container.
func TestASectionBelongsToItsContainer(t *testing.T) {
	const bad = "Dispatch performs a validation of each upload."
	const terms = "## Terms\n\n- **lease**: One record of a claim."
	const example = "## Example\n\nA note."
	const table = "| Id | Kind | Name | Purpose |\n| --- | --- | --- | --- |\n| `record` | component | Records | It performs a validation of each upload. |"
	for _, test := range []struct {
		name            string
		document        func(wrap func(string) string) string
		want, terms     []string
		names, purposes int
	}{
		{"Terms: a heading in a container inside the part",
			func(wrap func(string) string) string {
				return terms + "\n\n" + wrap(example) + "\n\n- **bad**: " + bad + "\n"
			},
			[]string{"definition bad ste100-nominalization"}, []string{"lease", "bad"}, 0, 0},
		{"Terms: the part in a container before a paragraph",
			func(wrap func(string) string) string { return wrap(terms) + "\n\nDispatch stores each record.\n" },
			nil, []string{"lease"}, 0, 0},
		{"Terms: the part in a container after the outer part ended",
			func(wrap func(string) string) string {
				return terms + "\n\n## Other\n\nDispatch stores each record.\n\n" + wrap("## Terms\n\n- **bad**: "+bad) + "\n\nDispatch keeps each record.\n"
			},
			[]string{"definition bad ste100-nominalization"}, []string{"lease", "bad"}, 0, 0},
		{"Terms: a heading in a container before the part",
			func(wrap func(string) string) string { return wrap(example) + "\n\n" + terms + "\n" },
			nil, []string{"lease"}, 0, 0},
		{"Terms: the part in a container judges a line in the container",
			func(wrap func(string) string) string {
				return wrap("## Terms\n\nA note.") + "\n\nDispatch stores each record.\n"
			},
			[]string{`line "A note." term-line`}, nil, 0, 0},
		{"a component: its heading and its purpose in a container",
			func(wrap func(string) string) string {
				return wrap("## Records (component `record`)\n\nIt stores each record.") + "\n\n" + bad + "\n"
			},
			nil, nil, 1, 1},
		{"a component: its heading at the end of a container",
			func(wrap func(string) string) string {
				return wrap("## Records (component `record`)") + "\n\n" + bad + "\n"
			},
			nil, nil, 1, 0},
		{"a component: a container between its heading and a paragraph",
			func(wrap func(string) string) string {
				return "## Records (component `record`)\n\n" + wrap("## Example\n\n"+bad) + "\n"
			},
			nil, nil, 1, 0},
		{"a component: a heading in a container before the component",
			func(wrap func(string) string) string {
				return wrap(example) + "\n\n## Records (component `record`)\n\n" + bad + "\n"
			},
			[]string{"purpose record ste100-nominalization"}, nil, 1, 1},
		{"the components table in a container",
			func(wrap func(string) string) string { return wrap(table) + "\n" },
			[]string{"purpose record ste100-nominalization"}, nil, 1, 1},
		{"the components table after a container that holds a part of Terms",
			func(wrap func(string) string) string { return wrap(terms) + "\n\n" + table + "\n" },
			[]string{"purpose record ste100-nominalization"}, []string{"lease"}, 1, 1},
		{"another table: in a part of Terms in a container, and after the container",
			func(wrap func(string) string) string {
				return wrap("## Terms\n\n| Note | Detail |\n| --- | --- |\n| One | Two |") + "\n\n| Note | Detail |\n| --- | --- |\n| Three | Four |\n"
			},
			[]string{`line "Note | Detail" term-line`, `line "One | Two" term-line`}, nil, 0, 0},
	} {
		for _, container := range sectionContainers {
			stored := storedMarkdown(t, test.document(container.wrap))
			extracted, err := ExtractDocument("spec.md", 1, stored)
			if err != nil {
				t.Fatalf("%s, in %s: %v", test.name, container.name, err)
			}
			var names []string
			for _, term := range extracted.Terms {
				names = append(names, term.Name)
			}
			got := findingNames(Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms}))
			if !slices.Equal(got, test.want) || !slices.Equal(names, test.terms) ||
				extracted.Counts[KindName] != test.names || extracted.Counts[KindPurpose] != test.purposes {
				t.Errorf("%s, in %s: stored %q gives the findings %q, the terms %q, %d names and %d purposes; want %q, %q, %d and %d",
					test.name, container.name, stored, got, names, extracted.Counts[KindName], extracted.Counts[KindPurpose],
					test.want, test.terms, test.names, test.purposes)
			}
		}
	}
}

// The texts of The Spec are a statement, the claim and the procedure of a check, the name and the
// purpose of a component and the term and the definition of a term, where plan decision 3 places
// each. Other prose of a document is no text of The Spec, and the form does not judge it: the
// reviewer of R25 reads the page. Three of the red team's inputs pin that, beside a statement
// that the form reads, and the control: the same link in a requirement is refused.
func TestOtherProseOfADocumentIsNoTextOfTheSpec(t *testing.T) {
	const control = ":::requirement{#control}\nDispatch stores each comment.\n:::\n\n"
	for _, test := range []struct {
		name, markdown string
		want           []string
	}{
		{"an ordinary paragraph with a link to an issue", control + "See [the issue](dispatch://ACME-12).\n", nil},
		{"an ordinary heading with the name of a person", control + "## Ada Lovelace\n", nil},
		{"the title of a callout with the name of a person", control + ":::callout{#note title=\"Ada Lovelace\"}\nDispatch stores each record.\n:::\n", nil},
		{"the control: the link in a requirement", control + requirement("linked", "See [the issue](dispatch://ACME-12)."), []string{"statement linked dispatch-link"}},
	} {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, test.markdown))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		got := findingNames(Check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts}))
		if !slices.Equal(got, test.want) || !slices.ContainsFunc(extracted.Texts, func(text Text) bool { return text.Node == "control" }) {
			t.Errorf("%s: the findings %q and the texts %s, want %q and the statement control read", test.name, got, fmt.Sprint(extracted.Texts), test.want)
		}
	}
}

// The reference rules judge the words exactly as the page shows them, with no mark of code font,
// while the rules of a sentence and the port of ste-lint keep code font: a name or the address of a
// record that code font splits is the name or the address that the person reads. A name or an
// address wholly in code font is refused too: code font is the way to write a name of code, and
// the page shows the person's name or the record's address all the same.
func TestTheReferenceRulesJudgeTheWordsThatThePageShows(t *testing.T) {
	rows := []judgedRow{
		{"one part of a name in code font", requirement("one", "Dispatch asks `Ada` Lovelace."),
			[]string{"statement one person"}, "one", KindStatement, "Dispatch asks `Ada` Lovelace."},
		{"both parts of a name in code font", requirement("both", "Dispatch asks `Ada` `Lovelace`."),
			[]string{"statement both person"}, "both", KindStatement, "Dispatch asks `Ada` `Lovelace`."},
		{"the middle of a name in code font", requirement("middle", "Dispatch asks A`da Love`lace."),
			[]string{"statement middle person"}, "middle", KindStatement, "Dispatch asks A`da Love`lace."},
		{"the address of a record split by code font", requirement("record", "Dispatch keeps `dispatch://AC`ME-12."),
			[]string{"statement record dispatch-link"}, "record", KindStatement, "Dispatch keeps `dispatch://AC`ME-12."},
		{"a name wholly in code font", requirement("whole", "Dispatch asks `Ada Lovelace`."),
			[]string{"statement whole person"}, "whole", KindStatement, "Dispatch asks `Ada Lovelace`."},
		{"the address of a record wholly in code font", requirement("whole-record", "Dispatch keeps `dispatch://ACME-12`."),
			[]string{"statement whole-record dispatch-link"}, "whole-record", KindStatement, "Dispatch keeps `dispatch://ACME-12`."},
		// R50 reads the name of a term as the page shows it, so code font makes no second name.
		{"a name in a term line split by code font", "## Terms\n\n- **`Ada` Lovelace**: One owner of a `request` of `Ada` Lovelace.\n",
			[]string{"term Ada Lovelace person", "definition Ada Lovelace person"}, "Ada Lovelace", KindDefinition, "One owner of a `request` of `Ada` Lovelace."},
		{"the control: the rules of a sentence keep code font", requirement("config", "Dispatch reads the file `Web.Config` at boot."),
			nil, "config", KindStatement, "Dispatch reads the file `Web.Config` at boot."},
	}
	requireJudged(t, rows)
	shown := map[string]string{
		"one": "Dispatch asks Ada Lovelace.", "both": "Dispatch asks Ada Lovelace.", "middle": "Dispatch asks Ada Lovelace.",
		"record": "Dispatch keeps dispatch://ACME-12.", "whole": "Dispatch asks Ada Lovelace.",
		"whole-record": "Dispatch keeps dispatch://ACME-12.", "config": "Dispatch reads the file Web.Config at boot.",
	}
	for _, row := range rows {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, row.markdown))
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		for _, text := range extracted.Texts {
			if want, ok := shown[text.Node]; ok && text.Held.Shown != want {
				t.Errorf("%s: the page shows %q, and the reference rules read %q", row.name, want, text.Held.Shown)
			}
		}
		for _, term := range extracted.Terms {
			if term.NameHeld.Shown != "Ada Lovelace" || term.DefinitionHeld.Shown != "One owner of a request of Ada Lovelace." {
				t.Errorf("%s: the reference rules read the name %q and the definition %q, want the words that the page shows",
					row.name, term.NameHeld.Shown, term.DefinitionHeld.Shown)
			}
		}
	}
}

// The address of a pull request has a host of its own: github.com in any case, or its www
// subdomain, which no letter, digit, dot or hyphen comes before, so github.com that ends another
// host names no pull request. A scheme and the address of a record of Dispatch have no case either.
func TestTheAddressOfAPullRequestHasAHostOfItsOwn(t *testing.T) {
	for _, test := range []struct {
		address string
		named   []string
	}{
		{"github.com/o/r/pull/1", []string{"github.com/o/r/pull/1"}},
		{"https://github.com/o/r/pull/1", []string{"github.com/o/r/pull/1"}},
		{"https://GITHUB.COM/o/r/pull/1", []string{"GITHUB.COM/o/r/pull/1"}},
		{"HTTPS://GitHub.com/o/r/pull/1", []string{"GitHub.com/o/r/pull/1"}},
		{"https://www.github.com/o/r/pull/1", []string{"www.github.com/o/r/pull/1"}},
		{"notgithub.com/o/r/pull/1", nil},
		{"https://notgithub.com/o/r/pull/1", nil},
		{"github.com.example/o/r/pull/1", nil},
		{"https://evil-github.com/o/r/pull/1", nil},
		{"https://example.com/github.com/o/r/pull/1", []string{"github.com/o/r/pull/1"}},
	} {
		var want []Finding
		text := Text{Node: "planted", Kind: KindStatement, Body: "Legion lands the change.", Where: where, Held: Held{Addresses: []string{test.address}}}
		for _, match := range test.named {
			want = append(want, finding(text, rulePullRequest, pullRequestNamed(match)))
		}
		if got := Check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("the address %q: got %#v, want %#v", test.address, got, want)
		}
	}
	requireJudged(t, []judgedRow{
		{"the address of a pull request in capitals", requirement("pr", "Dispatch shows [the change](https://GITHUB.COM/acme/repo/pull/12)."),
			[]string{"statement pr pull-request"}, "pr", KindStatement, "Dispatch shows the change."},
		{"the address of a pull request in capitals in the words", requirement("words", "Legion lands HTTPS://GitHub.com/acme/repo/pull/12 now."),
			[]string{"statement words pull-request"}, "", "", ""},
		{"the control: a host that ends in github.com", requirement("other", "Dispatch shows [the change](https://notgithub.com/acme/repo/pull/12)."),
			nil, "other", KindStatement, "Dispatch shows the change."},
		{"a scheme of Dispatch in capitals", requirement("scheme", "Dispatch keeps [the issue](DISPATCH://ACME-12)."),
			[]string{"statement scheme dispatch-link"}, "", "", ""},
		{"the address of the server in capitals", requirement("server", "Dispatch keeps [the ask](HTTPS://DISPATCH.EXAMPLE.COM/issues/ACME-3)."),
			[]string{"statement server dispatch-link"}, "", "", ""},
		{"an email address in capitals", requirement("mail", "Dispatch mails [the owner](MAILTO:ADA.LOVELACE@EXAMPLE.COM)."),
			[]string{"statement mail person"}, "", "", ""},
	})
}

// A definition can stand wholly in its footnote: the footnote is part of the definition before the
// line is read as a term line, and its words are judged as the definition.
func TestADefinitionCanStandWhollyInItsFootnote(t *testing.T) {
	requireJudged(t, []judgedRow{
		{"a definition wholly in its footnote", "## Terms\n\n- **lease**: [^a]\n\n[^a]: One record of a claim.\n",
			nil, "lease", KindDefinition, "One record of a claim."},
		{"a definition wholly in its footnote breaks a rule", "## Terms\n\n- **lease**: [^a]\n\n[^a]: Dispatch performs a validation of each upload.\n",
			[]string{"definition lease ste100-nominalization"}, "lease", KindDefinition, "Dispatch performs a validation of each upload."},
		{"the control: a term with no definition", "## Terms\n\n- **lease**:\n",
			[]string{`line "lease:" term-line`}, "", "", ""},
	})
}

// An address that the words show, such as https://example.com/Web.Config, is one word for the
// rules of a sentence, as the address of a link is no text of a sentence: no stop in it breaks
// the rule stop-no-space, and a stop after it still ends the sentence.
func TestABareAddressIsOneWordForTheRulesOfASentence(t *testing.T) {
	requireJudged(t, []judgedRow{
		{"an address in a sentence", requirement("url", "Dispatch opens https://example.com/Web.Config."),
			nil, "url", KindStatement, "Dispatch opens https://example.com/Web.Config."},
		{"an address at the end of a sentence", requirement("end", "Dispatch opens https://example.com/Web.Config. Dispatch shows it."),
			nil, "end", KindStatement, "Dispatch opens https://example.com/Web.Config. Dispatch shows it."},
		{"a stop after an address ends a sentence", requirement("three", "Dispatch opens https://example.com/Web.Config. Dispatch shows it. Dispatch keeps it."),
			[]string{"statement three sentences"}, "", "", ""},
		{"an address in brackets", requirement("brackets", "Dispatch opens the record (https://example.com/Web.Config)."),
			nil, "brackets", KindStatement, "Dispatch opens the record (https://example.com/Web.Config)."},
		{"an address with a query string", requirement("query", "Dispatch opens https://example.com/Web.Config?view=Raw.Text&Page=Two."),
			nil, "query", KindStatement, "Dispatch opens https://example.com/Web.Config?view=Raw.Text&Page=Two."},
		{"an address that starts with www", requirement("www", "Dispatch opens www.example.com/Web.Config."),
			nil, "www", KindStatement, "Dispatch opens www.example.com/Web.Config."},
		{"the control: a stop between two words", requirement("joined", "Dispatch keeps each comment.Dispatch shows it."),
			[]string{"statement joined stop-no-space"}, "", "", ""},
	})
}
