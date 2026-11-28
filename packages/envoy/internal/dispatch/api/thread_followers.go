package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Comment thread followers: the sessions a person's later replies in a comment thread reach on
// their own topics. Every session that writes in a thread follows it, with no row of its own (the
// outbox reads the thread's authors, outbox.commentRecipientsQuery); these routes record a
// session's own choice beside that, on the thread's root comment (comment_thread_follows): to
// leave the thread, so its replies stop reaching it, or to follow it again, or to follow a thread
// it never wrote in. A later comment of the session in the thread rejoins it, as a reply to an ask
// follows the ask. A reply that mentions the session reaches it whatever it chose: a mention is a
// delivery of its own.
//
// Who may act is the ask followers' rule (followerActor): a credential acting as the addressed
// session for that session alone, and a person, with the `person` scope, for any session. An ask's
// thread is the ask's, so a comment in one is refused 409 ASK_THREAD, naming the ask its followers
// route serves.

// PUT /api/v1/comments/{id}/followers/{session_id}
func (s *server) followCommentThread(w http.ResponseWriter, r *http.Request) {
	s.changeThreadFollower(w, r, true)
}

// DELETE /api/v1/comments/{id}/followers/{session_id}
func (s *server) unfollowCommentThread(w http.ResponseWriter, r *http.Request) {
	s.changeThreadFollower(w, r, false)
}

