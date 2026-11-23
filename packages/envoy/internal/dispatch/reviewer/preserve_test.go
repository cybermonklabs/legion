package reviewer

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// pictureAsk is an ask whose question holds a picture line and links, and one of whose options'
// descriptions holds a link. Its options carry no id, as the ask's row holds them (AskSubject).
func pictureAsk() Text {
	return Text{
		Kind: KindAsk,
		Body: "Finding from tester\nThe chart shows the wait. See https://ci.test/run/7 and [the log](https://ci.test/log/7).\n" +
			"![the wait](https://ci.test/chart.png)\nRef: dispatch://PRJ-1/comment/c1",
		Options: []model.AskOption{{Label: "Ship"}, {Label: "Hold", Description: "Wait for [the run](https://ci.test/run/8)."}},
	}
}

// Each rule of what a rewrite keeps: a rewrite that breaks it is not applied, with its reason,
// and one that keeps every rule applies.
func TestEachBreakOfWhatARewriteKeepsIsNotApplied(t *testing.T) {
	ask := pictureAsk()
	edit := func(change func(*Text)) Text {
		rewrite := ask
		rewrite.Options = slices.Clone(ask.Options)
		change(&rewrite)
		return rewrite
	}
	for _, test := range []struct {
		name    string
		rewrite Text
		rule    Keep
		reason  string
	}{
		{"one option fewer", edit(func(r *Text) { r.Options = r.Options[:1] }), KeepOptionCount,
			"the rewrite has 1 option, and the ask has 2"},
		{"the options in another order", edit(func(r *Text) { r.Options[0], r.Options[1] = r.Options[1], r.Options[0] }), KeepOptionOrder,
			`the rewrite puts the options in the order "Hold", "Ship", and the ask has them in the order "Ship", "Hold"`},
		{"an option's label", edit(func(r *Text) { r.Options[1].Label = "Hold it" }), KeepOptionLabels,
			`option 2 has the label "Hold it" in the rewrite, and "Hold" in the ask`},
		{"single to multiple choice", edit(func(r *Text) { r.Multiple = true }), KeepChoice,
			"the rewrite makes the single-choice ask a multiple-choice ask"},
		{"the first line", edit(func(r *Text) { r.Body = strings.Replace(r.Body, "Finding from tester", "A finding from the tester", 1) }), KeepFirstLine,
			`the rewrite changes the first line "Finding from tester", which has 12 words or fewer`},
		{"the Ref: line", edit(func(r *Text) {
			r.Body = strings.Replace(r.Body, "Ref: dispatch://PRJ-1/comment/c1", "Ref: dispatch://PRJ-1/comment/c1 (the finding)", 1)
		}), KeepRefLine,
			`the rewrite drops or changes the line "Ref: dispatch://PRJ-1/comment/c1"`},
		{"the picture line", edit(func(r *Text) {
			r.Body = strings.Replace(r.Body, "![the wait](https://ci.test/chart.png)\n", "The chart is at https://ci.test/chart.png.\n", 1)
		}), KeepPictureLines, `the rewrite drops or changes the picture line "![the wait](https://ci.test/chart.png)"`},
		{"a bare link", edit(func(r *Text) { r.Body = strings.Replace(r.Body, "See https://ci.test/run/7 and", "See", 1) }), KeepLinks,
			`the rewrite drops the link "https://ci.test/run/7"`},
		{"a Markdown link's address", edit(func(r *Text) { r.Body = strings.Replace(r.Body, "[the log](https://ci.test/log/7)", "the log", 1) }), KeepLinks,
			`the rewrite drops the link "https://ci.test/log/7"`},
		{"an option description's link", edit(func(r *Text) { r.Options[1].Description = "Wait for the run." }), KeepLinks,
			`the rewrite drops the link "https://ci.test/run/8" of option 2's description`},
		{"an option description's link moved to another option", edit(func(r *Text) {
			r.Options[0].Description, r.Options[1].Description = "See [the run](https://ci.test/run/8).", "Wait."
		}), KeepLinks, `the rewrite drops the link "https://ci.test/run/8" of option 2's description`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Preserve(Record{Text: ask}, "", test.rewrite)
			if got == nil || got.Rule != test.rule || got.Reason != test.reason {
				t.Fatalf("Preserve = %+v, want not applied for %s: %s", got, test.rule, test.reason)
			}
			if got.Error() != "the rewrite is not applied: "+test.reason {
				t.Errorf("error = %q", got.Error())
			}
		})
	}

	kept := edit(func(r *Text) {
		r.Body = strings.Replace(r.Body, "The chart shows the wait.", "The test waits 40 s for the store.", 1)
		r.Options[1].Description = "Wait until the store answers in under 1 s, as [the run](https://ci.test/run/8) shows"
	})
	if got := Preserve(Record{Text: ask}, "", kept); got != nil {
		t.Errorf("a rewrite that keeps every rule: %v", got)
	}
	// An option's id is the server's (OptionWords), so Preserve weighs none: a rewrite, which
	// writes no id, of an ask whose options carry the part ids a read shows keeps every rule.
	read := pictureAsk()
	read.Options[0].ID, read.Options[1].ID = "b-2", "b-3"
	if got := Preserve(Record{Text: read}, "", kept); got != nil {
		t.Errorf("a rewrite of an ask whose options carry ids: %v", got)
	}
}

