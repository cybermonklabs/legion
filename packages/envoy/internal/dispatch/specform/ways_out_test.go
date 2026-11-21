package specform

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// noted is a text of kind with body that refers to one footnote, labelled 1, whose body is note.
func noted(kind Kind, body, note string) Text {
	return Text{Node: "planted", Kind: kind, Body: body, Where: where, Notes: []*Footnote{{Label: "1", Body: note}}}
}

// wayRow is one input of the tests of the ways out, and where the writer who follows a finding of
// it changes the words (withWayOut): in the footnote labelled note, which its texts share, or in
// the words of each text where note is "". A row with markdown is the texts of that document as
// ExtractDocument reads them, the words that the page shows (Held.Shown) among them, and a way out
// changes the markdown, which is read again.
type wayRow struct {
	texts    []Text
	note     string
	markdown string
}

// own is a row of texts whose findings stand in their own words.
func own(texts ...Text) wayRow { return wayRow{texts: texts} }

// inNote is a row of texts whose findings stand in the footnote labelled label.
func inNote(label string, texts ...Text) wayRow { return wayRow{texts: texts, note: label} }

// document is a row of the texts of markdown.
func document(markdown string) wayRow { return wayRow{markdown: markdown} }

// read is the texts of the row.
func (row wayRow) read(t *testing.T) []Text {
	t.Helper()
	if row.markdown == "" {
		return row.texts
	}
	extracted, err := ExtractDocument("spec.md", 1, row.markdown)
	if err != nil {
		t.Fatal(err)
	}
	return extracted.Texts
}

// String is the row for a message: its markdown, or each text with the bodies of its footnotes.
func (row wayRow) String() string {
	if row.markdown != "" {
		return row.markdown
	}
	var texts []string
	for _, text := range row.texts {
		said := text.Body
		for _, note := range text.Notes {
			said += fmt.Sprintf(" [^%s]: %s", note.Label, note.Body)
		}
		texts = append(texts, said)
	}
	return strings.Join(texts, " | ")
}

// withWayOut is the texts of row with way applied at each place of the words that row's findings
// stand in, as a writer who follows the finding changes the words wherever they stand: in the
// markdown, which is read again; in the one footnote that the texts share, which stays one
// footnote of each; or in the words of each text. It reports false where those words do not hold
// way.from.
func withWayOut(t *testing.T, row wayRow, way wayOut) ([]Text, bool) {
	t.Helper()
	switch {
	case row.markdown != "":
		changed := wayRow{markdown: strings.ReplaceAll(row.markdown, way.from, way.to)}
		return changed.read(t), strings.Contains(row.markdown, way.from)
	case row.note == "":
		changed, found := make([]Text, len(row.texts)), false
		for index, text := range row.texts {
			found = found || strings.Contains(text.Body, way.from)
			text.Body = strings.ReplaceAll(text.Body, way.from, way.to)
			changed[index] = text
		}
		return changed, found
	}
	copies := map[*Footnote]*Footnote{}
	changed, found := make([]Text, len(row.texts)), false
	for index, text := range row.texts {
		text.Notes = slices.Clone(text.Notes)
		for at, note := range text.Notes {
			if note.Label != row.note {
				continue
			}
			if copies[note] == nil {
				copied := *note
				found = found || strings.Contains(note.Body, way.from)
				copied.Body = strings.ReplaceAll(note.Body, way.from, way.to)
				copies[note] = &copied
			}
			text.Notes[at] = copies[note]
		}
		changed[index] = text
	}
	return changed, found
}

// judgedAll is each finding of each text of texts with its ways out, as a route judges a document:
// one footnote judge for the texts, each text judged in turn.
func judgedAll(c Context, texts []Text) []broken {
	people := peopleNames(c, Input{Texts: texts})
	notes := newFootnoteJudge(c, people, texts)
	var found []broken
	for _, text := range texts {
		found = append(found, breaks(c, people, text, notes)...)
	}
	return found
}

