package docs

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// What the release before this one writes beside a comment after the backfill reaches the store at
// this build's next block-index tick, and the census reads none of it unmarked with no second
// backfill.
//
// That release writes a comment's row holding neither tuples_stored nor threads_recorded, the rows
// of the comment's body - its blocks and their links naming no node, its citations holding no
// tuples_stored - and, through its trigger (user_threads_comment), the user_threads row of each
// person who writes in the thread and the move of each row a reply reaches, none of them marked. The
// backfill gives each such row its tuples and its marker once, so a row written after it stays
// unmarked until someone runs the backfill again, which nothing tells anyone to do; and R9 says of
// the unmarked class: "That class reaches zero when the backfill ends ... No class is permanent."
// Every tick of this build reads the comments the events after its cursor name whose row names no
// node, gives each its node, and then, in the same transaction, runs the backfill's own passes over
// what was written with them (readEventsAfterCursor, graphcensus.CatchUpComments): each comment's
// pointers and tuples_stored, threads_recorded on one past the store's bound, the body's blocks,
// their links and the comment's citations, and the user_threads rows of the comment's thread.
//
// The stand-in for that release writes, after the backfill and while it holds every period of the
// lease, as its API, its index of an item's text and its triggers write them, three comments, each
// with a body of one block: bob's, whose block links issue CORE-1, beside the comment's citation of
// that issue; carol's reply to it, which begins her row of the thread and raises bob's; and dana's
// comment answering a block at an address past the store's bound, which no pointer pass takes. Each
// comes with the comment.created that stored it. Once that release leaves, this build's next tick
// must leave the census with no unmarked row of comment_pointers, comment_block, comment_block_ref
// or user_threads and no difference, no comments row the old triggers would still write for, and no
// citation of those comments the store does not state.
func TestWhatThePreviousReleaseWritesBesideACommentAfterTheBackfillReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "commented-beside"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, so the census can pass only on agreement.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-beside-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-beside-role' where key = 'CORE-1';
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

	// A process of the release before joins, takes every period of the lease, writes the three
	// comments, and leaves.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	root := writeBesideAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "bob"}, "", "",
		"A remark that cites CORE-1.", true)
	reply := writeBesideAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "carol"}, root, "",
		"A reply to bob's remark.", false)
	pastBound := writeBesideAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "dana"}, "",
		"comment/"+root+"#"+strings.Repeat("b", graphstore.MaxAddressBytes), "A reply to a block past the bound.", false)
	comments := []string{root, reply, pastBound}
	for _, owed := range owedBesideComments(t, database, comments) {
		if owed.rows == 0 {
			t.Fatalf("the previous release's comments leave no %s owed, so this test proves nothing", owed.class)
		}
	}
	previousLeaves()

	// This build's next tick, serving alone again.
	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), gave %d comment(s) their nodes, another build %t",
		tick.Events, tick.Comments, tick.AnotherBuild)
	if tick.Comments != len(comments) {
		t.Errorf("the first tick after the previous release left gave %d comment(s) their nodes, want the %d it wrote", tick.Comments, len(comments))
	}
	for _, owed := range owedBesideComments(t, database, comments) {
		if owed.rows != 0 {
			t.Errorf("%d %s still owed after this build's tick, with no second backfill, want none", owed.rows, owed.class)
		}
	}
	requireCensusReadsZero(t, database, "after the previous release, which wrote three comments, a body and a reply's thread after the backfill, left, with no second backfill")
}

// owedBeside is one class of what the release before wrote beside a comment that the store does not
// hold yet, and how many rows of it stand.
type owedBeside struct {
	class string
	rows  int64
}

