package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/text"
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
					bodies = append(bodies, heldWords(text.Body, text.Notes))
				}
			}
			for _, term := range extracted.Terms {
				if term.Name == row.node && row.kind == KindDefinition {
					bodies = append(bodies, heldWords(term.Definition, term.DefinitionNotes))
				}
			}
			if !slices.Equal(bodies, []string{row.body}) {
				t.Errorf("%s: stored %q gives the %s %s the bodies %q, want %q", row.name, stored, row.kind, row.node, bodies, row.body)
			}
		}
		got := findingNames(check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts, Proposed: extracted.Terms}))
		if !slices.Equal(got, row.want) {
			t.Errorf("%s: stored %q gives the findings %q, want %q", row.name, stored, got, row.want)
		}
	}
}

// heldWords is body and the words of each footnote that notes holds, directly or in turn, once
// and in the order of the first reference, each on a line: the words of a text that a person
// reads.
func heldWords(body string, notes []*Footnote) string {
	var lines []string
	if body != "" {
		lines = append(lines, body)
	}
	seen := map[*Footnote]bool{}
	queue := slices.Clone(notes)
	for index := 0; index < len(queue); index++ {
		note := queue[index]
		if seen[note] {
			continue
		}
		seen[note] = true
		if note.Body != "" {
			lines = append(lines, note.Body)
		}
		queue = append(queue, note.Notes...)
	}
	return strings.Join(lines, "\n")
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
		{"a footnote of a term is part of its definition, and no line of Terms",
			"## Terms\n\n- **lease**: One record of a claim.[^1]\n\n[^1]: The design note names it.\n",
			nil, "lease", KindDefinition, "One record of a claim.\nThe design note names it."},
		{"a footnote of a term that links to an issue",
			"## Terms\n\n- **lease**: One record of a claim.[^1]\n\n[^1]: See [the issue](dispatch://ACME-12).\n",
			[]string{"definition lease dispatch-link"}, "lease", KindDefinition, "One record of a claim.\nSee the issue."},
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

// A link whose address is its own shown words after "mailto:" is the form in which Dispatch stores
// an email address that the words hold, so it is judged one time, in the words of the page, where
// its neighbours are. So the SSH address of a repository opens in each of its forms, in words as
// in code font, and each form of an email address of a person is refused. Each other link is
// judged in its address too, whatever stands beside it: a word joined to an autolink of a record
// or a pull request hides it from the words, and not from the address. A link whose words and
// address give the same finding gives it once.
func TestAnAutolinkIsJudgedOnceInTheWordsOfThePage(t *testing.T) {
	requireJudged(t, []judgedRow{
		{"an SSH address in words", requirement("scp", "Legion clones git@github.com:owner/repo.git for each run."),
			nil, "", "", ""},
		{"an SSH address in words, at the end", requirement("scp-end", "Legion clones each repository from git@github.com:owner/repo.git"),
			nil, "", "", ""},
		{"the URL form of an SSH address in words", requirement("ssh", "Legion clones ssh://git@github.com/acme/widgets.git for each run."),
			nil, "", "", ""},
		{"the URL form of an SSH address of a package in words", requirement("git-ssh", "Legion installs git+ssh://git@github.com/acme/widgets.git for each run."),
			nil, "", "", ""},
		{"the URL form of an SSH address in code font", requirement("ssh-code", "Legion clones `ssh://git@github.com/acme/widgets.git` for each run."),
			nil, "", "", ""},
		{"an email address in words", requirement("mail", "Dispatch mails ada@example.com each day."),
			[]string{"statement mail person"}, "", "", ""},
		{"an email address after mailto: in words", requirement("mailto", "Dispatch mails mailto:ada@example.com each day."),
			[]string{"statement mailto person"}, "", "", ""},
		{"an email address in angle brackets", requirement("angle", "Dispatch mails <ada@example.com> each day."),
			[]string{"statement angle person"}, "", "", ""},
		{"a link whose words are its address", requirement("same", "Dispatch mails [ada@example.com](mailto:ada@example.com) each day."),
			[]string{"statement same person"}, "", "", ""},
		{"an email address in brackets, then a colon", requirement("colon", "Dispatch mails the owner (ada@example.com:) each day."),
			[]string{"statement colon person"}, "", "", ""},
		{"a link whose words and address differ, read in its address", requirement("address", "Legion clones [git@github.com:owner/repo.git](mailto:git@github.com) for each run."),
			[]string{"statement address person"}, "", "", ""},
		{"a link whose words and address differ, read in its words", requirement("words", "Dispatch mails [ada@example.com](https://example.com/contact) each day."),
			[]string{"statement words person"}, "", "", ""},
		{"the control: the address of a pull request in words", requirement("pr", "Legion lands https://github.com/acme/legion/pull/12 today."),
			[]string{"statement pr pull-request"}, "", "", ""},
		{"an autolink of a record", requirement("record", "Dispatch stores each comment, as <dispatch://ACME-1> shows."),
			[]string{"statement record dispatch-link"}, "", "", ""},
		{"an autolink of a record, a word joined after it", requirement("record-after", "Dispatch stores each comment, as <dispatch://ACME-1>xyz shows."),
			[]string{"statement record-after dispatch-link"}, "", "", ""},
		{"an autolink of a record, a word joined before it", requirement("record-before", "Dispatch stores each comment, as xyz<dispatch://ACME-1> shows."),
			[]string{"statement record-before dispatch-link"}, "", "", ""},
		{"a link of a record whose words are its address, a word joined after it", requirement("words-after",
			"Dispatch stores each comment, as [dispatch://ACME-1](dispatch://ACME-1)xyz shows."), []string{"statement words-after dispatch-link"}, "", "", ""},
		{"a link of a record whose words are its address, a word joined before it", requirement("words-before",
			"Dispatch stores each comment, as xyz[dispatch://ACME-1](dispatch://ACME-1) shows."), []string{"statement words-before dispatch-link"}, "", "", ""},
		{"an autolink of a pull request, a word joined after it", requirement("pull-after", "Dispatch stores each comment, as <https://github.com/acme/widgets/pull/12>x shows."),
			[]string{"statement pull-after pull-request"}, "", "", ""},
		{"an autolink of a pull request, a word joined before it", requirement("pull-before", "Dispatch stores each comment, as x<https://github.com/acme/widgets/pull/12> shows."),
			[]string{"statement pull-before pull-request"}, "", "", ""},
		{"the stated limit: a mail link after a /, judged in the words", requirement("mail-path", "Dispatch stores each comment, as x/[ada@example.com](mailto:ada@example.com) shows."),
			nil, "", "", ""},
		{"a mail link whose words are a reference, a word joined after it", requirement("mail-reference",
			"Dispatch stores each comment, as [dispatch://ACME-1](mailto:dispatch://ACME-1)xyz shows."), []string{"statement mail-reference dispatch-link"}, "", "", ""},
		{"a mail link whose words are a key, a word joined after it", requirement("mail-key",
			"Dispatch stores each comment, as [ACME-1](mailto:ACME-1)xyz shows."), []string{"statement mail-key dispatch-link"}, "", "", ""},
		{"a mail link whose words are an email address and more", requirement("mail-more",
			"Legion clones [git@github.com:ACME-1](mailto:git@github.com:ACME-1) for each run."), []string{"statement mail-more dispatch-link"}, "", "", ""},
	})
}

// One link gives one finding: where the address of a link and the words of the page name one
// record of Dispatch, the text has one finding, the address's. The words name that record also by
// a part of what the address names: the key of its issue, or its id. That one finding names the
// address and, where the words name the record in another form, the words' form too. So an
// autolink of a record beside a closing mark, a dash or a "_" gives one finding, and two links to
// two records give two. The stated scope: a digit joined after an autolink makes the words name
// another record, so that text names two records and gives two findings.
func TestOneLinkGivesOneFinding(t *testing.T) {
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	requireJudged(t, []judgedRow{
		{"an autolink of a record in curly quotes", requirement("curly", "Dispatch stores each comment, as \u201c<dispatch://ACME-1>\u201d shows."),
			[]string{"statement curly dispatch-link"}, "", "", ""},
		{"an autolink of a record, then a dash", requirement("dash", "Dispatch stores each comment, as <dispatch://ACME-1>\u2014 it shows."),
			[]string{"statement dash dispatch-link"}, "", "", ""},
		{"an autolink of a record, then a _", requirement("connector", "Dispatch stores each comment, as <dispatch://ACME-1>_ shows."),
			[]string{"statement connector dispatch-link"}, "", "", ""},
		{"a link of a record whose words are its key", requirement("key", "Dispatch stores each comment, as [ACME-1](dispatch://ACME-1) shows."),
			[]string{"statement key dispatch-link"}, "", "", ""},
		{"a link of the spec of an issue whose words are the key of the issue", requirement("spec", "Dispatch keeps [ACME-1](dispatch://ACME-1/spec)."),
			[]string{"statement spec dispatch-link"}, "", "", ""},
		{"a link of an ask whose words are its id", requirement("ask", "Dispatch keeps ["+id+"](dispatch://ACME-1/ask/"+id+")."),
			[]string{"statement ask dispatch-link"}, "", "", ""},
		{"two links to two records", requirement("two", "Dispatch stores each comment, as <dispatch://ACME-1> and <dispatch://ACME-2> show."),
			[]string{"statement two dispatch-link", "statement two dispatch-link"}, "", "", ""},
		{"a link of the spec of an issue, then a link of the issue", requirement("spec-issue", "Dispatch keeps [the spec](dispatch://ACME-1/spec) and [the issue](dispatch://ACME-1)."),
			[]string{"statement spec-issue dispatch-link", "statement spec-issue dispatch-link"}, "", "", ""},
		{"a link of an ask of an issue, then a link of the issue", requirement("ask-issue", "Dispatch keeps [the ask](dispatch://ACME-1/ask/"+id+") and [the issue](dispatch://ACME-1)."),
			[]string{"statement ask-issue dispatch-link", "statement ask-issue dispatch-link"}, "", "", ""},
		{"a path of the spec of an issue, then a path of the issue", requirement("paths", "Dispatch keeps [the spec](/issues/ACME-1/spec) and [the issue](/issues/ACME-1)."),
			[]string{"statement paths dispatch-link", "statement paths dispatch-link"}, "", "", ""},
		{"the control: a link of the issue, then a link of its spec", requirement("issue-spec", "Dispatch keeps [the issue](dispatch://ACME-1) and [the spec](dispatch://ACME-1/spec)."),
			[]string{"statement issue-spec dispatch-link", "statement issue-spec dispatch-link"}, "", "", ""},
		{"the control: a link of the spec of one issue whose words are the key of another", requirement("other", "Dispatch keeps [ACME-2](dispatch://ACME-1/spec)."),
			[]string{"statement other dispatch-link", "statement other dispatch-link"}, "", "", ""},
		{"the stated scope: a digit after an autolink names another record", requirement("digit", "Dispatch stores each comment, as <dispatch://ACME-1>5 shows."),
			[]string{"statement digit dispatch-link", "statement digit dispatch-link"}, "", "", ""},
	})
	for _, test := range []struct{ name, body, want string }{
		{"the key of the issue of the spec", "Dispatch keeps [ACME-1](dispatch://ACME-1/spec).",
			`it links to a record of Dispatch: "dispatch://ACME-1/spec", and its words name a record of Dispatch by its key: "ACME-1"`},
		{"the id of the ask", "Dispatch keeps [" + id + "](dispatch://ACME-1/ask/" + id + ").",
			`it links to a record of Dispatch: "dispatch://ACME-1/ask/` + id + `", and its words name a record of Dispatch by its id: "` + id + `"`},
		{"the control: the words are the address", "Dispatch stores each comment, as <dispatch://ACME-1> shows.",
			`it links to a record of Dispatch: "dispatch://ACME-1"`},
	} {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, requirement("r", test.body)))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		var got []string
		for _, finding := range check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts}) {
			got = append(got, finding.Detail)
		}
		if !slices.Equal(got, []string{test.want}) {
			t.Errorf("%s: the details %q, want %q", test.name, got, []string{test.want})
		}
	}
}