// procedureOf is a procedure of one check whose run is body.
func procedureOf(body string) Text {
	return Text{Node: "v-run", Kind: KindProcedure, Body: body, Where: where}
}

// claimOf is a claim of one check whose words are body.
func claimOf(body string) Text {
	return Text{Node: "c-claim", Kind: KindClaim, Body: body, Where: where}
}

// claimNamed is a claim of the check node whose words are body.
func claimNamed(node, body string) Text {
	return Text{Node: node, Kind: KindClaim, Body: body, Where: where}
}

// quotedRun is a statement whose two sentences ste-lint reads as one piece of 23 words, as no
// piece ends at a stop that a closing quote follows: the user and the host apart make 21 words of
// the second sentence, and a piece of 26.
const quotedRun = `The guide says "Run it." The job copies the report to deploy@build.example.com: each night after the tests of the merge pass on main.`

// statementOf is a statement named node whose body is body, referring to notes.
func statementOf(node, body string, notes ...*Footnote) Text {
	return Text{Node: node, Kind: KindStatement, Body: body, Where: where, Notes: notes}
}

// sharedNote is the footnote that the rows of one footnote under two statements share: a stop with
// no space after it, which a space would turn into a sentence of each statement that refers to it.
func sharedNote() *Footnote { return &Footnote{Label: "1", Body: "It reads Web.Config at boot."} }

// codedStatement is a statement of one sentence of 23 words that runs a command in code font that
// holds a stop and a space, and names a person. The page shows the code font's words with no mark
// (Held.Shown), where the stop and the space end a sentence; the rules of the form read one.
const codedStatement = "After the night build of the main branch the job runs `make test. all` and mails ada@example.com the log and the list of tests that failed."

