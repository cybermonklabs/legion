package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// A comment with two mentions this read can name no holder for, one settled and one live, keys
// the forward bound for the holder of its role route by that live mention, as a comment with
// that one mention alone does (TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey):
// a settled attempt is in no unpinned reading at all - only an attempt whose send is still
// coming is recorded, live or past its lease (loadMentionDeliveries,
// unpinnedRoleMentions.record) - so one key stands for the one mention still owed, and the
// forward carries it and goes in the pass that reads the event rather than waiting
// (unpinnedRoleMentions.keyFor, mentionHolds.waitsForAnUnnamedMention, roleFallbackKey).
//
// That is what makes this the one-target arm rather than the two-target one
// (TestTwoUnpinnedRoleMentionsHoldTheirCopiesAsBefore,
// TestAForwardWaitsForALiveUnnamedMentionBesideALapsedOne): two mentions of the comment named no
// session, but only one of them can still be pinned anywhere, and a key that stands for one
// mention stands beside no other.
func TestAForwardCarriesTheKeyOfALiveUnnamedMentionBesideASettledOne(t *testing.T) {
	const routed = "routed-to-a-session-the-live-mention-may-name"
	const settled = "mentioned-first-whose-attempt-has-settled"
	const live = "mentioned-second-whose-attempt-is-live"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	settledTarget, liveTarget := "role:"+settled, "role:"+live
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	rig.silent(t, contracts.RoleTopicPrefix+settled)
	rig.silent(t, contracts.RoleTopicPrefix+live)
	recv := rig.receiver(t, holder)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + settledTarget + " @" + liveTarget + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: settledTarget, Delivery: "steer"}, {Target: liveTarget, Delivery: "steer"}}},
			// No suppression: the route is another role than either mention's target.
		},
	})
	// The first mention's sender resolved its role to no session and failed the attempt, which
	// settles it: no send of it is owed, and it names no session for any copy to be keyed by.
	// The second is a send a sender claimed just now, and it can be pinned to this very holder.
	seedDelivery(t, rig.database, commentID, settledTarget, "", "failed")
	seedDelivery(t, rig.database, commentID, liveTarget, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, liveTarget, "steer"))
	prefixed := contracts.RoleForwardDedupePrefix + fmt.Sprintf("dispatch-%d", comment.ID)
	waitFor(t, 10*time.Second, "the forward the live mention's key covers", func() bool { return len(recv.received()) > 0 })
	forwards := recv.received()
	if len(forwards) != 1 || forwards[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v in the pass that read the comment, want one copy under %q, the key the mention of %s carries wherever its sender pins it: the settled mention of %s is no unpinned attempt of this comment, so that one key stands for the one mention still owed. A copy under %q is a key that host never claimed, and the mention a sender pins here would land beside it",
			holder, forwards, want, liveTarget, settledTarget, prefixed)
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Nothing of it waits: the one session its forward reaches takes the copy under the live mention's own key",
			attempts, publishFailure(failure), reached)
	}
}

