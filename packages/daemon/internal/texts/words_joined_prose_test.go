package texts

import (
	"strings"
	"testing"
)

// proseNews is the news record a start of LEGION-208 carries after its own text in these tests: one
// changed section, whose heading quotes the closing tag of a `<details>` section.
func proseNews(t *testing.T) string {
	t.Helper()
	news, err := Render(DesignNewsRecord{TaskHead: TaskHead{Kind: DesignNews, Issue: "LEGION-208"}, Tree: "LEGION-200",
		Audience: NewsToHolder, Phase: "implementing",
		Items:    []DesignNewsItem{{Step: "spec", Document: "dispatch://LEGION-200/spec", Artifact: "artifact-spec", Version: 5, Taken: new(4), Changes: NewsChangesListed}},
		Sections: []DesignNewsSection{{Item: 0, Unit: NewsUnitSection, Nth: 1, Change: "changed", Heading: &Words{Text: "The `</details>` line"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(news, "\n<heading ") || !strings.HasSuffix(news, "\n</heading>") {
		t.Fatalf("the news record draws no heading block:\n%s", news)
	}
	return news
}

// headingStepped is news with its heading block under heading-1, its attributes and words kept.
var headingStepped = strings.NewReplacer("\n<heading ", "\n<heading-1 ", "\n</heading>", "\n</heading-1>")

// A start's own text that an earlier release queued is prose, not a record Render drew: it holds no
// record line (Drawn), and a person's words in it can hold lines of a block's form, such as a
// `<details>` section of a review body or the two of a tester's log. Joined (the daemon's
// followedBy) sends such a start as that release wrote it, tag lines included, as a start's notes
// go in front of an operator's task (supervise's
// TestAnOperatorsTaskBehindACutTurnNoteGoesAsTheOperatorWroteIt), and draws the blocks of the news
// record after it under tags chosen against it: where the prose holds the tag of a news block,
// that block takes the next tag, with its attributes and its words as they were.
func TestJoinedSendsAProseStartAsWrittenTagLinesIncluded(t *testing.T) {
	news := proseNews(t)
	for _, c := range []struct {
		name, start, news string
	}{
		{name: "a section whose tag the news holds",
			start: "Answer the review. Issue: LEGION-208.\n<details>\nthe log of the run\n</details>\nPush when it passes.", news: news},
		{name: "two sections of one name",
			start: "Answer the review.\n<details>\nfirst run\n</details>\n<details>\nsecond run\n</details>", news: news},
		{name: "a section of the tag of the news record's block",
			start: "Answer the review.\n<heading>\nthe tests pass\n</heading>\nPush it.", news: headingStepped.Replace(news)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if Drawn(c.start) {
				t.Fatalf("the prose start reads as a text Render drew:\n%s", c.start)
			}
			if got, want := Joined(c.start, news), c.start+"\n\n"+c.news; got != want {
				t.Errorf("the joined task is\n%s\nwant the start as it was written, a blank line, and the news record with tags chosen against the start:\n%s", got, want)
			}
		})
	}
}

// A start an earlier release queued, with the news record of its holder after it (Joined), goes
// behind the cut-turn note as that release wrote it (InFrontOfWords), and the note goes word for
// word. The news record is this release's, and its blocks are drawn again against the note, as a
// task's blocks are behind a note (InFront): a cause that quotes the runtime's words, which can
// hold the closing tag of a news block, opens or closes no tag of theirs.
func TestANoteInFrontOfAProseStartWithNewsOpensNoTagOfTheNews(t *testing.T) {
	news := proseNews(t)
	start := "Continue Fix the race. Issue: LEGION-208. Phase: implementing. Reason: Two runs fail:\n" +
		"<details>\nrun 1\n</details>\n<details>\nrun 2\n</details>\nFix the lock order."
	task := Joined(start, news)
	if task != start+"\n\n"+news {
		t.Fatalf("the start with its news is joined as\n%s\nwant each as it was, a blank line apart", task)
	}
	note := "kind: cut-turn-note\nissue: LEGION-208\nYour process ended because the process died: \"log: </heading>\"."
	if got, want := InFrontOfWords([]string{note}, task), note+"\n\n"+start+"\n\n"+headingStepped.Replace(news); got != want {
		t.Errorf("the task is sent as\n%s\nwant the note and the start word for word, and the news record's heading under a tag the note holds none of:\n%s", got, want)
	}
	plain := "kind: cut-turn-note\nissue: LEGION-208\nYour process ended because the pod was replaced."
	if got := InFrontOfWords([]string{plain}, task); got != plain+"\n\n"+task {
		t.Errorf("a note that holds no tag changes the task it goes in front of:\n%s", got)
	}
}

// A text tells by itself whether this release drew it (Drawn): a record Render drew opens with its
// record line, `kind: ` and a kind on the list, or carries it second behind the look task's lead.
// Prose holds none, also where it opens with the lead main's look task opened with, and so does
// the controller's start message, a fixed sentence with no field.
func TestATextIsDrawnWhereItOpensWithARecordLine(t *testing.T) {
	look, err := Render(CheckLookRecord{Issue: "LEGION-208", Title: "root", Kind: CheckLook, Look: 2, WokenBy: WokenByNamed,
		PullRequest: "acme/widgets#42", Summary: &Words{Text: "Production does not carry the merge yet.", By: "tester"}})
	if err != nil {
		t.Fatal(err)
	}
	controllerStart, err := Render(ControllerStartRecord{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, text string
		drawn      bool
	}{
		{name: "this release's look task, behind its lead", text: look, drawn: true},
		{name: "a news record", text: proseNews(t), drawn: true},
		{name: "main's look task, behind the same lead", text: "Production check of LEGION-208: root.\n" +
			"Repository acme/widgets, pull request #42. Look 2, woken by the wake named at the last look — Production does not carry the merge yet."},
		{name: "a line of a kind's form that names no kind on the list", text: "kind: of a mess\nissue: LEGION-208"},
		{name: "a record line second, of a kind whose record has no lead", text: "Look at this.\nkind: design-news\nissue: LEGION-208"},
		{name: "a record line third", text: "Look at this.\nAnd this.\nkind: design-news"},
		{name: "the controller's start message", text: controllerStart},
		{name: "no text", text: ""},
	} {
		if got := Drawn(c.text); got != c.drawn {
			t.Errorf("%s: Drawn is %v, want %v:\n%s", c.name, got, c.drawn, c.text)
		}
	}
}
