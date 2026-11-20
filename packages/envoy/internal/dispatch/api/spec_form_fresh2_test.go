package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// Red test of the second fresh acceptance run at f61dfa6b66 (D1: D-H1's fix stops at the reads of
// the document whose own approved version Dispatch cannot read). R50: "The Spec holds a list of
// defined terms." The fix of D-H1: when Dispatch cannot read the newest approved version of a
// document under the root, the line that names it comes first, and it says that the list can lack
// terms. X under the root defines lease and is approved at version 1; the agent writes version 2,
// which nobody has approved yet; then X's approved version 1 is made unreadable. The list of the root
// now lacks lease. The read of another document (Y) names X's version 1 in unread: that is the
// control, and it passes. The read of X's own latest version answers the same list, which lacks
// lease, and its unread is empty, so no client can say that the list lacks terms (the command
// `dispatch spec-form --issue X --terms` prints the list as whole).
func TestTheReadOfADocumentNamesItsOwnUnreadableApprovedVersionFresh2(t *testing.T) {
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