// A link whose address is a path of a page of a record of Dispatch, on the server that shows the
// document, links to that record, as the whole address of the page does: an issue, a document of
// an issue, of a project or of an agent, an ask, a comment, a message and a block, as the server
// reads the address of its pages. An address with no scheme is read against the origin of the
// server, as the dashboard reads the address of a link in a component's prose, with each "\" read
// as "/". So a path that starts with "/", "./" or "../" or with none of them names the same page.
// A path of another page, and an address of another host, link to no record. A context that names
// no server still reads a path, which names one page on each server. These rows name no page of a
// document, which TestAnAddressIsReadAgainstThePageOfItsDocumentToo reads.
func TestARelativeLinkToARecordLinksToTheRecord(t *testing.T) {
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	requireJudged(t, []judgedRow{
		{"a path of a document of a project", requirement("project", "Dispatch keeps [the terms](/projects/ACME/documents/terms-md)."),
			[]string{"statement project dispatch-link"}, "", "", ""},
		{"a path of a document of a project on the server's host", requirement("host", "Dispatch keeps [the terms](//dispatch.example.com/projects/ACME/documents/terms-md)."),
			[]string{"statement host dispatch-link"}, "", "", ""},
		{"a path of the spec of an issue", requirement("spec", "Dispatch keeps [the spec](/issues/ACME-12/spec)."),
			[]string{"statement spec dispatch-link"}, "", "", ""},
		{"a path of an issue", requirement("issue", "Dispatch keeps [the issue](/issues/ACME-12)."),
			[]string{"statement issue dispatch-link"}, "", "", ""},
		{"a path of an ask", requirement("ask", "Dispatch keeps [the ask](/issues/ACME-12/asks/"+id+")."),
			[]string{"statement ask dispatch-link"}, "", "", ""},
		{"a path of a comment on a document", requirement("comment", "Dispatch keeps [the comment](/projects/ACME/documents/terms-md?comment="+id+")."),
			[]string{"statement comment dispatch-link"}, "", "", ""},
		{"a path with no / of a document of a project", requirement("bare", "Dispatch keeps [the terms](projects/ACME/documents/terms-md)."),
			[]string{"statement bare dispatch-link"}, "", "", ""},
		{"a path from ./ of a document of a project", requirement("dot", "Dispatch keeps [the terms](./projects/ACME/documents/terms-md)."),
			[]string{"statement dot dispatch-link"}, "", "", ""},
		{"a path from ../../ of a document of a project", requirement("up", "Dispatch keeps [the terms](../../projects/ACME/documents/terms-md)."),
			[]string{"statement up dispatch-link"}, "", "", ""},
		{"a path of \\ of a document of a project", requirement("back", `Dispatch keeps [the terms](\projects\ACME\documents\terms-md).`),
			[]string{"statement back dispatch-link"}, "", "", ""},
		{"a path with no / of an artifact of an agent", requirement("agent", "Dispatch keeps [the notes](agents/s-1/artifacts/notes-md)."),
			[]string{"statement agent dispatch-link"}, "", "", ""},
		{"a path with no / of an issue", requirement("bare-issue", "Dispatch keeps [the issue](issues/ACME-12)."),
			[]string{"statement bare-issue dispatch-link"}, "", "", ""},
		{"a path with no / of the spec of an issue", requirement("bare-spec", "Dispatch keeps [the spec](issues/ACME-12/spec)."),
			[]string{"statement bare-spec dispatch-link"}, "", "", ""},
		{"a path with no / of a document of an issue", requirement("bare-artifact", "Dispatch keeps [the notes](issues/ACME-12/artifacts/notes-md)."),
			[]string{"statement bare-artifact dispatch-link"}, "", "", ""},
		{"a path with no / of the decisions of an issue", requirement("bare-decisions", "Dispatch keeps [the decisions](issues/ACME-12/decisions)."),
			[]string{"statement bare-decisions dispatch-link"}, "", "", ""},
		{"a path with no / of an ask", requirement("bare-ask", "Dispatch keeps [the ask](issues/ACME-12/asks/"+id+")."),
			[]string{"statement bare-ask dispatch-link"}, "", "", ""},
		{"a path with no / of a comment", requirement("bare-comment", "Dispatch keeps [the comment](issues/ACME-12/comments/"+id+")."),
			[]string{"statement bare-comment dispatch-link"}, "", "", ""},
		{"a path with no / of a message", requirement("bare-message", "Dispatch keeps [the message](issues/ACME-12/messages/"+id+")."),
			[]string{"statement bare-message dispatch-link"}, "", "", ""},
		{"a path with no / of a comment on a document of a project", requirement("bare-project-comment",
			"Dispatch keeps [the comment](projects/ACME/documents/terms-md?comment="+id+")."), []string{"statement bare-project-comment dispatch-link"}, "", "", ""},
		{"a path with no / of a block of a document of a project", requirement("bare-block", "Dispatch keeps [the part](projects/ACME/documents/terms-md#b-x1)."),
			[]string{"statement bare-block dispatch-link"}, "", "", ""},
		{"the control: a path of a page that is no record", requirement("guide", "Dispatch keeps [the guide](/docs/guide)."),
			nil, "", "", ""},
		{"the control: a path with no / of a page that is no record", requirement("bare-guide", "Dispatch keeps [the guide](docs/guide)."),
			nil, "", "", ""},
		{"the control, with no page: a path from ../ that the origin reads as no record", requirement("near", "Dispatch keeps [the terms](../documents/terms-md)."),
			nil, "", "", ""},
		{"the control, with no page: a name of a document alone", requirement("slug", "Dispatch keeps [the terms](terms-md)."),
			nil, "", "", ""},
		{"the control: a path of a project", requirement("projects", "Dispatch keeps [the projects](/projects/ACME)."),
			nil, "", "", ""},
		{"the control: a path of a document on another host", requirement("other", "Dispatch keeps [the terms](//other.example.com/projects/ACME/documents/terms-md)."),
			nil, "", "", ""},
	})
	for _, address := range []string{"/projects/ACME/documents/terms-md", "projects/ACME/documents/terms-md"} {
		text := Text{Node: "planted", Kind: KindStatement, Body: "Dispatch keeps the terms.", Where: where, Held: Held{Addresses: []string{address}}}
		want := []Finding{finding(text, "dispatch-link", fmt.Sprintf("it links to a record of Dispatch: %q", address))}
		if got := check(Context{}, Input{Texts: []Text{text}}); !sameFindings(got, want) {
			t.Errorf("%s with no server: Check = %#v, want %#v", address, got, want)
		}
	}
}

