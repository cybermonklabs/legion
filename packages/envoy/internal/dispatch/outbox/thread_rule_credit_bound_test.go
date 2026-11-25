package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R6: "A session that the reply names, whose mention failed, still gets the copy that the author
// route sends under the key of that mention, as dispatch://INFRA-1368's R2 has it." The person's
// reply in B's thread (B live) names @role:R. The listener's lookup of R went unanswered when the
// comment was written, so the attempt names no session, and the outbox's read of R's claim fails,
// so the batch cannot tell whose copy the mention decides: A and C, which only the primary
// document credits, wait (creditWaits). The sender is alive and its lane is held - it writes the
// claim on the attempt again and again (agedUnnamedAttempts) - until the attempt is older than
// unnamedForwardBound, two leases; the outbox then leaves the credits out and settles the reply
// (creditCopyOf). The sender's lane reaches the send at last: it pins the attempt to A, which holds
// R, and the send fails. A, which the reply named and whose mention failed, never gets the reply.
// Without the thread rule (the control: B gone) A gets it.
func TestAReplysRoleMentionPinnedToACreditPastTheForwardsBoundStillReachesIt(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		t.Run("B "+b, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "pending")
			created := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root,
					Mentions: []model.Mention{{Target: target, Delivery: "steer"}},
				}},
			})
			publisher := &recordingPublisher{}
			deps := Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: b}}),
				RoleClaims: unreadableRoleClaims{}, Sessions: heldRoles{},
			}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			// The forward's bound passes with the sender alive and its lane held.
			agedUnnamedAttempts(t, database, reply, unnamedForwardBound+30*time.Second)
			dueNow(t, database, created.ID)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			// The sender pins the attempt to A, which holds R, and the send fails.
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set session_id = $3, state = 'failed', error = 'envoy listener: delivery failed'
				where comment_id = $1 and target = $2
			`, reply, target, issue.a.ID); err != nil {
				t.Fatalf("fail the mention pinned to %s: %v", issue.a.ID, err)
			}
			settleLater(t, database, deps, o.instance)
			if len(copiesOf(publisher.all(), created, issue.b.ID)) == 0 {
				t.Fatalf("B, the thread's session, got no copy of the reply")
			}
			if len(copiesOf(publisher.all(), created, issue.a.ID)) == 0 {
				t.Fatalf("A, which the reply named through @%s and whose mention failed, got no copy of the reply (fallback rows %v): the credits were left out once the attempt was older than unnamedForwardBound (%s), before its sender pinned it to A, and R6 keeps the author route's copy for a session the reply names whose mention failed",
					target, heldDestinations(t, database, reply), unnamedForwardBound)
			}
		})
	}
}

// R6, as above: the person's reply in B's thread (B live) names @role:R, whose listener lookup went
// unanswered, so its attempt names no session. The outbox reads R's claim as it publishes the
// reply and finds it held by another session (heldRoles: roleHolder), so it decides that the
// mention names no credit and leaves A and C out at once (creditCopyOf: the role resolved, nothing
// waits). R's claim moves to A before the sender resolves the role through the listener, a round
// trip or a held lane later (resolveUnpinnedRoleMentions: "a role it does name a holder for is not
// a role that holder keeps"); the sender pins the attempt to A and the send fails. A, which the
// reply named and whose mention failed, never gets the reply. Without the thread rule (the
// control: B gone) A gets it.
func TestAReplysRoleMentionTheOutboxResolvedElsewherePinnedToACreditStillReachesIt(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		t.Run("B "+b, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "pending")
			created := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root,
					Mentions: []model.Mention{{Target: target, Delivery: "steer"}},
				}},
			})
			publisher := &recordingPublisher{}
			deps := withHeldRoles(Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry: newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: b}}),
			})
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			// R's claim moved to A; the sender pins the attempt to A, and the send fails.
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set session_id = $3, state = 'failed', error = 'envoy listener: delivery failed'
				where comment_id = $1 and target = $2
			`, reply, target, issue.a.ID); err != nil {
				t.Fatalf("fail the mention pinned to %s: %v", issue.a.ID, err)
			}
			settleLater(t, database, deps, o.instance)
			if len(copiesOf(publisher.all(), created, issue.b.ID)) == 0 {
				t.Fatalf("B, the thread's session, got no copy of the reply")
			}
			if len(copiesOf(publisher.all(), created, issue.a.ID)) == 0 {
				t.Fatalf("A, which the reply named through @%s and whose mention failed, got no copy of the reply (fallback rows %v): the outbox resolved R to %s and left the credits out before the sender pinned the attempt to A, and R6 keeps the author route's copy for a session the reply names whose mention failed",
					target, heldDestinations(t, database, reply), roleHolder)
			}
		})
	}
}
