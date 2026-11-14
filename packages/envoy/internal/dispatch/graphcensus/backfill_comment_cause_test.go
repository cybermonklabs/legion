package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

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

// The backfill states of each comment's author exactly what graphstore.Authored states of it,
// though it states it in its own batch write rather than through Authored (writeCommentBatch,
// graphstore.AuthorFacts): the author's node, the authored_by tuple and the aliases the server
// proved. The comments are written, each with its comment.created event, by authors of every
// shape AuthorFacts reads: a person whose credential acted from a session it proved, a person
// whose stored id is not the canonical login, a session a person's token proved, one person twice,
// and the shared bearer, which names nobody. Once the backfill has run, Authored is run again over
// each comment, under the comment's own event and in a transaction rolled back, and held to adding
// nothing: every node and tuple it would write is one the backfill wrote already. The store is
// also read for the three aliases, so the comparison is over a write that stated them.
func TestTheBackfillStatesWhatAuthoredStatesOfEveryCommentAuthor(t *testing.T) {
	ctx := context.Background()
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", model.Actor{Kind: "user", ID: "alice"})
	bob := "bob"
	authors := []model.Actor{
		{Kind: "user", ID: "carol", Act: &model.ActorAct{Session: "ses_carol_proved"}},
		{Kind: "user", ID: "Dave"},
		{Kind: "session", ID: "ses_erin", Owner: &bob, Act: &model.ActorAct{Session: "ses_erin"}},
		{Kind: "user", ID: "frank"},
		{Kind: "user", ID: "frank"},
		{},
	}
	type writtenComment struct {
		id     string
		author model.Actor
		at     time.Time
		event  int64
	}
	comments := make([]writtenComment, 0, len(authors))
	for _, author := range authors {
		var comment model.Comment
		created := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			stored, err := json.Marshal(author)
			if err != nil {
				return model.Event{}, err
			}
			comment = model.Comment{IssueKey: new("CENS-1"), Author: author, Body: "Written before the store existed."}
			if err := tx.QueryRow(ctx, `
				insert into comments (issue_key, author, body) values ($1, $2, $3) returning id::text, created_at
			`, comment.IssueKey, stored, comment.Body).Scan(&comment.ID, &comment.CreatedAt); err != nil {
				return model.Event{}, err
			}
			comment.Mentions, comment.Deliveries = []model.Mention{}, []model.CommentDelivery{}
			return model.Event{
				IssueKey: new("CENS-1"), Type: "comment.created", Actor: author,
				Payload: model.CommentEventPayload{Comment: comment},
			}, nil
		})
		comments = append(comments, writtenComment{comment.ID, author, comment.CreatedAt, created.ID})
	}

	if _, err := Backfill(ctx, writes.database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	var aliases int64
	if err := writes.database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where (from_kind, from_id, kind, to_kind, to_id) in (
			($1, 'ses_carol_proved', $3, $2, 'carol'),
			($2, 'dave', $4, $5, 'Dave'),
			($1, 'ses_erin', $3, $2, 'bob'))
		  and asserted
	`, graphstore.KindSession, graphstore.KindPerson, graphstore.TupleAliasOf, graphstore.TupleRecordedAs,
		graphstore.KindValue).Scan(&aliases); err != nil {
		t.Fatalf("read the aliases the backfill stated: %v", err)
	}
	if aliases != 3 {
		t.Errorf("the store holds %d of the three aliases the comments' authors carry, want all three", aliases)
	}
	for _, comment := range comments {
		if added := addedByAuthored(t, writes.database, comment.id, comment.author, comment.at, comment.event); len(added) > 0 {
			t.Errorf("graphstore.Authored over comment %s by %+v still writes %v: the backfill did not state everything Authored states of its author",
				comment.id, comment.author, added)
		}
	}
}

// addedByAuthored runs graphstore.Authored over one comment in a transaction it rolls back, and
// answers every node and tuple that write added.
func addedByAuthored(t *testing.T, database *store.Store, id string, author model.Actor, at time.Time, event int64) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction Authored runs in: %v", err)
	}
	defer tx.Rollback(ctx)
	var nodes, lastSeq int64
	if err := tx.QueryRow(ctx, `
		select (select count(*) from graph_nodes), (select coalesce(max(seq), 0) from graph_tuples)
	`).Scan(&nodes, &lastSeq); err != nil {
		t.Fatalf("read the store before Authored: %v", err)
	}
	if err := graphstore.Authored(ctx, tx, commentNodeKind, id, author, at, event); err != nil {
		t.Fatalf("run Authored over comment %s: %v", id, err)
	}
	rows, err := tx.Query(ctx, `
		select 'tuple ' || from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id
		from graph_tuples where seq > $1 order by seq
	`, lastSeq)
	if err != nil {
		t.Fatalf("read what Authored added: %v", err)
	}
	added, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read what Authored added: %v", err)
	}
	var after int64
	if err := tx.QueryRow(ctx, `select count(*) from graph_nodes`).Scan(&after); err != nil {
		t.Fatalf("count the nodes after Authored: %v", err)
	}
	if after != nodes {
		added = append(added, fmt.Sprintf("%d node(s)", after-nodes))
	}
	return added
}
