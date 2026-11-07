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
)

// A comment mentions two roles this read can name no holder for: one attempt is past the lease
// the outbox waits a send out for, and one is live. The copy bound for the session that claims
// the issue is held as a comment_fallback_copies row, the lapsed attempt's state being every
// key's (unpinnedRoleMentions.resendingUnnamed), and the pass that reads that row must hold it
// while the live attempt is still being sent: nothing says which session that attempt is on its
// way to, and its sender can pin it to this very claimant
// (decideFallbackCopy, mentionHolds.liveUnnamed).
//
// The row's own key says nothing of that once the lapsed attempt settles: the claimant has no
// attempt of its own, and no unnamed attempt is past its lease any longer, so the key reads
// mentionFailed. A copy published on that reading goes under the event's own key, which no host
// claimed for the mention, and the mention the sender pins lands beside it - the double this
// row's wait is for, and one the batch's own hold already spares the claimant while the attempt
// is live (destinations.addWorkingSession).
//
// The wait is that attempt's own lease and no longer: past it the attempt reads
// mentionResending, which this pass waits on through the row's state, and then it settles. Here
// it fails, nothing is pinned, and the copy is the outbox's: the pass that reads the row next
// publishes it under the event's own key.
func TestAHeldCopyWaitsForALiveUnnamedMentionBesideALapsedOne(t *testing.T) {
	const lapsedRole = "mentioned-first-whose-attempt-is-past-its-lease"
	const liveRole = "mentioned-second-whose-attempt-is-live"
	const holder = "ses_claims_the_issue"
	ctx := store.WithTransactionTracking(context.Background())
	lapsed, live := "role:"+lapsedRole, "role:"+liveRole

	rig := newRoleRig(t)
	rig.silent(t, contracts.RoleTopicPrefix+lapsedRole)
	rig.silent(t, contracts.RoleTopicPrefix+liveRole)
	claimant := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + lapsed + " @" + live + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: lapsed, Delivery: "steer"}, {Target: live, Delivery: "steer"}}},
		},
	})
	// The first attempt's sender died with the send in flight, so the delivery sweep owes it a
	// resend; the second is a send a sender claimed just now.
	seedDelivery(t, rig.database, commentID, lapsed, "", "resumed")
	seedDelivery(t, rig.database, commentID, live, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})
	if row := readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder)); row.settled || row.published {
		t.Fatalf("the batch left the claimant's held copy settled=%v published=%v, want the unpublished row the lapsed attempt's state owes every key of this comment",
			row.settled, row.published)
	}
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %v by the batch, want nothing: its copy is the row's", holder, keysOf(got))
	}

	// The lapsed attempt's resend fails; the live one is still being sent, and its sender can
	// pin it to this very claimant.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1 and target = $2
	`, commentID, lapsed); err != nil {
		t.Fatalf("fail the resend of the mention of %s: %v", lapsed, err)
	}

	publishFallbackCopies(ctx, rig.deps(), unfenced{})
	time.Sleep(time.Second)
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("the fallback pass sent %s %v while the live unnamed mention, which its sender can pin to this very claimant, is still being sent: the mention would land beside the copy under %s",
			holder, keysOf(got),
			contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, live, "steer")))
	}
	row := readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder))
	if row.settled || row.published || row.attempts != 0 {
		t.Fatalf("the fallback pass left the claimant's held copy settled=%v published=%v attempts=%d, want it unsettled, unpublished and no attempt counted: a wait is no failure",
			row.settled, row.published, row.attempts)
	}

	// The live attempt's sender finds no session and fails it: nothing is pinned, so the copy is
	// the outbox's and the pass that reads the row next publishes it.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1 and target = $2
	`, commentID, live); err != nil {
		t.Fatalf("fail the mention of %s: %v", live, err)
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
		where comment_id = $1 and settled_at is null
	`, commentID); err != nil {
		t.Fatalf("pass the held copy's wait: %v", err)
	}

	publishFallbackCopies(ctx, rig.deps(), unfenced{})
	waitFor(t, 10*time.Second, "the copy both mentions' failure owes", func() bool { return len(claimant.received()) > 0 })
	want := []string{fmt.Sprintf("dispatch-%d", comment.ID)}
	if got := keysOf(claimant.received()); !slices.Equal(got, want) {
		t.Fatalf("%s was sent %v once both unnamed mentions had failed, want %v: no mention of the comment is coming, so the copy is the outbox's and carries the event's own key",
			holder, got, want)
	}
	if row := readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder)); !row.settled || !row.published {
		t.Fatalf("the held copy is settled=%v published=%v after the copy went out, want it settled and marked published",
			row.settled, row.published)
	}
}
