package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's notice of an unrecorded-blocker stall (api/architect.go:enterOnUnreadRepository at
// d46ec4e0) said "%s is todo but not recorded: the daemon records a child only under a recorded
// parent, and %s, on its parent chain, is %s and not recorded, so the recovery of %s (left out for
// now because GitHub did not answer its App installation read, or its default-branch read past
// GitHub's rate limit) will not record %s either.", where the repository is the child's own when
// that is unread, and otherwise the first unread repository on its unrecorded parent chain (the
// same code at the head). INFRA-3686's R2: "a step that a text gives today goes into the reader's
// prompt where the prompt does not give it"; the change's own wrong fact is its defect (the
// architect's ruling). e74a7f67's passage says of `repo`: "for `unrecorded-blocker` it is the
// child's own". A todo child on a served repository (acme/widgets) under a backlog parent on the
// unread zeta/tools is told with repo zeta/tools, the parent's.
func TestTheUnrecordedBlockersRepoIsTheOneTheArchitectsPromptSays(t *testing.T) {
	const parent, child = "LEGION-230", "LEGION-231"
	u := newUnreadRelease(t,
		on(parent, "zeta/tools", "backlog", unreadRoot, 1), on(child, "acme/widgets", "todo", parent, 2))
	u.release(t, child)
	notices := u.notices(t)
	if len(notices) != 1 || !strings.Contains(notices[0], texts.StallUnrecordedBlocker) {
		t.Fatalf("notices = %q, want one unrecorded-blocker stall for %s", notices, child)
	}
	body, err := os.ReadFile(filepath.Join("..", "prompts", "go", "architect-common.md"))
	if err != nil {
		t.Fatal(err)
	}
	const claim = "for `unrecorded-blocker` it is the child's own"
	if !strings.Contains(string(body), claim) {
		t.Skipf("the architect's prompt no longer says %q", claim)
	}
	if !strings.Contains(notices[0], "repo=acme/widgets ") {
		t.Errorf("the architect's prompt (go/architect-common.md) says %q, but the unrecorded-blocker notice of %s, whose own repository is acme/widgets, names another:\n%s",
			claim, child, notices[0])
	}
}