// owedBesideComments reads, class by class, what the release before wrote with comments that the
// store does not hold yet: the unmarked rows the census counts of comment_pointers, comment_block,
// comment_block_ref and user_threads (graphcensus.KindReport.Unmarked); the rows of comments holding
// neither tuples_stored nor threads_recorded, the marker test the old triggers read
// (user_threads_comment); and the citations out of comments that hold no tuples_stored.
func owedBesideComments(t *testing.T, database *store.Store, comments []string) []owedBeside {
	t.Helper()
	census := takeStoreCensus(t, database)
	var owed []owedBeside
	for _, kind := range []string{"comment_pointers", "comment_block", "comment_block_ref", "user_threads"} {
		found := false
		for _, report := range census.Kinds {
			if report.Kind == kind {
				owed, found = append(owed, owedBeside{"unmarked " + kind + " row(s)", report.Unmarked}), true
				break
			}
		}
		if !found {
			t.Fatalf("the census reads no kind %s", kind)
		}
	}
	var unrecorded, unstated int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments where id = any($1::uuid[]) and not (tuples_stored or threads_recorded)),
		       (select count(*) from refs where from_kind = 'comment' and from_id = any($1::text[]) and not tuples_stored)
	`, comments).Scan(&unrecorded, &unstated); err != nil {
		t.Fatalf("read the comments rows and citations the store does not hold: %v", err)
	}
	return append(owed, owedBeside{"comments row(s) holding neither tuples_stored nor threads_recorded", unrecorded},
		owedBeside{"citation(s) of those comments holding no tuples_stored", unstated})
}

// writeBesideAsThePreviousRelease has the stand-in for the release before this one write a comment
// by author on issue CORE-1 as that release's API writes one, in one transaction: the comments row,
// which names no node and holds neither marker, a reply to replyTo and an answer to the block
// repliesToBlock names where either is not empty; a comment_mentions row of each of mentions,
// holding no tuples_stored; the comment.created that stores it, whose payload is the comment
// (model.CommentEventPayload), its mentions among it; and what that release's index of the
// comment's text writes in the same transaction (api's queueItemReferences): the body's one block,
// b1, its comment_blocks row naming no node, and the comment's comment_block_state row at that
// index's revision with its owed_at cleared - and, where cites is set, the block's link to issue
// CORE-1, its comment_block_refs row naming no node, and the comment's citation of that issue, its
// refs row holding no tuples_stored. The insert fires that release's triggers: user_threads_comment,
// which writes the user_threads rows of the comment's thread as that release leaves them, and
// item_blocks_owe, which marks the state row owed and its windows owed, of which that release's
// index clears the first. It answers the comment.
func writeBesideAsThePreviousRelease(t *testing.T, database *store.Store, author model.Actor, replyTo, repliesToBlock, text string, cites bool,
	mentions ...model.Mention) string {
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
	written := model.Comment{IssueKey: new("CORE-1"), Author: author, Body: text,
		Mentions: append([]model.Mention{}, mentions...), Deliveries: []model.CommentDelivery{}}
	if replyTo != "" {
		written.ReplyTo = &replyTo
	}
	var block *string
	if repliesToBlock != "" {
		block = &repliesToBlock
	}
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body, reply_to, replies_to_block) values ('CORE-1', $1, $2, $3::uuid, $4)
		returning id::text, created_at
	`, encoded, text, written.ReplyTo, block).Scan(&written.ID, &written.CreatedAt); err != nil {
		t.Fatalf("write the comment of %s: %v", author.ID, err)
	}
	for _, mention := range mentions {
		if _, err := tx.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id) values ($1::uuid, $2, $3, $4)
		`, written.ID, mention.Target, mention.Delivery, mention.SessionID); err != nil {
			t.Fatalf("write the mention of %s of comment %s: %v", mention.Target, written.ID, err)
		}
	}
	payload, err := json.Marshal(model.CommentEventPayload{Comment: written})
	if err != nil {
		t.Fatalf("encode the comment.created event of %s: %v", written.ID, err)
	}
	event, err := recordIssueEvent(ctx, tx, "CORE-1", "comment.created", encoded, payload)
	if err != nil {
		t.Fatalf("append the comment.created event of %s: %v", written.ID, err)
	}
	type row struct {
		what, statement string
		args            []any
	}
	node := "comment/" + written.ID + "#b1"
	rows := []row{
		{"its block", `insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
			created_at, made_at, changed_revision, changed_at)
			values ($1::uuid, 'b1', $2, 0, 'paragraph', '', $3, now(), now(), 1, now())`, []any{written.ID, node, text}},
		{"its block state", `insert into comment_block_state (comment_id, revision, next_block) values ($1::uuid, 1, 2)
			on conflict (comment_id) do update set revision = excluded.revision, next_block = excluded.next_block, owed_at = null`,
			[]any{written.ID}},
	}
	if cites {
		rows = append(rows,
			row{"its block's link", `insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
				values ($1::uuid, 'b1', $2, 'issue', 'CORE-1', 'mentions', now(), $3)`, []any{written.ID, node, event}},
			row{"its citation", `insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
				values ('comment', $1, 'issue', 'CORE-1', 'mentions', now(), $2)`, []any{written.ID, event}})
	}
	for _, each := range rows {
		if _, err := tx.Exec(ctx, each.statement, each.args...); err != nil {
			t.Fatalf("write %s of comment %s: %v", each.what, written.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return written.ID
}
