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
// an object that is gone. A node the removal of its object stated gone is no finding.
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
		"artifact ORPH-1/notes-md: orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone",
		"ask 33333333-3333-4333-8333-000000000001: orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone",
		"issue ORPH-1: orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone",
		"message 33333333-3333-4333-8333-000000000002: orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone",
	}
	if !slices.Equal(incomplete, want) {
		t.Errorf("ProjectComplete answers %v, want %v: the four live nodes no row stands behind, each with no in-band cure, and not the gone one", incomplete, want)
	}
}

// An orphaned node's line names the node by its address as it stands and ends with its cure, which
// is none in band. A conversation's file belongs to no project and is addressed by its session's
// id, which the session names, here one holding a quote and a dollar sign: the line names it
// whole, `artifact <address>: orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone`.
//
// Its control is the cure taken off the orphaned class's line (orphanedCure): the line then ends
// at its class.
func TestAnOrphanedNodesLineNamesItsAddressWhole(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	session := `it's$HOME`
	ref := "agent/" + session + "/notes-md"
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.ArtifactNode(ref, "", session, model.Actor{Kind: "session", ID: session}, time.Now().UTC()),
		}, nil)
	}); err != nil {
		t.Fatalf("write the conversation file's node no row stands behind: %v", err)
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete nodes: %v", err)
	}
	if want := []string{"artifact " + ref + ": orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone"}; !slices.Equal(incomplete, want) {
		t.Errorf("ProjectComplete answers %q, want %q", incomplete, want)
	}
}
