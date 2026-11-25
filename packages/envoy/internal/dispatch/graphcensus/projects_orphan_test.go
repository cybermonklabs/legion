package graphcensus

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The project census reads its kinds from the rows: a row with no node, with a node naming no
// project, or with a node in another project. It reads the other direction too for the four kinds
// whose rows only their own creates write - an issue, a document, an ask and a message - since
// the store census reads that direction for a comment and a block already: a live node of one of
// those kinds that no row stands behind is a node a read limited to its project would answer for
// an object that is gone. A node the removal of its object stated gone is no finding. No issue row
// stands at ORPH-1, so the document node is at no issue's address, which no release's removal
// leaves and the block-index tick does not state gone: each of the four lines says it has no
// in-band cure, since only a delete outside the builds leaves such a node
// (TestAnOrphanedDocumentNodeIsStatedGoneUnderItsRemoval reads the cure of a document node at an
// issue's address).
//
// Its control is the orphaned class taken out of the census: the four live nodes with no row then
// read as nothing at all.
func TestTheProjectCensusNamesALiveNodeWhoseRowIsGone(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	alice := model.Actor{Kind: "user", ID: "alice"}
	gone := graphstore.ArtifactNode("ORPH-1/removed-md", "ORPH", "", alice, now)
	gone.State = graphstore.StateGone
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("ORPH-1", "ORPH", alice, now),
			graphstore.ArtifactNode("ORPH-1/notes-md", "ORPH", "", alice, now),
			graphstore.AskNode("33333333-3333-4333-8333-000000000001", "ORPH", nil, alice, now),
			graphstore.MessageNode("33333333-3333-4333-8333-000000000002", "ORPH", nil, alice, now),
			gone,
		}, nil)
	}); err != nil {
		t.Fatalf("write the nodes no row stands behind: %v", err)
	}

	census, err := ProjectCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	for _, kind := range census {
		want := int64(0)
		if slices.Contains([]string{graphstore.KindIssue, graphstore.KindArtifact, graphstore.KindAsk, graphstore.KindMessage}, kind.Kind) {
			want = 1
		}
		if kind.Orphaned != want || kind.Findings() != want {
			t.Errorf("the census reads %+v for %s, want orphaned=%d and that many findings", kind, kind.Kind, want)
		}
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete nodes: %v", err)
	}
	want := []string{
		"artifact ORPH-1/notes-md: orphaned; no in-band cure: a delete outside the builds made it",
		"ask 33333333-3333-4333-8333-000000000001: orphaned; no in-band cure: a delete outside the builds made it",
		"issue ORPH-1: orphaned; no in-band cure: a delete outside the builds made it",
		"message 33333333-3333-4333-8333-000000000002: orphaned; no in-band cure: a delete outside the builds made it",
	}
	if !slices.Equal(incomplete, want) {
		t.Errorf("ProjectComplete answers %v, want %v: the four live nodes no row stands behind, none of which the tick states gone, and not the gone one", incomplete, want)
	}
}