// Each way out that a finding gives is a form that every rule takes. For each finding of each row
// below, the test applies each of its ways out, as the change of the text that the finding's ways
// out are written from (wayOut), at each place of the finding's words (withWayOut), and judges the
// texts that result by every rule (Check): they must give no finding that the texts do not give,
// by its rule and its detail, word for word, and fewer findings of the finding's own rule. A
// finding's detail is the rule's words at its place, and the ways out that it gives are no part
// of it (Finding.Ways), so a way out that changes only what another finding offers gives no new
// finding, as in the row of file.Name and file.Size. So no refusal suggests a form that a rule
// refuses again, as the condition's first way out once did ("Before you plan, file an issue or
// post a finding. Then run the search." was refused like the sentence it replaced), nor one that
// another rule refuses, as "that" before the clause of an instruction of 20 words, which makes it
// 21, or a space after the stop of a statement of two sentences, which makes it three. Each row
// gives at least one way out, so a finding that stops giving any fails here too. The rows after the
// first sixteen are the forms whose ways out another rule refuses, each with the way out that is
// left: in the sentence as the rules read it, code font and all, also for a text that the page
// shows with no code font; at each place of one finding, in two sentences and in one; in a
// footnote, with the sentences of the largest statement that refers to it; and in the whole text,
// for the rules that read more than one sentence (wholeText): code font that takes a verb of a
// synonym group away from its first place, a piece of ste-lint that runs on past a closing quote,
// and a footnote whose verbs stand beside those of the text that holds it.
func TestEachWayOutOfARefusalPasses(t *testing.T) {
	for _, row := range []wayRow{
		own(procedureOf("Read each open approval request and record its version.")),
		own(procedureOf("Read each open approval request and record the document, its version, its author and the reason that the request gives for approval.")),
		own(procedureOf("Read the log, record its size.")),
		own(procedureOf("Run the test, then read the log.")),
		own(procedureOf("If the run fails, read the log and send the report.")),
		own(procedureOf("Run `go test` and read the log.")),
		own(procedureOf("Read the log and run `go test ./...` on it.")),
		own(procedureOf("Before you plan, file an issue or post a finding, run the search.")),
		own(procedureOf("After the run ends, read the log, record the time and send the report.")),
		own(procedureOf("Run the test of the step, name `lint`, with the race detector.")),
		own(procedureOf("Compare the input and output the tools write.")),
		own(procedureOf("Read the log and record the date, then send the report and close the issue.")),
		own(statement("Dispatch mails ada@example.com about each change.")),
		own(statement("Copy the report with scp report.txt deploy@build.example.com: to the host.")),
		own(statement("The service reads Web.Config at its start.")),
		own(statement("Dispatch writes each comment.Dispatch reads it later.")),
		own(procedureOf("Compare the input and output the tools write for each run of the merge test against the fixture server today.")),
		own(procedureOf("Read each open approval request of the root and record its version in the table of the review for today.")),
		own(procedureOf("Before you plan the work of the week, file an issue or post a finding in the stream, run the search.")),
		own(procedureOf("Read the diff or run the tests.")),
		own(procedureOf("Read the log and, if it fails, send the report.")),
		own(statement("The service reads Web.Config at its start. It logs each read.")),
		own(statement("Copy the report with scp report.txt deploy@build.example.com: to the host of the build each night after the tests of the merge pass today.")),
		// Two findings of two rules in one sentence: each way out leaves the other finding as it
		// was, so each is offered.
		own(statement("Dispatch mails ada@example.com about Web.Config.")),
		// Two findings of one rule in one sentence: the code font of one gives the other the space
		// too, which changes only what that finding offers, so it is no new finding.
		own(statement("The parser reads file.Name and file.Size at its start.")),
		// The sentence as the rules read it, with what code font holds: a span of code that holds a
		// stop and a space ends no sentence, so the user and the host apart, which make 22 words of
		// an instruction, are no way out; and in a statement of two sentences, the name in code font
		// is still a way out of Web.Config.
		own(procedureOf("Send the log of `make test. all` to deploy@build.example.com after each run of the nightly merge test of the week every day.")),
		own(procedureOf("Send the log of `git commit -m 'Fix it. Row added'` to deploy@build.example.com after each run of the nightly merge test of the week every day.")),
		own(statement("Dispatch reads `one. two` and Web.Config at boot. It logs each read.")),
		// The same, for a statement and a footnote that a document holds, which the page shows with
		// no code font: the user and the host apart make the sentence 26 words, so they are no way
		// out, though the words that the page shows end a sentence at the stop in the code.
		document(":::requirement{#honest}\n" + codedStatement + "\n:::\n"),
		document(":::requirement{#honest}\nDispatch keeps each record.[^1]\n:::\n\n[^1]: " + codedStatement + "\n"),
		// One finding at two places, whose way out the writer takes at both: the user and the host
		// apart make the second sentence 28 words, so they are no way out, in either order of the
		// two sentences.
		own(statement("Dispatch mails ada@example.com each day. The report of the night build goes to ada@example.com with the log of the build and the list of the tests that failed last night.")),
		own(statement("The report of the night build goes to ada@example.com with the log of the build and the list of the tests that failed last night. Dispatch mails ada@example.com each day.")),
		own(statement("The build copies the report with scp to deploy@build.example.com: each night. " +
			"The build server keeps each copy of the report for a week or two at most, and the release job copies it to deploy@build.example.com: again.")),
		// One finding at two places of one sentence: the user and the host apart, written at both,
		// add six words, which make 28 and 26.
		own(statement("Copy the report to ada@example.com and the log to ada@example.com after each build of the night and each merge of the day.")),
		own(statement("The job copies the report to deploy@build.example.com: and the build copies the log to deploy@build.example.com: each night after the tests.")),
		// A footnote's sentences count in the statement that refers to it, so a space after the stop
		// of a footnote of one sentence, under a statement of one or two, makes three or four.
		inNote("1", noted(KindStatement, "Dispatch keeps each record.", "It reads Web.Config at boot.")),
		inNote("1", noted(KindStatement, "Dispatch keeps each record. It reads them at boot.", "It reads Web.Config at boot.")),
		// One footnote under two statements, of no sentence and of one, in both orders: its ways out
		// are read in the larger, where the space makes three sentences.
		func() wayRow {
			note := sharedNote()
			return inNote("1", statementOf("a", "`Dispatch`.", note), statementOf("b", "Dispatch keeps each record.", note))
		}(),
		func() wayRow {
			note := sharedNote()
			return inNote("1", statementOf("b", "Dispatch keeps each record.", note), statementOf("a", "`Dispatch`.", note))
		}(),
		// The rules that read the whole text: the code font of Foo.Validate takes "Validate" away
		// from the first place of its verb, so the text's finding of two verbs for one action
		// quotes "validated", and only the space is left; the user and the host apart make a piece
		// of 26 words of two sentences that ste-lint joins at the closing quote, so only the paths
		// are left; and the code font of a footnote's Foo.Validate takes its verb away beside a
		// text that uses "check", which leaves no group of two verbs, and beside one that uses
		// "confirms", which gives the text a new finding, so there only the space is left; but a
		// text that uses "confirms" and refers to no footnote gives no finding with it, so beside
		// such a text both are left.
		own(claimOf("Dispatch checks Foo.Validate at boot. It validated the file.")),
		own(claimOf("Dispatch removes Foo.Delete at boot. It deleted the file.")),
		own(statement(quotedRun)),
		inNote("1", noted(KindStatement, "Dispatch checks each record.", "It calls Foo.Validate at boot.")),
		inNote("1", noted(KindClaim, "Dispatch confirms each record.", "It checks Foo.Validate at boot.")),
		inNote("1", noted(KindClaim, "Dispatch keeps each record.", "It checks Foo.Validate at boot."), claimNamed("c-other", "Dispatch confirms the log.")),
	} {
		c := planted()
		texts := row.read(t)
		count := func(texts []Text) map[findingKey]int {
			found := map[findingKey]int{}
			for _, each := range Check(c, Input{Texts: texts}) {
				found[findingKey{each.Rule, each.Detail}]++
			}
			return found
		}
		before := count(texts)
		offered := 0
		for _, each := range judgedAll(c, texts) {
			for _, way := range each.ways {
				if way == (wayOut{}) {
					continue
				}
				offered++
				changed, ok := withWayOut(t, row, way)
				if !ok {
					t.Errorf("%q: the way out of %s changes %q, which the words of its finding do not hold", row, each.rule, way.from)
					continue
				}
				ownBefore, ownAfter := 0, 0
				for key, times := range count(changed) {
					if times > before[key] {
						t.Errorf("%q: the way out %q to %q of %s gives a finding that the text does not give: %s: %s", row, way.from, way.to, each.rule, key.rule, key.detail)
					}
					if key.rule == each.rule {
						ownAfter += times
					}
				}
				for key, times := range before {
					if key.rule == each.rule {
						ownBefore += times
					}
				}
				if ownAfter >= ownBefore {
					t.Errorf("%q: the way out %q to %q of %s is refused again by its own rule", row, way.from, way.to, each.rule)
				}
			}
		}
		if offered == 0 {
			t.Errorf("%q: no finding gives a way out", row)
		}
	}
}

