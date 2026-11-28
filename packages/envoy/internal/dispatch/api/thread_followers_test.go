package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// followThread follows (PUT) or leaves (DELETE) the thread of commentID as session itself.
func (f *answersFixture) followThread(t *testing.T, method, commentID, session string) {
	t.Helper()
	response := sessionRequest(t, f.handler, method, "/api/v1/comments/"+commentID+"/followers/"+session, map[string]any{
		"actor": answersActor(session),
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("%s %s as a follower of the thread of comment %s: status=%d body=%s", method, session, commentID, response.Code, response.Body.String())
	}
}

// reaches says whether the comment.created event of commentID was published to session's own
// topic.
func (f *answersFixture) reaches(t *testing.T, commentID, session string) bool {
	t.Helper()
	return slices.Contains(f.destinations(t, "comment.created", commentID), contracts.LegacyAgentSubject(session))
}

// The reported case: a session that wrote in a person's comment thread kept receiving every later
// reply in it, and nothing let it stop. Once it leaves the thread, a person's later reply reaches
// the other session that wrote there and not the one that left, and the one that left lists
// nothing of it among what people wrote to it. The control is the other session, which wrote in
// the same thread and did not leave.
func TestASessionThatLeftACommentThreadIsNotSentItsLaterReplies(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
	f.comment(t, map[string]any{"body": "Friday works for me too.", "reply_to": root}, answersOther)
	f.followThread(t, http.MethodDelete, root, answersSession)

	reply := f.comment(t, map[string]any{"body": "Then Friday it is.", "reply_to": root}, "alice")
	f.publishOutbox(t)

	if f.reaches(t, reply, answersSession) {
		t.Fatalf("the reply reached %s, which left the thread", answersSession)
	}
	if !f.reaches(t, reply, answersOther) {
		t.Fatalf("the reply did not reach %s, which wrote in the thread and stayed", answersOther)
	}
	f.empty(t, answersSession)
	if item := f.only(t, answersOther); item.ID != reply || item.Relation != model.SessionAnswerRelationRepliedInCommentThread {
		t.Fatalf("%s's item = %+v, want alice's reply %s in the thread", answersOther, item, reply)
	}
}

// Leaving names the thread by any comment of it: a reply's id leaves the thread its root starts.
func TestLeavingAThreadByAReplysIDLeavesTheWholeThread(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	own := f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
	f.followThread(t, http.MethodDelete, own, answersSession)

	reply := f.comment(t, map[string]any{"body": "Then Friday it is.", "reply_to": own}, "alice")
	f.publishOutbox(t)
	if f.reaches(t, reply, answersSession) {
		t.Fatalf("the reply reached %s, which left the thread through its own reply's id", answersSession)
	}
}

// A comment as deep in a legacy nested thread as the outbox counts in it names that thread: the
// walk up to the root is bound edge for edge like the outbox's (maxThreadFollowDepth).
func TestLeavingAThreadByItsDeepestCommentTheOutboxCountsRecordsTheLeaveOnItsRoot(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	ctx := context.Background()
	deepest := root
	for range maxThreadFollowDepth {
		if err := f.database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, reply_to)
			values ($1, '{"kind":"user","id":"alice"}', 'A legacy nested reply.', $2)
			returning id::text
		`, f.issue, deepest).Scan(&deepest); err != nil {
			t.Fatalf("seed a nested reply: %v", err)
		}
	}
	f.followThread(t, http.MethodDelete, deepest, answersSession)
	var recorded string
	if err := f.database.Pool.QueryRow(ctx, `
		select comment_id::text from comment_thread_follows where session_id = $1 and not follows
	`, answersSession).Scan(&recorded); err != nil {
		t.Fatalf("read the recorded leave: %v", err)
	}
	if recorded != root {
		t.Fatalf("the leave is recorded on %s, want the thread's root %s", recorded, root)
	}
}

// A reply that mentions a session that left the thread still reaches it: the mention is a
// delivery of its own, which leaving a thread does not stop.
func TestAReplyThatMentionsASessionThatLeftTheThreadStillReachesIt(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
	f.followThread(t, http.MethodDelete, root, answersSession)

	mention := f.comment(t, map[string]any{
		"body": "@answers, can you confirm Friday?", "reply_to": root,
		"mentions": []map[string]any{{"target": "session:" + answersSession}},
	}, "alice")
	f.publishOutbox(t)
	if !f.sentCommentDelivery(t, mention, answersSession) {
		t.Fatalf("the mention of %s in the thread it left was not delivered", answersSession)
	}
	if item := f.only(t, answersSession); item.ID != mention || item.Relation != model.SessionAnswerRelationMentionedYou {
		t.Fatalf("%s's item = %+v, want alice's mention %s", answersSession, item, mention)
	}
}

// A session that left a thread rejoins it by following it again, or by writing in it again, as a
// reply to an ask follows the ask.
func TestASessionRejoinsAThreadItLeftByFollowingOrByWritingInIt(t *testing.T) {
	t.Parallel()
	t.Run("follow", func(t *testing.T) {
		t.Parallel()
		f := newAnswersFixture(t)
		root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
		f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
		f.followThread(t, http.MethodDelete, root, answersSession)
		f.followThread(t, http.MethodPut, root, answersSession)

		reply := f.comment(t, map[string]any{"body": "Then Friday it is.", "reply_to": root}, "alice")
		f.publishOutbox(t)
		if !f.reaches(t, reply, answersSession) {
			t.Fatalf("the reply did not reach %s, which followed the thread again", answersSession)
		}
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		f := newAnswersFixture(t)
		root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
		f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
		f.followThread(t, http.MethodDelete, root, answersSession)
		f.comment(t, map[string]any{"body": "On second thought, Thursday.", "reply_to": root}, answersSession)

		reply := f.comment(t, map[string]any{"body": "Thursday it is.", "reply_to": root}, "alice")
		f.publishOutbox(t)
		if !f.reaches(t, reply, answersSession) {
			t.Fatalf("the reply did not reach %s, which wrote in the thread after it left", answersSession)
		}
	})
	t.Run("a thread never written in", func(t *testing.T) {
		t.Parallel()
		f := newAnswersFixture(t)
		root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
		f.followThread(t, http.MethodPut, root, answersSession)

		reply := f.comment(t, map[string]any{"body": "Friday, then.", "reply_to": root}, "bob")
		f.publishOutbox(t)
		if !f.reaches(t, reply, answersSession) {
			t.Fatalf("the reply did not reach %s, which followed a thread it never wrote in", answersSession)
		}
	})
}

// Each change of a session's place in a thread appends one event naming the thread's root, the
// session and who made the change; a repeat of the same choice appends nothing.
func TestThreadFollowChangesAppendOneEventEach(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	own := f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)
	f.followThread(t, http.MethodPut, own, answersSession)
	f.followThread(t, http.MethodDelete, own, answersSession)
	f.followThread(t, http.MethodDelete, root, answersSession)
	f.followThread(t, http.MethodPut, root, answersSession)
	f.followThread(t, http.MethodPut, root, answersSession)

	rows, err := f.database.Pool.Query(context.Background(), `
		select type, payload->>'comment_id', payload->>'session_id', payload->'by'->>'id', actor->>'id'
		from events where type like 'thread.%' order by id
	`)
	if err != nil {
		t.Fatalf("read thread follow events: %v", err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var kind, comment, session, by, actor string
		if err := rows.Scan(&kind, &comment, &session, &by, &actor); err != nil {
			t.Fatalf("scan thread follow event: %v", err)
		}
		if comment != root || session != answersSession || by != answersSession || actor != answersSession {
			t.Fatalf("%s names comment %s session %s by %s actor %s, want the root %s and %s throughout", kind, comment, session, by, actor, root, answersSession)
		}
		seen = append(seen, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read thread follow events: %v", err)
	}
	if want := []string{"thread.follower_removed", "thread.follower_added"}; !slices.Equal(seen, want) {
		t.Fatalf("thread follow events = %v, want %v: a session that wrote in the thread already follows it, and each repeat changes nothing", seen, want)
	}
}

// A session credential leaves or joins a thread for itself alone; a person may move any session,
// and the write is the person's. An ask's thread is the ask's to follow, so its comments refuse the
// route, naming the ask.
func TestTheThreadFollowRouteActsOnTheCallersOwnSessionAndRefusesAnAsksThread(t *testing.T) {
	t.Parallel()
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "Which day do we cut the release?"}, "alice")
	f.comment(t, map[string]any{"body": "Friday.", "reply_to": root}, answersSession)

	other := sessionRequest(t, f.handler, http.MethodDelete, "/api/v1/comments/"+root+"/followers/"+answersSession, map[string]any{
		"actor": answersActor(answersOther),
	})
	if other.Code != http.StatusForbidden || responseCode(t, other) != "FOLLOWER_FORBIDDEN" {
		t.Fatalf("another session's unfollow: status=%d body=%s, want 403 FOLLOWER_FORBIDDEN", other.Code, other.Body.String())
	}

	person := dispatchRequest(t, f.handler, http.MethodDelete, "/api/v1/comments/"+root+"/followers/"+answersSession, nil, "alice")
	if person.Code != http.StatusNoContent {
		t.Fatalf("a person's unfollow of %s: status=%d body=%s", answersSession, person.Code, person.Body.String())
	}
	var actor, by string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select actor->>'id', payload->'by'->>'id' from events where type = 'thread.follower_removed'
	`).Scan(&actor, &by); err != nil {
		t.Fatalf("read the person's thread.follower_removed: %v", err)
	}
	if actor != "alice" || by != "alice" {
		t.Fatalf("thread.follower_removed actor=%s by=%s, want alice's own act", actor, by)
	}

	askID := f.ask(t, answersSession, "Which database backs the queue?")
	askReply := f.comment(t, map[string]any{"body": "Which pool size?", "ask_id": askID}, "alice")
	refused := sessionRequest(t, f.handler, http.MethodDelete, "/api/v1/comments/"+askReply+"/followers/"+answersSession, map[string]any{
		"actor": answersActor(answersSession),
	})
	if refused.Code != http.StatusConflict || responseCode(t, refused) != "ASK_THREAD" {
		t.Fatalf("unfollow of an ask thread's comment: status=%d body=%s, want 409 ASK_THREAD", refused.Code, refused.Body.String())
	}

	missing := sessionRequest(t, f.handler, http.MethodDelete, "/api/v1/comments/00000000-0000-4000-8000-000000000001/followers/"+answersSession, map[string]any{
		"actor": answersActor(answersSession),
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unfollow of a comment that does not exist: status=%d body=%s, want 404", missing.Code, missing.Body.String())
	}
}
