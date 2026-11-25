package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R6: "A session that the reply names, whose mention failed, still gets the copy that the author
// route sends under the key of that mention, as dispatch://INFRA-1368's R2 has it." The person's
// reply in B's thread (B live) names @role:R. Its first attempt fails with no session - the
// listener named no live holder of R ("no holder for role ...") - so no mention of the reply names
// A, which only the issue's primary document credits, and the reply is published leaving A out.
// The person retries the mention (POST /api/v1/comments/{id}/deliveries, which re-resolves the
// role): the retry is pinned to A, which holds R now, and its send fails. A is a session the
// reply names whose mention failed, and gets no copy: nothing reads the reply again. Without the
// thread rule (the control: B gone) A got the reply as a credit when it was published.
func TestARetriedRoleMentionPinnedToACreditThatFailsStillReachesIt(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		t.Run("B "+b, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			// The first attempt, which the listener failed with no holder named.
			seedDelivery(t, database, reply, target, "", "failed")
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
			if publishedAt(t, database, created.ID) == nil {
				t.Fatalf("the reply was not published, its only mention attempt failed with no session")
			}
			// The person's retry, pinned to A, which holds R now; its send fails.
			seedDelivery(t, database, reply, target, issue.a.ID, "failed")
			settleLater(t, database, deps, o.instance)
			if len(copiesOf(publisher.all(), created, issue.b.ID)) != 1 {
				t.Fatalf("B, the thread's session, got %d copies of the reply, want 1", len(copiesOf(publisher.all(), created, issue.b.ID)))
			}
			if len(copiesOf(publisher.all(), created, issue.a.ID)) == 0 {
				t.Fatalf("A, which the retried mention of the reply named and whose send failed, got no copy of the reply (fallback rows %v): R6 keeps the author route's copy for a session the reply names whose mention failed",
					heldDestinations(t, database, reply))
			}
		})
	}
}
