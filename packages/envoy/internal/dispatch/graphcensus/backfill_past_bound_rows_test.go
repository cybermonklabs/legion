package graphcensus

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// triggersStillWriteFor counts the comments and messages rows the user_threads triggers still
// write for, by their own marker test (migration 20261116160001): a comment holding neither
// tuples_stored nor threads_recorded, and a message naming no node and holding no
// threads_recorded. It is the test the root's drop of those triggers reads:
// the drop waits while a row of this count stands, since each trigger still writes for exactly
// those rows.
func triggersStillWriteFor(t *testing.T, database *store.Store) (comments, messages int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments where not (tuples_stored or threads_recorded)),
		       (select count(*) from messages
		        where not ((node_kind is not null and node_address is not null) or threads_recorded))
	`).Scan(&comments, &messages); err != nil {
		t.Fatalf("count the rows the triggers still write for: %v", err)
	}
	return comments, messages
}

// TestThePastBoundPassStopsAnOlderBuildsRowFromHoldingTheTriggers holds the backfill to the one
// pass that gives an older build's comment and message past the store's bound the marker the
// triggers pass a row by: a comment owned by a document whose ref_key the store cannot key, which
// no pointer pass takes, and a reply naming a block whose node id the store cannot key, which no
// messages pass takes. Before the backfill each holds the root's drop of the triggers
// (triggersStillWriteFor); after it neither does, and the backfill states no relation of either
// row's own. The one tuple of the reply it states is the thread pass's: the trigger moved alice's
// row of the message's thread when the reply was inserted, and that pass marks the row with the
// delivery the move stands for, as it marks any row an older build's write moved.
func TestThePastBoundPassStopsAnOlderBuildsRowFromHoldingTheTriggers(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('PBR', 'PBR project', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('PBR-1', 'PBR', 1, 'Messages past the bound', '{"kind":"user","id":"alice"}', 'U')
	`); err != nil {
		t.Fatalf("create the project and its issue: %v", err)
	}
	var artifactID, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PBR', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, randText(2688)).Scan(&artifactID, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store's bound, so this proves nothing")
	}
	// An older build's writes name neither marker: its comment on the long-keyed document and,
	// under an older message of its own, its reply naming a block whose node id is past the bound.
	var commentID, parentID, replyID string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on a long-keyed document.')
		returning id::text
	`, artifactID).Scan(&commentID); err != nil {
		t.Fatalf("write the older build's comment: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body) values ('PBR-1', '{"kind":"user","id":"alice"}', 'One.')
		returning id::text
	`).Scan(&parentID); err != nil {
		t.Fatalf("write the older build's message: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, in_reply_to, replies_to_block)
		values ('PBR-1', '{"kind":"user","id":"bob"}', 'On a long block.', $1::uuid, $2)
		returning id::text
	`, parentID, "message/"+parentID+"#"+strings.Repeat("b", graphstore.MaxAddressBytes)).Scan(&replyID); err != nil {
		t.Fatalf("write the older build's reply past the bound: %v", err)
	}
	// The triggers recorded both as they always have: alice keeps a row of the comment's thread,
	// and bob's reply moved alice's row of the message's thread.
	var rows int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from user_threads
		where (login = 'alice' and thread_id = $1::uuid) or (login = 'alice' and thread_id = $2::uuid and version = 1)
	`, commentID, parentID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("the triggers recorded %d of the two rows the older writes owe (err %v)", rows, err)
	}
	if comments, messages := triggersStillWriteFor(t, database); comments == 0 || messages == 0 {
		t.Fatalf("before the backfill the triggers still write for %d comments and %d messages, want the older build's rows among them", comments, messages)
	}

	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	// Each pass reports the rows it marked alone (pastBoundTable.markRecorded); what the two wrote
	// of the store is read off it below.
	for _, kind := range []string{"comments_past_bound", "messages_past_bound"} {
		if done := reportedKind(t, report, kind); done.Rows != 1 || !done.Complete {
			t.Errorf("the %s pass marked %d rows (complete %t), want 1 row, complete", kind, done.Rows, done.Complete)
		}
	}
	if comments, messages := triggersStillWriteFor(t, database); comments != 0 || messages != 0 {
		t.Fatalf("after the backfill the triggers still write for %d comments and %d messages, want none: the release that drops them could never run", comments, messages)
	}
	// The comment has no node at all; the reply's node is the object pass's, and no relation of
	// its own is stated, since no pass takes its row.
	var own, delivered int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where kind <> $3),
		       count(*) filter (where kind = $3 and from_kind = $4 and to_kind = $5 and to_id = 'alice')
		from graph_tuples
		where ((from_kind = $6 and from_id = $1) or (from_kind = $4 and from_id = $2)) and to_kind <> $7
	`, commentID, replyID, graphstore.TupleDeliveredTo, graphstore.KindMessage, graphstore.KindPerson,
		graphstore.KindComment, graphstore.KindValue).Scan(&own, &delivered); err != nil {
		t.Fatalf("count the relations the backfill stated of the two rows past the bound: %v", err)
	}
	if own != 0 || delivered != 1 {
		t.Fatalf("the backfill stated %d relations of the two rows' own and %d delivered_to tuples from the reply to alice, want none and one",
			own, delivered)
	}
	var marked bool
	if err := database.Pool.QueryRow(ctx, `
		select marked_seq is not null and marked_version = version from user_threads
		where login = 'alice' and thread_id = $1::uuid
	`, parentID).Scan(&marked); err != nil || !marked {
		t.Fatalf("alice's row of the message's thread is marked %t after the backfill (err %v), want the thread pass's mark", marked, err)
	}
	requireNoAddressPastTheBound(t, database)
}
