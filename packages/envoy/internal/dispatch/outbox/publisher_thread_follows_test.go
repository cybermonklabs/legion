package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// chooseThread records session's own choice on the thread rooted at root, as the PUT (follows) and
// DELETE (leaves) of /api/v1/comments/{id}/followers/{session_id} record it, at the database's
// clock_timestamp(), the clock a comment's created_at is written on.
func chooseThread(t *testing.T, database *store.Store, root, session string, follows bool) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_thread_follows (comment_id, session_id, follows, since)
		values ($1, $2, $3, clock_timestamp())
		on conflict (comment_id, session_id) do update set follows = excluded.follows, since = excluded.since
	`, root, session, follows); err != nil {
		t.Fatalf("record %s's choice on thread %s: %v", session, root, err)
	}
}

// TestASessionThatLeftAThreadIsLeftOutOfItsReplies appends comment events as the API writes them,
// runs one batch of the outbox over them, and reads which sessions' own topics each event reached,
// for a thread a session left. Each case's control is a session beside it that did not leave.
func TestASessionThatLeftAThreadIsLeftOutOfItsReplies(t *testing.T) {
	t.Parallel()
	alice := model.Actor{Kind: "user", ID: "alice"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	leaver := model.Actor{Kind: "session", ID: "session-leaver"}
	stayer := model.Actor{Kind: "session", ID: "session-stayer"}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck
	}{
		{
			// The reported case.
			name: "a person's reply, its edit and its delete reach the session that stayed and not the one that left",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", leaver, &root)
				seedThreadComment(t, database, "T-1", "", stayer, &root)
				chooseThread(t, database, root, leaver.ID, false)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				edited := appendCommentEvent(t, database, broker, "comment.edited", alice, model.Comment{ID: reply, ReplyTo: &root})
				deleted := appendCommentEvent(t, database, broker, "comment.deleted", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: created, want: []string{stayer.ID}},
					{event: edited, want: []string{stayer.ID}},
					{event: deleted, want: []string{stayer.ID}},
				}
			},
		},
		{
			name: "a session that left the thread its document credits it for is left out on an unclaimed issue",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer, stayer})
				root := seedThreadComment(t, database, "T-1", spec, alice, nil)
				chooseThread(t, database, root, writer.ID, false)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{stayer.ID}}}
			},
		},
		{
			name: "a session that left the thread and wrote in it after rejoins it",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", leaver, &root)
				chooseThread(t, database, root, leaver.ID, false)
				seedThreadComment(t, database, "T-1", "", leaver, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{leaver.ID}}}
			},
		},
		{
			name: "a session that follows a thread it never wrote in hears its replies after the sessions that wrote there, whatever the claim",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				chooseThread(t, database, root, leaver.ID, true)
				seedThreadComment(t, database, "T-1", "", stayer, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{claimant.ID, stayer.ID, leaver.ID}}}
			},
		},
		{
			// A mention is a delivery of its own: one that reached the session delivered the
			// comment, and one that failed leaves the copy to the outbox, under its key, for a
			// session that left the thread as for any other.
			name: "a reply that mentions a session that left the thread is left to the mention once it delivered, and published to when it failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", leaver, &root)
				seedThreadComment(t, database, "T-1", "", rooter, &root)
				chooseThread(t, database, root, leaver.ID, false)
				chooseThread(t, database, root, rooter.ID, false)
				sent := seedThreadComment(t, database, "T-1", "", alice, &root)
				seedMentionDelivery(t, database, sent, leaver.ID, "sent")
				failed := seedThreadComment(t, database, "T-1", "", alice, &root)
				seedMentionDelivery(t, database, failed, rooter.ID, "failed")
				delivered := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: sent, ReplyTo: &root}, leaver.ID)
				fallback := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: failed, ReplyTo: &root}, rooter.ID)
				return []recipientCheck{
					{event: delivered, want: nil},
					{event: fallback, want: []string{rooter.ID}},
				}
			},
		},
		{
			// A reply reopens a resolved thread, and the reopening reaches the thread root's
			// author alone: not one that left the thread.
			name: "a resolution and a reopening skip a root author that left the thread",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				otherRoot := seedThreadComment(t, database, "T-1", "", stayer, nil)
				chooseThread(t, database, root, rooter.ID, false)
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", alice, model.Comment{ID: root})
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", alice, model.Comment{ID: root})
				control := appendCommentEvent(t, database, broker, "comment.reopened", alice, model.Comment{ID: otherRoot})
				return []recipientCheck{
					{event: resolved, want: nil},
					{event: reopened, want: nil},
					{event: control, want: []string{stayer.ID}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			checkSessionRecipients(t, database, tc.arrange(t, database, broker))
		})
	}
}
