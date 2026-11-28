package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A check's block id is any text Dispatch takes but a control character, a backtick among them, so
// close-wait.md renders each id as one code span whose fence is longer than every run of backticks
// inside it, padded with a space where the id starts or ends with one. Fenced by a single
// backtick, an id holding one ended its span early and the rest of it read as markdown. The
// control is a plain id, which keeps its single-backtick span.
func TestCloseWaitDocumentKeepsEachCheckIdInOneCodeSpan(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	wait, err := json.Marshal(StoredWait{Entered: since,
		Checks: []WaitCheck{
			{ID: "c-after", When: "after_merge", Answer: "no_result", Since: since},
			{ID: "a`b c", When: "after_merge", Answer: "no_result", Since: since},
		},
		Unapproved: &WaitUnapproved{Checks: []string{"x``y", "`edge"}, Since: since}})
	if err != nil {
		t.Fatal(err)
	}
	document, err := CloseWaitDocument(Issue{Key: "LEGION-208", CloseWait: wait})
	if err != nil {
		t.Fatalf("render close-wait.md: %v", err)
	}
	for _, want := range []string{"- `c-after` (after_merge)", "- ``a`b c`` (after_merge)", "- ```x``y```\n", "- `` `edge ``\n"} {
		if !strings.Contains(document, want) {
			t.Errorf("close-wait.md holds no %q:\n%s", want, document)
		}
	}
}

// Every word close-wait.md takes from Dispatch renders as text, never as markdown: a check's `when`,
// its answer and its reason each in a code span, and the reason a read does not count, which keeps
// Dispatch's own words and a list of them, as an indented code block (writeAgentText, the
// document's rule for text of more than one line it does not write itself). Written raw, a reason
// or a cause holding a link, emphasis or a backtick rendered as markup in the issue's own document.
// The control is the daemon's own words around them, which stay plain.
func TestCloseWaitDocumentRendersDispatchsWordsAsText(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	wait, err := json.Marshal(StoredWait{Entered: since,
		Checks: []WaitCheck{{ID: "c-after", When: "after`merge", Answer: "no", Reason: "see [this](https://evil.example) **now**", Since: since}},
		Unread: &WaitUnread{Cause: "the read failed: [click](https://evil.example)\n- c-x: ``` fenced", Since: since}})
	if err != nil {
		t.Fatal(err)
	}
	document, err := CloseWaitDocument(Issue{Key: "LEGION-208", CloseWait: wait})
	if err != nil {
		t.Fatalf("render close-wait.md: %v", err)
	}
	for _, want := range []string{
		"- `c-after` (``after`merge``): `no`, `see [this](https://evil.example) **now**`, since",
		"The newest read of the checks does not count, since " + waitTime(since) + ":\n\n    the read failed: [click](https://evil.example)\n    - c-x: ``` fenced\n\n",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("close-wait.md holds no %q:\n%s", want, document)
		}
	}
}