// Each page that draws a document reads an address with no scheme against itself, as the editor
// and the browser do, and the origin reads it as the dashboard reads a component's prose. An
// address links to a record when any reading opens the page of one. The pages of a document are
// the dashboard's (text.DocumentPages): a spec is drawn on its issue's page, on the Spec tab and on
// its Artifacts page, so from a spec a document of its issue, its Verification document, its
// decisions, a tab of the issue, a block or a part of the spec itself, and the name of a document
// of the issue alone each name a record. A Verification document is drawn on its own tab and on
// its Artifacts page, so from it the name of another document of its issue and a path of the
// Artifacts route each name one; from a document of a project, another document of the project. A
// path that opens no record from any base links to none, and with no page the origin's reading
// alone decides.
func TestAnAddressIsReadAgainstThePageOfItsDocumentToo(t *testing.T) {
	spec := text.DocumentPages(text.DocumentOwner{IssueKey: "ACME-1", Slug: "spec", Primary: true})
	checks := text.DocumentPages(text.DocumentOwner{IssueKey: "ACME-1", Slug: "verification-md", Role: "verification"})
	project := text.DocumentPages(text.DocumentOwner{Project: "ACME", Slug: "notes-md"})
	for _, test := range []struct {
		pages   []string
		address string
		want    []string
	}{
		{spec, "artifacts/notes-md", []string{`it links to a record of Dispatch: "artifacts/notes-md"`}},
		{spec, "artifacts/verification-md", []string{`it links to a record of Dispatch: "artifacts/verification-md"`}},
		{spec, "decisions", []string{`it links to a record of Dispatch: "decisions"`}},
		{spec, "#b-x1", []string{`it links to a record of Dispatch: "#b-x1"`}},
		{spec, "#s-form", []string{`it links to a record of Dispatch: "#s-form"`}},
		{spec, "notes-md", []string{`it links to a record of Dispatch: "notes-md"`}},
		{spec, "terms-md", []string{`it links to a record of Dispatch: "terms-md"`}},
		{spec, "verification-md", []string{`it links to a record of Dispatch: "verification-md"`}},
		{spec, "../decisions", []string{`it links to a record of Dispatch: "../decisions"`}},
		{spec, "../spec", []string{`it links to a record of Dispatch: "../spec"`}},
		{spec, "artifact/notes-md", []string{`it links to a record of Dispatch: "artifact/notes-md"`}},
		{spec, "verification", []string{`it links to a record of Dispatch: "verification"`}},
		{spec, "../activity", []string{`it links to a record of Dispatch: "../activity"`}},
		{checks, "terms-md", []string{`it links to a record of Dispatch: "terms-md"`}},
		{checks, "artifacts/notes-md", []string{`it links to a record of Dispatch: "artifacts/notes-md"`}},
		{checks, "artifacts/terms-md", []string{`it links to a record of Dispatch: "artifacts/terms-md"`}},
		{project, "../documents/terms-md", []string{`it links to a record of Dispatch: "../documents/terms-md"`}},
		{project, "terms-md", []string{`it links to a record of Dispatch: "terms-md"`}},
		{spec, "docs/guide", []string{}},
		{spec, "../docs/guide", []string{}},
		{checks, "docs/guide", []string{}},
		{project, "docs/guide", []string{}},
		{spec, "../documents/terms-md", []string{}},
		{nil, "artifacts/notes-md", []string{}},
	} {
		context := planted()
		context.Pages = test.pages
		text := Text{Node: "planted", Kind: KindStatement, Body: "Dispatch keeps the record.", Where: where, Held: Held{Addresses: []string{test.address}}}
		got := []string{}
		for _, finding := range check(context, Input{Texts: []Text{text}}) {
			got = append(got, finding.Detail)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%q from the pages %q: the details %q, want %q", test.address, test.pages, got, test.want)
		}
	}
}

