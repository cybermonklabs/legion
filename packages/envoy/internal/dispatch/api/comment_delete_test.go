package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deleteComment deletes the comment as login, or as the session sessionActor() names when login
// is empty, and fails t on anything but 200.
func deleteThreadComment(t *testing.T, handler http.Handler, id, login string) model.Comment {
	t.Helper()
	var response = dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+id, nil, login)
	if login == "" {
		response = sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+id, map[string]any{
			"actor": sessionActor(),
		})
	}
	if response.Code != http.StatusOK {
		t.Fatalf("delete comment %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

// commentBlockRow is one comment_blocks row as this file reads it: whether the block is still
// in the body, and the excerpt a reply read would serve as its text.
type commentBlockRow struct {
	current bool
	excerpt string
}

func commentBlockRows(t *testing.T, database *store.Store, commentID string) []commentBlockRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select position is not null, excerpt from comment_blocks
		where comment_id = $1 order by block_id
	`, commentID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", commentID, err)
	}
	defer rows.Close()
	var blocks []commentBlockRow
	for rows.Next() {
		var block commentBlockRow
		if err := rows.Scan(&block.current, &block.excerpt); err != nil {
			t.Fatalf("scan a block of %s: %v", commentID, err)
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", commentID, err)
	}
	return blocks
}

func commentSearchVector(t *testing.T, database *store.Store, commentID string) string {
	t.Helper()
	var vector string
	if err := database.Pool.QueryRow(context.Background(),
		`select search::text from comments where id = $1`, commentID).Scan(&vector); err != nil {
		t.Fatalf("read the search vector of %s: %v", commentID, err)
	}
	return vector
}

// postEmbeddingWindows counts the embedding windows of one post, whose ids all begin with the
// owner key `<kind>/<uuid>` (refs.ItemKind.WindowOwnerKey): the rows a meaning search matches a
// post by.
func postEmbeddingWindows(t *testing.T, database *store.Store, ownerKey string) int {
	t.Helper()
	var windows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings where split_part(id, '#', 1) = $1
	`, ownerKey).Scan(&windows); err != nil {
		t.Fatalf("count the embedding windows of %s: %v", ownerKey, err)
	}
	return windows
}

// A delete clears the body and every copy of the body that does not follow the cleared column:
// the search vector, each block row with its stored excerpt, the document margin mark, and a
// suggestion's replacement text.
func TestDeleteCommentClearsEveryCopyOfItsBody(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a comment", "before")
	const body = "The first paragraph of the withdrawn claim.\n\nThe second paragraph of it."
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": body, "anchor": map[string]any{"artifact": "spec", "quote": "before"},
	}, "alice")
	if blocks := commentBlockRows(t, database, comment.ID); len(blocks) != 2 {
		t.Fatalf("the comment indexed %d blocks, want the body's two", len(blocks))
	}
	if vector := commentSearchVector(t, database, comment.ID); vector == "" {
		t.Fatalf("the comment's search vector is empty before the delete")
	}
	// A write of a post records its blocks and writes no window: the windows those blocks
	// stand in are the block-index tick's window arm's to write, one tick behind the write
	// (refs.WriteItemWindows, INFRA-988). So the windows the delete has to drop are written
	// here, as that arm writes them.
	windowOwedItems(t, database)
	if windows := postEmbeddingWindows(t, database, "comment/"+comment.ID); windows == 0 {
		t.Fatalf("the comment holds no embedding window before the delete, so dropping them proves nothing")
	}

	deleted := deleteThreadComment(t, handler, comment.ID, "alice")
	if deleted.Body != "" || deleted.DeletedAt == nil || deleted.DeletedBy == nil || deleted.DeletedBy.ID != "alice" {
		t.Fatalf("the delete answered %#v, want an empty body with the mark", deleted)
	}

	stored := readThreadComment(t, handler, comment.ID)
	if stored.Body != "" || stored.DeletedAt == nil || stored.DeletedBy == nil {
		t.Fatalf("the comment reads back as %#v, want an empty body with the mark", stored)
	}
	if vector := commentSearchVector(t, database, comment.ID); vector != "" {
		t.Fatalf("the search vector after the delete = %q, want the empty vector the cleared body makes", vector)
	}
	for index, block := range commentBlockRows(t, database, comment.ID) {
		if block.current {
			t.Fatalf("block %d is still in the body after the delete", index)
		}
		if block.excerpt != "" {
			t.Fatalf("block %d keeps the excerpt %q, which a reply read would serve as its text", index, block.excerpt)
		}
	}
	projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID)
	if projection["text"] != "" {
		t.Fatalf("the margin mark after the delete = %#v, want no text", projection)
	}
	if !strings.Contains(strings.Join(commentEventTypes(t, handler, issue.Key), ","), "comment.deleted") {
		t.Fatalf("the event log is missing comment.deleted")
	}
	// A deleted post is reachable by no meaning search: the re-index with the empty body leaves
	// the comment with no block, and the arm's next pass drops every window of an owner left
	// with no block in the statement that clears its marker (refs.WriteItemWindows,
	// refs/blocks.go, writeWindows).
	windowOwedItems(t, database)
	if windows := postEmbeddingWindows(t, database, "comment/"+comment.ID); windows != 0 {
		t.Fatalf("the deleted comment keeps %d embedding windows, want none: a meaning search would reach it", windows)
	}
}

