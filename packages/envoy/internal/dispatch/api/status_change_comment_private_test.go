package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAStatusChangesCommentsNameNoIssueOfAnotherPrivateProject (the live reviewer's F12, the
// comments): a status change's comments are stored once, each on its issue, and every reader of
// that issue reads them, so each is held at its write to what all of them may be told. carol, who
// sees both projects, closes a parent with include_children over a PUB child and a PRIV child, in
// triage. alice sees PUB alone.
//
//   - The comment on a PUB parent lists the PUB child's move and leaves the PRIV child's out,
//     counted nowhere (namedComment); alice's read of the parent's comments names the PUB child
//     and not the PRIV one.
//   - Under a PRIV parent, the comment of the PUB child's own step names the parent as an issue of
//     another project, in both places it names the call's issue (statusChangeStep.namedElsewhere);
//     the PRIV child's names the parent by its key, as before. alice's read of the PUB child's
//     comments does not name the PRIV parent.
//   - With PRIV public, so that no project is private, each comment is the one it was: the PUB
//     parent's lists both moves, and the PUB child's names its parent (R9).
//
// Controls: namedComment listing every move (its slices.DeleteFunc over run.unnamed taken out,
// F12-comment.patch), under which the PUB parent's comment lists the PRIV child (the first
// requireComments fails); and call naming the call's issue by its key whatever namedElsewhere says
// (F12-step-comment.patch), under which the PUB child's comment names the PRIV parent.
func TestAStatusChangesCommentsNameNoIssueOfAnotherPrivateProject(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	// family files a parent of project with a PUB child and a PRIV child, each in triage.
	family := func(t *testing.T, project, name string) (parent, pub, priv string) {
		t.Helper()
		parent = seed.issueUnder(t, project, name+", the parent", "")
		pub = seed.issueUnder(t, seedPublic, name+", the PUB child", parent)
		priv = seed.issueUnder(t, seedPrivate, name+", the PRIV child", parent)
		return parent, pub, priv
	}
	closeWithChildren := func(t *testing.T, parent string) {
		t.Helper()
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+parent+"/status-change",
			map[string]any{"status": "done", "include_children": true}, http.StatusOK)
	}
	// stepComment is the comment of the include_children step of key, under the call's issue as
	// the comment names it.
	stepComment := func(key, named string) string {
		return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `%s` to `done` with %s, because %s's status change of %s passed `include_children`.",
			model.IssueStatusTriage, named, seedCarol, named))
	}
	// listed is the comment on named that lists the moves of keys, in their order.
	listed := func(named string, keys ...string) string {
		lines := make([]string, 0, len(keys))
		for _, key := range keys {
			lines = append(lines, movedLine(key, model.IssueStatusTriage, "done", optionIncludeChildren))
		}
		return namedComment(named, seedCarol, "`include_children`", lines...)
	}
	// aliceReads fails t unless alice's read of key's comments names each key of shown and none of
	// hidden.
	aliceReads := func(t *testing.T, key string, shown, hidden []string) {
		t.Helper()
		text := seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, http.StatusOK).Body.String()
		for _, want := range shown {
			if !strings.Contains(text, want) {
				t.Errorf("alice's read of %s's comments does not name %s: %s", key, want, text)
			}
		}
		for _, unwanted := range hidden {
			if strings.Contains(text, unwanted) {
				t.Errorf("alice's read of %s's comments names %s: %s", key, unwanted, text)
			}
		}
	}

	t.Run("the comment on a PUB parent", func(t *testing.T) {
		parent, pub, priv := family(t, seedPublic, "A PUB parent carol closes")
		closeWithChildren(t, parent)
		requireComments(t, seed.database, []string{parent}, listed(parent, pub))
		aliceReads(t, parent, []string{pub}, []string{priv})
	})
	t.Run("the comments of the children of a PRIV parent", func(t *testing.T) {
		parent, pub, priv := family(t, seedPrivate, "A PRIV parent carol closes")
		closeWithChildren(t, parent)
		requireComments(t, seed.database, []string{pub}, stepComment(pub, issueOfAnotherProject))
		requireComments(t, seed.database, []string{priv}, stepComment(priv, parent))
		aliceReads(t, pub, nil, []string{parent})
	})

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	t.Run("with no private project", func(t *testing.T) {
		parent, pub, priv := family(t, seedPublic, "A PUB parent carol closes once PRIV is public")
		closeWithChildren(t, parent)
		requireComments(t, seed.database, []string{parent}, listed(parent, priv, pub))
		aliceReads(t, parent, []string{pub, priv}, nil)
		under, pubUnder, _ := family(t, seedPrivate, "A PRIV parent carol closes once PRIV is public")
		closeWithChildren(t, under)
		requireComments(t, seed.database, []string{pubUnder}, stepComment(pubUnder, under))
		aliceReads(t, pubUnder, []string{under}, nil)
	})
}