// A footnote is part of each text that refers to it, directly or through another footnote, and
// Check judges it once: each of its findings is given once, to the first text that holds it and
// whose kind the rule judges, and its words name the footnote and how many other texts refer to
// it. One footnote that breaks two rules gives two findings, and two footnotes that break one rule
// give two. The sentences of a footnote count in each statement that holds it, and the finding
// names each footnote whose sentences it counts, with how many it adds. Two verbs for one action
// (synonym-rotation) are one finding of the one text or footnote whose words hold them both, or
// with what it refers to holds them both where no one footnote of that holds them all: so a
// footnote of two such verbs gives one finding however many texts refer to it, and a verb of a
// text with its synonym in a footnote of that text gives the text's finding.
func TestAFootnoteIsJudgedOnceAndGivesEachFindingOnce(t *testing.T) {
	shared := requirement("first", "Dispatch stores each record.[^n]") + "\n" + requirement("second", "Dispatch shows each record.[^n]") +
		"\n[^n]: Ada Lovelace wrote it.\n"
	// claims is count claims that refer to the footnote [^n], and then note, its words.
	claims := func(count int, note string) string {
		var markdown strings.Builder
		for index := 1; index <= count; index++ {
			fmt.Fprintf(&markdown, ":::check{#c%d}\nDispatch keeps record %[1]d.[^n]\n:::\n\n", index)
		}
		return markdown.String() + "[^n]: " + note + "\n"
	}
	for _, test := range []struct {
		name, markdown string
		from           int
		want           []string
	}{
		{"two statements that share a footnote", shared, 0,
			[]string{`statement first person: in its footnote [^n] (1 other text also refers to it): it names a person: "Ada Lovelace"`}},
		{"the second of them alone", shared, 1,
			[]string{`statement second person: in its footnote [^n]: it names a person: "Ada Lovelace"`}},
		{"one footnote that breaks two rules", requirement("both", "Dispatch stores each record.[^n]") +
			"\n[^n]: Ada Lovelace performs a validation of each upload.\n", 0,
			[]string{`statement both ste100-nominalization: in its footnote [^n]: ASD-STE100 does not permit an action frozen into a noun: "performs a validation"`,
				`statement both person: in its footnote [^n]: it names a person: "Ada Lovelace"`}},
		{"two footnotes that break one rule", ":::check{#claim}\nDispatch keeps each record.[^a][^b]\n:::\n\n[^a]: Dispatch keeps it; it waits.\n\n[^b]: Dispatch shows it; it waits.\n", 0,
			[]string{`claim claim ste100-semicolon: in its footnote [^a]: ASD-STE100 does not permit a semicolon: ";"`,
				`claim claim ste100-semicolon: in its footnote [^b]: ASD-STE100 does not permit a semicolon: ";"`}},
		{"a footnote that a footnote refers to", requirement("chain", "Dispatch stores each record.[^a]") +
			"\n[^a]: The note[^b].\n\n[^b]: Ada Lovelace wrote it.\n", 0,
			[]string{`statement chain sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^a] adds 1 and [^b] adds 1`,
				`statement chain person: in the footnote [^b], which its footnote [^a] refers to: it names a person: "Ada Lovelace"`}},
		{"the first text is a name that the form rules do not judge", "## Records[^n] (component `record`)\n\n" +
			requirement("later", "Dispatch keeps each record.[^n]") + "\n[^n]: Dispatch keeps it; it waits.\n", 0,
			[]string{`statement later ste100-semicolon: in its footnote [^n] (1 other text also refers to it): ASD-STE100 does not permit a semicolon: ";"`}},
		{"the sentences of a shared footnote count in each statement", requirement("a", "Dispatch stores it. Dispatch shows it.[^n]") + "\n" +
			requirement("b", "Dispatch keeps it. Dispatch reads it.[^n]") + "\n[^n]: The note says so.\n", 0,
			[]string{"statement a sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^n] adds 1",
				"statement b sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^n] adds 1"}},
		{"a statement of one sentence and its footnote of two", requirement("year", "Dispatch keeps each record for one year.[^1]") +
			"\n[^1]: The year starts at the first write. It ends 365 days later.\n", 0,
			[]string{"statement year sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^1] adds 2"}},
		{"the sentences of a long chain of footnotes", requirement("long", "Dispatch stores it.[^1]") +
			"\n[^1]: One.[^2]\n\n[^2]: Two.[^3]\n\n[^3]: Three.[^4]\n\n[^4]: Four.\n", 0,
			[]string{"statement long sentences: it has at least 4 sentences with those of its footnotes, and a statement has one or two: " +
				"[^1] adds 1, [^2] adds 1 and [^3] adds 1, and Dispatch stops the count there"}},
		{"two footnotes that refer to each other", requirement("cycle", "Dispatch stores it.[^a]") +
			"\n[^a]: The note[^b].\n\n[^b]: The other note[^a].\n", 0,
			[]string{"statement cycle sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^a] adds 1 and [^b] adds 1"}},
		{"a verb of a claim and its synonym in its footnote", ":::check{#c}\nCheck the config file.[^1]\n:::\n\n[^1]: Then verify the output.\n", 0,
			[]string{`claim c ste100-synonym-rotation: with the words of its footnotes: ASD-STE100 does not permit two verbs for one action: "check" and "verify"`}},
		{"two verbs for one action in two footnotes of one claim", ":::check{#two}\nDispatch keeps each record.[^a][^b]\n:::\n\n" +
			"[^a]: It checks the size.\n\n[^b]: It confirms the type.\n", 0,
			[]string{`claim two ste100-synonym-rotation: with the words of its footnotes: ASD-STE100 does not permit two verbs for one action: "check" and "confirm"`}},
		{"a footnote of two verbs for one action, which two claims refer to", claims(2, "It checks the size and confirms the type."), 0,
			[]string{`claim c1 ste100-synonym-rotation: in its footnote [^n] (1 other text also refers to it): ASD-STE100 does not permit two verbs for one action: "confirms"`}},
		{"a footnote of two verbs for one action, which 2,000 claims refer to", claims(2_000, "It checks the size and confirms the type."), 0,
			[]string{`claim c1 ste100-synonym-rotation: in its footnote [^n] (1999 other texts also refer to it): ASD-STE100 does not permit two verbs for one action: "confirms"`}},
		{"two verbs for one action in a footnote and the footnote that it refers to", claims(2, "It checks the size.[^b]") + "\n[^b]: It confirms the type.\n", 0,
			[]string{`claim c1 ste100-synonym-rotation: in its footnote [^n] (1 other text also refers to it): with the words of the footnotes that it refers to: ASD-STE100 does not permit two verbs for one action: "check" and "confirm"`}},
		{"two verbs for one action in two footnotes that refer to each other", claims(2, "It checks the size.[^b]") + "\n[^b]: It confirms the type.[^n]\n", 0,
			[]string{`claim c1 ste100-synonym-rotation: in its footnote [^n] (1 other text also refers to it): with the words of the footnotes that it refers to: ASD-STE100 does not permit two verbs for one action: "check" and "confirm"`}},
		{"two verbs for one action in a loop of footnotes that a footnote with no verb refers to", ":::check{#ring}\nDispatch keeps it.[^a]\n:::\n\n" +
			"[^a]: The note.[^n]\n\n[^n]: It checks the size.[^b]\n\n[^b]: It confirms the type.[^n]\n", 0,
			[]string{`claim ring ste100-synonym-rotation: in the footnote [^n], which its footnote [^a] refers to: with the words of the footnotes that it refers to: ASD-STE100 does not permit two verbs for one action: "check" and "confirm"`}},
		{"two verbs for one action in a footnote that a footnote with no verb refers to", ":::check{#top}\nDispatch keeps it.[^a]\n:::\n\n" +
			"[^a]: The note.[^b]\n\n[^b]: It checks the size.[^c]\n\n[^c]: It confirms the type.\n", 0,
			[]string{`claim top ste100-synonym-rotation: in the footnote [^b], which its footnote [^a] refers to: with the words of the footnotes that it refers to: ASD-STE100 does not permit two verbs for one action: "check" and "confirm"`}},
		{"a claim that adds a third verb for one action to its footnote's two", ":::check{#more}\nValidate the file.[^n]\n:::\n\n[^n]: It checks the size and confirms the type.\n", 0,
			[]string{`claim more ste100-synonym-rotation: with the words of its footnotes: ASD-STE100 does not permit two verbs for one action: "check", "confirm" and "validate"`,
				`claim more ste100-synonym-rotation: in its footnote [^n]: ASD-STE100 does not permit two verbs for one action: "confirms"`}},
		{"the control: a verb of a claim that its footnote's two verbs for one action hold", ":::check{#again}\nCheck the file.[^n]\n:::\n\n[^n]: It checks the size and confirms the type.\n", 0,
			[]string{`claim again ste100-synonym-rotation: in its footnote [^n]: ASD-STE100 does not permit two verbs for one action: "confirms"`}},
		{"the control: one verb in a claim and in its footnote", ":::check{#same}\nCheck the config file.[^1]\n:::\n\n[^1]: Then check the output.\n", 0, nil},
		{"the control: two verbs for one action in the claim alone", ":::check{#own}\nCheck the file and verify it.[^1]\n:::\n\n[^1]: The note says so.\n", 0,
			[]string{`claim own ste100-synonym-rotation: ASD-STE100 does not permit two verbs for one action: "verify"`}},
		{"the control: two statements that share a clean footnote", requirement("c1", "Dispatch stores it.[^n]") + "\n" +
			requirement("c2", "Dispatch shows it.[^n]") + "\n[^n]: The note says so.\n", 0, nil},
	} {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, test.markdown))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		var got []string
		for _, finding := range check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts[test.from:], Proposed: extracted.Terms}) {
			got = append(got, fmt.Sprintf("%s %s %s: %s", finding.Kind, finding.Node, finding.Rule, finding.Detail))
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: the findings\n%q\nwant\n%q", test.name, got, test.want)
		}
	}
}

