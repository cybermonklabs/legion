package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
)

// R3 of dispatch://INFRA-1523: a delete is final, and a new delivery of a deleted post is refused
// 409 POST_DELETED, so no send carries the text the delete withdrew. A person's Retry reads the
// post before it takes the owner's lock; this row holds that lock while the Retry waits on it,
// deletes the post as the delete route writes it under the same lock, and lets the Retry through.
// The Retry must then read the post again under the lock and refuse, opening no attempt: a Retry
// judged on the post it read before the lock claims an attempt after the delete and sends the
// withdrawn body. holdWriterBehindOwnerLock is the race of the route table: the writer queues on
// the owner lock with nothing written, and the holder's meanwhile commits before the writer runs.
func TestARetryQueuedBehindADeleteOpensNoAttemptOfTheDeletedPost(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		ready func(t *testing.T, base routeFixture) lockedItemWrite
	}{
		{name: "the mention retry route", ready: readyRetryOfADeletedMention},
		{name: "the message retry route", ready: readyRetryOfADeletedMessage},
	} {
		t.Run(row.name, func(t *testing.T) { holdWriterBehindOwnerLock(t, row.name, row.ready(t, routeFixture{})) })
	}
}

// readyRetryOfADeletedMention is readyMentionRetry with the comment deleted while the Retry waits.
func readyRetryOfADeletedMention(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "steer")
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Mention retry behind a delete")
	comment := decodeMentionedComment(t, postMentionedComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body":     "The withdrawn mention the Retry must not send again.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, fixture.database, comment.ID)
	write := fixture.routeWrite(t, "the delivery attempt it claims",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			refusedAsDeleted(t, "a mention Retry the delete beat to the owner lock", answer)
			if attempts := fixture.rowCount(t, `select count(*) from comment_deliveries where comment_id = $1`, comment.ID); attempts != 1 {
				t.Fatalf("the deleted mention holds %d attempts after the Retry, want its one first attempt", attempts)
			}
		})
	write.meanwhile = func(t *testing.T, holder pgx.Tx) {
		t.Helper()
		if _, err := holder.Exec(t.Context(), `
			update comments set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'::jsonb
			where id = $1
		`, comment.ID); err != nil {
			t.Fatalf("delete the comment while the Retry waits: %v", err)
		}
	}
	return write
}

// readyRetryOfADeletedMessage is readyMessageRetry with the message deleted while the Retry waits.
func readyRetryOfADeletedMessage(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "btw")
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Message retry behind a delete")
	message := createIssueMessage(t, fixture.handler, fixture.issueKey, map[string]any{
		"body": "The withdrawn message the Retry must not send again.", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the message this row retries = %#v, want one attempt sent to the listener's session",
			message.Deliveries)
	}
	write := fixture.routeWrite(t, "the delivery attempt it claims",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
				map[string]any{"delivery": "btw"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			refusedAsDeleted(t, "a message Retry the delete beat to the owner lock", answer)
			if attempts := fixture.rowCount(t, `select count(*) from message_deliveries where message_id = $1`, message.ID); attempts != 1 {
				t.Fatalf("the deleted message holds %d attempts after the Retry, want its one first attempt", attempts)
			}
		})
	write.meanwhile = func(t *testing.T, holder pgx.Tx) {
		t.Helper()
		if _, err := holder.Exec(t.Context(), `
			update messages set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'::jsonb
			where id = $1
		`, message.ID); err != nil {
			t.Fatalf("delete the message while the Retry waits: %v", err)
		}
	}
	return write
}