// A way out stays where it leaves the findings of the other rules as they were: one sentence that
// names a person and holds a stop with no space after it gives each finding every way out. And a
// way out goes where another rule refuses its text: in a statement of two sentences, a space after
// the stop makes three, so that finding gives the code font alone; in a sentence of 23 words, the
// user and the host apart make 26, so the person's finding gives the two paths alone. The judge
// reads the sentence that the rules read, with its code font, so a stop and a space in code font
// change no way out, also in a text that the page shows with no code font. It reads each place of
// a finding, so an address in a short sentence and in one of 25 words gives the two paths alone,
// as does an address at two places of one sentence of 22 words, and an address at two places that
// each way out keeps in bounds gives all three. It reads a footnote's sentences with those of the
// largest statement that refers to it, so a space in a footnote of one sentence under a statement
// of one is no way out, in either order of a statement of one and one of no sentence, while under
// a claim, or under a statement whose own words are code font alone, it is. It reads the rules
// that read more than one sentence in the whole text (wholeText): the code font of Foo.Validate,
// beside "validated" in the next sentence, is no way out, so a statement of the two sentences
// gives none, and a claim the space alone, while beside "reads" it is; the user and the host apart
// are no way out where ste-lint joins their sentence to one before a closing quote, and are one
// where no quote joins them; a way out of a list item of two sentences that ends in "and" changes
// the item that the finding of dangling-conjunction quotes, so it gives none, while the item that
// ends in a stop gives both; and the code font of a footnote's Foo.Validate is a way out beside a
// text that uses "keeps" or "checks", and none beside one that uses "confirms", while a text that
// uses "confirms" and refers to no footnote, beside one that uses "keeps" and refers to it, leaves
// it a way out: no text that holds the footnote gives a new finding, though "check" and "confirm"
// stand in the judgment after it.
func TestAWayOutStaysWhereTheOtherFindingsStayAsTheyWere(t *testing.T) {
	for _, test := range []struct {
		row  wayRow
		ways map[Rule]int
	}{
		{own(statement("Dispatch mails ada@example.com about Web.Config.")), map[Rule]int{rulePerson: 3, ruleStopNoSpace: 2}},
		{own(statement("Dispatch mails ada@example.com about Web.Config. It mails her once.")), map[Rule]int{rulePerson: 3, ruleStopNoSpace: 1}},
		{own(statement("Copy the report with scp report.txt deploy@build.example.com: to the host of the build each night after the tests of the merge pass today.")),
			map[Rule]int{rulePerson: 2}},
		{own(statement("Dispatch reads `one. two` and Web.Config at boot. It logs each read.")), map[Rule]int{ruleStopNoSpace: 1}},
		{own(procedureOf("Send the log of `make test. all` to deploy@build.example.com after each run of the nightly merge test of the week every day.")),
			map[Rule]int{rulePerson: 2}},
		{own(procedureOf("Send the log of `make test all` to deploy@build.example.com after each run of the nightly merge test of the week every day.")),
			map[Rule]int{rulePerson: 2}},
		{document(":::requirement{#honest}\n" + codedStatement + "\n:::\n"), map[Rule]int{rulePerson: 2}},
		{document(":::requirement{#honest}\nDispatch keeps each record.[^1]\n:::\n\n[^1]: " + codedStatement + "\n"), map[Rule]int{rulePerson: 2}},
		{own(statement("Dispatch mails ada@example.com each day. The report of the night build goes to ada@example.com with the log of the build and the list of the tests that failed last night.")),
			map[Rule]int{rulePerson: 2}},
		{own(statement("Dispatch mails ada@example.com each day. It mails ada@example.com at night.")), map[Rule]int{rulePerson: 3}},
		{own(statement("Copy the report to ada@example.com and the log to ada@example.com after each build of the night and each merge of the day.")),
			map[Rule]int{rulePerson: 2}},
		{own(statement("Copy the report to ada@example.com and the log to ada@example.com after each build.")), map[Rule]int{rulePerson: 3}},
		{inNote("1", noted(KindStatement, "Dispatch keeps each record.", "It reads Web.Config at boot.")), map[Rule]int{ruleStopNoSpace: 1}},
		{inNote("1", noted(KindStatement, "Dispatch keeps each record.", "It mails ada@example.com at boot.")), map[Rule]int{rulePerson: 3}},
		{inNote("1", noted(KindClaim, "Dispatch keeps each record.", "It reads Web.Config at boot.")), map[Rule]int{ruleStopNoSpace: 2}},
		{inNote("1", noted(KindStatement, "`Dispatch`.", "It reads Web.Config at boot.")), map[Rule]int{ruleStopNoSpace: 2}},
		{func() wayRow {
			note := sharedNote()
			return inNote("1", statementOf("a", "`Dispatch`.", note), statementOf("b", "Dispatch keeps each record.", note))
		}(), map[Rule]int{ruleStopNoSpace: 1}},
		{func() wayRow {
			note := sharedNote()
			return inNote("1", statementOf("b", "Dispatch keeps each record.", note), statementOf("a", "`Dispatch`.", note))
		}(), map[Rule]int{ruleStopNoSpace: 1}},
		{own(statement("Dispatch checks Foo.Validate at boot. It validated the file.")), map[Rule]int{}},
		{own(statement("Dispatch removes Foo.Delete at boot. It deleted the file.")), map[Rule]int{}},
		{own(claimOf("Dispatch checks Foo.Validate at boot. It validated the file.")), map[Rule]int{ruleStopNoSpace: 1}},
		{own(claimOf("Dispatch checks Foo.Validate at boot. It reads the file.")), map[Rule]int{ruleStopNoSpace: 2}},
		{own(claimOf("Dispatch checks Foo.Validate at boot.\u2028It validated the file.")), map[Rule]int{ruleStopNoSpace: 1}},
		{own(statement(quotedRun)), map[Rule]int{rulePerson: 2}},
		{own(statement("The guide says to run it. The job copies the report to deploy@build.example.com: each night after the tests of the merge pass on main.")),
			map[Rule]int{rulePerson: 3}},
		{own(claimOf("- Dispatch reads Web.Config. It keeps the record and")), map[Rule]int{}},
		{own(claimOf("- Dispatch reads Web.Config. It keeps the record.")), map[Rule]int{ruleStopNoSpace: 2}},
		{inNote("1", noted(KindClaim, "Dispatch keeps each record.", "It checks Foo.Validate at boot.")), map[Rule]int{ruleStopNoSpace: 2}},
		{inNote("1", noted(KindStatement, "Dispatch checks each record.", "It calls Foo.Validate at boot.")), map[Rule]int{ruleStopNoSpace: 1}},
		{inNote("1", noted(KindClaim, "Dispatch confirms each record.", "It checks Foo.Validate at boot.")), map[Rule]int{ruleStopNoSpace: 1}},
		{inNote("1", noted(KindClaim, "Dispatch keeps each record.", "It checks Foo.Validate at boot."), claimNamed("c-other", "Dispatch confirms the log.")),
			map[Rule]int{ruleStopNoSpace: 2}},
	} {
		got := map[Rule]int{}
		for _, each := range judgedAll(planted(), test.row.read(t)) {
			for _, way := range each.ways {
				if way != (wayOut{}) {
					got[each.rule]++
				}
			}
		}
		if !maps.Equal(got, test.ways) {
			t.Errorf("%q: the ways out offered of each rule are %v, want %v", test.row, got, test.ways)
		}
	}
}

