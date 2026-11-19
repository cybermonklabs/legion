package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestTheObjectBatchDistinguishesReplacedRowsWithTheSameCreationTime(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seedIdentityIssue(t, database, at, model.Actor{Kind: "user", ID: "alice"})
	first := insertIdentityDocument(t, database, "alice", at)
	insertCreateEvent(t, database, 1, first)
	kind := objectKindOf(t, graphstore.KindArtifact)
	batch, err := readObjectNodes(ctx, database, kind.kind, kind.read, kind.start, backfillBatch)
	if err != nil || len(batch) != 1 {
		t.Fatalf("read the first row: %d node(s), %v; want 1", len(batch), err)
	}
	if _, err := database.Pool.Exec(ctx, `delete from artifacts where id = $1`, first); err != nil {
		t.Fatal(err)
	}
	second := insertIdentityDocument(t, database, "carol", at)
	insertCreateEvent(t, database, 2, second)
	written, err := writeObjectBatch(ctx, database, kind, batch)
	if err != nil {
		t.Fatal(err)
	}
	if written.Nodes != 0 {
		t.Errorf("the stale batch wrote %d node(s) for a different document UUID with the same created_at; want none", written.Nodes)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, "IDEN-1/notes-md"); err != nil || held {
		t.Errorf("the stale batch left node %+v (held %t, %v), want no node describing the removed row", node, held, err)
	}
}

func TestAStaleCommentBatchDoesNotRestoreAnErasedComment(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seedIdentityIssue(t, database, at, model.Actor{Kind: "user", ID: "alice"})
	const id = "11111111-1111-4111-8111-111111111111"
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, issue_key, author, body, created_at)
		values ($1, 'IDEN-1', '{}', 'A remark.', $2)
	`, id, at); err != nil {
		t.Fatal(err)
	}
	batch, err := readCommentRows(ctx, database, zeroUUID)
	if err != nil || len(batch) != 1 {
		t.Fatalf("read the unmarked comment: %d row(s), %v; want 1", len(batch), err)
	}
	first, _, _, err := writeCommentBatch(ctx, database, batch)
	if err != nil {
		t.Fatalf("write the first run's batch: %v", err)
	}
	if first != 1 {
		t.Fatalf("the first run's batch counts %d row(s) written, want 1: the comment stood unmarked", first)
	}
	// The erase takes the issue's owner lock, deletes the stamped row, then erases its node and
	// tuples.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := store.LockOwners(ctx, tx, store.IssueOwner("IDEN-1")); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select 1 from graph_nodes where kind = 'comment' and id = $1 for update`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from comments where id = $1`, id); err != nil {
			return err
		}
		var seqs []int64
		if err := tx.QueryRow(ctx, `select array_agg(seq) from graph_tuples where from_kind = 'comment' and from_id = $1`, id).Scan(&seqs); err != nil {
			return err
		}
		return graphstore.EraseNode(ctx, tx, graphstore.KindComment, id, seqs)
	}); err != nil {
		t.Fatalf("erase the first run's comment: %v", err)
	}
	written, _, _, err := writeCommentBatch(ctx, database, batch)
	if err != nil {
		t.Fatalf("write the second run's cached batch: %v", err)
	}
	if written != 0 {
		t.Errorf("the stale batch counts %d row(s) written, want 0: the comment it read was erased before it wrote", written)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, id); err != nil || held {
		t.Errorf("the stale batch restored node %+v (held %t, %v) without its erased comment row; want no node", node, held, err)
	}
}

// writeCommentBatch reads its batch again under the owner lock (CommentsNamingNoNode) into a write
// of its own, and leaves the caller's batch as it read it: a caller that reads its batch after the
// write reads the rows it passed. Two comments are read, the first is removed, and the write gives
// the second alone its node while the batch still lists both, first to last.
func TestTheCommentBatchLeavesTheCallersBatch(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seedIdentityIssue(t, database, at, model.Actor{Kind: "user", ID: "alice"})
	const removed, standing = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	for _, id := range []string{removed, standing} {
		if _, err := database.Pool.Exec(ctx, `
			insert into comments (id, issue_key, author, body, created_at)
			values ($1, 'IDEN-1', '{}', 'A remark.', $2)
		`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := readCommentRows(ctx, database, zeroUUID)
	if err != nil || len(batch) != 2 || batch[0].id != removed || batch[1].id != standing {
		t.Fatalf("read the two unmarked comments: %+v, %v; want %s then %s", batch, err, removed, standing)
	}
	if _, err := database.Pool.Exec(ctx, `delete from comments where id = $1`, removed); err != nil {
		t.Fatal(err)
	}
	written, _, _, err := writeCommentBatch(ctx, database, batch)
	if err != nil || written != 1 {
		t.Fatalf("the batch counts %d row(s) written (%v), want the standing comment alone", written, err)
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, standing); err != nil || !held {
		t.Errorf("the standing comment holds no node (held %t, %v)", held, err)
	}
	if batch[0].id != removed || batch[1].id != standing {
		t.Errorf("after the write the caller's batch lists %s then %s, want %s then %s, as it read them",
			batch[0].id, batch[1].id, removed, standing)
	}
}

// ObjectRowStands answers whether an issue's, a document's, an ask's or a message's row stands at
// an id, which the erase reads under the node's lock. An ask's and a message's row is keyed by a
// uuid, so an id that is no uuid names no row and answers false with no error, rather than abort
// the erase's transaction on the cast; a uuid names its row when one stands there, and none when
// none does.
//
// Its control is the uuid test taken out: the id that is no uuid then fails the read.
func TestObjectRowStandsReadsTheRowsOfAnAskAndAMessage(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seedIdentityIssue(t, database, at, model.Actor{Kind: "user", ID: "alice"})
	const ask, absent = "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	if _, err := database.Pool.Exec(ctx, `
		insert into asks (id, issue_key, author, question, created_at)
		values ($1, 'IDEN-1', '{"kind":"user","id":"alice"}', 'Does it ship?', $2)
	`, ask, at); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		kind, id string
		want     bool
	}{
		{graphstore.KindAsk, "x", false},
		{graphstore.KindMessage, "x", false},
		{graphstore.KindAsk, absent, false},
		{graphstore.KindAsk, ask, true},
	} {
		stands, err := ObjectRowStands(ctx, database.Pool, row.kind, row.id)
		if err != nil || stands != row.want {
			t.Errorf("ObjectRowStands(%s, %q) = %t, %v; want %t and no error", row.kind, row.id, stands, err, row.want)
		}
	}
}
