package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAMentionThatSettlesAfterItsCommentsBatchIsRecordedWhenItSettles is the defect
// dispatch://INFRA-1149's c-person-graph found in production: a mention whose attempt settled
// sent after the outbox had published its comment was recorded nowhere - no delivered_to tuple,
// no page of the session and no part_of under it - because the comment's batch reads the
// attempts as they stand when it runs, and nothing holds a comment back for a mention of a
// session it sends no copy to. The delivery is recorded under the attempt's own comment.delivery
// receipt (deliveredGraph, mentionReached), whenever the outbox reads that receipt.
//
// The run publishes the comment while both of its mentions are still being sent, which records
// nothing of either. It then settles one attempt sent and the other failed, as the API's sender
// settles them (settleMention), and scans again. The sent attempt is the comment's delivered_to
// tuple into its session, the session's node, the session's page and the page's part_of, and the
// tuples and the page are caused by that attempt's receipt. The failed attempt is nothing at
// all. A third scan sends all three events again: the comment's batch now reads its attempt
// sent, as an attempt that settled before its comment was read leaves it, and the receipt's
// batch states its delivery once more. The store gains no row.
func TestAMentionThatSettlesAfterItsCommentsBatchIsRecordedWhenItSettles(t *testing.T) {
	const reached, unreached = "session-late-reader", "session-never-reached"
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedThreadComment(t, database, "T-1", "", alice, nil)
	created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{
		ID: commentID, Body: "@" + reached + " and @" + unreached + ", one word for each of you.",
	}, reached, unreached)
	seedMentionDelivery(t, database, commentID, reached, "pending")
	seedMentionDelivery(t, database, commentID, unreached, "pending")

	outbox := holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker})
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, created, true)
	if delivered := history(t, database, commentNodeKind, commentID, graphstore.TupleDeliveredTo); len(delivered) != 0 {
		t.Fatalf("the comment's batch, which read both attempts pending, recorded the deliveries %+v; a pending attempt delivered nothing", delivered)
	}
	requireNoNode(t, database, graphstore.KindPage, graphstore.SessionPageID(reached))

	sent := settleMention(t, database, broker, commentID, reached, alice, "")
	failed := settleMention(t, database, broker, commentID, unreached, alice, "not sent: no live session")
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, sent, true)
	requirePublished(t, database, failed, true)
	requireMentionRecorded(t, database, commentID, reached, sent.ID)
	requireNoNode(t, database, graphstore.KindSession, unreached)
	requireNoNode(t, database, graphstore.KindPage, graphstore.SessionPageID(unreached))

	nodes, tuples := storeRows(t, database)
	for _, event := range []model.Event{created, sent, failed} {
		if _, err := database.Pool.Exec(ctx, `
			update events set published_at = null, published_destinations = '{}', next_attempt_at = now()
			where id = $1
		`, event.ID); err != nil {
			t.Fatalf("put event %d back as it was before its send: %v", event.ID, err)
		}
	}
	outbox.scan(ctx, unfenced{})
	for _, event := range []model.Event{created, sent, failed} {
		requirePublished(t, database, event, true)
	}
	requireMentionRecorded(t, database, commentID, reached, sent.ID)
	if gotNodes, gotTuples := storeRows(t, database); gotNodes != nodes || gotTuples != tuples {
		t.Fatalf("the store holds %d nodes and %d tuples after the same events were sent and recorded again, and %d and %d before: one delivery is stated once",
			gotNodes, gotTuples, nodes, tuples)
	}
}

// TestAMentionThatSettlesBeforeItsCommentsBatchIsRecordedOnce is the other order: the attempt
// settled sent before the outbox read its comment, so the batch reads the comment with that
// attempt sent and the receipt beside it. The delivery is stated once, under the receipt, and the
// comment's own event states no second row of it.
func TestAMentionThatSettlesBeforeItsCommentsBatchIsRecordedOnce(t *testing.T) {
	const reached = "session-early-reader"
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedThreadComment(t, database, "T-1", "", alice, nil)
	created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{
		ID: commentID, Body: "@" + reached + " a word before the outbox reads it.",
	}, reached)
	seedMentionDelivery(t, database, commentID, reached, "pending")
	sent := settleMention(t, database, broker, commentID, reached, alice, "")

	outbox := holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker})
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, created, true)
	requirePublished(t, database, sent, true)
	requireMentionRecorded(t, database, commentID, reached, sent.ID)
}

