package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A release_children of C (todo on the unread zeta/tools under the root) and G (backlog on
// acme/widgets under C) writes G nothing: it is under C, which is left for zeta/tools (api's
// waveRelease, stall: unrecorded-ancestor), so G stays backlog in Dispatch and its wait is
// zeta's. Once GitHub answers, the recovery records C, and G, never written todo, is named in a
// recovered-unrecorded notice with parent_recorded: true. Main's notice gave the step at once:
// "%s (its parent %s is recorded now: release %s again)" (daemon/repositories.go:tellReleaseWaits
// at d46ec4e0); only main's let-go notice (tellLetGoWaits) said "release it again once it is
// todo". INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the
// prompt does not give it". e74a7f67's step row maps the recovered sentence to the let-go
// passage "Otherwise, release the child again once it is `todo`.", which waits for G to be todo,
// which only a release writes.
func TestARecoveredChildWhoseParentIsRecordedIsReleasedAgainWhateverItsStatus(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := repositoryRigWith(t, installs, quickInstallReads)
	const tree, c, g = "LEGION-1650", "LEGION-1651", "LEGION-1652"
	liveRoot(t, w, client, tree)
	client.show(issue(c, "zeta/tools", "todo", false, tree, 1), issue(g, "acme/widgets", "backlog", false, c, 1))
	leftForRecovery(t, w, issue(c, "zeta/tools", "todo", false, tree, 1), tree, tree)
	if !w.awaitRecovery("zeta/tools", tree, g, c) {
		t.Fatalf("the wait for %s on zeta/tools was refused", g)
	}
	before := len(treeNotices(t, w, c))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, c); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want the recovery to record it", c, got)
	}
	if got := recordedIssue(t, w, g); got != nil {
		t.Fatalf("%s recorded as %+v while it is backlog", g, got)
	}
	// The notice is queued on the recorded parent (the owner walk takes it to its architect), as
	// TestARecoveryTellsEachChildItDidNotRecordOnItsRecordedParent says.
	notices := treeNotices(t, w, c)[before:]
	if len(notices) != 1 || !recoveredUnrecorded(t, notices[0], []texts.UnrecordedChild{{Child: g, Parent: c, ParentRecorded: true}}) {
		t.Fatalf("notices after the recovery = %q, want one naming %s under %s, recorded now", notices, g, c)
	}
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, "packages", "daemon", "internal", "prompts", "go", "architect-common.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range goldenSteps {
		if step.kind != texts.ChildReleaseStalled || step.sentence != "%s (its parent %s is recorded now: release %s again)" {
			continue
		}
		if !strings.Contains(string(body), step.passage) {
			t.Fatalf("go/architect-common.md does not hold the row's passage %q", step.passage)
		}
		if strings.Contains(step.passage, "once it is `todo`") {
			t.Errorf("the architect's step for %s, named in %s with parent_recorded: true, is %q, but %s is backlog in Dispatch and only a release writes it todo; main's notice said %q",
				g, notices[0], step.passage, g, g+" (its parent "+c+" is recorded now: release "+g+" again)")
		}
	}
}
