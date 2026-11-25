package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main's pr-merged reason at c03d3afe (workflow/pullrequest.go:waitingForTree) says that the
// merged issue "closes once the last of them leaves the workflow", whoever moves them, and then
// "End a child with your own status write in Dispatch." (record.EndChildRule): how the reader
// itself ends one. A waiting issue also leaves at a person's move, and a grandchild also at its
// sub-architect's own write, as the same prompt part says of child-closed ("by a human, or by
// your own status write"). INFRA-3686's R2: "a step that a text gives today goes into the
// reader's prompt where the prompt does not give it"; the change's own wrong fact is its defect
// (the architect's ruling). The passage that holds main's step for both architects must not state
// that only the reader's own write takes such an issue out.
func TestThePRMergedPassageSaysNotThatOnlyYourOwnWriteEndsAWaitingIssue(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(goPrompts+"architect-common.md")))
	if err != nil {
		t.Fatalf("read architect-common.md: %v", err)
	}
	var prMerged, childClosed string
	for _, paragraph := range strings.Split(string(body), "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		switch {
		case strings.HasPrefix(paragraph, "A `pr-merged` notice is"):
			prMerged = paragraph
		case strings.HasPrefix(paragraph, "A `child-closed` notice is"):
			childClosed = paragraph
		}
	}
	if prMerged == "" || childClosed == "" {
		t.Fatalf("architect-common.md lacks the pr-merged or the child-closed paragraph")
	}
	const only = "leaves without its own merge only by your own status write"
	const human = "A `child-closed` notice is a child reaching `done`: at its pull request's merge, by a human, or by your own status write."
	if strings.Contains(prMerged, only) && strings.Contains(childClosed, human) {
		at := strings.Index(prMerged, "An issue that `waiting_for` names")
		t.Fatalf("the pr-merged paragraph says %q, but main's text has the issue close once the last of them leaves the workflow by any move, and the same prompt part says %q",
			prMerged[at:at+strings.Index(prMerged[at:], ".")+1], human)
	}
}