// A suggestion's replacement text is the comment's text too, so the delete clears it with the
// body.
func TestDeleteCommentClearsItsSuggestion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a suggestion", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Say this instead.", "suggestion": map[string]string{"replace_with": "the replacement text"},
	}, "alice")
	if comment.Suggestion == nil {
		t.Fatalf("the comment was created with no suggestion: %#v", comment)
	}

	if deleted := deleteThreadComment(t, handler, comment.ID, "alice"); deleted.Suggestion != nil {
		t.Fatalf("the delete answered the suggestion %#v, want none", deleted.Suggestion)
	}
	stored := readThreadComment(t, handler, comment.ID)
	if stored.Suggestion != nil {
		t.Fatalf("the comment keeps the suggestion %#v after the delete", stored.Suggestion)
	}
}

// Each reply keeps its own text and its own place: the deleted root stays where it was in the
// thread, with the reply under it, reading as it always did.
func TestDeleteCommentKeepsEachReplysTextAndPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a root", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the withdrawn root"}, "alice")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the reply that stands", "reply_to": root.ID,
	}, "bob")

	deleteThreadComment(t, handler, root.ID, "alice")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+root.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the deleted thread: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[commentRead](t, response)
	if read.Comment.Body != "" || read.Comment.DeletedAt == nil {
		t.Fatalf("the deleted root reads as %#v, want an empty body with the mark", read.Comment)
	}
	if len(read.Replies) != 1 || read.Replies[0].ID != reply.ID || read.Replies[0].Body != "the reply that stands" {
		t.Fatalf("the thread's replies = %#v, want the one reply with its own text", read.Replies)
	}
	if read.Replies[0].DeletedAt != nil {
		t.Fatalf("the reply carries the deleted mark %#v, which belongs to the root alone", read.Replies[0].DeletedAt)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's comments: status=%d body=%s", listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]model.Comment](t, listed)
	if len(comments) != 2 || comments[0].ID != root.ID || comments[1].ID != reply.ID {
		t.Fatalf("the issue's comments = %#v, want the deleted root in its place with the reply after it", comments)
	}
}

