//go:build !race

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// messageRouteXidWindows is the rows of the route window table (route_xid_window_test.go) for the
// routes that write to people and sessions: an ask's followers and its decline, a mention's retry,
// a broadcast, a direct message, and the Inbox's mark-all and dismissal.
var messageRouteXidWindows = []routeXidWindowCase{
	{
		name: "the ask follow route", route: "PUT /api/v1/asks/{id}/followers/{session_id} (changeAskFollower)",
		ready: readyAskFollow, window: storetest.XidWindowOf(3,
			"the follower row's insert, the batch appending ask.follower_added and the COMMIT ending them"),
	},
	{
		name: "the ask unfollow route", route: "DELETE /api/v1/asks/{id}/followers/{session_id} (changeAskFollower)",
		ready: readyAskUnfollow, window: storetest.XidWindowOf(3,
			"the follower row's delete, the batch appending ask.follower_removed and the COMMIT ending them"),
	},
	// The Retry runs three transactions, the listener call between the last two holding none:
	// the claim, the resolution's record on the attempt it claimed (recordCommentDeliveryResolution)
	// - that update and its COMMIT - and the settle - the attempt's update, the batch appending
	// the delivery's receipt and the COMMIT. One shape judges all three, so its bound is the
	// claim's.
	{
		name: "the mention retry route", route: "POST /api/v1/comments/{id}/deliveries (claimCommentDeliveryAttempt)",
		ready: readyMentionRetry, window: storetest.XidWindowOf(4,
			"the claim's lock of the mention row, its locked read of the pending attempt, the new "+
				"attempt's insert and the COMMIT ending them"),
	},
	// A broadcast writes inline, five statements per recipient, so its window grows with them;
	// this one goes to two.
	{
		name: "the broadcast route", route: "POST /api/v1/broadcasts (writeBroadcast)",
		ready: readyBroadcast, window: storetest.XidWindowOf(14,
			"the broadcast's insert, its keyed request's recovery point, then for each of its two "+
				"recipients the message's insert, the read of its block state, the batch writing its "+
				"block index, the batch appending message.created and its first attempt's insert, and "+
				"last the keyed request's commit mark and the COMMIT ending them"),
	},
	// The direct message runs three transactions, the listener call holding none: the create -
	// its write phase's one batch, the keyed request's commit mark and its COMMIT - the
	// delivery's claim, and its settle - the attempt's update, the batch appending
	// message.delivery, the commit mark and the COMMIT. One shape judges all three, so its
	// bound is the claim's.
	{
		name: "the direct message route", route: "POST /api/v1/agents/{session_id}/messages (createAgentMessage, createStoredMessage)",
		ready: readyDirectMessage, window: storetest.XidWindowOf(5,
			"the claim's lock of the message row, its read of the pending attempt, the attempt's "+
				"claim, the keyed request's commit mark and the COMMIT ending them"),
	},
	// A decline of a block ask writes the block resolved into the spec, and that document write
	// is queued on the ledger with the ask's own writes: the window is the one batch.
	{
		name: "the ask decline route", route: "POST /api/v1/asks/{id}/decline on a block ask (declineAsk)",
		ready: readyBlockAskDecline, window: storetest.XidBatchedWindow,
	},
	{
		name: "the inbox mark-all route", route: "POST /api/v1/me/inbox/read (markInboxRead)",
		ready: readyInboxMarkAll, window: storetest.XidWindowOf(5,
			"the lock of the caller's thread rows, the marks' update, the lock of the project its "+
				"event names, the batch appending user_thread_state.updated and the COMMIT ending them"),
	},
	{
		name: "the inbox dismiss route", route: "POST /api/v1/me/inbox/dismiss (dismissInbox)",
		ready: readyInboxDismiss, window: storetest.XidWindowOf(5,
			"the lock of the caller's thread rows, the dismissals' update, the lock of the project its "+
				"event names, the batch appending user_thread_state.updated and the COMMIT ending them"),
	},
}

