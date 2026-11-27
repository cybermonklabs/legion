package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// itemBlockServer is the server the test's own re-index reads mentions with; a dispatch://
// reference reads the same whichever server reads it.
const itemBlockServer = "https://dispatch.example"

// itemBlockSort is one sort of item whose body is a list of blocks: its kind, which is its node's
// kind and the prefix of its blocks' addresses, and the tables of its blocks and of their links,
// keyed by the item's id in column.
type itemBlockSort struct {
	kind          refs.ItemKind
	blocks, links string
	column        string
	// addressForm is how graph_edges renders the near end of an edge out of one of its blocks.
	addressForm string
}

var (
	commentBlocks = itemBlockSort{refs.ItemComment, "comment_blocks", "comment_block_refs", "comment_id", "comment_block"}
	messageBlocks = itemBlockSort{refs.ItemMessage, "message_blocks", "message_block_refs", "message_id", "message_block"}
	askBlocks     = itemBlockSort{refs.ItemAsk, "ask_blocks", "ask_block_refs", "ask_id", "ask_block"}
)

// A comment, an issue message and an ask, each written through its real handler, read back their
// body blocks as `block` nodes whose part_of is the item, each row naming its node, and no node of
// the comment_block, message_block or ask_block kind is written, those staying address forms. The
// block holding a citation reads its link as a tuple out of its node, and the item reads the
// citation as a mentions tuple out of the item's node, its refs row holding tuples_stored. An ask's
// insert carries tuples_stored_at at its created_at. A re-index of the unchanged text appends no
// tuple. An edit that drops the citation appends the retraction of the item's mention and of the
// block's link.
//
// graph_edges answers each relation once: a marked row from the tuples, under the item's block
// address form for a block's edge; an older build's comment, its blocks and its refs row named by
// no node, from the old arms alone; and a document's refs row from the refs arm, which this child
// leaves to the work that moves a document's citations.
func TestItemBodyBlocksAreBlockNodesWithTheirLinksAndCitations(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	cited := createInteractionIssue(t, handler, "IBLC", "The cited issue", "Cited.\n")
	issue := createInteractionIssue(t, handler, "IBLK", "Item blocks", "The spec cites dispatch://"+cited.Key+".\n")
	citing := "Alpha.\n\nBeta cites dispatch://" + cited.Key + "."

	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": citing}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": citing}, "alice")
	question := "Does dispatch://" + cited.Key + " ship?"
	ask := createPartAsk(t, handler, issue.Key, question)

	for _, item := range []struct {
		what  string
		sort  itemBlockSort
		id    string
		parts int
	}{
		{"the comment", commentBlocks, comment.ID, 2},
		{"the message", messageBlocks, message.ID, 2},
		{"the ask", askBlocks, ask.ID, 1},
	} {
		requireItemBlockNodes(t, database, item.what, item.sort, item.id, item.parts)
		requireItemLinkAndCitation(t, database, item.what, item.sort, item.id, cited.Key)
	}
	var none int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_nodes where kind in ('comment_block', 'message_block', 'ask_block')
	`).Scan(&none); err != nil {
		t.Fatalf("count the nodes of an item block kind: %v", err)
	}
	if none != 0 {
		t.Errorf("the store holds %d nodes of the comment_block, message_block or ask_block kind, want none: an item's block is a block node", none)
	}
	var stampedAtCreate bool
	if err := database.Pool.QueryRow(ctx,
		`select tuples_stored_at is not null and tuples_stored_at = created_at from asks where id = $1::uuid`, ask.ID).Scan(&stampedAtCreate); err != nil {
		t.Fatalf("read the ask's tuples_stored_at: %v", err)
	}
	if !stampedAtCreate {
		t.Errorf("the ask's row holds no tuples_stored_at at its created_at, want the insert to set it")
	}

	// A re-index of the unchanged text, as an owed pass or a rebuild reads it, appends nothing.
	before := countTuples(t, database)
	reindexItem(t, database, "the comment", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		return reindexItemBody(ctx, tx, comment.Author, refs.ItemComment, comment.ID, citing)
	})
	reindexItem(t, database, "the message", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		return reindexItemBody(ctx, tx, message.Author, refs.ItemMessage, message.ID, citing)
	})
	reindexItem(t, database, "the ask", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		cited, err := reindexReferences(ctx, tx, ask.Author, refs.ItemAsk, ask.ID, question)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		index, err := refs.ReadItemIndex(ctx, tx, refs.ItemAsk, ask.ID)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		indexing, err := refs.PlanAskParts(index, refs.NewReader(ctx, tx, ask.Author, "ask", ask.ID, itemBlockServer),
			model.Ask{ID: ask.ID, Question: question}, time.Now())
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		if err := store.RunWrite(ctx, tx, indexing.Queue); err != nil {
			return graphstore.Uncaused{}, err
		}
		written := indexing.Stated()
		written.Tuples = append(written.Tuples, cited.Tuples...)
		return written, nil
	})
	if after := countTuples(t, database); after != before {
		t.Errorf("a re-index of unchanged text appended %d tuples, want none", after-before)
	}

	// graph_edges answers each of the marked comment's relations once, from the tuples.
	linkBlock := blockHoldingLink(t, database, commentBlocks, comment.ID, cited.Key)
	requireEdgeOnce(t, database, "the comment's citation", graphstore.KindComment, comment.ID, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the comment block's link", commentBlocks.addressForm, linkBlock, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the comment block's part_of", commentBlocks.addressForm, linkBlock, graphstore.TuplePartOf, graphstore.KindComment, comment.ID)
	requireEdgeOnce(t, database, "the message's citation", graphstore.KindMessage, message.ID, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the ask's citation", graphstore.KindAsk, ask.ID, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the ask part's part_of", askBlocks.addressForm, "ask/"+ask.ID+"#b1", graphstore.TuplePartOf, graphstore.KindAsk, ask.ID)

	// The edit that drops the citation appends the retraction of the comment's mention and of its
	// block's link, and graph_edges answers neither.
	if edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
		"body": "Alpha.\n\nBeta cites nothing.",
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the comment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	requireRetracted(t, database, "the comment's citation", graphstore.KindComment, comment.ID, cited.Key)
	requireRetracted(t, database, "the comment block's link", refs.BlockNodeKind, linkBlock, cited.Key)
	requireEdgeAbsent(t, database, "the dropped citation", graphstore.KindComment, comment.ID, cited.Key)
	requireEdgeAbsent(t, database, "the dropped link", commentBlocks.addressForm, linkBlock, cited.Key)

	// An older build's comment: the row alone, named by no node, indexed by the owed pass, which
	// writes its blocks, their link and its refs row and names no node in any of them. Its old
	// arms answer each relation once, and the store holds no tuple of it.
	var older string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ($1, '{"kind":"user","id":"alice"}', $2)
		returning id::text
	`, issue.Key, citing).Scan(&older); err != nil {
		t.Fatalf("write the older build's comment: %v", err)
	}
	if indexed, err := refs.IndexOwedItemBlocks(ctx, database.Pool, refs.OwedItemBlock{Kind: refs.ItemComment, ID: older}, itemBlockServer); err != nil || !indexed {
		t.Fatalf("index the older build's comment: indexed=%v err=%v", indexed, err)
	}
	olderBlock := blockHoldingLink(t, database, commentBlocks, older, cited.Key)
	requireEdgeOnce(t, database, "the older comment's citation", graphstore.KindComment, older, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the older comment block's link", commentBlocks.addressForm, olderBlock, "mentions", "issue", cited.Key)
	requireEdgeOnce(t, database, "the older comment block's part_of", commentBlocks.addressForm, olderBlock, graphstore.TuplePartOf, graphstore.KindComment, older)
	var olderTuples int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where (from_kind = 'comment' and from_id = $1) or (from_kind = 'block' and from_id like 'comment/' || $1 || '#%')
	`, older).Scan(&olderTuples); err != nil {
		t.Fatalf("count the older comment's tuples: %v", err)
	}
	if olderTuples != 0 {
		t.Errorf("the store holds %d tuples of the older build's comment, want none: its rows name no node", olderTuples)
	}

	// The spec's own citation is a document's refs row, which the refs arm answers.
	var documentStored bool
	if err := database.Pool.QueryRow(ctx, `
		select tuples_stored from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = $2
	`, issue.PrimaryArtifactID, cited.Key).Scan(&documentStored); err != nil {
		t.Fatalf("read the spec's refs row: %v", err)
	}
	if documentStored {
		t.Errorf("the spec's refs row holds tuples_stored, want a document's row left to its refs arm")
	}
	requireEdgeOnce(t, database, "the spec's citation", "artifact", issue.PrimaryArtifactID, "mentions", "issue", cited.Key)
}

// requireItemBlockNodes fails unless item id of sort holds parts current blocks, each row naming
// its `block` node, each node live with one current part_of to the item and nothing else of it.
func requireItemBlockNodes(t *testing.T, database *store.Store, what string, sort itemBlockSort, id string, parts int) {
	t.Helper()
	ctx := context.Background()
	addresses, err := readStrings(ctx, database.Pool, `
		select node_id from `+sort.blocks+`
		where `+sort.column+` = $1::uuid and position is not null
		  and node_kind = $2 and node_address = node_id
		order by position
	`, id, refs.BlockNodeKind)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", what, err)
	}
	if len(addresses) != parts {
		t.Fatalf("%s holds %d current blocks naming their node, want %d", what, len(addresses), parts)
	}
	for _, address := range addresses {
		node, found, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, address)
		if err != nil {
			t.Fatalf("read the node of %s's block %s: %v", what, address, err)
		}
		if !found || node.State != graphstore.StateLive {
			t.Errorf("%s's block %s has no live block node (found=%v)", what, address, found)
		}
		placed, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
		if err != nil {
			t.Fatalf("read the part_of of %s's block %s: %v", what, address, err)
		}
		if len(placed) != 1 || placed[0].ToKind != string(sort.kind) || placed[0].ToID != id {
			t.Errorf("%s's block %s is part of %v, want %s:%s alone", what, address, placed, sort.kind, id)
		}
	}
}

// requireItemLinkAndCitation fails unless the block of item id holding the citation of issue
// reads it as a current mentions tuple out of the block's node, its link row naming that node, and
// the item reads it as a current mentions tuple out of the item's node, its refs row holding
// tuples_stored.
func requireItemLinkAndCitation(t *testing.T, database *store.Store, what string, sort itemBlockSort, id, issue string) {
	t.Helper()
	ctx := context.Background()
	block := blockHoldingLink(t, database, sort, id, issue)
	var named bool
	if err := database.Pool.QueryRow(ctx, `
		select node_kind = $3 and node_address = from_node from `+sort.links+`
		where `+sort.column+` = $1::uuid and from_node = $2 and to_kind = 'issue' and to_id = $4
	`, id, block, refs.BlockNodeKind, issue).Scan(&named); err != nil {
		t.Fatalf("read the link row of %s: %v", what, err)
	}
	if !named {
		t.Errorf("the link row of %s's block %s names no node", what, block)
	}
	if !mentions(t, database, refs.BlockNodeKind, block, issue) {
		t.Errorf("%s's block %s states no current mentions of issue %s", what, block, issue)
	}
	if !mentions(t, database, string(sort.kind), id, issue) {
		t.Errorf("%s states no current mentions of issue %s out of its own node", what, issue)
	}
	var stored bool
	if err := database.Pool.QueryRow(ctx, `
		select tuples_stored from refs where from_kind = $1 and from_id = $2 and to_kind = 'issue' and to_id = $3
	`, string(sort.kind), id, issue).Scan(&stored); err != nil {
		t.Fatalf("read the refs row of %s's citation: %v", what, err)
	}
	if !stored {
		t.Errorf("the refs row of %s's citation holds no tuples_stored", what)
	}
}

// blockHoldingLink is the address of the one block of item id whose link names issue.
func blockHoldingLink(t *testing.T, database *store.Store, sort itemBlockSort, id, issue string) string {
	t.Helper()
	blocks, err := readStrings(context.Background(), database.Pool, `
		select from_node from `+sort.links+` where `+sort.column+` = $1::uuid and to_kind = 'issue' and to_id = $2
	`, id, issue)
	if err != nil {
		t.Fatalf("read the block of %s %s holding its link: %v", sort.kind, id, err)
	}
	if len(blocks) != 1 {
		t.Fatalf("%s %s holds %d links to issue %s, want one", sort.kind, id, len(blocks), issue)
	}
	return blocks[0]
}

// mentions reports whether the node kind:id states a current mentions tuple of issue.
func mentions(t *testing.T, database *store.Store, kind, id, issue string) bool {
	t.Helper()
	current, err := graphstore.CurrentOut(context.Background(), database.Pool, kind, id, []string{"mentions"})
	if err != nil {
		t.Fatalf("read the mentions of %s:%s: %v", kind, id, err)
	}
	return slices.ContainsFunc(current, func(tuple graphstore.Tuple) bool {
		return tuple.ToKind == "issue" && tuple.ToID == issue
	})
}

// requireRetracted fails unless the store holds a retraction of kind:id's mention of issue, and no
// current one.
func requireRetracted(t *testing.T, database *store.Store, what, kind, id, issue string) {
	t.Helper()
	var retractions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = 'mentions' and to_kind = 'issue' and to_id = $3 and not asserted
	`, kind, id, issue).Scan(&retractions); err != nil {
		t.Fatalf("count the retractions of %s: %v", what, err)
	}
	if retractions != 1 || mentions(t, database, kind, id, issue) {
		t.Errorf("%s: the store holds %d retractions and a current mention=%v, want one retraction and no current mention",
			what, retractions, mentions(t, database, kind, id, issue))
	}
}

