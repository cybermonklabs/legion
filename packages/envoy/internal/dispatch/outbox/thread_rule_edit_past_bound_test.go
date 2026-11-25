package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The architect ruled in round 8 of dispatch://INFRA-3686 that the edit follows the reply: a
// session the reply names gets the person's edit of it, past unnamedForwardBound as before it,
// R6 keeping no copy from a session the reply names. So a credit whose own mention of the reply
// is still owed past the bound keeps the edit, and holds it beside that mention should the
// mention land; that is no failure, and the test that read it as one is gone. The test below is
// the other side of that ruling: a session that got the reply by the failed-mention rule gets
// the edit too.
//
// R6: "A session that the reply names, whose mention failed, still gets the copy that the author
// route sends under the key of that mention", read for the person's edit as the round-5 test
// TestAnEditMadeWhileTheReplysMentionOfACreditIsBeingSentReachesItOnceThatMentionFails reads it:
// the edit follows the reply to a named credit whose mention failed. The person's reply in B's
// thread (B live) names @role:R, whose listener lookup went unanswered and whose claim the outbox
// cannot read; the sender holds the attempt past unnamedForwardBound, so A's copy of the reply is
// a credit's row (creditHeldAsARow). The person edits the reply (as the API writes it), and the
// edit, read past the bound, leaves A out. The sender then pins the attempt to A, and it fails: A
// gets the reply from its row, and never the edit.
func TestAnEditOfAReplyWhoseLateRoleMentionOfACreditFailsReachesIt(t *testing.T) {
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
	edited := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.edited", Actor: issue.person,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root, Body: "Edited.",
			Mentions: []model.Mention{},
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
	if _, err := database.Pool.Exec(ctx, `
		update comment_deliveries set session_id = $3, state = 'failed', error = 'envoy listener: delivery failed'
		where comment_id = $1 and target = $2
	`, reply, target, issue.a.ID); err != nil {
		t.Fatalf("fail the mention pinned to %s: %v", issue.a.ID, err)
	}
	settleLater(t, database, deps, o.instance)
	if len(copiesOf(publisher.all(), created, issue.a.ID)) != 1 {
		t.Fatalf("A, named by the reply, whose mention failed, got %d copies of the reply, want 1", len(copiesOf(publisher.all(), created, issue.a.ID)))
	}
	if len(copiesOf(publisher.all(), edited, issue.a.ID)) == 0 {
		t.Fatalf("A, named by the reply, whose mention failed, got the reply and not the person's edit of it (the edit reached %v): it holds the reply's first words",
			agentTopics(readEventRow(t, database, edited.ID).destinations))
	}
}

// The other side of TestAnEditOfAReplyWhoseLateRoleMentionOfACreditFailsReachesIt: the settle of
// a credit's row sends the session the reply's edits only where it sends that session the reply
// (decideFallbackCopy, creditRowEdits). The person's reply in B's thread (B live) names @role:R,
// whose listener lookup went unanswered and whose claim the outbox cannot read; the sender holds
// the attempt past unnamedForwardBound, so A's copy of the reply is a credit's row
// (creditHeldAsARow), and the person's edit, read past the bound, leaves A out. The sender then
// pins the attempt to A and the send lands, or pins it to C and the send lands: no mention of the
// reply failed to reach A, so A's row settles with nothing sent, and A gets neither the outbox's
// copy of the reply nor the edit.
func TestACreditRowThatSettlesWithNothingSentSendsNoEditOfTheReply(t *testing.T) {
	for _, pinned := range []string{"A", "C"} {
		t.Run("the mention pinned to "+pinned+" and sent", func(t *testing.T) {
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
			edited := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.edited", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root, Body: "Edited.",
					Mentions: []model.Mention{},
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
			session := issue.a.ID
			if pinned == "C" {
				session = issue.c.ID
			}
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set session_id = $3, state = 'sent'
				where comment_id = $1 and target = $2
			`, reply, target, session); err != nil {
				t.Fatalf("land the mention pinned to %s: %v", session, err)
			}
			settleLater(t, database, deps, o.instance)
			if row := readHeldCopy(t, database, reply, contracts.LegacyAgentSubject(issue.a.ID)); !row.settled || row.published {
				t.Errorf("A's credit row is settled %v and published %v, want it settled with nothing sent: no mention of the reply failed to reach A",
					row.settled, row.published)
			}
			if got := copiesOf(publisher.all(), created, issue.a.ID); len(got) != 0 {
				t.Errorf("A, which no failed mention of the reply names, got %d copies of the reply from the outbox, want none", len(got))
			}
			if got := copiesOf(publisher.all(), edited, issue.a.ID); len(got) != 0 {
				t.Errorf("A, whose credit row settled with nothing sent, got %d copies of the person's edit, want none: the row sends the edit only where it sends the reply",
					len(got))
			}
		})
	}
}