// A delivery still pending would send the text after the delete, so the delete fails it with
// the one error that says why, and appends the comment.delivery receipt that settle owes.
func TestDeleteCommentFailsEachPendingMentionAttempt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete with a pending send", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "mentioning a session"}, "alice")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
		values ($1, 'session:s1', 1, 'steer', 's1', 'pending')
	`, comment.ID); err != nil {
		t.Fatalf("seed a pending attempt: %v", err)
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	var state, failure string
	if err := database.Pool.QueryRow(context.Background(), `
		select state, coalesce(error, '') from comment_deliveries
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID).Scan(&state, &failure); err != nil {
		t.Fatalf("read the attempt after the delete: %v", err)
	}
	if state != "failed" || failure != deletedPostDeliveryError {
		t.Fatalf("the attempt reads %q / %q, want failed with %q", state, failure, deletedPostDeliveryError)
	}

	receipts := issueEventsOfType(t, handler, issue.Key, "comment.delivery")
	if len(receipts) != 1 {
		t.Fatalf("the issue holds %d comment.delivery receipts, want the one the settle owes", len(receipts))
	}
	var payload struct {
		CommentID string `json:"comment_id"`
		Target    string `json:"target"`
		State     string `json:"state"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(receipts[0].Payload, &payload); err != nil {
		t.Fatalf("decode the comment.delivery receipt: %v", err)
	}
	if payload.CommentID != comment.ID || payload.Target != "session:s1" ||
		payload.State != "failed" || payload.Error != deletedPostDeliveryError {
		t.Fatalf("the receipt says %#v, want the failed attempt with its error", payload)
	}
}

// The refusals are in one order on this write: the author first, then the post already deleted,
// since a delete is final. A session deletes its own comment, and no other session's.
func TestDeleteCommentIsAuthorOnlyAndFinal(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Who may delete", "before")
	agents := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the session's own comment", "actor": sessionActor(),
	}, "")

	person := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, nil, "alice")
	if person.Code != http.StatusForbidden || !strings.Contains(person.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("a person deletes a session's comment: status=%d body=%s, want 403 NOT_AUTHOR", person.Code, person.Body.String())
	}
	other := sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-fedcba9876543210"},
	})
	if other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("another session deletes it: status=%d body=%s, want 403 NOT_AUTHOR", other.Code, other.Body.String())
	}
	if stored := readThreadComment(t, handler, agents.ID); stored.Body != "the session's own comment" {
		t.Fatalf("the comment after the refused deletes = %q, want it untouched", stored.Body)
	}

	deleteThreadComment(t, handler, agents.ID, "")
	again := sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, map[string]any{
		"actor": sessionActor(),
	})
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), `"code":"POST_DELETED"`) {
		t.Fatalf("a second delete: status=%d body=%s, want 409 POST_DELETED", again.Code, again.Body.String())
	}
}

// A pinned block reference serves the text the block's revision recorded (refs.ItemBlockAt), and
// the delete's re-index writes a null text at the new revision alone, so every earlier revision
// of the block still held the withdrawn text. The delete clears them, and a pin to any revision
// of a deleted comment's block then reads as one the comment does not hold. The same pinned read
// of a comment that stands is the control: it still gives its text.
func TestAPinnedBlockReferenceToADeletedCommentGivesNoText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned blocks of a deleted comment", "before")
	const withdrawn = "The second paragraph of the withdrawn claim."
	const standing = "The second paragraph of the claim that stands."
	withdrawnComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\n" + withdrawn}, "alice")
	standingComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Gamma.\n\n" + standing}, "alice")
	withdrawnRef := "dispatch://" + issue.Key + "/comment/" + withdrawnComment.ID + "#b-b2"
	standingRef := "dispatch://" + issue.Key + "/comment/" + standingComment.ID + "#b-b2"
	requirePinnedText := func(step, ref, text string) {
		t.Helper()
		read := readGraph(t, handler, url.Values{"to": {ref + "@v1"}})
		if read.Node.Pin == nil || read.Node.Pin.Text != text {
			t.Fatalf("%s: the pin reads %#v, want the text %q", step, read.Node.Pin, text)
		}
	}
	requirePinnedText("before the delete", withdrawnRef, withdrawn)
	requirePinnedText("the comment that stands, before the delete", standingRef, standing)

	deleteThreadComment(t, handler, withdrawnComment.ID, "alice")

	response := graphRequest(t, handler, url.Values{"to": {withdrawnRef + "@v1"}})
	if response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), `no block \"b2\" in comment `+withdrawnComment.ID+" at revision 1") {
		t.Fatalf("the pinned read of a deleted comment's block: status=%d body=%s, want 404 for a revision that holds no block",
			response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), withdrawn) {
		t.Fatalf("the pinned read of a deleted comment's block gives its text: %s", response.Body.String())
	}
	requirePinnedText("the comment that stands, after the delete", standingRef, standing)
}

