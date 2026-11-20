package grapherase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeAsThisBuild runs write in a transaction of its own, as this release's writers run theirs,
// and appends what it held for its cause at the sentinel, then commits.
func writeAsThisBuild(t *testing.T, database *store.Store, what string, write func(tx pgx.Tx) (graphstore.Uncaused, error)) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	held, err := write(tx)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := graphstore.Caused(ctx, tx, held, graphstore.NoEvent); err != nil {
		t.Fatalf("state %s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// openAttempt writes the node of the attempt key names before the row insert writes that names
// it, and what the row then holds, in one write phase, as this release's writers open one
// (graphstore.AttemptOpened, graphstore.QueueAttempted), and answers the node held for its cause.
func openAttempt(ctx context.Context, tx pgx.Tx, key graphstore.AttemptKey, insert string, args ...any) (graphstore.Uncaused, error) {
	var at time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&at); err != nil {
		return graphstore.Uncaused{}, err
	}
	kind, address, opened := graphstore.AttemptOpened(key, graphstore.AttemptWriter, at)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := graphstore.QueueNodes(b, opened); err != nil {
			return err
		}
		b.Queue("open the attempt", insert, append(args, kind, address)...)
		return graphstore.QueueAttempted(b, graphstore.AttemptWriter, graphstore.NoEvent, key)
	}); err != nil {
		return graphstore.Uncaused{}, err
	}
	return opened, nil
}

// indexItem indexes the body of comment or message itemID, whose row the calling transaction
// holds, as this release's item writers index one (api's planItemWrite): its blocks and the
// mentions each holds (refs.PlanItemIndex), then its own references (refs.PlanItemReplace), from
// one reading of body, queued as one write phase. It answers what the pass states in the store,
// held for the cause the caller gives it: the block nodes, each block's part_of and links, and
// the item's citations.
func indexItem(ctx context.Context, tx pgx.Tx, kind refs.ItemKind, itemID, body string, at time.Time) (graphstore.Uncaused, error) {
	index, err := refs.ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	located := refs.NewReader(ctx, tx, string(kind), itemID, eraseServerURL).Mentions(body)
	indexing, err := refs.PlanItemIndex(index, kind, itemID, body, eraseServerURL, at, located, nil)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	scope, err := refs.ReadItemScope(ctx, tx, kind, itemID)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	replacement, err := refs.PlanItemReplace(ctx, tx, scope, kind, itemID, text.Refs(located), at)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := indexing.Queue(b); err != nil {
			return err
		}
		replacement.Queue(b)
		return nil
	}); err != nil {
		return graphstore.Uncaused{}, err
	}
	stated := indexing.Stated()
	stated.Tuples = append(stated.Tuples, replacement.Cited().Tuples...)
	return stated, nil
}

