package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The backfill causes a comment's node and every tuple it writes for the comment by the
// comment.created event that stored the comment, and by the store's sentinel only where no such
// event exists. The event's payload is the comment flat (model.CommentEventPayload embeds
// model.Comment), so the comment's id is the payload's own `id`: a lookup of a nested `comment`
// object finds nothing and dates every comment from the sentinel.
//
// One comment is written as the API's createCommentFor writes it, with its comment.created event,
// and the test first reads that event's stored payload to see the id at its top level. The other
// is a row no event stored, as a seeded database holds one.
func TestTheBackfillCausesACommentByTheEventThatStoredIt(t *testing.T) {
	ctx := context.Background()
	writes := newAddressingWrites(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	writes.createIssue(t, "CENS-1", 1, "U", alice)

	var stored string
	created := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		author, err := json.Marshal(alice)
		if err != nil {
			return model.Event{}, err
		}
		comment := model.Comment{IssueKey: new("CENS-1"), Author: alice, Body: "A remark the event stored."}
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ($1, $2, $3) returning id::text, created_at
		`, comment.IssueKey, author, comment.Body).Scan(&comment.ID, &comment.CreatedAt); err != nil {
			return model.Event{}, err
		}
		stored = comment.ID
		comment.Mentions, comment.Deliveries = []model.Mention{}, []model.CommentDelivery{}
		return model.Event{
			IssueKey: new("CENS-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: comment},
		}, nil
	})
	var flatID string
	var nested bool
	if err := writes.database.Pool.QueryRow(ctx, `
		select coalesce(payload->>'id', ''), payload ? 'comment' from events where id = $1
	`, created.ID).Scan(&flatID, &nested); err != nil {
		t.Fatalf("read the comment.created event's payload: %v", err)
	}
	if flatID != stored || nested {
		t.Fatalf("the comment.created payload carries id %q and a nested comment %t, want the comment's id %s at its top level",
			flatID, nested, stored)
	}

	var unstored string
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CENS-1', '{"kind":"user","id":"alice"}', 'A remark no event stored.')
		returning id::text
	`).Scan(&unstored); err != nil {
		t.Fatalf("write the comment no event stored: %v", err)
	}

	if _, err := Backfill(ctx, writes.database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	requireCommentCause(t, writes.database, stored, created.ID)
	requireCommentCause(t, writes.database, unstored, graphstore.NoEvent)
}

// requireCommentCause fails unless the comment's node and every tuple out of it - its two base
// attributes and its authored_by at least - are caused by cause.
func requireCommentCause(t *testing.T, database *store.Store, comment string, cause int64) {
	t.Helper()
	ctx := context.Background()
	node, found, err := graphstore.ReadNode(ctx, database.Pool, commentNodeKind, comment)
	if err != nil || !found {
		t.Fatalf("read the node of comment %s: found %t, %v", comment, found, err)
	}
	if node.SourceEvent != cause {
		t.Fatalf("the node of comment %s is caused by event %d, want %d", comment, node.SourceEvent, cause)
	}
	rows, err := database.Pool.Query(ctx, `
		select kind, source_event from graph_tuples where from_kind = $1 and from_id = $2 order by seq
	`, commentNodeKind, comment)
	if err != nil {
		t.Fatalf("read the tuples of comment %s: %v", comment, err)
	}
	defer rows.Close()
	kinds := map[string]bool{}
	for rows.Next() {
		var kind string
		var event int64
		if err := rows.Scan(&kind, &event); err != nil {
			t.Fatalf("read a tuple of comment %s: %v", comment, err)
		}
		if event != cause {
			t.Fatalf("the %s tuple of comment %s is caused by event %d, want %d", kind, comment, event, cause)
		}
		kinds[kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuples of comment %s: %v", comment, err)
	}
	for _, kind := range []string{tupleVisibility, tupleState, graphstore.TupleAuthoredBy} {
		if !kinds[kind] {
			t.Fatalf("comment %s holds no %s tuple; its tuples are %v", comment, kind, kinds)
		}
	}
}
