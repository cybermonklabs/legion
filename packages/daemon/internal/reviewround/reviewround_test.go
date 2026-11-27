package reviewround

import (
	"strings"
	"testing"
)

// Split reads back only the two sentences Sentences writes, and only at the task's very end: each
// pair Sentences writes comes off whole and leaves the text before it. A task whose earlier text
// quotes the words, as a retry's reason can, is cut at its own end where the daemon's pair stands
// there, and not at all where it does not. Words of the same shape that Sentences never writes (a
// commit that is no head, an entry with a leading zero or of zero, text after the entry's period)
// leave the task whole.
func TestSplitReadsOnlyTheSentencesSentencesWritesAtTheVeryEnd(t *testing.T) {
	const (
		head = "0123456789abcdef0123456789abcdef01234567"
		body = "Review the change. Issue: LEGION-208. Phase: reviewing."
	)
	quoted := body + " Reason: the architect quoted" + Sentences(head, 2) + " and asked again."
	for _, tc := range []struct {
		name      string
		task      string
		body      string
		sentences string
	}{
		{name: "a commit and an entry", task: body + Sentences(head, 7), body: body, sentences: Sentences(head, 7)},
		{name: "no commit and no entry", task: body + Sentences("", 0), body: body, sentences: Sentences("", 0)},
		{name: "no commit and an entry", task: body + Sentences("", 12), body: body, sentences: Sentences("", 12)},
		{name: "the two sentences alone", task: Sentences(head, 3), body: "", sentences: Sentences(head, 3)},
		{name: "quoted words earlier and the daemon's pair at the end", task: quoted + Sentences("", 3), body: quoted,
			sentences: Sentences("", 3)},
		{name: "quoted words earlier and nothing at the end", task: quoted},
		{name: "text after the entry's period", task: body + Sentences(head, 7) + "\n\nThe approved design changed."},
		{name: "a commit that is no head", task: body + " " + CommitLabel + ": abc1234. " + EntryLabel + ": 7."},
		{name: "a commit in capitals", task: body + " " + CommitLabel + ": " + strings.ToUpper(head) + ". " + EntryLabel + ": 7."},
		{name: "an entry with a leading zero", task: body + " " + CommitLabel + ": none. " + EntryLabel + ": 07."},
		{name: "an entry of zero", task: body + " " + CommitLabel + ": none. " + EntryLabel + ": 0."},
		{name: "no period after the entry", task: body + " " + CommitLabel + ": none. " + EntryLabel + ": 7"},
		{name: "no task", task: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantBody, wantNamed := tc.body, tc.sentences != ""
			if !wantNamed {
				wantBody = tc.task
			}
			body, sentences, named := Split(tc.task)
			if body != wantBody || sentences != tc.sentences || named != wantNamed {
				t.Fatalf("Split(%q) = %q, %q, %t; want %q, %q, %t", tc.task, body, sentences, named, wantBody, tc.sentences, wantNamed)
			}
		})
	}
}