func (s *server) changeThreadFollower(w http.ResponseWriter, r *http.Request, follow bool) {
	if !requireUUIDPath(w, r, "comment") {
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, "FOLLOWER_INPUT", http.StatusBadRequest, "follower session id is required")
		return
	}
	actor, ok := s.followerActor(w, r, sessionID)
	if !ok {
		return
	}

	ctx := r.Context()
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	comment, err := s.loadComment(ctx, tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	own := ownerOf(comment.IssueKey, comment.ArtifactID)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The thread's head, climbed under the owner's lock: its root comment, or the ask it hangs
	// under.
	root, askID, err := threadRootOf(ctx, tx, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if askID != "" {
		verb := "unfollow"
		if follow {
			verb = "follow"
		}
		writeError(w, "ASK_THREAD", http.StatusConflict, fmt.Sprintf(
			"comment %s is in the thread of ask %s, which is the ask's to follow: run `dispatch %s --ask %s` (%s /api/v1/asks/%s/followers/%s)",
			comment.ID, askID, verb, askID, r.Method, askID, sessionID))
		return
	}
	if root == "" {
		writeError(w, "INVALID_COMMENT", http.StatusConflict, fmt.Sprintf(
			"comment %s's thread has no root comment within %d replies above it", comment.ID, maxThreadFollowDepth))
		return
	}
	changed, err := setThreadFollow(ctx, tx, root, sessionID, follow)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	eventType := "thread.follower_removed"
	if follow {
		eventType = "thread.follower_added"
	}
	event, err := s.appendEvent(ctx, tx, own.event(eventType, actor, model.ThreadFollowerEventPayload{
		CommentID: root, SessionID: sessionID, By: actor,
	}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(ctx, event)
	w.WriteHeader(http.StatusNoContent)
}

// threadFollowState is what the store says of one session in one thread before a change: the
// session's recorded choice (comment_thread_follows), if any, and when the session last wrote a
// comment in the thread, if ever.
type threadFollowState struct {
	recorded    bool
	follows     bool
	since       time.Time
	lastWritten *time.Time
}

// following is whether the thread's replies reach the session by the thread itself: its choice
// to follow, or a comment it wrote there that no later leave stands over. It is the outbox's rule
// (outbox.commentRecipientsQuery) without the credits of the thread's document, which a thread
// names no session by.
func (state threadFollowState) following() bool {
	if state.recorded && state.follows {
		return true
	}
	return state.lastWritten != nil && (!state.recorded || state.lastWritten.After(state.since))
}

// leaveStands is whether the session's recorded choice is a leave that no later comment of it in
// the thread has undone, which a repeated leave leaves as it is.
func (state threadFollowState) leaveStands() bool {
	return state.recorded && !state.follows &&
		(state.lastWritten == nil || !state.lastWritten.After(state.since))
}

// threadFollowStateQuery reads threadFollowState for the thread rooted at $1 and the session $2.
// It takes no row lock: every writer of comment_thread_follows holds the thread owner's lock
// (changeThreadFollower), so the row holds still until the caller commits. The walk down from the
// root reads every comment of the thread to the depth the outbox's walk stops at
// (maxThreadFollowDepth), legacy nested replies included.
const threadFollowStateQuery = `
	with recursive thread as (
		select id, 0 as depth from comments where id = $1
		union
		select c.id, t.depth + 1
		from thread t
		join comments c on c.reply_to = t.id
		where t.depth < $3
	),
	written as (
		select max(c.created_at) as at
		from thread t
		join comments c on c.id = t.id
		where c.author->>'kind' = 'session' and c.author->>'id' = $2
	)
	select f.follows, f.since, (select at from written)
	from (select 1) one
	left join comment_thread_follows f on f.comment_id = $1 and f.session_id = $2
`

// maxThreadFollowDepth bounds the walks up and down a thread, as the outbox's
// maxCommentThreadDepth bounds its walks, edge for edge (outbox.commentRecipientsQuery): a comment
// the outbox counts in a thread names that thread here. comments.reply_to has no acyclicity
// constraint, so a cycle ends at the bound rather than spinning.
const maxThreadFollowDepth = 32

// threadRootOf climbs reply_to from commentID to the head of its thread, as the outbox's walk up
// does: the root comment's id, or, where a comment of the chain is in an ask's thread, that ask's
// id instead. Both are empty for a chain that reaches no root within maxThreadFollowDepth replies,
// which only a cycle or a legacy chain deeper than the outbox reads can be.
func threadRootOf(ctx context.Context, tx pgx.Tx, commentID string) (root, askID string, err error) {
	var rootID, ask *string
	if err := tx.QueryRow(ctx, `
		with recursive up as (
			select id, reply_to, ask_id, 0 as depth from comments where id = $1
			union
			select c.id, c.reply_to, c.ask_id, u.depth + 1
			from up u
			join comments c on c.id = u.reply_to
			where u.depth < $2
		)
		select (select id::text from up where reply_to is null order by depth limit 1),
		       (select ask_id::text from up where ask_id is not null order by depth limit 1)
	`, commentID, maxThreadFollowDepth).Scan(&rootID, &ask); err != nil {
		return "", "", fmt.Errorf("find the thread of comment %s: %w", commentID, err)
	}
	if ask != nil {
		return "", *ask, nil
	}
	if rootID == nil {
		return "", "", nil
	}
	return *rootID, "", nil
}

// setThreadFollow records that sessionID follows (follow) or left the thread rooted at root, in the
// caller's transaction, and reports whether that changed anything: a follow changes nothing for a
// session the thread already reaches (threadFollowState.following), and a leave nothing for one
// whose leave stands. A change writes the choice with the database's clock_timestamp(), the clock
// a comment's created_at is written on, so the outbox compares the two on one clock.
func setThreadFollow(ctx context.Context, tx pgx.Tx, root, sessionID string, follow bool) (bool, error) {
	var state threadFollowState
	var follows *bool
	var since *time.Time
	if err := tx.QueryRow(ctx, threadFollowStateQuery, root, sessionID, maxThreadFollowDepth).
		Scan(&follows, &since, &state.lastWritten); err != nil {
		return false, fmt.Errorf("read session %s's place in the thread of comment %s: %w", sessionID, root, err)
	}
	if follows != nil && since != nil {
		state.recorded, state.follows, state.since = true, *follows, *since
	}
	if follow && state.following() || !follow && state.leaveStands() {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		insert into comment_thread_follows (comment_id, session_id, follows, since)
		values ($1, $2, $3, clock_timestamp())
		on conflict (comment_id, session_id) do update set follows = excluded.follows, since = excluded.since
	`, root, sessionID, follow); err != nil {
		return false, fmt.Errorf("record session %s's place in the thread of comment %s: %w", sessionID, root, err)
	}
	return true, nil
}