// The judge offers a way out only where it leaves fewer findings of the finding's own rule: a
// change that keeps the finding where it stands, and gives no new one, is no way out. Each way out
// that a rule writes today takes its finding away at each place, so these ways are the judge's
// own: one that adds words before the finding, and one that adds a space after it. Control: each
// way out that the rule stop-no-space writes passes.
func TestAWayOutThatKeepsItsFindingIsNoWayOut(t *testing.T) {
	const line = "The service reads Web.Config at its start."
	judge := newWayJudge(planted(), nil, KindStatement, false)
	sentences := sentencesOf(line)
	judge.told(len(sentences), joinedIn(sentences), line, nil)
	unit := unitKey{text: line}
	state := judge.unit(unit)
	for _, test := range []struct {
		way  wayOut
		want bool
	}{
		{wayOut{"reads", "reads the file"}, false},
		{wayOut{"Web.Config", "Web.Config "}, false},
		{stopNoSpaceWays("Web.Config")[0], true},
		{stopNoSpaceWays("Web.Config")[1], true},
	} {
		if got := judge.passes(state, unit, ruleStopNoSpace, test.way); got != test.want {
			t.Errorf("%q: the judge passes %q to %q: %v, want %v", line, test.way.from, test.way.to, got, test.want)
		}
	}
}