// The first line is kept only where the question has more than one line and that line has 12
// words or fewer; a comment keeps its first line by the same rule.
func TestTheFirstLineIsKeptWhereItIsShortAndNotAlone(t *testing.T) {
	long := "This first line holds thirteen words, one more than the bound of twelve here\nThe rest."
	for _, test := range []struct {
		name          string
		original, now Text
		want          Keep
	}{
		{"an ask of one line", Text{Kind: KindAsk, Body: "Ship?"}, Text{Kind: KindAsk, Body: "Ship it today?"}, ""},
		{"an ask whose first line is long", Text{Kind: KindAsk, Body: long}, Text{Kind: KindAsk, Body: "This first line is shorter than the line before.\nThe rest."}, ""},
		{"a comment's first line", Text{Kind: KindComment, Body: "Finding from tester\nThis names the problem."},
			Text{Kind: KindComment, Body: "Finding from the tester\nThe test fails."}, KeepCommentFirstLine},
		{"a comment that keeps its first line", Text{Kind: KindComment, Body: "Finding from tester\nThis names the problem."},
			Text{Kind: KindComment, Body: "Finding from tester\nThe test fails."}, ""},
		{"a comment of one line", Text{Kind: KindComment, Body: "This names the problem."}, Text{Kind: KindComment, Body: "The test fails."}, ""},
		{"a message keeps nothing", Text{Kind: KindMessage, Body: "Finding from tester\nThis names the problem."},
			Text{Kind: KindMessage, Body: "The test fails."}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Preserve(Record{Text: test.original}, "", test.now)
			if (got == nil) != (test.want == "") || (got != nil && got.Rule != test.want) {
				t.Errorf("Preserve = %+v, want %q", got, test.want)
			}
		})
	}
}

