package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Main's child-status notice of a re-parented root (workflow/adopt.go:tellReparented at d46ec4e0)
// was queued on root.Key, the root of the live tree whose architect reads it, and its reason named
// the re-parented root first: "%s is now a child of %s in Dispatch, and it stays a tree of its own
// in Legion: %s, so the tree of %s cannot take it in and no architect of that tree can release
// it", with issue.Key, fact.Parent, the reason and root.Key. The branch's ReparentedNotice keeps
// the tree's root in `issue` and names the re-parented root in `child`, while
// go/architect-common.md says "A `child-status` notice names the child (`issue`) and its change
// (`change`)". INFRA-3686's R2: "The fields are those of the kind, the issue, the act, ..."; a
// passage of the reader's prompt that states a fact wrongly for an input the kind can have is the
// change's own defect.
func TestTheArchitectsPromptSaysWhichFieldNamesAReparentedChild(t *testing.T) {
	notice := workflow.ReparentedNotice("LEGION-1", "LEGION-7", "LEGION-3", texts.StaysOwnRan)
	if notice.Issue != "LEGION-1" || notice.Child != "LEGION-7" {
		t.Fatalf("the re-parented child-status notice is issue %q child %q; want issue LEGION-1 (the tree's root) and child LEGION-7", notice.Issue, notice.Child)
	}
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(goPrompts+"architect-common.md")))
	if err != nil {
		t.Fatalf("read architect-common.md: %v", err)
	}
	const says = "A `child-status` notice names the child (`issue`)"
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, says) && notice.Issue != notice.Child {
			t.Errorf("go/architect-common.md says %q, but the child-status notice of change %s names the root of the reader's own tree in `issue` (%s) "+
				"and the child in `child` (%s); main's reason began \"LEGION-7 is now a child of LEGION-3 in Dispatch\"",
				says, notice.Change, notice.Issue, notice.Child)
		}
	}
}
