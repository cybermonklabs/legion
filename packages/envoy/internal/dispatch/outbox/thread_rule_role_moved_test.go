package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
)

// claimedBy is a role-claim store and session reader whose every role is claimed by holder, a
// live self-subscribed session: heldRoles with a holder the test names.
type claimedBy struct {
	roles.Windows
	holder string
}

func (c claimedBy) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: c.holder}, nil
}

func (claimedBy) Holder(context.Context, string) (session.SessionEntry, error) {
	return session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().UnixMilli()}, nil
}

func (claimedBy) LastSeen(string) int64 { return 0 }

// R6: "a person's reply in a thread ... reach[es] the thread's sessions and no session that only a
// document credits (the issue's primary document, or the document the thread is anchored on), by
// any rule of Dispatch's outbox." The person's reply in B's thread (B live) names @role:R, whose
// listener lookup went unanswered, so its attempt names no session. The outbox reads R's claim as
// it publishes the reply and finds A, which only the primary document credits, so the attempt's
// state decides A's copy (resolveUnpinnedRoleMentions, mentionDeliveries.decidesCopyFor): the
// attempt is past the lease the outbox waits a send out for, so A's copy is kept as a
// comment_fallback_copies row of A's own key (destinations.leftTo). The sender resolves R through
// the listener a round trip or a held lane later: R's claim moved to C, and the sender pins the
// attempt there (sent or failed), or the listener names no live holder and the attempt fails with
// no session. No mention of the reply names A any more, yet the fallback pass publishes A's row,
// reading only that A's own attempts did not reach it (decideFallbackCopy), and A gets the reply.
// Without the thread rule (B gone) A gets it as a credit, which is the control.
func TestAReplysRoleMentionTheOutboxResolvedToACreditThatTheSenderPinsElsewhereLeavesTheCreditOut(t *testing.T) {
	for _, after := range []string{"pinned to C, sent", "pinned to C, failed", "failed with no session"} {
		t.Run(after, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "resumed")
			created := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root,
					Mentions: []model.Mention{{Target: target, Delivery: "steer"}},
				}},
			})
			publisher := &recordingPublisher{}
			claims := claimedBy{holder: issue.a.ID}
			deps := Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
				RoleClaims: claims, Sessions: claims,
			}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			session, state := issue.c.ID, "sent"
			switch after {
			case "pinned to C, failed":
				state = "failed"
			case "failed with no session":
				session, state = "", "failed"
			}
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries
				set session_id = nullif($3, ''), state = $4, error = case when $4 = 'failed' then 'envoy listener: delivery failed' end
				where comment_id = $1 and target = $2
			`, reply, target, session, state); err != nil {
				t.Fatalf("settle the mention: %v", err)
			}
			settleLater(t, database, deps, o.instance)
			if len(copiesOf(publisher.all(), created, issue.b.ID)) != 1 {
				t.Fatalf("B, the thread's session, got %d copies of the reply, want 1", len(copiesOf(publisher.all(), created, issue.b.ID)))
			}
			if got := copiesOf(publisher.all(), created, issue.a.ID); len(got) != 0 {
				t.Fatalf("A, which only the primary document credits and which no mention of the reply names (the attempt was %s), got the reply %d times, keyed %v (fallback rows %v): the outbox resolved R to A, kept A's copy as a row, and published the row once the sender pinned the attempt elsewhere",
					after, len(got), keysOf(got), heldDestinations(t, database, reply))
			}
		})
	}
}
