package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// A role route's forward whose holder a mention of the comment delivered to carries that
// mention's own key (roleFallbackKey), and the holder's host claimed that key when the send
// landed, so the copy is dropped there. It therefore waits for no attempt of the comment that
// names no session yet, however many of them are owed a send and however little one key stands
// for them: the wait exists to keep a copy from reaching that host under a key no mention
// claimed, beside a mention a sender may yet pin to this very holder, and a copy the host drops
// is no such double (mentionHolds.keyedByADeliveredMention, roleCopyWaitsForAMention).
//
// That wait is what this round changed. waitsForAnUnnamedMention answers true here - two
// unpinned mentions target two roles this read can name no holder for, so no one key stands for
// either, and a resend of each is owed within unnamedForwardBound - and the forward took the
// wait whatever key it was about to carry
// (TestAForwardWaitsForTwoUnnamedMentionsPastTheirLease, which is this comment without the
// holder's own mention and still waits). It bought nothing and it cost the holder this comment
// and every later event of the issue behind it, for up to that bound: a forward that waits
// holds its key against the owner's later events (destination.waiting, planChains), which is
// what the later event here stands for.
//
// The boundary is the delivered mention and no wider. A mention of this holder that only failed
// keys the forward too, and that key may be one its host never claimed - a send that failed may
// never have landed - so a forward carrying it waits exactly as before, which is why the
// settled key prefers a delivered mention at all (mentionDeliveries.settledKeys,
// settledMention.beats). The holder's own live mention is a wait of its own and is untouched:
// a live mention is in no settled map, so no key of it can be carried
// (TestAForwardToALiveHolderWaitsForThatHoldersOwnMention).
//
// The holder claims the role after the comment was written, which is what puts a role forward
// and the holder's own mention on one comment: the comment's payload suppresses no route, since
// the route was held by nobody when it was written, so the role route is published and resolves
// to a session this comment mentions (resolveRoleHolders, destinations.leftToMention leaving
// that session's own copy to the mention that delivered it).
func TestAForwardKeyedByADeliveredMentionSkipsTheWaitForAnUnnamedMention(t *testing.T) {
	const routed = "routed-to-a-session-either-unnamed-mention-may-name"
	const first = "mentioned-first-whose-attempt-is-past-its-lease"
	const second = "mentioned-second-whose-attempt-is-past-its-lease"
	const holder = "ses_holds_the_routed_role_and_its_mention_landed"
	ctx := store.WithTransactionTracking(context.Background())
	firstTarget, secondTarget := "role:"+first, "role:"+second
	holderTarget := "session:" + holder
	route := "role:" + routed
	roleTopic := contracts.RoleTopicPrefix + routed

	rig := newRoleRig(t)
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + holder + " @" + firstTarget + " @" + secondTarget + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: append(
				mentionOf(holder),
				model.Mention{Target: firstTarget, Delivery: "steer"},
				model.Mention{Target: secondTarget, Delivery: "steer"})},
			// No suppression: nobody held the route when the comment was written.
		},
	})
	// The mention of the holder itself reached it: its host claimed that send's key.
	seedMentionDelivery(t, rig.database, commentID, holder, "sent")
	// Both role mentions' senders died with their sends in flight, so a resend of each is owed
	// and the outbox waits neither out any longer (model.MentionResumedSQL). Neither role is
	// claimed, so this read can name no holder for either and no one key stands for them
	// (unpinnedRoleMentions.keyFor).
	seedDelivery(t, rig.database, commentID, firstTarget, "", "resumed")
	seedDelivery(t, rig.database, commentID, secondTarget, "", "resumed")

	// The role is claimed after the comment was written, which is how its holder comes to have
	// a mention of that comment delivered already.
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})

	// The owner's next event of the same issue, which a waiting forward holds behind the
	// holder's key (destination.key, planChains).
	laterBody := "and one more thing"
	laterID := seedComment(t, rig.database, "T-1", alice, laterBody, nil)
	later := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: laterID, IssueKey: new("T-1"), Body: laterBody},
		},
	})

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, holderTarget, "steer"))
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, due := nextAttempt(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, next_attempt_at %v. Its forward carries %q, the key of the holder's own mention of this comment, which that host claimed when the send landed - the copy is dropped there, so waiting for an unnamed mention no key stands for prevents no double and holds the holder's later events for nothing",
			attempts, due, want)
	}
	if publishedAt(t, rig.database, later.ID) == nil {
		t.Fatalf("the owner's later event (%d) was not published in that pass, want it published: nothing of the comment waits at this holder, so nothing holds its key against the issue's next event",
			later.ID)
	}
	if rows := heldDestinations(t, rig.database, commentID); len(rows) != 0 {
		t.Fatalf("the comment holds fallback rows %v, want none: the forward goes in the pass that reads the event under the delivered mention's own key, so no row takes it over",
			rows)
	}

	waitFor(t, 10*time.Second, "the comment's forward and the owner's later event at the holder",
		func() bool { return len(recv.received()) > 1 })
	time.Sleep(time.Second)
	copies := recv.received()
	if len(copies) != 2 {
		t.Fatalf("%s was sent %+v, want two copies: the comment's forward under its own mention's key and the owner's later event behind it. The copy bound for the holder's own topic is left to the mention that delivered the comment (destinations.leftToMention)",
			holder, copies)
	}
	if copies[0].SourceEventID != fmt.Sprint(comment.ID) || copies[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v, want the comment (%d) first under %q: the forward carries the key of the holder's own delivered mention, which its host drops",
			holder, copies, comment.ID, want)
	}
	if copies[1].SourceEventID != fmt.Sprint(later.ID) {
		t.Fatalf("%s was sent %+v, want the owner's later event (%d) second: the comment's forward held nothing back",
			holder, copies, later.ID)
	}
}

