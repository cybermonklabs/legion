package docs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestATickPassesOverACommentBetweenRefusedParties: a conversation document's parties are its
// session's, and a session id that holds white space is a party graphstore.PartiesValue refuses
// (ErrBadParty), so no writer can give a comment on that document its node. The block-index tick
// reads every comment the events after its build's cursor name (refs.CommentsNamingNoNode); one it
// went on to state would fail the tick's transaction on the same page every time, and the cursor
// would never move. The comment here is written as the release before this one writes one: its
// row, which names no node, and its comment.created event after this build's cursor. The tick
// must commit, move its cursor past that event, and leave the comment's row naming no node, as
// the backfill leaves it (graphcensus's partiesRefused).
//
// Control: refs.CommentsNamingNoNode keeping such a row (the PartiesValue skip in
// scanCommentRows taken out), under which the tick fails with ErrBadParty and its cursor stays.
func TestATickPassesOverACommentBetweenRefusedParties(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	awaitedBlockIndexTick(t, service)
	before := blockIndexEventCursor(t, database, "")

	const session = "ses bad"
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'notes-md', 'notes.md', 'doc', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, session).Scan(&document); err != nil {
		t.Fatalf("write the conversation document of session %q: %v", session, err)
	}
	author := model.Actor{Kind: "session", ID: session}
	encoded, err := json.Marshal(author)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	written := model.Comment{ArtifactID: &document, Author: author, Body: "A remark on the conversation's notes.",
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{}}
	if err := tx.QueryRow(ctx, `
		insert into comments (artifact_id, author, body) values ($1::uuid, $2, $3)
		returning id::text, created_at
	`, document, encoded, written.Body).Scan(&written.ID, &written.CreatedAt); err != nil {
		t.Fatalf("write the comment on document %s: %v", document, err)
	}
	payload, err := json.Marshal(model.CommentEventPayload{Comment: written})
	if err != nil {
		t.Fatal(err)
	}
	created, err := recordIssueEvent(ctx, tx, "CORE-1", "comment.created", encoded, payload)
	if err != nil {
		t.Fatalf("append the comment.created event of %s: %v", written.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if created <= before {
		t.Fatalf("the comment's event %d is not after this build's cursor %d, so this test proves nothing", created, before)
	}

	tick := awaitedBlockIndexTick(t, service)
	if after := blockIndexEventCursor(t, database, ""); after < created {
		t.Fatalf("the tick left this build's cursor at %d, before the comment's event %d (tick %+v): a comment between parties the store refuses stops the tick on that page",
			after, created, tick)
	}
	if tick.Comments != 0 {
		t.Errorf("the tick gave %d comment(s) their nodes, want none: the comment's parties are ones the store refuses", tick.Comments)
	}
	if commentNamesItsNode(t, database, written.ID) {
		t.Errorf("comment %s names a node after the tick, want none: no writer can give a node between the parties %q", written.ID, session)
	}
}
