package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestARoleTargetedMessageGivesItsRoleANodeAndAPage is dispatch://INFRA-1149's R19 for a message
// aimed at a role. POST /api/v1/issues/{key}/messages with target "role:X" stores messages.target
// role:X, which the store census holds role X to (graphcensus's addressedRoles). Its event is a
// targeted message, which reaches no route (eventDestinations), so no destination of its batch
// names the role and no resolution reads the role's claim. The batch that records the message
// writes role X's node, its page and the page's part_of the role all the same, each caused by the
// message's event, and no held_by, since nothing read who holds the role (deliveredGraph,
// addressedRoles).
func TestARoleTargetedMessageGivesItsRoleANodeAndAPage(t *testing.T) {
	const role = "targeted-message-reader"
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	target := "role:" + role
	message := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{
			ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f30", IssueKey: new("T-1"), Body: "For the role alone.", Target: &target,
		}},
	})

	holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker}).scan(context.Background(), unfenced{})
	requirePublished(t, database, message, true)
	requireRoleNodeAndPage(t, database, role, message.ID)
	if claims := claimTuples(t, database, role); len(claims) != 0 {
		t.Fatalf("the role a targeted message names holds the claims %+v, want no held_by: nothing read its claim", claims)
	}
}

// TestARoleAnEventNamesGetsANodeAndAPageWithoutARoute is the same rule for the two other places an
// event names a role, and for the route a batch never resolves. Each case appends what the API
// appends and runs one batch over it, and requires the role the event names to hold its node, its
// page and the page's part_of, caused by that event, whether or not the batch routed anything to
// the role.
//
//   - An agent's route change: patchIssue records the route in its issue.updated event, and a
//     session's write wakes nobody (events.Broker.Notify), so no destination names the role.
//   - Two route changes one batch reads: the batch routes both events by the route the issue
//     holds when it reads them, the second, so only the event's own route names the first. The
//     second is resolved and keeps its held_by; the first has none.
//   - A comment's @role: mention on an issue routed to no role: the comment's event names the
//     mention's target, and comment_deliveries.target holds it, which no route reaches.
//   - A role a targeted message names that a later event of the same batch routes to: the route
//     resolves the role's holder, and the message's naming of it, read first, does not take that
//     resolution away, so the role keeps its held_by.
func TestARoleAnEventNamesGetsANodeAndAPageWithoutARoute(t *testing.T) {
	const first, second = "first-route-of-the-issue", "second-route-of-the-issue"
	routeEvent := func(t *testing.T, database *store.Store, broker *events.Broker, actor model.Actor, role string) model.Event {
		t.Helper()
		route := "role:" + role
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "issue.updated", Actor: actor,
			Payload: model.IssueEventPayload{Issue: model.Issue{Key: "T-1", Route: &route}, Changed: []string{"route"}},
		})
	}

	t.Run("an agent's route change", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "role:" + first
		seedIssue(t, database, "T-1", &route)
		routed := routeEvent(t, database, broker, model.Actor{Kind: "session", ID: "ses_routes_the_issue"}, first)
		if routed.Notify {
			t.Fatalf("a session's route change notifies, so its route would be a destination; the case needs one that is not")
		}

		holding(t, withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker})).
			scan(context.Background(), unfenced{})
		requirePublished(t, database, routed, true)
		requireRoleNodeAndPage(t, database, first, routed.ID)
		if claims := claimTuples(t, database, first); len(claims) != 0 {
			t.Fatalf("a role no batch routed to holds the claims %+v, want none", claims)
		}
	})

	t.Run("two route changes one batch reads", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "role:" + second
		seedIssue(t, database, "T-1", &route)
		alice := model.Actor{Kind: "user", ID: "alice"}
		routedFirst := routeEvent(t, database, broker, alice, first)
		routedSecond := routeEvent(t, database, broker, alice, second)

		holding(t, withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker})).
			scan(context.Background(), unfenced{})
		requirePublished(t, database, routedFirst, true)
		requirePublished(t, database, routedSecond, true)
		requireRoleNodeAndPage(t, database, first, routedFirst.ID)
		if claims := claimTuples(t, database, first); len(claims) != 0 {
			t.Fatalf("the first route, which the batch did not resolve, holds the claims %+v, want none", claims)
		}
		requireLiveNode(t, database, graphstore.KindRole, second)
		requireLiveNode(t, database, graphstore.KindPage, graphstore.RolePageID(second))
		requireTuple(t, database, graphstore.KindRole, second, graphstore.TupleHeldBy, graphstore.KindSession, roleHolder, true)
	})

	t.Run("a comment's mention of a role", func(t *testing.T) {
		const mentioned, reader = "mentioned-on-the-issue", "ses_holds_the_mentioned_role"
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		alice := model.Actor{Kind: "user", ID: "alice"}
		target := "role:" + mentioned
		commentID := seedThreadComment(t, database, "T-1", "", alice, nil)
		seedDelivery(t, database, commentID, target, reader, "sent")
		created := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Author: alice, Body: "@role:" + mentioned + " a word for you.",
				Mentions: []model.Mention{{Target: target, Delivery: "steer", SessionID: new(reader)}},
			}},
		})

		holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker}).scan(context.Background(), unfenced{})
		requirePublished(t, database, created, true)
		requireRoleNodeAndPage(t, database, mentioned, created.ID)
		if claims := claimTuples(t, database, mentioned); len(claims) != 0 {
			t.Fatalf("the role a mention names holds the claims %+v, want none: nothing read its claim", claims)
		}
	})

	t.Run("a role the batch also routes to", func(t *testing.T) {
		const routed = "targeted-and-routed"
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "role:" + routed
		seedIssue(t, database, "T-1", &route)
		alice := model.Actor{Kind: "user", ID: "alice"}
		targeted := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: alice,
			Payload: model.MessageEventPayload{Message: model.Message{
				ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f31", IssueKey: new("T-1"), Body: "For the role alone.", Target: &route,
			}},
		})
		untargeted := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: alice,
			Payload: model.MessageEventPayload{Message: model.Message{
				ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f32", IssueKey: new("T-1"), Body: "For whoever the issue is routed to.",
			}},
		})

		holding(t, withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker})).
			scan(context.Background(), unfenced{})
		requirePublished(t, database, targeted, true)
		requirePublished(t, database, untargeted, true)
		requireLiveNode(t, database, graphstore.KindRole, routed)
		requireLiveNode(t, database, graphstore.KindPage, graphstore.RolePageID(routed))
		requireTuple(t, database, graphstore.KindRole, routed, graphstore.TupleHeldBy, graphstore.KindSession, roleHolder, true)
	})
}

// requireRoleNodeAndPage fails unless the store holds role's node and its page live, and the
// page's one part_of the role, all three caused by the event cause.
func requireRoleNodeAndPage(t *testing.T, database *store.Store, role string, cause int64) {
	t.Helper()
	if node := requireLiveNode(t, database, graphstore.KindRole, role); node.SourceEvent != cause {
		t.Fatalf("the node of role %s was caused by event %d, want %d", role, node.SourceEvent, cause)
	}
	page := graphstore.RolePageID(role)
	if node := requireLiveNode(t, database, graphstore.KindPage, page); node.SourceEvent != cause {
		t.Fatalf("the page %s was caused by event %d, want %d", page, node.SourceEvent, cause)
	}
	partOf := history(t, database, graphstore.KindPage, page, graphstore.TuplePartOf)
	if len(partOf) != 1 || partOf[0].ToKind != graphstore.KindRole || partOf[0].ToID != role ||
		!partOf[0].Asserted || partOf[0].SourceEvent != cause {
		t.Fatalf("the page %s hangs under %+v, want the role %s alone, caused by event %d", page, partOf, role, cause)
	}
}