// The judge reads a sentence of a text that the page shows with no code font as the page shows it:
// withoutCodeFont of the words that ExtractDocument writes with code in code font is the words that
// it writes as the page shows them, for code that holds a stop and a space, a backtick, a space at
// its ends or nothing but spaces. Control: a lone backtick, which opens no code, stays.
func TestTheWordsWithNoCodeFontAreTheWordsThatThePageShows(t *testing.T) {
	for _, words := range []string{
		"The job runs `make test. all` and mails ada@example.com the log.",
		"Legion runs `scp report.txt deploy@build.example.com:` before each build.",
		"It reads ``a`b`` and `` `c ``, then `  ` and ` d ` at boot.",
		"It reads a lone ` mark and `e` at boot.",
	} {
		extracted, err := ExtractDocument("spec.md", 1, ":::requirement{#honest}\n"+words+"\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		if len(extracted.Texts) != 1 {
			t.Fatalf("%q: %d texts, want 1", words, len(extracted.Texts))
		}
		text := extracted.Texts[0]
		if got, want := withoutCodeFont(oneLine(text.Body)), oneLine(text.Held.Shown); got != want {
			t.Errorf("%q: the words %q with no code font are %q, and the page shows %q", words, text.Body, got, want)
		}
	}
}

// The ways out that one finding offers do not change with the white space after another stop: the
// judge reads a stop with no space after it, of another finding, as the end of a sentence, since
// the space that the other finding asks for makes it one. So the finding of "file.Size" offers the
// same ways out whether the stop of "file.Name" before it has its space after it or not: in a
// statement, a space in each makes three sentences, so each offers the code font alone. Control:
// a statement whose words differ, with no "file.Name", gives "file.Size" the space too.
func TestTheWaysOutOfAFindingStayWhateverSpaceFollowsTheStopOfAnother(t *testing.T) {
	offered := func(body, word string) (string, int) {
		for _, each := range Check(planted(), Input{Texts: []Text{statement(body)}}) {
			if each.Rule == ruleStopNoSpace && strings.Contains(each.Detail, fmt.Sprintf("%q", word)) {
				ways := 0
				for _, way := range []string{"put a space after the stop", "put the name in code font"} {
					if strings.Contains(strings.ToLower(each.Ways), way) {
						ways++
					}
				}
				return each.Said(), ways
			}
		}
		t.Fatalf("%q: no finding of %s names %q", body, ruleStopNoSpace, word)
		return "", 0
	}
	joined, joinedWays := offered("The parser reads file.Name and file.Size at its start.", "file.Size")
	spaced, spacedWays := offered("The parser reads file. Name and file.Size at its start.", "file.Size")
	if joined != spaced || joinedWays != spacedWays {
		t.Errorf("the finding of file.Size is %q after file.Name and %q after file. Name, want the same", joined, spaced)
	}
	if joinedWays != 1 {
		t.Errorf("the finding of file.Size offers %d ways out, want the code font alone: %q", joinedWays, joined)
	}
	if alone, ways := offered("The parser reads file.Size at its start.", "file.Size"); ways != 2 {
		t.Errorf("the control: the finding of file.Size with no file.Name is %q, with %d ways out, want both", alone, ways)
	}
}

