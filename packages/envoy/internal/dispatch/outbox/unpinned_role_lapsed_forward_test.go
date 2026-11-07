package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// A comment with two unpinned mentions, both past the lease the outbox waits a send out for and
// both still owed a resend, holds the forward bound for a session either may be pinned to, as a
// live one beside a lapsed one does (TestAForwardWaitsForALiveUnnamedMentionBesideALapsedOne):
// no key stands for either mention, one key serving one mention, and the resend each is owed can
// pin it to this very holder (unpinnedRoleMentions.keyFor, mentionHolds.waitsForAnUnnamedMention,
// roleCopyWaitsForAMention).
//
// That wait is what round 19 added. waitsForAnUnnamedMention asked whether such an attempt was
// live, which neither is here, and keyFor answers "" for two targets, so the forward went in the
// pass that reads the event under contracts.RoleForwardDedupePrefix and dispatch-<event id> - a
// key that host never claimed - and the mention a resend then pinned to this holder landed
// beside it. With one such mention the forward carries that mention's own key instead and goes
// in that pass, past the lease as while the attempt is live
// (TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey).
//
// The wait costs the comment's own response no copy, which is what this pass is read for: every
// other destination of the event is reached in it, the event is blocked for mentionPendingDelay
// alone and no attempt is counted. Nor is the holder's own copy later for being waited for here
// rather than held as a comment_fallback_copies row: the pass that reads such a row waits on
// that same owed resend (mentionHolds.state, decideFallbackCopy), while a copy taken out of the
// event's list would let the owner's later events bound for this holder overtake the comment,
// nothing else delivering a role copy (destination.waiting, planChains).
func TestAForwardWaitsForTwoUnnamedMentionsPastTheirLease(t *testing.T) {
	const routed = "routed-to-a-session-either-mention-may-name"
	const first = "mentioned-first-whose-attempt-is-past-its-lease"
	const second = "mentioned-second-whose-attempt-is-past-its-lease"
	const holder = "ses_holds_the_routed_role"
	const issueTopic = "notifications.dispatch.issue.T-1.comment.created"
	ctx := store.WithTransactionTracking(context.Background())
	firstTarget, secondTarget := "role:"+first, "role:"+second
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + firstTarget + " @" + secondTarget + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: firstTarget, Delivery: "steer"}, {Target: secondTarget, Delivery: "steer"}}},
		},
	})
	// Both senders died with their sends in flight, so the delivery sweep owes each attempt a
	// resend and the outbox waits neither out any longer (model.MentionResumedSQL).
	seedDelivery(t, rig.database, commentID, firstTarget, "", "resumed")
	seedDelivery(t, rig.database, commentID, secondTarget, "", "resumed")

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)

	if got := recv.received(); len(got) != 0 {
		t.Fatalf("the route holder was forwarded %v past the lease while both unnamed mentions are owed a resend, want nothing: one key serves one mention, so neither copy can carry the key the mention a resend pins here would repeat",
			keysOf(got))
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while a resend of an unpinned mention of the comment is still owed", at)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on the comment's unpinned mentions, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if !slices.Contains(reached, issueTopic) {
		t.Fatalf("the comment reached %v, want %s among them: the forward's wait is the one copy it holds, and costs the comment's own response nothing",
			reached, issueTopic)
	}
	if slices.Contains(reached, contracts.AgentSubject(holder)) || slices.Contains(reached, contracts.RoleTopicPrefix+routed) {
		t.Fatalf("the comment reached %v, want neither the route %s nor %s among them: the forward waits for the comment's unpinned mentions",
			reached, route, contracts.AgentSubject(holder))
	}

	// Both resends are made, resolve to no session and fail, as a sweep's pass makes them:
	// nothing is pinned, so the copy is the outbox's and goes under the event's own key.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("fail the comment's mentions: %v", err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once both mentions settled: attempt_count %d, publish_error %s, reached %v",
			attempts, publishFailure(failure), reached)
	}
	waitFor(t, 10*time.Second, "the copy both mentions' failure owes", func() bool { return len(recv.received()) > 0 })
}

// The one unpinned mention of a comment keys the forward past its lease as it does while its
// send is live (TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey): the copy
// carries the key that mention's own send goes under at this holder, so whichever of the two
// reaches the host second is dropped there, and the forward goes in the pass that reads the
// event rather than waiting out the resend (unpinnedRoleMentions.keyFor, roleFallbackKey).
//
// It is the arm that holds the wait above narrow, and the reason the two-target case is a
// defect rather than the rule: waitsForAnUnnamedMention asks for an owed send and for no key
// standing for it, and one target always has that key (mentionHolds.waitsForAnUnnamedMention).
// A comment whose resend is owed therefore costs this holder nothing where the outbox can mint
// the mention's key for it.
func TestAForwardCarriesTheKeyOfAnUnnamedMentionPastItsLease(t *testing.T) {
	const routed = "routed-to-a-session-the-unanswered-mention-may-name"
	const mentioned = "mentioned-and-held-by-nobody-the-outbox-can-name"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + mentioned
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	rig.silent(t, contracts.RoleTopicPrefix+mentioned)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			// No suppression: the route is another role than the mention's target.
		},
	})
	// The sender died with the send in flight, so the sweep owes this attempt a resend and the
	// outbox waits it out no longer (model.MentionResumedSQL).
	seedDelivery(t, rig.database, commentID, target, "", "resumed")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	waitFor(t, 5*time.Second, "the forward the mention's key covers", func() bool { return len(recv.received()) > 0 })
	forwards := recv.received()
	if len(forwards) != 1 || forwards[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v in the pass that read the comment, want one copy under %q, the key the mention of %s carries wherever a resend pins it: the one-target forward waits for no owed resend, the key standing for that mention",
			holder, forwards, want, target)
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Nothing of it waits: the one session its forward reaches takes the copy under the mention's own key",
			attempts, publishFailure(failure), reached)
	}

	// The resend lands on this very session, as it may: the sweep pins the session on the row
	// and the send goes under that same key, which the host drops.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2) where comment_id = $1 and target = $3
	`, commentID, holder, target); err != nil {
		t.Fatalf("settle the mention of %s: %v", target, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)

	if copies := recv.received(); len(copies) != 1 {
		t.Fatalf("%s was sent %+v once the resend was pinned and sent, want the one copy it already has: the copy carries that mention's own key, so the session is shown the comment once",
			holder, copies)
	}
}
