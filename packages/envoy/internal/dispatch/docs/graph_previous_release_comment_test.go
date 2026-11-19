package docs

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A comment and a reply the release before this one writes after the backfill reach the store at
// this build's next block-index tick, and the census reaches zero with no second backfill.
//
// That release writes a comment's row with no node, appends the comment.created event that
// stores it, or the comment.answered that stores a reply, and states nothing in the node and tuple
// store. The backfill gives a node only to the rows it reads when it runs, so a comment written
// after it names no node, and the census refuses it until someone runs the backfill again, which
// nothing tells anyone to do. Every tick of this build reads the events after its cursor, and gives
// each comment those two events name whose row names no node the backfill's own write for that
// row, in the tick's transaction (readEventsAfterCursor, writeCommentsTheEventsName): the
// comment's node, the facts its row projects and who wrote it, with the event that stored it as
// their cause.
//
// The stand-in for that release writes the rows and appends the events as its API does: a
// person's comment on issue CORE-1 with its comment.created, and a session's reply to it with its
// comment.answered, each event's payload the comment it stores (model.CommentEventPayload). It
// writes them while it serves and holds every period of the shared block-index lease, so no tick
// of this build runs before it leaves.
func TestACommentThePreviousReleaseWritesAfterTheBackfillReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "commented"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, so the census can pass only on agreement.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-comment-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-comment-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")

	// A process of the release before joins, takes every period of the lease, writes a comment
	// and a reply to it, and leaves.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	person := model.Actor{Kind: "user", ID: "Bob"}
	session := model.Actor{Kind: "session", ID: "previous-release-replying-session"}
	comment, commented := commentAsThePreviousRelease(t, database, person, "", "A remark the previous release wrote after the backfill.")
	reply, answered := commentAsThePreviousRelease(t, database, session, comment, "A reply the previous release wrote after the backfill.")
	written := map[string]struct {
		by    model.Actor
		event int64
	}{comment: {person, commented}, reply: {session, answered}}
	for id := range written {
		if named := commentNamesItsNode(t, database, id); named {
			t.Fatalf("the previous release's comment %s names its node, so this test proves nothing", id)
		}
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over the comments the previous release wrote, so this test proves nothing")
	}
	previousLeaves()

	// This build's next tick, serving alone again.
	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), gave %d comment(s) their nodes, another build %t",
		tick.Events, tick.Comments, tick.AnotherBuild)
	if tick.Comments != len(written) {
		t.Errorf("the first tick after the previous release left gave %d comment(s) their nodes, want the %d it wrote", tick.Comments, len(written))
	}

	for id, wrote := range written {
		if !commentNamesItsNode(t, database, id) {
			t.Errorf("comment %s names no node after this build's tick", id)
		}
		kind, author, _ := graphstore.AuthorOf(wrote.by)
		held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, id, []string{graphstore.TupleAuthoredBy})
		if err != nil {
			t.Fatalf("read who the store says wrote comment %s: %v", id, err)
		}
		if len(held) != 1 || held[0].ToKind != kind || held[0].ToID != author {
			t.Errorf("the store states %#v as the writer of comment %s, want %s %s", held, id, kind, author)
		}
		var stated, caused int
		if err := database.Pool.QueryRow(ctx, `
			select count(*), count(*) filter (where source_event = $3)
			from graph_tuples where from_kind = $1 and from_id = $2 and asserted
		`, graphstore.KindComment, id, wrote.event).Scan(&stated, &caused); err != nil {
			t.Fatalf("read the tuples of comment %s: %v", id, err)
		}
		if stated == 0 || caused != stated {
			t.Errorf("comment %s carries %d tuple(s), %d of them caused by the event that stored it (%d), want every one of them so caused",
				id, stated, caused, wrote.event)
		}
	}
	requireCensusReadsZero(t, database, "after the previous release, which wrote a comment and a reply after the backfill, left, with no second backfill")
}

// commentAsThePreviousRelease has the stand-in for the release before this one write a comment by
// author on issue CORE-1, a reply to comment replyTo where that is not empty, as that release's API
// writes one: the comments row, which names no node, and in the same transaction the event that
// stores it, comment.created for a comment and comment.answered for a reply, whose payload is the
// comment (model.CommentEventPayload). It answers the comment and the event.
func commentAsThePreviousRelease(t *testing.T, database *store.Store, author model.Actor, replyTo, body string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode the author %s: %v", author.ID, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	written := model.Comment{IssueKey: new("CORE-1"), Author: author, Body: body,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{}}
	if replyTo != "" {
		written.ReplyTo = &replyTo
	}
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', $1, $2, $3::uuid)
		returning id::text, created_at
	`, encoded, body, written.ReplyTo).Scan(&written.ID, &written.CreatedAt); err != nil {
		t.Fatalf("write the comment of %s: %v", author.ID, err)
	}
	kind := "comment.created"
	if replyTo != "" {
		kind = "comment.answered"
	}
	payload, err := json.Marshal(model.CommentEventPayload{Comment: written})
	if err != nil {
		t.Fatalf("encode the %s event of %s: %v", kind, written.ID, err)
	}
	var event int64
	if err := tx.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, $1, $2, $3, false
		from events where issue_key = 'CORE-1'
		returning id
	`, kind, encoded, payload).Scan(&event); err != nil {
		t.Fatalf("append the %s event of %s: %v", kind, written.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return written.ID, event
}

// commentNamesItsNode is whether comment's row names its node.
func commentNamesItsNode(t *testing.T, database *store.Store, comment string) bool {
	t.Helper()
	var named bool
	if err := database.Pool.QueryRow(context.Background(), `
		select node_kind is not null from comments where id = $1::uuid
	`, comment).Scan(&named); err != nil {
		t.Fatalf("read the row of comment %s: %v", comment, err)
	}
	return named
}