// The judge reads the ways out of the first maxJudgedInSentence findings of one sentence, so it
// reads a sentence a bounded number of times however many findings it holds. A finding after them
// is still refused and named, and the words of its ways out say that Dispatch offers no way out of
// it, not that none exists (pastNote). Control: the eighth finding of the sentence gives its ways
// out.
func TestAFindingPastTheEighthOfASentenceIsRefusedWithNoWayOutOffered(t *testing.T) {
	var words, names []string
	for index := 1; index <= maxJudgedInSentence+1; index++ {
		words = append(words, fmt.Sprintf("a%d.B", index))
		names = append(names, fmt.Sprintf("a%d@example.com", index))
	}
	for _, test := range []struct {
		rule  Rule
		body  string
		names []string
		base  func(name string) string
	}{
		{ruleStopNoSpace, "Dispatch reads " + strings.Join(words, " ") + " at boot.", words, stopNoSpace},
		{rulePerson, "Dispatch mails " + strings.Join(names, " ") + " each day.", names,
			func(name string) string { return personBreak(name, offer{}).detail }},
	} {
		var details, ways []string
		for _, each := range Check(planted(), Input{Texts: []Text{statement(test.body)}}) {
			if each.Rule == test.rule {
				details, ways = append(details, each.Detail), append(ways, each.Ways)
			}
		}
		if len(details) != len(test.names) {
			t.Fatalf("%q: %d findings of %s, want %d: %q", test.body, len(details), test.rule, len(test.names), details)
		}
		last := len(details) - 1
		const note = "Dispatch offers ways out for the first 8 findings of a sentence, so it offers none for this one, though one can exist"
		if want := test.base(test.names[last]); details[last] != want || ways[last] != note {
			t.Errorf("%q: the finding after the %dth is %q with the ways out %q, want %q with %q", test.body, maxJudgedInSentence, details[last], ways[last], want, note)
		}
		if eighth := ways[last-1]; strings.Contains(eighth, note) || eighth == "" || details[last-1] != test.base(test.names[last-1]) {
			t.Errorf("%q: the %dth finding is %q with the ways out %q, want its ways out and no note", test.body, maxJudgedInSentence, details[last-1], eighth)
		}
	}
}