// The same comment with the holder's own mention failed rather than delivered: the forward
// waits, exactly as it does for a holder no mention of the comment reached at all
// (TestAForwardWaitsForTwoUnnamedMentionsPastTheirLease). A failed mention's key is no key the
// holder's host is known to have claimed - a send that failed may never have landed, and one
// that never landed claims nothing - so a copy carrying it can be shown beside the mention a
// resend pins to this very holder, which is the double the wait exists to stop
// (mentionDeliveries.settledKeys, settledMention.beats,
// mentionHolds.keyedByADeliveredMention).
//
// It is the boundary of the skip above and the guard on it: the skip reads the settled
// mentions that delivered and no others, and a reading that counted a failed mention among
// them would send this copy under a key that may claim nothing. Nothing else in the package
// pins that line, the comment's other mentions naming no session at all
// (TestAForwardCarriesTheKeyOfALiveUnnamedMentionBesideASettledOne).
func TestAForwardKeyedByAFailedMentionStillWaitsForAnUnnamedMention(t *testing.T) {
	const routed = "routed-to-a-session-either-unnamed-mention-may-name"
	const first = "mentioned-first-whose-attempt-is-past-its-lease"
	const second = "mentioned-second-whose-attempt-is-past-its-lease"
	const holder = "ses_holds_the_routed_role_and_its_mention_failed"
	ctx := store.WithTransactionTracking(context.Background())
	firstTarget, secondTarget := "role:"+first, "role:"+second
	holderTarget := "session:" + holder
	route := "role:" + routed
	roleTopic := contracts.RoleTopicPrefix + routed

	rig := newRoleRig(t)
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + holder + " @" + firstTarget + " @" + secondTarget + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: append(
				mentionOf(holder),
				model.Mention{Target: firstTarget, Delivery: "steer"},
				model.Mention{Target: secondTarget, Delivery: "steer"})},
		},
	})
	// The mention of the holder itself failed: its send may never have landed, so nothing says
	// that host claimed the key it went under.
	seedMentionDelivery(t, rig.database, commentID, holder, "failed")
	// Both role mentions are owed a resend and no one key stands for them, as above.
	seedDelivery(t, rig.database, commentID, firstTarget, "", "resumed")
	seedDelivery(t, rig.database, commentID, secondTarget, "", "resumed")

	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})

	laterBody := "and one more thing"
	laterID := seedComment(t, rig.database, "T-1", alice, laterBody, nil)
	later := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: laterID, IssueKey: new("T-1"), Body: laterBody},
		},
	})

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)

	failedKey := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, holderTarget, "steer"))
	if got := recv.received(); len(got) != 0 {
		t.Fatalf("%s was sent %v while a resend of two unnamed mentions no key stands for is owed, want nothing: the only key this forward has is %q, the key of this holder's own mention of the comment, whose send failed and may never have landed - that host may never have claimed it, so the copy stands beside the mention a resend pins here rather than being dropped. Only a mention that delivered claims its key at the host (mentionHolds.keyedByADeliveredMention, settledMention.beats)",
			holder, keysOf(got), failedKey)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while the forward's only key is a failed mention's, want the comment waiting on its unnamed mentions", at)
	}
	if at := publishedAt(t, rig.database, later.ID); at != nil {
		t.Fatalf("the later event has published_at = %v while the comment's forward holds the holder's key, want it held back: a forward that waits holds the owner's later events behind it",
			at)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on the comment's unpinned mentions, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}
	if rows := heldDestinations(t, rig.database, commentID); slices.Contains(rows, roleTopic) {
		t.Fatalf("the comment holds fallback rows %v, want %s among them nowhere: the forward waits in the event's list while the bound holds, a row taking it over only past it (holdsTheForwardAsARow)",
			rows, roleTopic)
	}
}