// R3: every read that quotes a post says that it is deleted. A block of a deleted comment is
// gone with the body, so neither block read can give text; what each must not do is answer that
// the block is missing and leave the deletion unsaid. Both reads of the withdrawn comment's
// block - the unpinned one and a pin to the revision that recorded it - name the deletion in
// the words every refusal of a deleted post uses, and give no part of the body. The same two
// reads of a comment that stands are the control: unchanged, text and all.
func TestABlockReferenceToADeletedCommentNamesTheDeletion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Blocks of a deleted comment", "before")
	const withdrawn = "The second paragraph of the withdrawn claim."
	const standing = "The second paragraph of the claim that stands."
	withdrawnComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\n" + withdrawn}, "alice")
	standingComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Gamma.\n\n" + standing}, "alice")
	withdrawnRef := "dispatch://" + issue.Key + "/comment/" + withdrawnComment.ID + "#b-b2"
	standingRef := "dispatch://" + issue.Key + "/comment/" + standingComment.ID + "#b-b2"

	deleteThreadComment(t, handler, withdrawnComment.ID, "alice")

	deletion := `: this comment is deleted`
	for _, read := range []struct{ name, ref, want string }{
		{
			name: "the unpinned read of a deleted comment's block",
			ref:  withdrawnRef,
			want: `no block \"b2\" in comment ` + withdrawnComment.ID + deletion,
		},
		{
			name: "a pin to the revision that recorded a deleted comment's block",
			ref:  withdrawnRef + "@v1",
			want: `no block \"b2\" in comment ` + withdrawnComment.ID + " at revision 1" + deletion,
		},
	} {
		response := graphRequest(t, handler, url.Values{"to": {read.ref}})
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) ||
			!strings.Contains(response.Body.String(), read.want) {
			t.Fatalf("%s: status=%d body=%s, want 404 NOT_FOUND saying %q", read.name, response.Code, response.Body.String(), read.want)
		}
		if strings.Contains(response.Body.String(), withdrawn) {
			t.Fatalf("%s gives its text: %s", read.name, response.Body.String())
		}
	}
	// A pin past the deleted comment's current revision is refused by the revision it names
	// rather than by the block, and it says the comment is deleted all the same.
	pastRevision := graphRequest(t, handler, url.Values{"to": {withdrawnRef + "@v99"}})
	if body := pastRevision.Body.String(); pastRevision.Code != http.StatusNotFound ||
		!strings.Contains(body, "no revision 99 of comment "+withdrawnComment.ID) || !strings.Contains(body, deletion) ||
		strings.Contains(body, withdrawn) {
		t.Fatalf("a pin past a deleted comment's current revision: status=%d body=%s, want 404 naming the revision and saying %q", pastRevision.Code, body, deletion)
	}

	if node := readGraph(t, handler, url.Values{"to": {standingRef}}).Node; node.Text != standing || node.Pin != nil {
		t.Fatalf("the unpinned read of a standing comment's block = %#v, want the text %q and no pin", node, standing)
	}
	pinned := readGraph(t, handler, url.Values{"to": {standingRef + "@v1"}}).Node
	if pinned.Pin == nil || pinned.Pin.Text != standing || pinned.Pin.State != "unchanged" {
		t.Fatalf("the pinned read of a standing comment's block = %#v, want its text unchanged", pinned.Pin)
	}
}