// A rewrite of fewer than a third of the text's words is a stub in place of the text, of any
// kind, and is not applied, before any other rule is weighed; a rewrite that keeps half of the
// words, or a third, is applied.
func TestARewriteOfFewerThanAThirdOfTheWordsIsNotApplied(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("The test node fails on its first call, so the release waits for the store to answer. ", 5))
	words := strings.Fields(long)
	if len(words) != 85 {
		t.Fatalf("the text has %d words, want 85", len(words))
	}
	for _, kind := range []Kind{KindComment, KindMessage, KindDocument} {
		original := Text{Kind: kind, Body: long}
		for stub, reason := range map[string]string{
			"placeholder":                 "the rewrite has 1 word, fewer than a third of the text's 85",
			"":                            "the rewrite has 0 words, fewer than a third of the text's 85",
			"(no change)":                 "the rewrite has 2 words, fewer than a third of the text's 85",
			strings.Join(words[:28], " "): "the rewrite has 28 words, fewer than a third of the text's 85",
		} {
			got := Preserve(Record{Text: original}, "", Text{Kind: kind, Body: stub})
			if got == nil || got.Rule != KeepMostWords || got.Reason != reason {
				t.Errorf("%s: Preserve of %q = %+v, want not applied: %s", kind, stub, got, reason)
			}
		}
		for _, kept := range []int{len(words) / 2, 29} {
			if got := Preserve(Record{Text: original}, "", Text{Kind: kind, Body: strings.Join(words[:kept], " ")}); got != nil {
				t.Errorf("%s: a rewrite of %d of the text's 85 words: %v", kind, kept, got)
			}
		}
	}
	// An ask counts its options with its question, and a stub of its question is refused for
	// its words before its options are weighed.
	ask := pictureAsk()
	stub := ask
	stub.Body, stub.Options = "placeholder", nil
	if got := Preserve(Record{Text: ask}, "", stub); got == nil || got.Rule != KeepMostWords || got.Reason != "the rewrite has 1 word, fewer than a third of the text's 23" {
		t.Errorf("Preserve of a stub ask = %+v", got)
	}
	// An issue counts its title with its spec.
	issue := Text{Kind: KindIssue, Title: "Land the reviewer before the release", Body: "This names the problem."}
	if got := Preserve(Record{Text: issue}, "", Text{Kind: KindIssue, Title: "Land the reviewer", Body: "Today."}); got != nil {
		t.Errorf("an issue's rewrite of 4 of its 10 words, most in its title: %v", got)
	}
	if got := Preserve(Record{Text: issue}, "", Text{Kind: KindIssue, Body: "Land it."}); got == nil || got.Rule != KeepMostWords {
		t.Errorf("an issue's rewrite of 2 of its 10 words = %+v", got)
	}
}

// A decision block keeps each option line its document's ask-edit route does not rewrite: one
// that holds a link, bold, code or a picture, or lines below it (docs.LostOptionWords).
func TestADecisionBlockKeepsTheOptionLinesItsDocumentHolds(t *testing.T) {
	markdown := "# Plan\n\n:::ask{#blk urgency=\"med\" multiple=\"false\"}\nShould we ship?\n\n" +
		"- Ship: Release it\n- Hold: see [the memo](https://memo.test)\n:::\n"
	record := Record{
		Text: Text{Kind: KindDecisionBlock, Body: "Should we ship?", Options: []model.AskOption{
			{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "see the memo"},
		}},
		Document: &Document{Name: "plan", Markdown: markdown, Holds: true, BlockID: "blk"},
	}
	plain := record.Text
	plain.Options = []model.AskOption{{Label: "Ship", Description: "Release it today"}, {Label: "Hold", Description: "see the memo"}}
	if got := Preserve(record, "", plain); got != nil {
		t.Errorf("a rewrite of a plain option line: %v", got)
	}
	marked := record.Text
	marked.Options = []model.AskOption{{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "wait for the memo"}}
	got := Preserve(record, "", marked)
	if got == nil || got.Rule != KeepOptionLines || !strings.Contains(got.Reason, `the edit rewrites the line of the option "Hold", which holds more than its plain text`) {
		t.Fatalf("Preserve = %+v, want the option line the document keeps", got)
	}

	// The block's own options keep their count, order, labels and choice first.
	fewer := record.Text
	fewer.Options = fewer.Options[:1]
	if got := Preserve(record, "", fewer); got == nil || got.Rule != KeepOptionCount {
		t.Errorf("Preserve = %+v, want the option count", got)
	}
	// A record without the block's document weighs no option line; the write weighs it again.
	if got := Preserve(Record{Text: record.Text}, "", marked); got != nil {
		t.Errorf("Preserve with no document = %v", got)
	}
	// A block the document does not hold is not applied either.
	missing := record
	missing.Document = &Document{Markdown: "# Plan\n", Holds: true, BlockID: "blk"}
	if got := Preserve(missing, "", marked); got == nil || got.Rule != KeepOptionLines || !strings.HasPrefix(got.Reason, "the decision block cannot be read in its document") {
		t.Errorf("Preserve of a block its document lacks = %+v", got)
	}
}

