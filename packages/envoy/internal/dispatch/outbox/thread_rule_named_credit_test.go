package outbox

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// copiesOf is every envelope of event that items sent to sessionID's own topic.
func copiesOf(items []contracts.Envelope, event model.Event, sessionID string) []contracts.Envelope {
	var sent []contracts.Envelope
	for _, item := range items {
		if item.SourceEventID == fmt.Sprint(event.ID) && item.Topic == contracts.LegacyAgentSubject(sessionID) {
			sent = append(sent, item)
		}
	}
	return sent
}

// settleLater runs, three times over, what a running outbox does after a batch: the events and the
// fallback rows due again at once, a batch and the fallback pass.
func settleLater(t *testing.T, database *store.Store, deps Deps, instance uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for range 3 {
		if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = null where published_at is null`); err != nil {
			t.Fatalf("make the events due: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `update comment_fallback_copies set next_attempt_at = null where settled_at is null`); err != nil {
			t.Fatalf("make the fallback rows due: %v", err)
		}
		if _, err := scanBatch(ctx, deps, unfenced{}, instance); err != nil {
			t.Fatalf("scan batch: %v", err)
		}
		publishFallbackCopies(ctx, deps, unfenced{}, instance)
	}
}

// R6: "A session that the reply names, whose mention failed, still gets the copy that the author
// route sends under the key of that mention, as dispatch://INFRA-1368's R2 has it." The person's
// reply in B's thread (B live) names @role:R. The listener's lookup of R went unanswered when the
// comment was written, so the attempt names no session, and the outbox's read of R's claim fails
// in the batch, so the batch cannot tell whose copy the mention decides. The sender then pins the
// attempt to A - A claims R, and the issue's primary document credits A - and the send fails.
// Without the thread rule (the control: B gone) A gets the reply: the attempt live, at once; the
// attempt past its lease, as a fallback row published under A's failed mention's key. With the
// thread rule A is left out as a credit before the mention rule is asked of it, no row is kept for
// it, and A never gets the reply that named it.
func TestAReplysRoleMentionPinnedToACreditAfterTheBatchStillReachesIt(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		for _, outcome := range []string{"pending", "resumed"} {
			t.Run("B "+b+", the attempt "+outcome, func(t *testing.T) {
				ctx := context.Background()
				database := storetest.Open(t)
				broker := events.NewBroker()
				issue := seedThreadRuleIssue(t, database)
				target := "role:legion-reviewer-t1"
				root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
				reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
				seedDelivery(t, database, reply, target, "", outcome)
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
					t.Fatalf("A, which the reply named through @%s and whose mention failed, got no copy of the reply (fallback rows %v): R6 keeps the author route's copy for a session the reply names whose mention failed",
						target, heldDestinations(t, database, reply))
				}
			})
		}
	}
}

// R6: "a person's reply in a thread, and the person's edit of such a reply, reach the thread's
// sessions and no session that only a document credits ... A session that the reply names, whose
// mention failed, still gets the copy that the author route sends under the key of that mention".
// The person's reply in B's thread (B live) names C, which the issue's primary document credits,
// and the person edits it while the mention of C is still being sent. The reply's own copy to C
// waits for that mention (destinations.leftTo); the edit's does not: the thread rule leaves C out
// of the edit at once, as no mention of C has failed yet (creditFilter, mentionFallbackKeys). The
// mention then fails. C gets the reply under its failed mention's key and never the edit, so it
// holds the reply's first words; without the thread rule (the control: B gone) it gets both.
func TestAnEditMadeWhileTheReplysMentionOfACreditIsBeingSentReachesItOnceThatMentionFails(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		t.Run("B "+b, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedMentionDelivery(t, database, reply, issue.c.ID, "pending")
			created := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: reply, ReplyTo: &root}, issue.c.ID)
			edited := appendCommentEvent(t, database, broker, "comment.edited", issue.person, model.Comment{ID: reply, ReplyTo: &root, Body: "Edited."}, issue.c.ID)
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry: newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: b}})}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set state = 'failed', error = 'envoy listener: delivery failed' where comment_id = $1
			`, reply); err != nil {
				t.Fatalf("fail the mention of %s: %v", issue.c.ID, err)
			}
			settleLater(t, database, deps, o.instance)
			if len(copiesOf(publisher.all(), created, issue.c.ID)) == 0 {
				t.Fatalf("C, named by the reply, whose mention failed, got no copy of the reply")
			}
			if len(copiesOf(publisher.all(), edited, issue.c.ID)) == 0 {
				t.Fatalf("C, named by the reply, whose mention failed, got no copy of the person's edit of it (the edit reached %v): it holds the reply's first words",
					agentTopics(readEventRow(t, database, edited.ID).destinations))
			}
		})
	}
}
