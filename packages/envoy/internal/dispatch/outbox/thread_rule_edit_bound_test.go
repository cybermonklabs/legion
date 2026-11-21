package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
)

// The wait of the copy of a reply's edit bound for a session the issue's primary document credits
// alone has the bound the reply's own credit wait has (mentionDeliveries.editCreditCopyOf,
// writtenPastTheBound, unnamedForwardBound). The person's reply in B's thread (B live) names C,
// and C's mention is past the lease the outbox waits a send out for, its sender alive and its
// lane held, so the attempt stays pending however old it grows (agedUnnamedAttempts). The reply's
// copy to C is a fallback row (destinations.leftTo). The person's edit, as the API writes it
// (mentions []), waits for that mention within the bound, holding none of C's later events of the
// issue while it waits (TestAPersonsLaterCommentReachesACreditWithoutWaitingOnTheEditsCopy). Past
// the bound nothing else would end the wait: the edit is decided then, and C, which the reply
// names by a mention still owed, keeps it, so the edit is published.
func TestAnEditOfAReplyWaitsForTheMentionOfACreditNoLongerThanTheBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		waits bool
	}{
		{name: "within the bound", age: model.DeliveryClaimLapse + model.DeliveryClaimLapse/2, waits: true},
		{name: "past the bound", age: unnamedForwardBound + 30*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedMentionDelivery(t, database, reply, issue.c.ID, "pending")
			agedUnnamedAttempts(t, database, reply, tc.age)
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
				Registry: newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}})}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			if publishedAt(t, database, created.ID) == nil {
				t.Fatalf("the reply was not published: its copy to C, whose mention is past its lease, is a fallback row")
			}
			if held := heldDestinations(t, database, reply); len(held) != 1 || held[0] != contracts.LegacyAgentSubject(issue.c.ID) {
				t.Fatalf("the reply's fallback rows are %v, want C's own topic alone", held)
			}
			published := publishedAt(t, database, edited.ID) != nil
			copied := len(copiesOf(publisher.all(), edited, issue.c.ID)) > 0
			switch {
			case tc.waits && (published || copied):
				t.Fatalf("the edit was published %v and reached C %v while C's mention, %s old, was still owed within the bound (%s): its copy to C waits for it",
					published, copied, tc.age, unnamedForwardBound)
			case !tc.waits && !published:
				t.Fatalf("the edit is still unpublished while C's mention, %s old, is past the bound (%s): its wait for that mention has no end",
					tc.age, unnamedForwardBound)
			case !tc.waits && !copied:
				t.Fatalf("the edit reached %v and not C, whose mention by the reply is still owed past the bound: the reply reaches C by that mention or by its row",
					agentTopics(readEventRow(t, database, edited.ID).destinations))
			}
		})
	}
}

// R6 of dispatch://INFRA-3686 with the architect's ruling in its round 9: the wait of the copy of
// a reply's edit bound for a session a document credits alone ends at unnamedForwardBound, the
// bound of the mention's own wait, and past it the edit goes to the session that got, or is owed,
// the reply's copy by the failed-mention rule, and to no other credit
// (mentionDeliveries.editCreditCopyOf). The person's reply in B's thread (B live) names C, whose
// mention failed, and @role:R, whose listener lookup went unanswered, so that attempt names no
// session; its sender is alive and holds its lane, so the attempt stays pending past the bound
// (agedUnnamedAttempts). The outbox cannot read R's claim, or reads it as A's: either way A, which
// the issue's primary document credits, gets the reply by a credit's row alone, should the sender
// pin the attempt to A and the send fail (creditHeldAsARow), so A's mention of the reply is still
// unsettled. C gets the reply under its failed mention's key, in this pass where the outbox read
// R as A's and by a row of C's own key where it could not read R. The person's edit of the reply,
// as the API writes it (mentions []), is read past the bound: it waits for neither A's row nor
// A's attempt, and is published in this pass, to C and not to A. A gets the edit from its row
// should that row later send it the reply (creditRowEdits,
// TestAnEditOfAReplyWhoseLateRoleMentionOfACreditFailsReachesIt).
func TestAnEditPastTheBoundGoesToTheCreditThatGotTheReplyWithoutWaitingForAnotherCreditsMention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roles func(issue threadRuleIssue) (roles.ClaimStore, roles.SessionLookup)
	}{
		{name: "R's claim unread", roles: func(threadRuleIssue) (roles.ClaimStore, roles.SessionLookup) {
			return unreadableRoleClaims{}, heldRoles{}
		}},
		{name: "R read as A's", roles: func(issue threadRuleIssue) (roles.ClaimStore, roles.SessionLookup) {
			claims := claimedBy{holder: issue.a.ID}
			return claims, claims
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "pending")
			seedMentionDelivery(t, database, reply, issue.c.ID, "failed")
			agedUnnamedAttempts(t, database, reply, unnamedForwardBound+30*time.Second)
			appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root,
					Mentions: append([]model.Mention{{Target: target, Delivery: "steer"}}, mentionOf(issue.c.ID)...),
				}},
			})
			edited := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.edited", Actor: issue.person,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &root, Body: "Edited.",
					Mentions: []model.Mention{},
				}},
			})
			claims, sessions := tc.roles(issue)
			publisher := &recordingPublisher{}
			deps := Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
				RoleClaims: claims, Sessions: sessions,
			}
			o := holding(t, deps)
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			if held := heldDestinations(t, database, reply); !slices.Contains(held, contracts.LegacyAgentSubject(issue.a.ID)) {
				t.Fatalf("the reply's fallback rows are %v, want A's own topic among them: A gets the reply by a credit's row alone", held)
			}
			if publishedAt(t, database, edited.ID) == nil {
				_, _, reached := eventState(t, database, edited.ID)
				t.Fatalf("the edit is unpublished (it reached %v), read past the bound (%s) while A's mention of the reply is still unsettled: its wait for that mention, or for A's row, has no end",
					reached, unnamedForwardBound)
			}
			if len(copiesOf(publisher.all(), edited, issue.c.ID)) == 0 {
				t.Fatalf("C, whose mention of the reply failed, got no copy of the edit (the edit reached %v): past the bound the edit goes to the session that got, or is owed, the reply by the failed-mention rule",
					agentTopics(readEventRow(t, database, edited.ID).destinations))
			}
			if got := copiesOf(publisher.all(), edited, issue.a.ID); len(got) != 0 {
				t.Fatalf("A, whose mention of the reply is still unsettled and which gets the reply by a credit's row alone, got %d copies of the edit: past the bound the edit goes to no other credit than the one that got, or is owed, the reply",
					len(got))
			}
		})
	}
}