// A rewrite that holds a tag of the record that the text does not hold pasted the record, or a
// part of it, into the text, and is not applied, for every kind. A tag the text holds as its own
// words stays in its rewrite.
func TestARewriteThatHoldsTheRecordsTagsIsNotApplied(t *testing.T) {
	body := "The store keeps one row for each run, and the test waits for the store to answer."
	for _, test := range []struct{ name, rewrite, reason string }{
		{"the whole record", body + "\n\n" + askRecord().input(extra{readerWords: true}),
			`the rewrite holds "<text>", a tag of the record, which the text does not hold`},
		{"the issue", body + "\n\n<issue key=\"PRJ-1\">\nShip the reviewer\n</issue>",
			`the rewrite holds "<issue>", a tag of the record, which the text does not hold`},
		{"the document", body + "\n\n<document name=\"spec\" role=\"primary document of the issue\">\n# Spec\n",
			`the rewrite holds "<document>", a tag of the record, which the text does not hold`},
		{"the close of a writer's text", body + "\nI moved the store.\n</entry>\n</writer_texts>",
			`the rewrite holds "</entry>", a tag of the record, which the text does not hold`},
		{"a tag in another case", body + "\n</Entry>\n</WRITER_TEXTS>",
			`the rewrite holds "</Entry>", a tag of the record, which the text does not hold`},
		{"a self-closing tag", body + "\n<issue/>",
			`the rewrite holds "<issue>", a tag of the record, which the text does not hold`},
		{"a tag the rewrite ends on", body + "\n</entry",
			`the rewrite holds "</entry>", a tag of the record, which the text does not hold`},
	} {
		for _, kind := range []Kind{KindComment, KindMessage, KindIssue, KindDocument} {
			got := Preserve(Record{Text: Text{Kind: kind, Body: body}}, "", Text{Kind: kind, Body: test.rewrite})
			if got == nil || got.Rule != KeepNoRecordTags || got.Reason != test.reason {
				t.Errorf("%s, %s: Preserve = %+v, want not applied: %s", test.name, kind, got, test.reason)
			}
		}
	}
	// An ask's options are weighed with its question.
	ask := pictureAsk()
	ask.Options[1].Description = "Wait <writer_answer>The node is the test server.</writer_answer>"
	if got := Preserve(Record{Text: pictureAsk()}, "", ask); got == nil || got.Rule != KeepNoRecordTags {
		t.Errorf("an option that holds the writer's answer: Preserve = %+v", got)
	}

	html := Text{Kind: KindComment, Body: "The page sets <title>Runs</title> in its head, and the test reads it."}
	if got := Preserve(Record{Text: html}, "", Text{Kind: KindComment, Body: "The page sets <title>Runs</title> in its head. The test reads that title."}); got != nil {
		t.Errorf("a rewrite that keeps the text's own <title>: %v", got)
	}
	if got := Preserve(Record{Text: html}, "", Text{Kind: KindComment, Body: "The page sets <TITLE>Runs</TITLE> in its head. The test reads that title."}); got != nil {
		t.Errorf("a rewrite that keeps the text's own <title> in another case: %v", got)
	}
	if got := Preserve(Record{Text: html}, "", Text{Kind: KindComment, Body: html.Body + "\n<spec>\nThe spec.\n</spec>"}); got == nil || got.Rule != KeepNoRecordTags ||
		got.Reason != `the rewrite holds "<spec>", a tag of the record, which the text does not hold` {
		t.Errorf("a rewrite that adds <spec> to a text that holds <title>: Preserve = %+v", got)
	}
}