// requireEdgeOnce fails unless graph_edges answers the edge exactly once.
func requireEdgeOnce(t *testing.T, database *store.Store, what, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if answered := countEdges(t, database, fromKind, fromID, kind, toKind, toID); answered != 1 {
		t.Errorf("graph_edges answers %s (%s %s -%s-> %s %s) %d times, want once", what, fromKind, fromID, kind, toKind, toID, answered)
	}
}

// requireEdgeAbsent fails unless graph_edges answers no mentions of issue out of fromKind:fromID.
func requireEdgeAbsent(t *testing.T, database *store.Store, what, fromKind, fromID, issue string) {
	t.Helper()
	if answered := countEdges(t, database, fromKind, fromID, "mentions", "issue", issue); answered != 0 {
		t.Errorf("graph_edges answers %s %d times, want none", what, answered)
	}
}

func countEdges(t *testing.T, database *store.Store, fromKind, fromID, kind, toKind, toID string) int {
	t.Helper()
	var answered int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_edges
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, fromKind, fromID, kind, toKind, toID).Scan(&answered); err != nil {
		t.Fatalf("count the edges of %s:%s: %v", fromKind, fromID, err)
	}
	return answered
}

// countTuples is how many tuples the store holds.
func countTuples(t *testing.T, database *store.Store) int {
	t.Helper()
	var tuples int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from graph_tuples`).Scan(&tuples); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	return tuples
}

// reindexReferences reconciles the references of item kind/itemID with body, as an owed pass
// reads them for writer, the author its row records, and answers the citations that states.
func reindexReferences(ctx context.Context, tx pgx.Tx, writer model.Actor, kind refs.ItemKind, itemID, body string) (graphstore.Uncaused, error) {
	found := text.Refs(refs.NewReader(ctx, tx, writer, string(kind), itemID, itemBlockServer).Mentions(body))
	replacement, _, err := planFoundReferences(ctx, tx, string(kind), itemID, found, nil, nil, time.Now().UTC())
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}); err != nil {
		return graphstore.Uncaused{}, err
	}
	return replacement.Cited(), nil
}

// reindexItemBody re-indexes a comment's or a message's body, written by writer: its references
// and its blocks with their links, and answers what both state.
func reindexItemBody(ctx context.Context, tx pgx.Tx, writer model.Actor, kind refs.ItemKind, itemID, body string) (graphstore.Uncaused, error) {
	cited, err := reindexReferences(ctx, tx, writer, kind, itemID, body)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	located := refs.NewReader(ctx, tx, writer, string(kind), itemID, itemBlockServer).Mentions(body)
	_, written, err := refs.IndexItem(ctx, tx, kind, itemID, body, itemBlockServer, time.Now().UTC(), located, nil)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	written.Tuples = append(written.Tuples, cited.Tuples...)
	return written, nil
}

// reindexItem runs index in a transaction of its own and appends what it states at the sentinel,
// as an owed pass does, then commits.
func reindexItem(t *testing.T, database *store.Store, what string, index func(tx pgx.Tx) (graphstore.Uncaused, error)) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the re-index of %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	written, err := index(tx)
	if err != nil {
		t.Fatalf("re-index %s: %v", what, err)
	}
	if err := graphstore.Caused(ctx, tx, written, graphstore.NoEvent); err != nil {
		t.Fatalf("state the re-index of %s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the re-index of %s: %v", what, err)
	}
}
