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

// The session that wrote the thread a comment replies to is sent nothing in the pass that reads
// the event while an attempt of that comment which names no session yet is past the lease the
// outbox waits a send out for: its copy is held as a comment_fallback_copies row, the lapsed
// attempt's state being every key's, and the pass that reads that row waits on while the send is
// still owed (destinations.leftToMention, mentionHolds.state,
// unpinnedRoleMentions.resendingUnnamed, decideFallbackCopy).
//
// It is the other side of TestAnUnnamedRoleMentionPinnedToTheThreadAuthorShowsItTheCommentTwice,
// the same staging while that attempt is live: there the author's copy goes at once under the
// event's own key and the double a sender that pins the mention to this very author then costs
// it is the trade this head accepts, bounded by model.DeliveryClaimLapse. Past that lease the
// bound is gone - the sweep's resend lands whenever its lane reaches it, and it can pin this
// author - so no copy of the comment goes out under the event's own key while that send is owed,
// the thread author's no more than the issue claimant's
// (TestAFallbackRowWaitsWhileItsUnnamedMentionIsOwedAResend). The hold costs the comment's own response nothing:
// the event is published in this pass and every other destination reached, the author's copy
// alone being the row's.
//
// A follower and a primary document's author are the same session to this rule, each reaching
// addSession by a road of its own (destinations.leftToMention,
// TestAnUnnamedRoleMentionPinnedToAFollowerShowsItTheCommentTwice).
func TestAThreadAuthorsCopyWaitsForAnUnnamedMentionPastItsLease(t *testing.T) {
	const role = "mentioned-and-held-by-nobody-this-read-can-name"
	const author = "ses_wrote_the_thread_root"
	const issueTopic = "notifications.dispatch.issue.T-1.comment.created"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	// No claim stands for the role when the outbox reads it, so this read can name no holder for
	// the role the comment mentions; the listener names one to the mention's sender, and the
	// sweep's resend pins it on the attempt row - this very author among the sessions it may
	// name.
	rig.silent(t, contracts.RoleTopicPrefix+role)
	wrote := rig.receiver(t, author, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "Who is on this?", nil)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	// The sender died with the send in flight, so the delivery sweep owes this attempt a resend
	// and the outbox waits it out no longer (model.MentionResumedSQL).
	seedDelivery(t, rig.database, commentID, target, "", "resumed")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	eventKey := fmt.Sprintf("dispatch-%d", comment.ID)
	mentionKey := contracts.AgentSendDedupeKey(author, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	authorSubject := contracts.AgentSubject(author)
	attempts, failure, reached := eventState(t, rig.database, comment.ID)
	if publishedAt(t, rig.database, comment.ID) == nil {
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Nothing of it waits for the owed resend: the author's copy is the row's, and the event publishes at the lease as it always has",
			attempts, publishFailure(failure), reached)
	}
	if !slices.Contains(reached, issueTopic) {
		t.Fatalf("the comment reached %v, want %s among them: holding the author's copy as a row costs the comment's own response no destination",
			reached, issueTopic)
	}
	if got := wrote.received(); slices.Contains(reached, authorSubject) || len(got) != 0 {
		t.Fatalf("the comment reached %v and %s was sent %v in the pass that read the event, want %s not among them and nothing sent: a resend of the mention of %s is still owed and can pin this very author, so a copy under the event's own key %q would stand beside that mention's own send under %q",
			reached, author, keysOf(got), authorSubject, target, eventKey, mentionKey)
	}
	if row := readHeldCopy(t, rig.database, commentID, authorSubject); row.settled || row.published {
		t.Fatalf("the batch left the author's held copy settled=%v published=%v, want the unsettled, unpublished row the lapsed attempt's state owes every key of this comment",
			row.settled, row.published)
	}

	// The pass that reads that row waits on it: the resend is still owed, and whichever session
	// it pins is a session this read cannot name (mentionHolds.state).
	publishFallbackCopies(ctx, rig.deps(), unfenced{}, holdingProcess(t, rig.database))
	time.Sleep(time.Second)
	if got := wrote.received(); len(got) != 0 {
		t.Fatalf("the fallback pass sent %s %v while the resend of the mention of %s is still owed: the resend can pin this very author, and that send lands under %q, which this copy does not claim",
			author, keysOf(got), target, mentionKey)
	}
	row := readHeldCopy(t, rig.database, commentID, authorSubject)
	if row.settled || row.published || row.attempts != 0 {
		t.Fatalf("the fallback pass left the author's held copy settled=%v published=%v attempts=%d, want it unsettled, unpublished and no attempt counted: a wait is no failure",
			row.settled, row.published, row.attempts)
	}

	// The resend is made, resolves to no session and fails, as a sweep's pass makes it: nothing
	// is pinned, so the copy is the outbox's and the pass that reads the row next publishes it.
	// The wait is a wait and not a drop.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1 and target = $2
	`, commentID, target); err != nil {
		t.Fatalf("fail the resend of the mention of %s: %v", target, err)
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
		where comment_id = $1 and settled_at is null
	`, commentID); err != nil {
		t.Fatalf("pass the held copy's wait: %v", err)
	}

	publishFallbackCopies(ctx, rig.deps(), unfenced{}, holdingProcess(t, rig.database))
	waitFor(t, 10*time.Second, "the copy the mention's failure owes the thread author", func() bool { return len(wrote.received()) > 0 })
	want := []string{eventKey}
	if got := keysOf(wrote.received()); !slices.Equal(got, want) {
		t.Fatalf("%s was sent %v once the resend had failed, want %v: no mention of the comment is coming, so the copy is the outbox's and carries the event's own key",
			author, got, want)
	}
	if row := readHeldCopy(t, rig.database, commentID, authorSubject); !row.settled || !row.published {
		t.Fatalf("the held copy is settled=%v published=%v after the copy went out, want it settled and marked published",
			row.settled, row.published)
	}
}