// A pending suggestion is proof in the document as much as a row: its mark says the suggestion
// stands, and an insert suggestion's text is in the document's own body. The delete rejects it,
// so the mark goes and an insert's text goes with it; nothing else could, since an accept and a
// reject of a deleted post answer 409 POST_DELETED and a resolve unmarks nothing. Each check
// reads the document rather than the comment row.
//
// A closed issue takes that delete whole: its document takes the reject as it takes the
// comment's own anchor writes (docs.RejectDeletedCommentSuggestion), so the withdrawn text
// leaves the document there too, where an accept and a reject of the suggestion are refused.
func TestDeleteCommentRejectsItsPendingSuggestion(t *testing.T) {
	for _, test := range []struct {
		name, quote, replaceWith, want string
		insert, closed                 bool
	}{
		{name: "a replace suggestion", quote: "quick", replaceWith: "slow", want: "The quick brown fox\n"},
		{name: "an insert suggestion", quote: "quick ", want: "The brown fox\n", insert: true},
		{name: "an insert suggestion on a closed issue", quote: "quick ", want: "The brown fox\n", insert: true, closed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Delete a pending suggestion", "The quick brown fox")
			anchor := map[string]any{"artifact": "spec", "quote": test.quote}
			if test.insert {
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    "m-insert",
					Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
				}, test.quote)
				anchor = map[string]any{"artifact": "spec", "mark_id": "m-insert"}
			}
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Say this instead.", "anchor": anchor,
				"suggestion": map[string]string{"replace_with": test.replaceWith},
			}, "alice")
			if comment.Anchor == nil || comment.Suggestion == nil {
				t.Fatalf("the comment was created as %#v, want an anchored suggestion", comment)
			}
			if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); err != nil {
				t.Fatalf("the suggestion's mark before the delete: %v, want it in the document", err)
			}
			// The suggestion stands when the issue is closed: a closed issue takes no suggestion
			// of its own, and this one was made while the issue was open.
			if test.closed {
				closeIssue(t, handler, issue.Key)
			}

			deleted := deleteThreadComment(t, handler, comment.ID, "alice")
			if deleted.Body != "" || deleted.DeletedAt == nil || deleted.Suggestion != nil {
				t.Fatalf("the delete answered %#v, want an empty body with the mark and no suggestion", deleted)
			}
			if stored := readThreadComment(t, handler, comment.ID); stored.Body != "" || stored.DeletedAt == nil {
				t.Fatalf("the comment reads back as %#v, want an empty body with the mark", stored)
			}

			// The mark is read as a closed issue's document is read for a comment: SuggestionKind
			// under docs.WithCommentMarks, which that document's room takes (allowInject).
			// VerifyMark is the open issue's read: its own owner-verified path is a comment
			// mark's alone (closedIssueCommentAnchor), so a suggestion's mark read that way on a
			// closed issue is refused.
			var markErr error
			if test.closed {
				_, markErr = documentService.SuggestionKind(docs.WithCommentMarks(context.Background()),
					issue.PrimaryArtifactID, comment.Anchor.MarkID)
			} else {
				_, markErr = documentService.VerifyMark(context.Background(),
					issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID)
			}
			if !errors.Is(markErr, docs.ErrAnchorMissing) {
				t.Fatalf("the suggestion's mark after the delete = %v, want it gone from the document", markErr)
			}
			markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if err != nil || markdown != test.want {
				t.Fatalf("the document after the delete = %q (%v), want %q", markdown, err, test.want)
			}
		})
	}
}

// An orphaned suggestion's mark is already out of the document, so the delete has nothing to
// reject: it records the orphaning on the anchor, as the accept and the reject arms do, and
// stands with 200 where those two answer 409. A delete nothing can undo is what a withdrawn post
// needs, and the orphaned mark is the one case the reject itself refuses.
func TestDeleteCommentWithAnOrphanedSuggestionStands(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an orphaned suggestion", "The quick brown fox")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Use red.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
		"suggestion": map[string]string{"replace_with": "red"},
	}, "alice")
	// A browser edit takes the marked text out of the live document, which orphans the mark;
	// the service settles no version here (Settle is an hour), so nothing has refreshed the
	// comment's own anchor and the row still reads as anchored, which the delete is left to
	// find out for itself.
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The quick fox\n")
	if before := readThreadComment(t, handler, comment.ID); before.Anchor == nil || before.Anchor.Orphaned {
		t.Fatalf("the comment before the delete = %#v, want an anchor not yet known to be orphaned", before.Anchor)
	}

	deleted := deleteThreadComment(t, handler, comment.ID, "alice")
	if deleted.Body != "" || deleted.DeletedAt == nil || deleted.Anchor == nil || !deleted.Anchor.Orphaned {
		t.Fatalf("the delete answered %#v, want an empty body with the mark and an orphaned anchor", deleted)
	}
	stored := readThreadComment(t, handler, comment.ID)
	if stored.Body != "" || stored.DeletedAt == nil || stored.Suggestion != nil {
		t.Fatalf("the comment reads back as %#v, want an empty body with the mark and no suggestion", stored)
	}
	if stored.Anchor == nil || !stored.Anchor.Orphaned {
		t.Fatalf("the deleted comment's anchor = %#v, want the orphaning recorded", stored.Anchor)
	}
}
