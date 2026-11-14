package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A rescue whose full list is longer than one Dispatch document gets a document that stops the
// list before the bound and ends with the sentence that says how many lines it leaves out. The
// room the list keeps for that sentence is the sentence at its longest, so the document is at
// most record.MaxMessageArtifactBytes, and the outbox takes the message: validateOutboxPayload,
// which NewOutboxRow runs, refuses a document past that bound. The check pushed 30,000 commits,
// so the full list is near 1.3 MiB. Each commit's line is 45 bytes, less than half the sentence,
// so the room after the last line that fits is smaller than the sentence, and a list that kept no
// room for the sentence would pass the bound.
func TestRescueMessageOfAListPastOneDocumentStopsTheListAtTheBoundAndSaysSo(t *testing.T) {
	const pushed = 30000
	commits := make([]runtime.RescuedCommit, pushed)
	for i := range commits {
		commits[i] = runtime.RescuedCommit{ID: fmt.Sprintf("%040x", pushed-i), Subject: "work"}
	}
	checked := pushCheck{workspaces: []runtime.WorkspaceRescue{{
		Dir: "/legion/workspaces/sjawhar/legion/legion-484", Status: runtime.RescueRescued,
		Branches: []runtime.RescuedBranch{{Name: podUpdateRescueTarget, Commit: commits[0].ID, Pushed: true, Commits: commits}},
	}}}

	post := rescueMessage(podUpdateIssue, podUpdateRound, checked)
	if post.Artifact == nil {
		t.Fatalf("the rescue message carries no document:\n%s", post.Body)
	}
	content := post.Artifact.Content
	if len(content) > record.MaxMessageArtifactBytes {
		t.Fatalf("the message's document holds %d bytes, more than the %d of one Dispatch document", len(content), record.MaxMessageArtifactBytes)
	}
	open, end := strings.Index(content, "```\n"), strings.LastIndex(content, "\n```\n")
	if open < 0 || end <= open {
		t.Fatalf("the message's document has no fenced list:\n%.2000s", content)
	}
	// The branch's repository, name, tip, tip before and count, then one line for each commit.
	kept := strings.Count(content[open+len("```\n"):end], "\n") + 1
	if kept >= pushed+5 {
		t.Fatalf("the message's document keeps all %d lines of a list past the bound", kept)
	}
	stops := fmt.Sprintf("\n```\n\nThe list stops here. It leaves out %d more lines, because one Dispatch document holds at most %d bytes.\n", pushed+5-kept, record.MaxMessageArtifactBytes)
	if !strings.HasSuffix(content, stops) {
		t.Fatalf("the message's document does not end with the list's closing fence and %q; it ends with:\n%s", stops, content[max(0, len(content)-500):])
	}
	if _, err := record.NewOutboxRow(podUpdateIssue, post, time.Now()); err != nil {
		t.Fatalf("the outbox refuses the rescue message: %v", err)
	}
}