// A reference reads the footnote that the parser links to it: the first whose label it writes, or
// else the first whose label matches it up to case (pmdoc.FootnoteLabelKey). The browser editor's
// parser links it to the first that matches up to case, so the reference also reads each other
// footnote whose label matches its own up to case, a second footnote of its own label among them,
// and no such definition goes unread: each holds the next one of its label up to case. The
// sentences of each count in a statement that refers to it, and its findings stand under the
// first text that refers to it. The control: a footnote that no reference reads, whatever its
// label, is read by no text, as no other prose outside a block is.
func TestAReferenceReadsEachFootnoteWhoseLabelMatchesItUpToCase(t *testing.T) {
	const ada = `it names a person: "Ada Lovelace"`
	const twelve = `it names a pull request: "#12". ` + pullRequestForms
	for _, test := range []struct {
		name, markdown string
		linked         string
		held           []string
		want           []string
	}{
		{"the review's probe: [^A] under [^a] and [^A]", requirement("r", "Dispatch keeps it.[^A]") +
			"\n[^a]: Dispatch keeps it.\n\n[^A]: Ada Lovelace approved it in #12. It runs. It stops. It ends.\n",
			"A", []string{"A", "a"}, []string{
				"statement r sentences: it has at least 5 sentences with those of its footnotes, and a statement has one or two: [^A] adds 4, and Dispatch stops the count there",
				"statement r pull-request: in its footnote [^A]: " + twelve,
				"statement r person: in its footnote [^A]: " + ada,
			}},
		{"[^a] under [^a] and [^A]", requirement("r", "Dispatch keeps it.[^a]") +
			"\n[^a]: Dispatch keeps it.\n\n[^A]: Ada Lovelace approved it in #12.\n",
			"a", []string{"a", "A"}, []string{
				"statement r sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^a] adds 1 and [^A] adds 1",
				"statement r pull-request: in the footnote [^A], which has the label of its footnote [^a] up to case: " + twelve,
				"statement r person: in the footnote [^A], which has the label of its footnote [^a] up to case: " + ada,
			}},
		{"[^A] under [^a] alone", requirement("r", "Dispatch keeps it.[^A]") + "\n[^a]: Ada Lovelace approved it.\n",
			"a", []string{"a"}, []string{"statement r person: in its footnote [^a]: " + ada}},
		{"[^a] under two definitions of [^a]", requirement("r", "Dispatch keeps it.[^a]") +
			"\n[^a]: Dispatch keeps it.\n\n[^a]: Ada Lovelace approved it.\n",
			"a", []string{"a", "a"}, []string{
				"statement r sentences: it has 3 sentences with those of its footnotes, and a statement has one or two: [^a] adds 1 and [^a] adds 1",
				"statement r person: in the footnote [^a], which has the label of its footnote [^a] up to case: " + ada,
			}},
		{"the control: a footnote of another label that no reference reads", requirement("r", "Dispatch keeps it.[^a]") +
			"\n[^a]: Dispatch keeps it.\n\n[^z]: Ada Lovelace approved it.\n",
			"a", []string{"a"}, nil},
	} {
		extracted, err := ExtractDocument("spec.md", 1, storedMarkdown(t, test.markdown))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		statement := extracted.Texts[0]
		if len(statement.Notes) != 1 || statement.Notes[0].Label != test.linked {
			t.Errorf("%s: the reference reads %d footnotes, want the one that the parser links, [^%s]", test.name, len(statement.Notes), test.linked)
		}
		// held is each footnote that the statement holds, in the order of a walk from it, once.
		var held []string
		seen := map[*Footnote]bool{}
		for walk := slices.Clone(statement.Notes); len(walk) > 0; walk = walk[1:] {
			if note := walk[0]; !seen[note] {
				seen[note] = true
				held = append(held, note.Label)
				walk = append(walk, note.Notes...)
			}
		}
		if !slices.Equal(held, test.held) {
			t.Errorf("%s: the statement holds the footnotes %q, want %q", test.name, held, test.held)
		}
		var got []string
		for _, finding := range check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts}) {
			got = append(got, fmt.Sprintf("%s %s %s: %s", finding.Kind, finding.Node, finding.Rule, finding.Detail))
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: the findings\n%q\nwant\n%q", test.name, got, test.want)
		}
	}
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
			got := findingNames(check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms}))
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
// each. Other prose of a document is no text of The Spec, and the form does not judge it: R49
// leaves it to the reviewer of R25. Three of the red team's inputs pin that, beside a statement
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
		got := findingNames(check(planted("ada.lovelace@example.com"), Input{Texts: extracted.Texts}))
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
		{"a name in a term line split by code font", "## Terms\n\n- **`Ada` Lovelace**: One owner of a `request` of `Ada` Lovelace.\n",
			[]string{"term `Ada` Lovelace person", "definition `Ada` Lovelace person"}, "`Ada` Lovelace", KindDefinition, "One owner of a `request` of `Ada` Lovelace."},
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
		if got := check(planted(), Input{Texts: []Text{text}}); !sameFindings(got, want) {
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
