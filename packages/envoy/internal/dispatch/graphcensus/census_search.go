package graphcensus

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The search topic's lists stand in census_search.go and not in census_windows.go: Go builds a
// file whose name ends in _windows.go for GOOS windows alone.

// nodeSearchKind is the census's and the backfill's name of node_search (migration
// 20261125160000_issue_tree_node_search), the search row of each block node that holds text.
const nodeSearchKind = "node_search"

// searchProjections are the census's kinds of what search reads of a node's text (projections):
// node_search against the text values (R17 of the issue-tree spec). Each row names its
// block's node, and the digest is the row's text against the node's newest text value, so a row
// whose node is gone, whose text is not the node's, or that stands for no node, and a present
// block node whose newest text value asserts and no row stands for, are each a difference. A row
// holds the vector search_vector builds of its text, which one statement writes beside the text
// (graphstore.QueueNodeSearch), so the text is what the census holds it to.
var searchProjections = []projection{{
	name:     nodeSearchKind,
	nodeKind: refs.BlockNodeKind,
	table:    "node_search",
	note: "one row per block node not gone whose newest text value asserts: the digest is the md5 of the row's" +
		" text against the md5 of the text value its node holds now, so a row of a node gone, a row holding" +
		" another text than its node's, and a node holding text with no row are each a difference",
	rows: `
		select '` + nodeSearchKind + ` ' || s.node_address, s.node_kind, s.node_address,
		       md5(s.node_address || '|' || md5(s.text))
		from node_search s`,
	nodes: `
		select n.id, md5(n.id || '|' || md5(held.text))
		from graph_nodes n
		cross join lateral (select ` + currentValue(graphstore.AttributeText) + ` as text) held
		where n.kind = '` + refs.BlockNodeKind + `' and held.text is not null`,
	emptyAgrees: true,
}}

// searchBackfills are the backfill's passes over what search reads of a node's text
// (backfillPasses): the node_search row of every block node whose row does not stand for it
// (backfillNodeSearch).
var searchBackfills = []backfillPass{oneKind(backfillNodeSearch)}

// nodeSearchOwedSQL is the FROM and WHERE text over every block node n whose node_search row does
// not stand for what the store holds of it: a node present whose newest text tuple asserts, whose
// row holds another tuple's text or is missing, and any other node whose row stands. Every writer
// of a node's text or of its state writes its row in its own transaction (graphstore.QueueHeads),
// so what a pass finds here is a node a process of the release before stated gone during a roll,
// which writes no node_search row and no text value, and a row a test seeded the store under.
const nodeSearchOwedSQL = `
	from graph_nodes n
	left join lateral (
		select t.seq, t.asserted from graph_tuples t
		where t.from_kind = n.kind and t.from_id = n.id
		  and t.kind = '` + graphstore.AttributeText + `' and t.to_kind = '` + graphstore.KindValue + `'
		order by t.seq desc
		limit 1
	) said on true
	left join node_search s on s.node_kind = n.kind and s.node_address = n.id
	where n.kind = '` + refs.BlockNodeKind + `'
	  and case when n.state <> '` + graphstore.StateGone + `' and said.asserted
	           then s.text_seq is distinct from said.seq
	           else s.node_address is not null end`

// nodeSearchOwedKeysStatement is backfillNodeSearch's selection: at most $2 of the block nodes
// whose row is owed (nodeSearchOwedSQL), past the id $1, in id order, read off graph_nodes'
// primary key from the cursor on, so a run reads each node once however many batches it takes.
const nodeSearchOwedKeysStatement = `select n.id` + nodeSearchOwedSQL + `
	  and n.id > $1
	order by n.id
	limit $2`

// backfillNodeSearch writes the node_search row of every block node whose row does not stand for
// it (nodeSearchOwedSQL), backfillBatch nodes a transaction (writeNodeSearchBatch), and counts the
// rows still owed once it has walked them all.
func backfillNodeSearch(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: nodeSearchKind}
	after := ""
	for {
		rows, err := database.Pool.Query(ctx, nodeSearchOwedKeysStatement, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the block nodes whose search row is owed: %w", err)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return done, fmt.Errorf("read the block nodes whose search row is owed: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1]
		written, err := writeNodeSearchBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+nodeSearchOwedSQL).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the block nodes whose search row is still owed: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeNodeSearchBatch writes the node_search rows of the block nodes ids in one transaction, and
// answers how many it wrote. It takes each node's row `for share` first, in ascending id, the order
// a node write takes them in (graphstore's queueNodes): a writer that states a node gone or back
// moves its row, so that writer has committed before this transaction reads the node's state, or
// waits until it has committed. A writer that moves a node's text alone moves no node row, and its
// row keeps what it wrote whichever of the two commits first, since the write of a row replaces
// one of a lower text seq alone (graphstore.QueueNodeSearch).
func writeNodeSearchBatch(ctx context.Context, database *store.Store, ids []string) (int64, error) {
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			select 1 from graph_nodes
			where kind = '`+refs.BlockNodeKind+`' and id = any($1::text[])
			order by id
			for share`, ids); err != nil {
			return fmt.Errorf("take the rows of a batch of block nodes: %w", err)
		}
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueNodeSearch(b, ids, &written)
			return nil
		})
	}); err != nil {
		return 0, fmt.Errorf("write a batch of search rows: %w", err)
	}
	return written, nil
}

// movedOwnerWindowsStatement counts the document windows whose owner key is a document converted
// into its issue's room (document_rooms): the windows that document's blocks were embedded in
// before the conversion moved them under the room's tree artifact, which no writer of this build
// writes or deletes again (refs.DocumentWindowOwner). A rollback to the release before searches by
// meaning over them; no search of this build reads them (refs.WindowDocumentSQL); the root's own
// pull request deletes them. Each converted document is one probe of embeddings_window_owner
// under its own key, the kind written as a literal so the planner proves the index's predicate.
const movedOwnerWindowsStatement = `
	select count(*) from document_rooms moved
	join embeddings e on e.kind = '` + refs.DocumentWindowKind + `'
	  and split_part(e.id, '#', 1) = moved.artifact_id::text`

// movedOwnerWindows reads, in the census's snapshot, how many windows the documents converted into
// their issues' rooms still hold under their own artifact ids (movedOwnerWindowsStatement): the one
// class of window the root's own pull request deletes.
func movedOwnerWindows(ctx context.Context, tx pgx.Tx) (int64, error) {
	var windows int64
	if err := tx.QueryRow(ctx, movedOwnerWindowsStatement).Scan(&windows); err != nil {
		return 0, fmt.Errorf("count the windows of the converted documents' old owner keys: %w", err)
	}
	return windows, nil
}

// writeMovedOwnerWindows prints movedOwnerWindows' count on one line of its own, the class the
// root's pull request deletes. It refuses nothing: the rows stay for one release by design.
func writeMovedOwnerWindows(w io.Writer, windows int64) {
	fmt.Fprintf(w, "census: store moved_owner_windows=%s (the windows of the documents converted into their issues' rooms, still under each document's artifact id: a rollback's search by meaning reads them, none of this build's does, and the root's pull request deletes them)\n",
		pgmigrate.Thousands(windows))
}