// heldOfParts counts what the store holds of the nodes of kind whose ids start prefix, and of the
// tuples on both sides of them.
func heldOfParts(t *testing.T, database *store.Store, kind, prefix string) int64 {
	t.Helper()
	var held int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes where kind = $1 and starts_with(id, $2))
		     + (select count(*) from graph_tuples where (from_kind = $1 and starts_with(from_id, $2))
		                                         or (to_kind = $1 and starts_with(to_id, $2)))
	`, kind, prefix).Scan(&held); err != nil {
		t.Fatalf("count the %s nodes under %s: %v", kind, prefix, err)
	}
	return held
}

// A comment's mention is a delivery_attempt node, and its body's blocks are block nodes. The erase
// of the comment lists and deletes both with it, and every tuple on either side of them, since the
// rows that name them - its comment_deliveries row and its comment_blocks row - go with its
// comments row.
func TestTheEraseOfACommentTakesItsAttemptsAndItsBlocks(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	key := graphstore.CommentAttempt(seeded.comment, "session:s1", 1)
	writeAsThisBuild(t, database, "the comment's blocks and its mention's attempt", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		indexed, err := indexItem(ctx, tx, refs.ItemComment, seeded.comment, "A remark on the spec.", time.Now().UTC())
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		opened, err := openAttempt(ctx, tx, key, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, node_kind, node_address)
			values ($1::uuid, $2, 1, 'steer', 's1', 'pending', $3, $4)`, seeded.comment, "session:s1")
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		indexed.Nodes = append(indexed.Nodes, opened.Nodes...)
		indexed.Tuples = append(indexed.Tuples, opened.Tuples...)
		return indexed, nil
	})
	blocks := seeded.comment + "#"
	if heldOfParts(t, database, graphstore.KindDeliveryAttempt, key.ID()) == 0 || heldOfParts(t, database, graphstore.KindBlock, "comment/"+blocks) == 0 {
		t.Fatalf("the store holds no attempt or no block node of comment %s, so this test would prove nothing", seeded.comment)
	}

	listing := dryRun(t, database, commentNodeKind, seeded.comment)
	if !listed(listing, "delete graph_nodes "+graphstore.KindDeliveryAttempt+":"+key.ID()) ||
		!listed(listing, "delete graph_nodes "+graphstore.KindBlock+":comment/"+blocks+"b1") {
		t.Errorf("the listing of comment:%s reads %q, want its attempt's node and its block's node named for deletion", seeded.comment, listing.Listing)
	}
	requireErased(t, database, commentNodeKind, seeded.comment)
	if held := heldOfParts(t, database, graphstore.KindDeliveryAttempt, key.ID()); held != 0 {
		t.Errorf("after the erase the store holds %d row(s) of comment %s's attempt node and its tuples, want none", held, seeded.comment)
	}
	if held := heldOfParts(t, database, graphstore.KindBlock, "comment/"+blocks); held != 0 {
		t.Errorf("after the erase the store holds %d row(s) of comment %s's block nodes and their tuples, want none", held, seeded.comment)
	}
}

