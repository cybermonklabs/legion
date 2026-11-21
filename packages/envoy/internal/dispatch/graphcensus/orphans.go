package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sweepActor is who states an orphaned document's node gone: the block-index tick of this
// build, while it owes the object pass (docs' writeObjectNodesAnotherBuildWrote). The removal it
// completes was a person's, and the event that removal appended names them.
var sweepActor = model.Actor{Kind: "system", ID: "dispatch-block-index"}

// OrphanedDocumentNodes answers, in address order, at most limit of the document nodes the
// project census reads as orphaned at an issue's address: a node at any state but gone at an
// address no artifacts row holds, whose address names an issue. It reads through the artifacts
// table's unique index on ref_key and the issues table's key, so a read with nothing to find costs
// an index scan of the document nodes alone (README's cost of the pass).
//
// A document is the one kind of the four whose row any build deletes: no build deletes an issue,
// an ask or a message, and none of the four tables cascades a delete from another. Only an issue's
// document is removable, by this build's removal and by the one before it. This build's removal
// states the node gone in the transaction that deletes the row (api's removeArtifact); a build
// before this one writes no node and states none gone, so a document it removes after this build
// wrote the node leaves the node standing behind no row, which is all this read can find. No
// writer of this build commits a node before its row: each writes it in the transaction that
// writes its row.
func OrphanedDocumentNodes(ctx context.Context, q graphstore.Queryer, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		select n.id from graph_nodes n
		where n.kind = '`+graphstore.KindArtifact+`' and `+orphanedDocumentNode("n")+`
		order by n.id
		limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("read the orphaned document nodes: %w", err)
	}
	orphaned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the orphaned document nodes: %w", err)
	}
	return orphaned, nil
}

// orphanedDocumentNode is, for the document node n, whether it is orphaned at an issue's address:
// at any state but gone, at an address no artifacts row holds, and naming an issue.
func orphanedDocumentNode(n string) string {
	return n + `.state <> '` + graphstore.StateGone + `'
		  and not exists (select 1 from artifacts a where a.ref_key = ` + n + `.id)
		  and exists (select 1 from issues i where i.key = split_part(` + n + `.id, '/', 1))`
}

// ErrNotOrphaned is StateOrphanedGone's answer for a node it may not state gone, which it leaves
// as it is: the store holds no such node, or holds it gone already, or a row stands at its address
// by the time the call reads under its lock of the node, or its address is no issue's document.
var ErrNotOrphaned = errors.New("the node is not an orphaned document's")

// StateOrphanedGone states the orphaned document node at ref gone, as this build's removal of the
// document would have stated it (api's removeArtifact): the node as it stands, its state gone, at
// the time the store dates the address's next statement (graphstore.StatementTime), caused by the
// artifact.removed event the release before this one appended for the removal, the newest one of
// its issue naming the address, and at the sentinel where there is none. Only who states it
// differs, the block-index tick (sweepActor), since the person who removed it is the event's.
//
// It takes the store's lock order: the issue's owner lock first (events.LockOwnerRows), as the
// removal and every create of a document of that issue take it, then the node's row `for
// update`, and only under that lock reads whether a row stands at the address. A create of this
// build writes the node and then the row in one transaction under that owner lock, so a create
// that got there first has committed its row when the read runs, and the node is left: the answer
// is ErrNotOrphaned, as it is for every node it leaves. An address that names no issue is no
// removable document's, which only an issue's are, and is left too.
func StateOrphanedGone(ctx context.Context, database *store.Store, ref string) error {
	issue, _, found := strings.Cut(ref, "/")
	if !found {
		return fmt.Errorf("%w: %s holds no issue key", ErrNotOrphaned, ref)
	}
	return pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var isIssue bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from issues where key = $1)`, issue).Scan(&isIssue); err != nil {
			return fmt.Errorf("read the issue of %s: %w", ref, err)
		}
		if !isIssue {
			return fmt.Errorf("%w: %s is no issue's document", ErrNotOrphaned, ref)
		}
		owners := events.OwnerRows{}
		owners.Add(issue, "", "the issue of document "+ref)
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		var state string
		switch err := tx.QueryRow(ctx, `select state from graph_nodes where kind = $1 and id = $2 for update`,
			graphstore.KindArtifact, ref).Scan(&state); {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: the store holds no node %s", ErrNotOrphaned, ref)
		case err != nil:
			return fmt.Errorf("lock the node of %s: %w", ref, err)
		case state == graphstore.StateGone:
			return fmt.Errorf("%w: the node of %s is gone", ErrNotOrphaned, ref)
		}
		if stands, err := ObjectRowStands(ctx, tx, graphstore.KindArtifact, ref); err != nil {
			return err
		} else if stands {
			return fmt.Errorf("%w: a row stands at %s", ErrNotOrphaned, ref)
		}
		node, _, err := graphstore.ReadNode(ctx, tx, graphstore.KindArtifact, ref)
		if err != nil {
			return err
		}
		removal := graphstore.NoEvent
		if err := tx.QueryRow(ctx, `
			select coalesce(max(id), $3) from events
			where issue_key = $1 and type = 'artifact.removed' and payload->'artifact'->>'ref_key' = $2
		`, issue, ref, graphstore.NoEvent).Scan(&removal); err != nil {
			return fmt.Errorf("read the removal event of %s: %w", ref, err)
		}
		removedAt, err := graphstore.StatementTime(ctx, tx, graphstore.KindArtifact, ref)
		if err != nil {
			return err
		}
		gone := graphstore.ArtifactNode(ref, node.Project, sessionParty(node.Parties), sweepActor, removedAt)
		gone.State = graphstore.StateGone
		uncaused := graphstore.PlanUncaused([]graphstore.Node{gone}, nil)
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueNodes(b, uncaused)
			return nil
		}); err != nil {
			return fmt.Errorf("state the orphaned node of %s gone: %w", ref, err)
		}
		return graphstore.Caused(ctx, tx, uncaused, removal)
	})
}

// sessionParty is the session a document node's parties name, the empty string for a node of a
// project: graphstore.ArtifactNode takes the session and writes its party.
func sessionParty(parties []string) string {
	for _, party := range parties {
		if session, ok := strings.CutPrefix(party, "session:"); ok {
			return session
		}
	}
	return ""
}
