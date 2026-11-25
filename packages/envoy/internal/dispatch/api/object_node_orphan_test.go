package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The block-index tick's sweep states an orphaned document's node gone as this build's removal of
// the document states it, so the store holds one form of a removed document whichever build
// served the removal. Two documents of one issue are removed: one through this build's route
// (removeArtifact), and one as the release before this one removes it - its row goes and its
// artifact.removed event is appended, and nothing states the node gone - after which the sweep
// states that node gone (graphcensus.StateOrphanedGone). The two nodes then agree in every field
// but their address, each states live and then gone over its life, and each gone statement is
// caused by its own removal's event. Only who stated it differs: the person who removed it through
// the route, and the block-index tick for the sweep, whose removal event names the person. The
// census reads the store complete.
//
// Its control is the sweep's gone statement caused at the sentinel: the swept node's gone
// statement then names no event, where the removal's names its own.
func TestTheSweepStatesAnOrphanGoneAsTheRemovalStatesIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "SWP", "A document each build removes", "The spec.\n")

	here := emptyDocument(t, handler, issue.Key, "Removed here.md")
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/artifacts/"+here.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("remove %s through the route: %d %s", here.ID, response.Code, response.Body.String())
	}
	hereRemoval := artifactEvent(t, database, "artifact.removed", here.ID)

	there := emptyDocument(t, handler, issue.Key, "Removed there.md")
	thereRemoval := removeAsTheReleaseBefore(t, database, there)
	if err := graphcensus.StateOrphanedGone(ctx, database, there.RefKey); err != nil {
		t.Fatalf("sweep the orphaned node of %s: %v", there.RefKey, err)
	}

	removed, swept := readDocumentNode(t, database, here.RefKey), readDocumentNode(t, database, there.RefKey)
	if removed.Supertag != swept.Supertag || removed.Visibility != swept.Visibility || removed.State != swept.State ||
		removed.Project != swept.Project || !slices.Equal(removed.Parties, swept.Parties) || removed.MadeBy != swept.MadeBy ||
		swept.State != graphstore.StateGone {
		t.Errorf("the removed node is %+v and the swept one %+v, want them alike but for their address, and gone", removed, swept)
	}
	for _, node := range []struct {
		ref     string
		removal int64
		who     model.Actor
	}{
		{here.RefKey, hereRemoval, model.Actor{Kind: "user", ID: "alice"}},
		{there.RefKey, thereRemoval, model.Actor{Kind: "system", ID: "dispatch-block-index"}},
	} {
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, node.ref, graphstore.AttributeState)
		if err != nil || len(history) != 2 || history[0].ToID != graphstore.StateLive || history[1].ToID != graphstore.StateGone {
			t.Fatalf("the state history of %s is %+v (%v), want live then gone", node.ref, history, err)
		}
		if gone := history[1]; gone.SourceEvent != node.removal || gone.Who.Kind != node.who.Kind || gone.Who.ID != node.who.ID || !gone.Asserted {
			t.Errorf("the gone statement of %s is %+v, want it asserted, caused by its removal %d and stated by %+v", node.ref, gone, node.removal, node.who)
		}
	}
	requireProjectsComplete(t, database)
}

// removeAsTheReleaseBefore removes the untouched document as the release before this one does,
// in one transaction: the document's owner lock first, then the rows removeArtifact clears, the
// artifact row and an artifact.removed event naming it, as that release appends one, and no node
// written or stated gone. It answers the event's id.
func removeAsTheReleaseBefore(t *testing.T, database *store.Store, artifact model.Artifact) int64 {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(map[string]any{"artifact": artifact})
	if err != nil {
		t.Fatal(err)
	}
	var removal int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		owners.Add(*artifact.IssueKey, artifact.ID)
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		if err := deleteArtifactRows(ctx, tx, artifact); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select $1, coalesce(max(seq), 0) + 1, 'artifact.removed', '{"kind":"user","id":"alice"}', $2::jsonb, false
			from events where issue_key = $1
			returning id
		`, *artifact.IssueKey, payload).Scan(&removal)
	}); err != nil {
		t.Fatalf("remove %s as the release before does: %v", artifact.ID, err)
	}
	return removal
}

// readDocumentNode reads the document node at ref, which has to be there.
func readDocumentNode(t *testing.T, database *store.Store, ref string) graphstore.Node {
	t.Helper()
	node, held, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindArtifact, ref)
	if err != nil || !held {
		t.Fatalf("read the node of %s: held %t, %v", ref, held, err)
	}
	return node
}