// A message is a node its messages row names (migration 20261116114515), with block nodes of its
// body and a delivery_attempt node of its send. While it stands, the erase of the person who wrote
// it refuses, naming its authored_by, since its row says so. The erase of the message deletes its
// row, the rows that cannot exist without it, its node, its parts and every tuple of them, and the
// person can be erased after it.
func TestTheEraseOfAMessageTakesItsRowAndItsParts(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedPreStoreDocument(t, database)
	const writer = "erase-messages-person"
	author := model.Actor{Kind: "user", ID: writer}
	message := "00000000-0000-4000-8000-000000001152"
	key := graphstore.MessageAttempt(message, 1)
	writeAsThisBuild(t, database, "the message", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		at := time.Now().UTC()
		row := graphstore.MessageRow{ID: message, IssueKey: "CORE-1", Target: "session:s1"}
		nodes, tuples, _ := graphstore.MessageFacts([]graphstore.Node{graphstore.MessageNode(message, "CORE", nil, author, at)}, nil, row, author, at, graphstore.NoEvent)
		held, err := graphstore.WriteUncaused(ctx, tx, nodes, tuples)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into messages (id, issue_key, author, body, target, created_at, node_kind, node_address)
			values ($1::uuid, 'CORE-1', $2, 'A message nobody may keep.', 'session:s1', $3, 'message', $1)
		`, message, `{"kind":"user","id":"`+writer+`"}`, at); err != nil {
			return graphstore.Uncaused{}, err
		}
		if err := graphstore.Authored(ctx, tx, graphstore.KindMessage, message, author, at, graphstore.NoEvent); err != nil {
			return graphstore.Uncaused{}, err
		}
		indexed, err := indexItem(ctx, tx, refs.ItemMessage, message, "A message nobody may keep.", at)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		opened, err := openAttempt(ctx, tx, key, `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, node_kind, node_address)
			values ($1::uuid, 1, 'steer', 's1', 'pending', $2, $3)`, message)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		for _, part := range []graphstore.Uncaused{indexed, opened} {
			held.Nodes = append(held.Nodes, part.Nodes...)
			held.Tuples = append(held.Tuples, part.Tuples...)
		}
		return held, nil
	})
	if heldOfParts(t, database, graphstore.KindDeliveryAttempt, key.ID()) == 0 || heldOfParts(t, database, graphstore.KindBlock, "message/"+message+"#") == 0 {
		t.Fatalf("the store holds no attempt or no block node of message %s, so this test would prove nothing", message)
	}
	// The message's own event carries its body (model.MessageEventPayload), and so does another
	// message's that says the same words, which is that message's text.
	const body = "A message nobody may keep."
	own := appendEvent(t, database, "CORE-1", "message.created",
		`{"id":"`+message+`","issue_key":"CORE-1","body":"`+body+`","target":"session:s1"}`)
	other := appendEvent(t, database, "CORE-1", "message.created",
		`{"id":"00000000-0000-4000-8000-000000002304","issue_key":"CORE-1","body":"`+body+`"}`)

	requireRefused(t, database, graphstore.KindPerson, writer, graphstore.KindMessage+":"+message+" states "+graphstore.TupleAuthoredBy)
	requireErased(t, database, graphstore.KindMessage, message)
	if _, raw := eventPayload(t, database, own); strings.Contains(raw, body) || !strings.Contains(raw, "[erased by envoy-dispatch erase message:"+message) {
		t.Errorf("after the erase the message's own message.created reads %s, want its body replaced by the erase's marker", raw)
	}
	if _, raw := eventPayload(t, database, other); !strings.Contains(raw, body) {
		t.Errorf("after the erase another message's message.created reads %s, want its own words left standing", raw)
	}
	var naming int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = 'message' and id = $1)
		     + (select count(*) from graph_tuples where (from_kind = 'message' and from_id = $1) or (to_kind = 'message' and to_id = $1))
		     + (select count(*) from messages where id::text = $1 or (node_kind = 'message' and node_address = $1))
		     + (select count(*) from message_deliveries where message_id::text = $1)
		     + (select count(*) from message_blocks where message_id::text = $1)
	`, message).Scan(&naming); err != nil {
		t.Fatalf("count what names message:%s: %v", message, err)
	}
	if naming != 0 {
		t.Errorf("after the erase %d row(s) of the node, its tuples or its rows name message:%s, want none", naming, message)
	}
	for _, part := range []struct{ kind, prefix string }{
		{graphstore.KindDeliveryAttempt, key.ID()},
		{graphstore.KindBlock, "message/" + message + "#"},
	} {
		if held := heldOfParts(t, database, part.kind, part.prefix); held != 0 {
			t.Errorf("after the erase the store holds %d row(s) of message %s's %s nodes and their tuples, want none", held, message, part.kind)
		}
	}
	requireErased(t, database, graphstore.KindPerson, writer)
}

// A comment whose body cites a block states the link out of its own block's node, and its
// comment_block_refs row names that node (migration 20261116140000). While it stands, the erase of
// the cited block refuses, naming the comment's block, as it names a document's block that cites it.
func TestTheEraseRefusesABlockAnItemsBlockCites(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	body := "See dispatch://" + seeded.document.refKey + "#b-inner for the detail."
	if _, err := database.Pool.Exec(ctx, `update comments set body = $2 where id = $1::uuid`, seeded.comment, body); err != nil {
		t.Fatalf("write the comment's body: %v", err)
	}
	writeAsThisBuild(t, database, "the comment's citing body", func(tx pgx.Tx) (graphstore.Uncaused, error) {
		return indexItem(ctx, tx, refs.ItemComment, seeded.comment, body, time.Now().UTC())
	})
	var links int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from comment_block_refs where comment_id = $1::uuid and to_id = $2 and node_kind = 'block'
	`, seeded.comment, seeded.inner).Scan(&links); err != nil || links != 1 {
		t.Fatalf("the comment's block holds %d marked link(s) to %s (err %v), want one, or this test would prove nothing", links, seeded.inner, err)
	}
	requireRefused(t, database, refs.BlockNodeKind, seeded.inner, refs.BlockNodeKind+":comment/"+seeded.comment+"#b1 states mentions")
}
