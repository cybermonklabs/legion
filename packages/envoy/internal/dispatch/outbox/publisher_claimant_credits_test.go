package outbox

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAPersonsCommentOnASessionClaimedIssueObligesTheClaimantAlone: a person's comment on an
// issue a session claims obliges that claimant, and the credits of the document the comment
// hangs on are no second reason to deliver what the claimant already hears on its own topic
// (addAuthorRoutes). So the session that wrote the document gets no copy of the person's comment
// on an issue it does not hold - at its head, in a reply under it, or in that comment's edit.
//
// A session that wrote in the comment's own thread is no credit of that kind: the thread names
// it, so it keeps the copy whatever the issue's claim is, and keeps it where the document it
// wrote credits it too - the read marks such a session a member of the thread and the member
// wins (commentRecipientsQuery, threadRecipient.fromThread).
func TestAPersonsCommentOnASessionClaimedIssueObligesTheClaimantAlone(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	second := model.Actor{Kind: "session", ID: "session-second"}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck
	}{
		{
			name: "a person's comment at the head of a thread on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			name: "a person's reply in a thread on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &head)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: reply, ReplyTo: &head})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			// The case the marking can get wrong: the same session is a member of the thread and
			// a credit of its document, and the member wins.
			name: "a person's reply reaches the session that wrote in the thread, its document's credit notwithstanding",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				plan := seedDocument(t, database, new("T-1"), "plan", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", plan, second, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &head)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: reply, ReplyTo: &head})
				return []recipientCheck{{event: created, want: []string{claimant.ID, second.ID}}}
			},
		},
		{
			name: "a person's edit of their comment on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				edited := appendCommentEvent(t, database, broker, "comment.edited", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}})
				return []recipientCheck{{event: edited, want: []string{claimant.ID}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			claimIssue(t, database, "T-1", claimant)
			checkSessionRecipients(t, database, tc.arrange(t, database, broker))
		})
	}
}