// The architect's round-9 ruling on dispatch://INFRA-3686 goes on: nothing else of the issue
// waits on the copy of an edit that waits, and a person's later comment on the issue never does
// (destinations.waitForCredits, mentionDeliveries.editCreditCopyOf). The person's reply in B's
// thread (B live) names @role:R, whose listener lookup went unanswered; the outbox reads R's claim
// as A's, and the attempt is past the lease the outbox waits a send out for and within
// unnamedForwardBound. So A, which the issue's primary document credits, gets the reply by a
// credit's row alone (creditHeldAsARow), and the copy of the person's edit bound for A waits for
// that attempt. The person then writes a comment on the issue, which reaches A as a credit of the
// primary document. It reaches A in the pass that reads it, whether that pass reads the waiting
// edit as due beside it or as an event that waits to be read again, and a mention of A in it
// would wait behind no event (model.MentionCopyHolderSQL, copyHolder).
func TestAPersonsLaterCommentReachesACreditWithoutWaitingOnTheEditsCopy(t *testing.T) {
	for _, written := range []string{"before the edit is read", "while the edit waits to be read again"} {
		t.Run("the later comment written "+written, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			target := "role:legion-reviewer-t1"
			root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
			reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
			seedDelivery(t, database, reply, target, "", "resumed")
			appendEvent(t, database, broker, model.Event{
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
			claims := claimedBy{holder: issue.a.ID}
			publisher := &recordingPublisher{}
			deps := Deps{
				Store: database, Publisher: publisher, Broker: events.NewBroker(),
				Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
				RoleClaims: claims, Sessions: claims,
			}
			o := holding(t, deps)
			writeLater := func() model.Event {
				comment := seedThreadComment(t, database, "T-1", "", issue.person, nil)
				return appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: comment})
			}
			var later model.Event
			if written == "before the edit is read" {
				later = writeLater()
			}
			if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			if written != "before the edit is read" {
				later = writeLater()
				if _, err := scanBatch(ctx, deps, unfenced{}, o.instance); err != nil {
					t.Fatalf("scan batch: %v", err)
				}
			}
			if publishedAt(t, database, edited.ID) != nil || len(copiesOf(publisher.all(), edited, issue.a.ID)) != 0 {
				t.Fatalf("the edit was published, or reached A, while A's mention of the reply was still owed within the bound (%s): the edit's copy to A waits for it, which is the wait this test reads past",
					unnamedForwardBound)
			}
			if len(copiesOf(publisher.all(), later, issue.a.ID)) == 0 {
				_, _, reached := eventState(t, database, later.ID)
				t.Errorf("A got no copy of the person's later comment (it reached %v): the edit's copy to A, which waits for A's mention of the reply, held it, and nothing else of the issue may wait on that copy",
					reached)
			}
			by, err := copyHolder(ctx, deps, later.ID, contracts.LegacyAgentSubject(issue.a.ID), issue.a.ID)
			if err != nil {
				t.Fatalf("read the copy rule for A: %v", err)
			}
			if by != nil {
				t.Errorf("a mention of A in the person's later comment waits behind %s, want nothing: the edit owes A nothing while its copy to A waits",
					describeHolder(by))
			}
		})
	}
}