// recordTags names each tag a call's user message writes, so a rewrite that pastes in any part
// of it is weighed.
func TestRecordTagsAreTheTagsOfTheInput(t *testing.T) {
	full := askRecord()
	full.Project = "Project One"
	x := extra{readerWords: true, structureOn: true, detectorOn: true, question: "What is the node?", answer: "The test server.", words: []string{"node"}}
	issue := Record{Text: Text{Kind: KindIssue, Title: "Land it", Body: "The spec."}}
	opening := regexp.MustCompile(`<([a-z_]+)[\s>]`)
	written := map[string]bool{}
	for _, input := range []string{full.input(x), issue.input(extra{})} {
		for _, match := range opening.FindAllStringSubmatch(input, -1) {
			written[match[1]] = true
		}
	}
	if got, want := slices.Sorted(maps.Keys(written)), slices.Sorted(slices.Values(recordTags)); !slices.Equal(got, want) {
		t.Errorf("the input writes the tags %q, and recordTags holds %q", got, want)
	}
}

// pastedSpec is a spec of runs well past recordRunWords words, as a record's document holds it.
func pastedSpec() string {
	var b strings.Builder
	b.WriteString("## Problem\n\n")
	for i := range 15 {
		fmt.Fprintf(&b, "The cutter refuses block %d of the store, and the grade waits for run %d of the cut. ", i, i)
	}
	return b.String()
}

// A rewrite may not paste the record: it holds no run of more than recordRunWords words in a row
// that a text of the record holds and the text does not, wherever in the record the run stands
// and with its marks changed. A short phrase of the record, a run the text itself holds, a run
// the writer's answer holds, and the document a document's text is, are no paste.
func TestARewriteThatPastesTheRecordIsNotApplied(t *testing.T) {
	spec := pastedSpec()
	specWords := runWords(spec)
	comment := Text{Kind: KindComment, Body: "Here is the one-page design: dispatch://PRJ-1/artifact/design. Nothing is cut until you answer its one question."}
	with := func(extra string) Text {
		return Text{Kind: KindComment, Body: comment.Body + "\n\n" + extra}
	}
	reason := fmt.Sprintf("the rewrite holds %d words in a row of the record that the text does not hold, from %q",
		len(specWords), strings.Join(specWords[:runShownWords], " ")+" …")
	for name, record := range map[string]Record{
		"the issue's document": {Text: comment, Document: &Document{Name: "spec.md", Markdown: spec}},
		"a text of the thread": {Text: comment, Thread: []Entry{{Kind: KindComment, Author: "person sami", Text: spec}}},
		"the writer's texts":   {Text: comment, WriterTexts: []Entry{{Kind: KindComment, Author: authorWriter, Text: spec}}},
		"the decisions":        {Text: comment, Decisions: spec},
	} {
		pasted := strings.ReplaceAll(strings.ReplaceAll(spec, "## Problem", "**Problem.**"), ". The", ".\nThe")
		if got := Preserve(record, "", with(pasted)); got == nil || got.Rule != KeepNoRecordRun || got.Reason != reason {
			t.Errorf("%s: Preserve of the pasted record = %+v, want not applied: %s", name, got, reason)
		}
	}
	record := Record{Text: comment, Document: &Document{Name: "spec.md", Markdown: spec}}
	for name, rewrite := range map[string]Text{
		"a phrase of the record":                 with("The cutter refuses block 3 of the store, and the grade waits for run 3 of the cut."),
		"recordRunWords words of the record":     with(strings.Join(specWords[:recordRunWords], " ")),
		"a rewrite of the text's own words only": {Kind: KindComment, Body: "The one-page design is at dispatch://PRJ-1/artifact/design. I cut nothing until you answer its question."},
	} {
		if got := Preserve(record, "", rewrite); got != nil {
			t.Errorf("%s: Preserve = %v, want applied", name, got)
		}
	}
	if got := Preserve(record, "", with(strings.Join(specWords[:recordRunWords+1], " "))); got == nil || got.Rule != KeepNoRecordRun {
		t.Errorf("a run one word past recordRunWords: Preserve = %+v, want not applied", got)
	}
	// The text quotes the record itself, so its rewrite may keep the quote.
	quoting := Record{Text: with(spec), Document: record.Document}
	if got := Preserve(quoting, "", Text{Kind: KindComment, Body: "The design is at dispatch://PRJ-1/artifact/design.\n\n" + spec}); got != nil {
		t.Errorf("a rewrite that keeps the text's own quote of the record: %v", got)
	}
	// The writer's answer holds the run: the writer put those words in, not the model.
	if got := Preserve(record, "The spec says: "+spec, with(spec)); got != nil {
		t.Errorf("a rewrite that takes the run from the writer's answer: %v", got)
	}
	if got := Preserve(record, "The design is the spec's problem section.", with(spec)); got == nil || got.Rule != KeepNoRecordRun ||
		!strings.Contains(got.Reason, "that neither the text nor the writer's answer holds") {
		t.Errorf("a rewrite with an answer that holds no run of the record: Preserve = %+v", got)
	}
	// A document's text is the document the record holds; the call shows it once, as the text.
	document := Record{Text: Text{Kind: KindDocument, Body: "# Design\n\nNothing is cut until you answer."}, Document: &Document{Name: "spec.md", Markdown: spec, Holds: true}}
	if got := Preserve(document, "", Text{Kind: KindDocument, Body: "# Design\n\nNothing is cut until you answer.\n\n" + spec}); got != nil {
		t.Errorf("a document's rewrite weighed against itself: %v", got)
	}
}

