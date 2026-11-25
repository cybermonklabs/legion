package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The architect's round-9 ruling on dispatch://INFRA-3686 names the edit's copy and the credit's
// row: nothing else of the issue waits on them, a person's later comment above all. The same wait
// stands on the reply's own copy of a credit: the person's reply in B's thread (B live) names
// @role:R, whose listener lookup went unanswered, and the outbox cannot read R's claim, so the
// attempt names no session and no holder, live and within unnamedForwardBound. A's and C's copies
// of the reply - A and C only the issue's primary document credits - wait in the reply's list for
// that attempt (creditWaits, destinations.waitForCredits), holding A's and C's keys against the
// owner's later events (planChains) and owed (owedKeys). The person then writes a comment on the
// issue, which reaches A and C as the primary document's credits. It reaches neither until the
// attempt is pinned or passes the bound - through the real API and the real outbox the comment's
// copy to s3 came 1m29.9s after the write (zz_cw9_api_e2e_test.go) - and its fallback copy, or
// the copy of a mention of A in it, waits behind the reply (model.MentionCopyHolderSQL).
//
// This test holds the ruling's words for the reply's own wait; whether the ruling covers it is the
// architect's to say.
func TestAPersonsLaterCommentReachesACreditWithoutWaitingOnTheReplysCopy(t *testing.T) {
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
		Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
		RoleClaims: unreadableRoleClaims{}, Sessions: heldRoles{},
	}
	o := holding(t, deps)
	if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	if publishedAt(t, database, created.ID) != nil {
		t.Fatalf("the reply was published while an attempt that names no session, whose holder the outbox cannot name, may yet be pinned to a credit within the bound (%s)", unnamedForwardBound)
	}
	comment := seedThreadComment(t, database, "T-1", "", issue.person, nil)
	later := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: comment})
	if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = null where published_at is null`); err != nil {
		t.Fatalf("make the events due: %v", err)
	}
	if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	for _, credited := range []string{issue.a.ID, issue.c.ID} {
		if len(copiesOf(publisher.all(), later, credited)) == 0 {
			_, _, reached := eventState(t, database, later.ID)
			t.Errorf("%s got no copy of the person's later comment (it reached %v): the reply's copy to %s, which waits for the reply's @role: attempt within the bound, held it",
				credited, reached, credited)
		}
	}
	by, err := copyHolder(ctx, deps, later.ID, contracts.LegacyAgentSubject(issue.a.ID), issue.a.ID)
	if err != nil {
		t.Fatalf("read the copy rule for A: %v", err)
	}
	if by != nil {
		t.Errorf("a mention of A in the person's later comment waits behind %s, want nothing", describeHolder(by))
	}
}