// The same comment with the surviving mention past the lease the outbox waits a send out for:
// the forward carries that mention's key and goes in the pass that reads the event, as it does
// for a comment whose one unpinned mention is past its lease
// (TestAForwardCarriesTheKeyOfAnUnnamedMentionPastItsLease). The settled attempt beside it is in
// no unpinned reading (loadMentionDeliveries), so one key stands for the resend still owed and
// the forward waits for nothing (unpinnedRoleMentions.keyFor,
// mentionHolds.waitsForAnUnnamedMention).
//
// It is the arm that holds TestAForwardWaitsForTwoUnnamedMentionsPastTheirLease narrow: two
// mentions past that lease wait, because no key can serve both, and the same comment with one of
// them settled does not, because then one key serves the one that is left.
func TestAForwardCarriesTheKeyOfALapsedUnnamedMentionBesideASettledOne(t *testing.T) {
	const routed = "routed-to-a-session-the-lapsed-mention-may-name"
	const settled = "mentioned-first-whose-attempt-has-settled"
	const lapsed = "mentioned-second-whose-attempt-is-past-its-lease"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	settledTarget, lapsedTarget := "role:"+settled, "role:"+lapsed
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	rig.silent(t, contracts.RoleTopicPrefix+settled)
	rig.silent(t, contracts.RoleTopicPrefix+lapsed)
	recv := rig.receiver(t, holder)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + settledTarget + " @" + lapsedTarget + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: settledTarget, Delivery: "steer"}, {Target: lapsedTarget, Delivery: "steer"}}},
		},
	})
	// The first mention's sender resolved its role to no session and failed the attempt; the
	// second's died with the send in flight, so the delivery sweep owes it a resend and the
	// outbox waits it out no longer (model.MentionResumedSQL).
	seedDelivery(t, rig.database, commentID, settledTarget, "", "failed")
	seedDelivery(t, rig.database, commentID, lapsedTarget, "", "resumed")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, lapsedTarget, "steer"))
	prefixed := contracts.RoleForwardDedupePrefix + fmt.Sprintf("dispatch-%d", comment.ID)
	waitFor(t, 10*time.Second, "the forward the lapsed mention's key covers", func() bool { return len(recv.received()) > 0 })
	forwards := recv.received()
	if len(forwards) != 1 || forwards[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v in the pass that read the comment, want one copy under %q, the key the mention of %s carries wherever the resend pins it: the settled mention of %s is no unpinned attempt of this comment, so that one key stands for the resend still owed. A copy under %q is a key that host never claimed, and the resend a sweep pins here would land beside it",
			holder, forwards, want, lapsedTarget, settledTarget, prefixed)
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Nothing of it waits: the one session its forward reaches takes the copy under the lapsed mention's own key",
			attempts, publishFailure(failure), reached)
	}
}

// A comment whose one mention this read can name no holder for has settled by the first read
// holds nothing back and keys nothing: no send of it is owed, so the forward to the holder of
// the comment's role route goes in that pass, and the attempt names no session, so no settled
// mention of this holder keys the copy either - it carries the event's own key under
// contracts.RoleForwardDedupePrefix, the key of a copy no mention of the comment stands behind
// (roleFallbackKey, contracts.RoleForwardDedupeKey).
//
// It is the floor of the same matrix: with that mention live or past its lease the copy carries
// the mention's own key (TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey,
// TestAForwardCarriesTheKeyOfAnUnnamedMentionPastItsLease), and a key minted for a mention
// nothing is owed of would be a key no send of this comment will ever repeat.
func TestAForwardGoesUnkeyedOnceTheOneUnnamedMentionHasSettled(t *testing.T) {
	const routed = "routed-to-a-session-no-mention-of-the-comment-reaches"
	const mentioned = "mentioned-and-settled-before-the-outbox-read-it"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + mentioned
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	rig.silent(t, contracts.RoleTopicPrefix+mentioned)
	recv := rig.receiver(t, holder)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	// The sender resolved the role to no session and failed the attempt before the outbox read
	// the event: the mention is settled, and it named no session on its way out.
	seedDelivery(t, rig.database, commentID, target, "", "failed")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.RoleForwardDedupePrefix + fmt.Sprintf("dispatch-%d", comment.ID)
	mentionKey := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	waitFor(t, 10*time.Second, "the forward the settled mention holds back no longer", func() bool { return len(recv.received()) > 0 })
	forwards := recv.received()
	if len(forwards) != 1 || forwards[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v in the pass that read the comment, want one copy under %q: the comment's one unnamed mention has settled, so nothing is owed for the forward to wait on and no send of this comment will ever carry %q, which a copy keyed by that mention would claim at this host",
			holder, forwards, want, mentionKey)
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Its one mention has settled, so no copy of it waits",
			attempts, publishFailure(failure), reached)
	}
}