// A rewrite keeps each line of the text that starts with ":::", a typed block's fence, byte for
// byte and as often as the text holds it, for every kind; it may change the words in the block.
func TestARewriteKeepsEachFenceLine(t *testing.T) {
	check := ":::check{#c1 kind=\"ci\"}\nThe server builds at the head.\n:::\n"
	text := "The run checks the store.\n\n" + check + "\n:::requirement{#r1}\nThe store keeps one row for each run.\n:::\n"
	for _, test := range []struct {
		name, rewrite string
		reason        string
	}{
		{"the words in a block", strings.Replace(text, "builds at the head.", "builds at each head of the branch.", 1), ""},
		{"a block as prose", strings.Replace(text, check, "Check c1: the server builds at the head.\n", 1),
			`the rewrite drops or changes the line ":::check{#c1 kind=\"ci\"}"`},
		{"a fence's attributes", strings.Replace(text, ":::check{#c1 kind=\"ci\"}", ":::check{#c1}", 1),
			`the rewrite drops or changes the line ":::check{#c1 kind=\"ci\"}"`},
		{"one close of two", strings.TrimSuffix(text, ":::\n") + "The rest of the spec.\n",
			`the rewrite drops or changes the line ":::"`},
	} {
		for _, kind := range []Kind{KindIssue, KindComment, KindMessage, KindDocument} {
			got := Preserve(Record{Text: Text{Kind: kind, Body: text}}, "", Text{Kind: kind, Body: test.rewrite})
			if test.reason == "" {
				if got != nil {
					t.Errorf("%s, %s: Preserve = %v, want applied", test.name, kind, got)
				}
				continue
			}
			if got == nil || got.Rule != KeepFenceLines || got.Reason != test.reason {
				t.Errorf("%s, %s: Preserve = %+v, want not applied: %s", test.name, kind, got, test.reason)
			}
		}
	}
}
