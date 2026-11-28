package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// linkStateSpec is the spec of the link-state test: two requirements and a decision that shaped
// the first.
const linkStateSpec = ":::requirement{#r1}\nThe service answers over one transport.\n:::\n\n" +
	":::requirement{#r2}\nThe service runs in one region first.\n:::\n\n" +
	":::decision{#d1 shaped=\"dispatch://CORE-1/spec#b-r1\"}\nWe ship REST first.\n:::\n"

// decisionLinksOf is every link the decision whose block is d1 answers on the issue key's two
// reads of its decisions: the Decisions view of the tree read, which the page reads, and the
// Decisions read a page of the release before reads.
func decisionLinksOf(t *testing.T, handler http.Handler, key string) map[string][]model.DecisionLink {
	t.Helper()
	read := map[string][]model.DecisionLink{}
	for _, node := range treeRead(t, handler, key, "?view=decisions", "alice", false).Nodes {
		if node.Decision != nil && node.Decision.BlockID == "d1" {
			read["the tree read's Decisions view"] = node.Decision.Links
		}
	}
	for _, decision := range readIssueDecisions(t, handler, key).Decisions {
		if decision.BlockID == "d1" {
			read["the Decisions read"] = decision.Links
		}
	}
	if len(read) != 2 {
		t.Fatalf("the decision d1 of %s is read on %d of its two reads: %s", key, len(read), entryJSON(t, read))
	}
	return read
}

// requireLinkState holds every link of d1 on both reads to the one target r1, answering the state
// and the visibility the store holds of r1's block node.
func requireLinkState(t *testing.T, when string, links map[string][]model.DecisionLink, target, state string) {
	t.Helper()
	for read, held := range links {
		if len(held) != 1 || held[0].To.ID != target {
			t.Fatalf("%s %s answers d1's links %s, want one, to %s", when, read, entryJSON(t, held), target)
		}
		if to := held[0].To; to.State != state || to.Visibility != graphstore.VisibilityProject {
			t.Errorf("%s %s answers d1's link to %s at state %q and visibility %q, want %q and %q, as the store holds its node",
				when, read, target, to.State, to.Visibility, state, graphstore.VisibilityProject)
		}
	}
}

// A decision's link answers the state its target's block node holds in the store, on the tree
// read's Decisions view and on the Decisions read alike: live while the spec holds the block, and
// gone once a version of the spec removes it, while the decision still shapes it. Read without the
// store's state, each link answered an empty state and visibility, which neither the contract's
// states nor its visibilities admit.
func TestADecisionLinkAnswersTheStateItsTargetHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", linkStateSpec)
	if issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which the decision's shaped link names", issue.Key)
	}
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	settleIssue(t, documents, issue.Key)
	target := spec.pointed("r1")

	requireLinkState(t, "while the spec holds r1,", decisionLinksOf(t, handler, issue.Key), target, graphstore.StateLive)

	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1, map[string]string{"op": "delete", "block": "r1"})
	settleIssue(t, documents, issue.Key)
	requireLinkState(t, "once the spec's version removes r1,", decisionLinksOf(t, handler, issue.Key), target, graphstore.StateGone)
}
