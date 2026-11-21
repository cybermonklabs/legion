package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A person's Retry reads the post before it takes the owner's lock, and an edit of the post takes
// that lock too. This row holds the lock while the Retry waits on it, edits the post as the edit
// route writes its row under the same lock, and lets the Retry through. The Retry must then send
// the text the edit wrote, in the frame and in the body beside it alike: both are built from the
// post as the claim reads it under the lock (mentionFrameEvent, recordPendingMessageDelivery). A
// frame built from what was read before the lock - a message's row, or the comment.created event
// a comment's frame replays - carries the text the edit replaced. holdWriterBehindOwnerLock is
// the race of the route table, as in post_deleted_retry_race_test.go.
func TestARetryQueuedBehindAnEditSendsTheEditedText(t *testing.T) {
	for _, row := range []struct {
		name  string
		ready func(t *testing.T, base routeFixture) lockedItemWrite
	}{
		{name: "the mention retry route", ready: readyRetryOfAnEditedMention},
		{name: "the message retry route", ready: readyRetryOfAnEditedMessage},
	} {
		t.Run(row.name, func(t *testing.T) { holdWriterBehindOwnerLock(t, row.name, row.ready(t, routeFixture{})) })
	}
}

// editedText is the body each row's edit writes while the Retry waits.
const editedText = "The corrected text the Retry must send."

// sentTheEditedText fails t unless the listener took two sends, the post's first and the Retry's,
// and the Retry's carries the edited text in its frame and beside it.
func sentTheEditedText(t *testing.T, frames []listenedFrame) {
	t.Helper()
	if len(frames) != 2 {
		t.Fatalf("the listener took %d sends, want the post's first and the Retry's", len(frames))
	}
	if retried := frames[1]; retried.message != editedText || retried.eventBody != editedText {
		t.Fatalf("the Retry sent %q beside a frame carrying %q, want the edited text %q in both",
			retried.message, retried.eventBody, editedText)
	}
}

// readyRetryOfAnEditedMention is readyMentionRetry with the comment edited while the Retry waits.
func readyRetryOfAnEditedMention(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, frames := recordingSessionListener(t, "steer")
	return mentionRetryWrite(t, base, listener.URL, "The mention whose text an edit replaces.",
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated || decodeBody[commentDeliveryRead](t, answer).Attempt != 2 {
				t.Fatalf("retry the edited mention: status=%d body=%s, want 201 and attempt 2", answer.Code, answer.Body.String())
			}
			sentTheEditedText(t, frames())
		},
		func(t *testing.T, holder pgx.Tx, _ itemRowFixture, commentID string) {
			t.Helper()
			if _, err := holder.Exec(t.Context(), `update comments set body = $2, edited_at = now() where id = $1`,
				commentID, editedText); err != nil {
				t.Fatalf("edit the comment while the Retry waits: %v", err)
			}
		})
}

// readyRetryOfAnEditedMessage is readyMessageRetry with the message edited while the Retry waits.
func readyRetryOfAnEditedMessage(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, frames := recordingSessionListener(t, "btw")
	return messageRetryWrite(t, base, listener.URL, "The message whose text an edit replaces.",
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated || decodeBody[model.MessageDelivery](t, answer).Attempt != 2 {
				t.Fatalf("retry the edited message: status=%d body=%s, want 201 and attempt 2", answer.Code, answer.Body.String())
			}
			sentTheEditedText(t, frames())
		},
		func(t *testing.T, holder pgx.Tx, _ itemRowFixture, messageID string) {
			t.Helper()
			if _, err := holder.Exec(t.Context(), `update messages set body = $2, edited_at = now() where id = $1`,
				messageID, editedText); err != nil {
				t.Fatalf("edit the message while the Retry waits: %v", err)
			}
		})
}
