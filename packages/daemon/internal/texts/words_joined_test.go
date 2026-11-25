package texts

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// A start task that carries news is the start's own record and then the news record of each
// audience, each joined to what stands before it as InFront joins a note to a task (the daemon's
// followedBy), so a news record's blocks take tags chosen against the start's text alone. The
// claim's send then puts its notes in front of that whole task (supervise's Delivery.message,
// through InFront), and a note can hold another author's words: a cut turn's cause, an abandoned
// session's reason, a provider's error. Every block of every record, the news record's as well as
// the start's, is drawn again under a tag that nothing else in what is sent opens or closes, here
// behind a note that holds the closing tag of each block of the news record, and each block's
// attributes and words stay as they were. Where the note holds no tag of a block, the task goes as
// it stands.
func TestANoteInFrontOfATaskThatCarriesNewsOpensNoTagOfAnyRecordsBlocks(t *testing.T) {
	at := time.Date(2026, 11, 22, 17, 5, 0, 0, time.UTC)
	start, err := Render(PhaseAssignmentRecord{TaskHead: TaskHead{Kind: PhaseAssignment, Issue: "LEGION-208"}, Title: "root",
		Phase: "implementing", MovedBackFrom: "testing", Reason: &Words{Text: "The tests fail on the second run.", By: "tester", At: at}})
	if err != nil {
		t.Fatal(err)
	}
	taken := 2
	news, err := Render(DesignNewsRecord{TaskHead: TaskHead{Kind: DesignNews, Issue: "LEGION-208"}, Tree: "LEGION-200",
		Audience: NewsToHolder, Phase: "implementing",
		Items: []DesignNewsItem{
			{Step: "spec", Document: "dispatch://LEGION-200/spec", Artifact: "artifact-1", Version: 3, Taken: &taken, Changes: NewsChangesListed},
			{Step: "criteria", Document: "dispatch://LEGION-200/artifact/verification-md", Artifact: "artifact-2", Version: 2,
				Changes: NewsChangesUnavailable, Reason: &Words{Text: "Dispatch answered 503.", By: "dispatch", At: at}},
		},
		Sections: []DesignNewsSection{{Item: 0, Unit: NewsUnitSection, Nth: 1, Change: "changed", Heading: &Words{Text: "Requirements"}}},
		Blocks:   []DesignNewsBlock{{Item: 0, Type: "requirement", Change: "changed", ID: &Words{Text: "R2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := InFront([]string{start}, news)
	words := []string{"The tests fail on the second run.", "Dispatch answered 503.", "Requirements", "R2"}

	opening := regexp.MustCompile(`(?m)^<([a-z_]+(?:-[0-9]+)?)((?: [a-z]+="[^"<>]*")*)>$`)
	type drawn struct{ tag, attributes, words string }
	read := func(text string) []drawn {
		var blocks []drawn
		for _, m := range opening.FindAllStringSubmatchIndex(text, -1) {
			tag, wordsAt := text[m[2]:m[3]], m[1]+1
			closed := strings.Index(text[wordsAt:], "\n</"+tag+">")
			if closed < 0 {
				t.Fatalf("the block %s never closes:\n%s", tag, text)
			}
			blocks = append(blocks, drawn{tag: tag, attributes: text[m[4]:m[5]], words: text[wordsAt : wordsAt+closed]})
		}
		return blocks
	}
	joined := read(task)
	newsAt := strings.Index(task, "\n\nkind: design-news\n")
	if newsAt < 0 || len(joined) != len(words) {
		t.Fatalf("the task is not the start's record with its block and then the news record with its three:\n%s", task)
	}
	var closings []string
	for _, b := range read(task[newsAt:]) {
		closings = append(closings, "</"+b.tag+">")
	}
	if len(closings) != 3 {
		t.Fatalf("the news record draws %d blocks, want 3:\n%s", len(closings), task)
	}

	note := "kind: cut-turn-note\nissue: LEGION-208\nYour process ended because the process died: \"log: " + strings.Join(closings, " ") + "\"."
	sent := InFront([]string{note}, task)
	if !strings.HasPrefix(sent, note+"\n\n") {
		t.Fatalf("the note is not sent word for word in front of the task:\n%s", sent)
	}
	got := read(sent[len(note)+2:])
	if len(got) != len(words) {
		t.Fatalf("the task is sent with %d blocks, want %d:\n%s", len(got), len(words), sent)
	}
	mark := regexp.MustCompile(`</?([A-Za-z_][A-Za-z0-9_]*(?:-[0-9]+)?)`)
	marks := map[string]int{}
	for _, m := range mark.FindAllStringSubmatch(sent, -1) {
		marks[m[1]]++
	}
	for i, b := range got {
		if n := marks[b.tag]; n != 2 {
			t.Errorf("the block of %q is drawn under %s, which what is sent opens or closes %d times, want twice: its own opening and closing lines\n%s",
				b.words, b.tag, n, sent)
		}
		if b.words != words[i] || b.attributes != joined[i].attributes {
			t.Errorf("block %d is sent with attributes %q and words %q, want %q and %q as the task drew them", i, b.attributes, b.words,
				joined[i].attributes, words[i])
		}
	}

	plain := "kind: cut-turn-note\nissue: LEGION-208\nYour process ended because the pod was replaced."
	if sent := InFront([]string{plain}, task); sent != plain+"\n\n"+task {
		t.Errorf("a note that holds no tag of a block changes the task that carries news it goes in front of:\n%s", sent)
	}
}
