package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's recovered-unrecorded notice (daemon/repositories.go:tellReleaseWaits at d46ec4e0) named a
// child whose parent is recorded now as "%s (its parent %s is recorded now: release %s again)",
// and gave no cause. The head's architect prompt gives that step with a cause: "Release a child of
// `unrecorded` whose parent is recorded now (`parent_recorded: true`) again at once, whatever
// status Dispatch gives it, because the release that left it to that recovery did not write it
// `todo` where an issue above it was not recorded." A todo child that a release left to the
// recovery of its own unread repository (api's enterOnUnreadRepository) was todo already, so the
// release wrote nothing, and no issue above it was unrecorded: its parent is the recorded root. A
// person who moves it out of todo before the recovery leaves it unrecorded, while another todo
// child on the same repository keeps the daemon waiting for it, so the recovery runs and its notice
// names the child with parent_recorded: true.
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it"; the change's own wrong fact is its defect (the architect's ruling).
func TestARecoveredChildsReleaseStepGivesNoCauseTheChildDidNotHave(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs, quickInstallReads)
	const tree, c, e = "LEGION-1660", "LEGION-1661", "LEGION-1662"
	liveRoot(t, w, client, tree)
	client.show(issue(c, "zeta/tools", "todo", false, tree, 1), issue(e, "zeta/tools", "todo", false, tree, 1))
	leftForRecovery(t, w, issue(c, "zeta/tools", "todo", false, tree, 1), tree, tree)
	leftForRecovery(t, w, issue(e, "zeta/tools", "todo", false, tree, 1), tree, tree)
	// A person moves the child to backlog while GitHub does not answer for its repository.
	client.show(issue(c, "zeta/tools", "backlog", false, tree, 2))
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, e); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want the recovery to record it in %s's tree", e, got, tree)
	}
	if got := recordedIssue(t, w, c); got != nil {
		t.Fatalf("%s recorded as %+v while it is backlog", c, got)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !recoveredUnrecorded(t, notices[0], []texts.UnrecordedChild{{Child: c, Parent: tree, ParentRecorded: true}}) {
		t.Fatalf("notices after the recovery = %q, want one naming %s under %s, recorded", notices, c, tree)
	}
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(goPrompts+"architect-common.md")))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		for _, sentence := range strings.SplitAfter(line, ". ") {
			if strings.Contains(sentence, "Release a child of `unrecorded` whose parent is recorded now") &&
				strings.Contains(sentence, "did not write it `todo` where an issue above it was not recorded") {
				t.Errorf("go/architect-common.md says %q, but %s, named in %s, was todo when its release left it to the recovery, under the recorded %s",
					strings.TrimSpace(sentence), c, notices[0], tree)
			}
		}
	}
}