// TestALateMentionOfASessionThatClaimedARoleGivesItNoPageOfItsOwn is the other half of
// c-person-graph's "a session that claimed no role carries its own page and no part_of tuple": a
// session that claimed a role carries no page of its own, the role's page being its page. An
// earlier batch resolved the role to the session - a message on another issue routed to the role
// - so the batch that records the mention's receipt resolves no role itself, and what it knows of
// the claim is what the store holds (sessionsInARole). The mention settles sent after its
// comment's batch, as in TestAMentionThatSettlesAfterItsCommentsBatchIsRecordedWhenItSettles, and
// its receipt records the delivered_to tuple and no page of the session: the session stays
// part_of its role and nothing else.
func TestALateMentionOfASessionThatClaimedARoleGivesItNoPageOfItsOwn(t *testing.T) {
	// The holder is the session recordingPublisher acknowledges a role forward to, so the routed
	// message is published and its record written like any other.
	const role, holder = "late-mention-role", roleHolder
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Routed issue', $1, '{"kind":"user","id":"alice"}', 'V')
	`, "role:"+role); err != nil {
		t.Fatalf("create issue T-2: %v", err)
	}
	// The claim store names the session and its record was written now, so every resolution of
	// the role reads it live and holding.
	claim := roleClaimStub{holder: holder, lastSeen: time.Now().UnixMilli()}
	outbox := holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker,
		RoleClaims: claim, Sessions: claim})
	alice := model.Actor{Kind: "user", ID: "alice"}

	routed := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f20", IssueKey: new("T-2"), Body: "for the role"},
	})
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, routed, true)
	requireTuple(t, database, graphstore.KindSession, holder, graphstore.TuplePartOf, graphstore.KindRole, role, true)

	commentID := seedThreadComment(t, database, "T-1", "", alice, nil)
	created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{
		ID: commentID, Body: "@" + holder + " a word for the role's holder.",
	}, holder)
	seedMentionDelivery(t, database, commentID, holder, "pending")
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, created, true)
	if delivered := history(t, database, commentNodeKind, commentID, graphstore.TupleDeliveredTo); len(delivered) != 0 {
		t.Fatalf("the comment's batch, which read the attempt pending, recorded the deliveries %+v", delivered)
	}

	sent := settleMention(t, database, broker, commentID, holder, alice, "")
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, sent, true)
	requireDeliveredOnce(t, database, commentID, holder, sent.ID)
	requireLiveNode(t, database, graphstore.KindSession, holder)
	page := graphstore.SessionPageID(holder)
	requireNoNode(t, database, graphstore.KindPage, page)
	if under := history(t, database, graphstore.KindPage, page, graphstore.TuplePartOf); len(under) != 0 {
		t.Fatalf("the page %s of a session that claimed a role hangs under %+v, want no page of its own", page, under)
	}
	parts := history(t, database, graphstore.KindSession, holder, graphstore.TuplePartOf)
	if len(parts) != 1 || parts[0].ToKind != graphstore.KindRole || parts[0].ToID != role || !parts[0].Asserted {
		t.Fatalf("the session %s is part of %+v, want the role %s alone", holder, parts, role)
	}
}

// settleMention settles the pending attempt of commentID's mention of session as the API's sender
// settles it once its send is answered (api's completeCommentDelivery and settleDeliveryAttempt):
// in one transaction, under the comment's owner lock, the row is set sent, or failed with
// failure when that is not empty; the session a sent attempt reached is made a node
// (graphstore.Reached); and the attempt's comment.delivery receipt is appended. It returns the
// receipt.
func settleMention(t *testing.T, database *store.Store, broker *events.Broker, commentID, session string, actor model.Actor, failure string) model.Event {
	t.Helper()
	ctx := context.Background()
	state, failedWith := graphstore.DeliverySent, (*string)(nil)
	if failure != "" {
		state, failedWith = "failed", &failure
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settle of the mention of %s: %v", session, err)
	}
	defer tx.Rollback(ctx)
	owner := model.Event{IssueKey: new("T-1")}
	if err := broker.LockOwners(ctx, tx, owner); err != nil {
		t.Fatalf("lock the comment's owner: %v", err)
	}
	receipt := model.CommentDeliveryEventPayload{CommentID: commentID, State: state, Error: failure}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		update comment_deliveries set state = $3, error = $4
		where comment_id = $1 and target = $2 and state = 'pending'
		returning target, attempt, delivery, session_id, created_at
	`, commentID, "session:"+session, state, failedWith).Scan(
		&receipt.Target, &receipt.Attempt, &receipt.Delivery, &receipt.SessionID, &createdAt); err != nil {
		t.Fatalf("settle the mention of %s %s: %v", session, state, err)
	}
	if err := graphstore.Reached(ctx, tx, state, session, actor, createdAt); err != nil {
		t.Fatalf("record the session the mention of %s reached: %v", session, err)
	}
	event := owner
	event.Type, event.Actor, event.Payload = "comment.delivery", actor, receipt
	appended, err := broker.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append the receipt of the mention of %s: %v", session, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the settle of the mention of %s: %v", session, err)
	}
	return appended
}

