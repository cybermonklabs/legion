package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R6: "a person's reply in a thread, and the person's edit of such a reply, reach the thread's
// sessions and no session that only a document credits ... A session that the reply names, whose
// mention failed, still gets the copy that the author route sends under the key of that mention".
// The person's reply in B's thread (B live) names C, which the issue's primary document credits,
// and C's mention fails, before the outbox reads the reply or while it waits on it. The person then
// edits the reply. The edit's event is the one the API writes (editComment in
// internal/dispatch/api/comment_actions.go): the comment it reads back (loadComment) carries none
// of the comment's mentions, so the payload's mentions are [], as the API's own comment.edited
// events of a mentioning comment carry them. C gets the reply under its failed mention's key, and
// the edit too, as the edit's credits filter has it (mentionDeliveries.editCreditCopyOf); without
// the thread rule (the control: B gone) it gets both.
func TestAPersonsEditAsTheAPIWritesItReachesACreditTheReplyNamedWhoseMentionFailed(t *testing.T) {
	for _, b := range []string{envoy.StateLive, envoy.StateGone} {
		for _, mention := range []string{"failed", "pending"} {
			t.Run("B "+b+", the mention "+mention+" when the outbox reads the reply", func(t *testing.T) {
				ctx := context.Background()
				database := storetest.Open(t)
				broker := events.NewBroker()
				issue := seedThreadRuleIssue(t, database)
				root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
				reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
				seedMentionDelivery(t, database, reply, issue.c.ID, mention)
				created := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: reply, ReplyTo: &root}, issue.c.ID)
				edited := appendEvent(t, database, broker, model.Event{
					IssueKey: new("T-1"), Type: "comment.edited", Actor: issue.person,
					Payload: model.CommentEventPayload{Comment: model.Comment{
						ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root, Body: "Edited.",
						Mentions: []model.Mention{},
					}},
				})
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
					t.Fatalf("C, named by the reply, whose mention failed, got no copy of the person's edit of it as the API writes the edit (the edit reached %v): it holds the reply's first words",
						agentTopics(readEventRow(t, database, edited.ID).destinations))
				}
			})
		}
	}
}
