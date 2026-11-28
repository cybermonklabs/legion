package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The read of a document names its own unreadable approved version in unread. R50: "The Spec holds
// a list of defined terms." X under the root defines lease and is approved at version 1; the agent
// writes version 2, which nobody has approved yet; then X's approved version 1 is made unreadable,
// so the list of the root lacks lease. The read of X's version 2 answers that list and names X's
// version 1 in unread, so a client can say that the list can lack terms. The control: the read of
// another document (Y) names it too.
func TestTheReadOfADocumentNamesItsOwnUnreadableApprovedVersion(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	x := createFormIssue(t, tree.handler, tree.root, "X", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
	y := createFormIssue(t, tree.handler, tree.root, "Y", specWithTerms(termLine("gale", "A strong wind of one coast.")))
	reviseDocument(t, tree.handler, x.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, x.PrimaryArtifactID, y.PrimaryArtifactID)
	if read := specFormReadOf(t, tree.handler, y.PrimaryArtifactID); !slices.ContainsFunc(read.Terms, func(term specFormTerm) bool { return term.Term == "lease" }) {
		t.Fatalf("setup: the list of the read of Y = %#v, want lease of %s", read.Terms, x.Key)
	}
	storeUnreadableVersion(t, tree.database, x.PrimaryArtifactID, 1)
	line := specform.UnreadLine("spec.md", 1, x.Key)
	names := func(read specFormRead) bool {
		return slices.ContainsFunc(read.Unread, func(unread specFormUnread) bool { return unread.Line == line })
	}

	if read := specFormReadOf(t, tree.handler, y.PrimaryArtifactID); !names(read) {
		t.Fatalf("control: the read of Y has unread %#v, want the line %q", read.Unread, line)
	}
	read := specFormReadOf(t, tree.handler, x.PrimaryArtifactID)
	lacks := !slices.ContainsFunc(read.Terms, func(term specFormTerm) bool { return term.Term == "lease" })
	if read.Version != 2 || !lacks {
		t.Fatalf("setup: the read of X is version %d with the list %#v, want version 2 and a list that lacks lease", read.Version, read.Terms)
	}
	if !names(read) {
		t.Errorf("the read of X's version 2 answers a list of %d terms that lacks lease, and unread %#v; want the line %q, so that the list says it can lack terms",
			len(read.Terms), read.Unread, line)
	}
}