// requireMentionRecorded fails unless the store holds what a mention of commentID delivered to a
// session that claimed no role states, each fact once and every tuple and the page caused by the
// receipt cause: the comment's delivered_to tuple into the session, the session's node, the
// session's own page, and that page's part_of the session, with the session part of no role.
func requireMentionRecorded(t *testing.T, database *store.Store, commentID, session string, cause int64) {
	t.Helper()
	requireDeliveredOnce(t, database, commentID, session, cause)
	requireLiveNode(t, database, graphstore.KindSession, session)
	page := graphstore.SessionPageID(session)
	if node := requireLiveNode(t, database, graphstore.KindPage, page); node.SourceEvent != cause {
		t.Fatalf("the session's page was caused by event %d, want %d", node.SourceEvent, cause)
	}
	partOf := history(t, database, graphstore.KindPage, page, graphstore.TuplePartOf)
	if len(partOf) != 1 || partOf[0].ToKind != graphstore.KindSession || partOf[0].ToID != session ||
		!partOf[0].Asserted || partOf[0].SourceEvent != cause {
		t.Fatalf("the page %s hangs under %+v, want the session %s alone, caused by event %d", page, partOf, session, cause)
	}
	if roles := history(t, database, graphstore.KindSession, session, graphstore.TuplePartOf); len(roles) != 0 {
		t.Fatalf("the session %s, which claimed no role, is part of %+v", session, roles)
	}
}

// requireDeliveredOnce fails unless the comment's one delivered_to tuple is into session, written
// by the outbox and caused by the receipt cause.
func requireDeliveredOnce(t *testing.T, database *store.Store, commentID, session string, cause int64) {
	t.Helper()
	delivered := history(t, database, commentNodeKind, commentID, graphstore.TupleDeliveredTo)
	if len(delivered) != 1 || delivered[0].ToKind != graphstore.KindSession || delivered[0].ToID != session ||
		!delivered[0].Asserted || delivered[0].SourceEvent != cause || !delivered[0].Who.SameAs(graphWriter) {
		t.Fatalf("the comment's deliveries read %+v, want one delivered_to tuple into session %s caused by event %d",
			delivered, session, cause)
	}
}

// history is every tuple of one node under one kind, in the order the log holds them.
func history(t *testing.T, database *store.Store, fromKind, fromID, kind string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.History(context.Background(), database.Pool, fromKind, fromID, kind)
	if err != nil {
		t.Fatalf("read the %s tuples of %s %s: %v", kind, fromKind, fromID, err)
	}
	return tuples
}

// requireLiveNode fails unless the store holds the node live, and returns it.
func requireLiveNode(t *testing.T, database *store.Store, kind, id string) graphstore.Node {
	t.Helper()
	node, held, err := graphstore.ReadNode(context.Background(), database.Pool, kind, id)
	if err != nil {
		t.Fatalf("read node %s %s: %v", kind, id, err)
	}
	if !held || node.State != graphstore.StateLive {
		t.Fatalf("the store holds node %s %s %t, in state %q, want it live", kind, id, held, node.State)
	}
	return node
}

// requireNoNode fails if the store holds the node.
func requireNoNode(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	if _, held, err := graphstore.ReadNode(context.Background(), database.Pool, kind, id); err != nil || held {
		t.Fatalf("the store holds node %s %s %t (err %v), want none", kind, id, held, err)
	}
}

// storeRows counts every node and every tuple the store holds.
func storeRows(t *testing.T, database *store.Store) (nodes, tuples int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes), (select count(*) from graph_tuples)
	`).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("count the store's rows: %v", err)
	}
	return nodes, tuples
}