// readyAskUnfollow drives DELETE /api/v1/asks/{id}/followers/{session_id}, a person taking a
// follower off an ask, which deletes the follower row under the ask owner's lock
// (changeAskFollower).
func readyAskUnfollow(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Ask unfollow")
	created := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks",
		map[string]any{"question": "Who follows this?"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	if followed := dispatchRequest(t, fixture.handler, http.MethodPut, "/api/v1/asks/"+ask.ID+"/followers/s3", nil, "alice"); followed.Code != http.StatusNoContent {
		t.Fatalf("follow the ask: status=%d body=%s", followed.Code, followed.Body.String())
	}
	return fixture.routeWrite(t, "the ask's follower row it deletes",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+ask.ID+"/followers/s3", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusNoContent {
				t.Fatalf("unfollow the ask: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if followers := fixture.rowCount(t, `select count(*) from ask_followers where ask_id = $1 and session_id = 's3'`, ask.ID); followers != 0 {
				t.Fatalf("the ask still has %d followers named s3", followers)
			}
		})
}

// readyBroadcast drives POST /api/v1/broadcasts to two live sessions, which writes the broadcast,
// its recovery point and each recipient's message, index, event and first attempt in one
// transaction (writeBroadcast) and then delivers to each.
func readyBroadcast(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Broadcast")
	return fixture.routeWrite(t, "the broadcast and its messages",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postBroadcast(t, handler, map[string]any{
				"body": "Stand down and report status.", "delivery": "steer",
				"session_ids": []string{"planner", "tester"},
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("broadcast: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if sent := decodeBody[broadcastResponse](t, answer); len(sent.Recipients) != 2 {
				t.Fatalf("the broadcast reached %d recipients, want planner and tester", len(sent.Recipients))
			}
		})
}

// readyDirectMessage drives POST /api/v1/agents/{session_id}/messages, a person's direct message
// to a live session, which stores the message (createStoredMessage) and delivers it.
func readyDirectMessage(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, _ := newBroadcastListener(t, xidDeliverySessions)
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Direct message")
	return fixture.routeWrite(t, "the direct message",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postAgentMessage(t, handler, "s1", map[string]any{
				"body": "Read the migration section of the spec and tell me what it costs.", "delivery": "aside",
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("direct message to s1: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyBlockAskDecline drives POST /api/v1/asks/{id}/decline on the ask settlement opened from
// the spec's `:::ask` block, whose block the decline writes resolved into the spec.
func readyBlockAskDecline(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.spec += "\n" + transportAsk
	fixture := base.issue(t, "Decline")
	awaitIndexedAskBlock(t, fixture.handler, fixture.artifactID, "decision", "Which transport?")
	askID := blockAskID(t, fixture.handler, fixture.issueKey, "decision")
	note := "The team decides this one, not me."
	return fixture.routeWrite(t, "the declined ask",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/decline", declineBody(&note), "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("decline the ask: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if resolution := storedAskResolution(t, fixture.database, askID); resolution.Kind != "declined" {
				t.Fatalf("the ask's resolution = %q, want declined", resolution.Kind)
			}
		})
}

// unreadReplyThread gives alice a Reply thread in her Inbox: her own comment on the issue, which
// gives her the thread's user_threads row, and a session's reply to it, which leaves that row
// unread, so a mark or a dismissal of her Inbox moves it.
func unreadReplyThread(t *testing.T, fixture itemRowFixture) model.Comment {
	t.Helper()
	root := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body": "Does the migration section still hold?",
	}, "alice")
	sessionComment(t, fixture.handler, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
		"body": "It does, and the next release carries it.", "reply_to": root.ID,
	})
	return root
}

// readyInboxMarkAll drives POST /api/v1/me/inbox/read, the Inbox's Mark all read, over one
// unread Reply thread (markInboxReadThreads).
func readyInboxMarkAll(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Inbox mark all")
	unreadReplyThread(t, fixture)
	return fixture.routeWrite(t, "the thread's read mark",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{
				"read_through": time.Now().UTC().Format(time.RFC3339Nano),
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("mark the inbox read: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if marked := decodeBody[inboxReadResponse](t, answer).Marked; marked != 1 {
				t.Fatalf("the mark-all marked %d threads, want the one unread thread", marked)
			}
		})
}

// readyInboxDismiss drives POST /api/v1/me/inbox/dismiss under the list's filters, over one
// unread Reply thread (dismissInboxThreads).
func readyInboxDismiss(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Inbox dismiss")
	unreadReplyThread(t, fixture)
	return fixture.routeWrite(t, "the thread's dismissal",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postInboxDismiss(t, handler, "alice", map[string]any{
				"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("dismiss the inbox: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if dismissed := dismissedBy(t, answer); dismissed != 1 {
				t.Fatalf("the dismissal changed %d threads, want the one unread thread", dismissed)
			}
		})
}