// The judge reads a text that ste-lint reads as more than one line whole for each way out
// (wholeText.lines), so it reads such a text for the ways out of its first maxJudgedInSentence
// findings only, and reads it a bounded number of times however many lines it holds. A finding
// after them is still refused and named, and the words of its ways out say that Dispatch offers no
// way out of it, not that none exists (wholeNote). Control: the eighth finding, on the eighth line,
// gives its ways out.
func TestAFindingPastTheEighthOfATextReadWholeIsRefusedWithNoWayOutOffered(t *testing.T) {
	var lines, words []string
	for index := 1; index <= maxJudgedInSentence+1; index++ {
		words = append(words, fmt.Sprintf("a%d.B", index))
		lines = append(lines, fmt.Sprintf("Dispatch reads a%d.B at boot.", index))
	}
	body := strings.Join(lines, "\u2028")
	var details, ways []string
	for _, each := range Check(planted(), Input{Texts: []Text{claimOf(body)}}) {
		if each.Rule == ruleStopNoSpace {
			details, ways = append(details, each.Detail), append(ways, each.Ways)
		}
	}
	if len(details) != len(words) {
		t.Fatalf("%q: %d findings of %s, want %d: %q", body, len(details), ruleStopNoSpace, len(words), details)
	}
	last := len(details) - 1
	const note = "Dispatch offers ways out for the first 8 findings of a text that ste-lint reads as more than one line, so it offers none for this one, though one can exist"
	if want := stopNoSpace(words[last]); details[last] != want || ways[last] != note {
		t.Errorf("%q: the finding after the %dth is %q with the ways out %q, want %q with %q", body, maxJudgedInSentence, details[last], ways[last], want, note)
	}
	if eighth := ways[last-1]; strings.Contains(eighth, note) || eighth == "" || details[last-1] != stopNoSpace(words[last-1]) {
		t.Errorf("%q: the %dth finding is %q with the ways out %q, want its ways out and no note", body, maxJudgedInSentence, details[last-1], eighth)
	}
}
