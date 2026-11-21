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

// dispatch://INFRA-1368's R1, which R8 keeps: "A person's comment on an issue that a session claims
// obliges one session: the holder of the claim." Every batch reads the issue's route and claim
// again (routing), "so a retry after the route or the claim moved reaches the new destination".
// The person's reply in B's thread (B live) names @role:R, whose listener lookup went unanswered
// and whose claim the outbox cannot read; the sender holds the attempt past unnamedForwardBound.
// The outbox's first read keeps A's copy - A only credited by the primary document then - as a
// credit's comment_fallback_copies row (creditHeldAsARow), and the reply's send to the issue's
// topic fails, so the reply is backed off. A then claims the issue (or the issue is routed to A),
// and the retry reads A as the reply's claimant (or route). The sender pins the attempt to another
// session and it lands there. A's own topic is still owned by the credit's row
// (mentionDeliveries.held), which settles with nothing sent, since no mention of the reply failed
// to reach A (decideFallbackCopy), so the issue's claimant never hears the person's reply.
func TestACreditRowLeavesTheReplyToTheSessionTheRetryReads(t *testing.T) {
	for _, change := range []string{"A claims the issue", "the issue is routed to A"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "pending")
			agedUnnamedAttempts(t, database, reply, unnamedForwardBound+30*time.Second)
			created := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root,
					Mentions: []model.Mention{{Target: target, Delivery: "steer"}},
				}},
			})
			publisher := &recordingPublisher{failTopic: "notifications.dispatch.issue.T-1.comment.created"}
			deps := Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
				RoleClaims: unreadableRoleClaims{}, Sessions: heldRoles{},
			}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			if publishedAt(t, database, created.ID) != nil {
				t.Fatalf("the reply was published on its first read, want it backed off by its failed send to the issue's topic")
			}
			switch change {
			case "A claims the issue":
				claimIssue(t, database, "T-1", issue.a)
			case "the issue is routed to A":
				if _, err := database.Pool.Exec(ctx, `update issues set route = $2 where key = $1`, "T-1", "session:"+issue.a.ID); err != nil {
					t.Fatalf("route the issue to %s: %v", issue.a.ID, err)
				}
			}
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set session_id = 'session-h', state = 'sent' where comment_id = $1 and target = $2
			`, reply, target); err != nil {
				t.Fatalf("land the mention at session-h: %v", err)
			}
			publisher.mu.Lock()
			publisher.failTopic = ""
			publisher.mu.Unlock()
			settleLater(t, database, deps, o.instance)
			if publishedAt(t, database, created.ID) == nil {
				t.Fatalf("the reply is still unpublished after its retry")
			}
			if got := copiesOf(publisher.all(), created, issue.a.ID); len(got) == 0 {
				t.Fatalf("A, which %s before the reply's retry, got no copy of the person's reply (fallback rows %v): the credit's row kept for A when A was only a credit owns A's topic and settled with nothing sent",
					change, heldDestinations(t, database, reply))
			}
		})
	}
}
