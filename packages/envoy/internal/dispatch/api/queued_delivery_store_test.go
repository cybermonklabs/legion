package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// queuedModes are the delivery modes the stopped session of these tests takes.
var queuedModes = []string{"aside", "btw", "steer"}

// heldSession is a second stopped session, whose sends the listener holds unanswered, so the
// attempt carrying one stays pending while the test reads the store.
const heldSession = "ses_held0a1b2c3d4"

// A mention of a session the listener holds stopped settles `queued`, and a queued attempt is a
// delivery (this change's R14 at version 37: "an attempt the listener took is one",
// model.DeliveryTaken). So that settle makes the session a node of the store, the outbox's
// record of its receipt states the comment delivered to it, and the person the credential
// proved that session for reads the comment on their own page. Before cybermonklabs/legion#2643
// the same send ended failed, no node was written and a fallback copy carried the comment to
// the page; since it the attempt is queued, the mention counts as delivered and no fallback
// copy is published, so this is the one write that puts the comment on that page.
//
// A pending attempt is no delivery ("a pending or failed one is not"), so it earns nothing: the
// test first mentions a session whose send the listener holds, and while that attempt stands
// pending, opened, claimed and pinned to the session as its sender leaves it before the settle,
// it reads no node of the session, no tuple naming it and no receipt the outbox could state one
// from.
func TestAMentionQueuedForAStoppedSessionReachesItsPerson(t *testing.T) {
	hold := newSendHold()
	defer hold.free()
	listener := newQueueingListener(t, map[string]fakeSession{
		brokerTestSession: {title: "alice's agent", capabilities: queuedModes, stopped: true},
		heldSession:       {title: "a session whose send is held", capabilities: queuedModes, stopped: true, hold: hold},
	})
	rig := newIdentityRigOn(t, listener.url)
	caller := newBrokerCaller(t)
	const token = "a-token-proving-alices-stopped-session"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	issue := rig.seedRoutedIssue(t)

	held := decodeMentionedComment(t, dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body":     "For a session whose send is still on its way.",
		"mentions": []map[string]any{{"target": "session:" + heldSession}},
	}, "bob"))
	select {
	case <-hold.arrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("the mention of %s never reached the listener, so no attempt stood pending", heldSession)
	}
	requireAPendingAttemptEarnsNothing(t, rig.database, held.ID, heldSession)
	hold.free()
	settledCommentDeliveries(t, rig.database, held.ID)

	mention := decodeMentionedComment(t, dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body":     "Pick this up when you are back.",
		"mentions": []map[string]any{{"target": "session:" + brokerTestSession}},
	}, "bob"))
	attempts := settledCommentDeliveries(t, rig.database, mention.ID)
	if len(attempts) != 1 || attempts[0].State != model.DeliveryStateQueued ||
		attempts[0].SessionID == nil || *attempts[0].SessionID != brokerTestSession {
		t.Fatalf("the mention's settled attempts = %#v, want one queued attempt pinned to %s", attempts, brokerTestSession)
	}
	if _, held, err := graphstore.ReadNode(context.Background(), rig.database.Pool, graphstore.KindSession, brokerTestSession); err != nil || !held {
		t.Errorf("the store holds the queued attempt's session as a node: %t (err %v), want it from the settle on", held, err)
	}
	// The proved session's comment ties it to its person (the alias_of tuple graphstore.AuthorFacts
	// states), which carries a delivery to the session onto that person's page. It is written after
	// the node check because it writes the same node (graphstore.Authored). That check does not stop
	// the test, so the page below is read whatever the check found.
	rig.created(t, "the proved session's comment", rig.serve(caller.presenting(t, http.MethodPost,
		"/api/v1/issues/"+issue+"/comments", token, map[string]any{"body": "Written by the session the broker proved."})))
	rig.publish(t, issue)

	page := rig.page(t, brokerTestPerson, brokerTestPerson)
	if !slices.ContainsFunc(page.Rows, func(row personNode) bool {
		return row.ID == mention.ID && slices.Contains(row.Relations, graphstore.TupleDeliveredTo)
	}) {
		t.Errorf("the page of %s after a mention queued for their proved session holds %+v, want the comment as delivered_to",
			brokerTestPerson, page.Rows)
	}
	requireStoreCensusZero(t, rig.database, "after a mention queued for a stopped session")
}

// requireAPendingAttemptEarnsNothing reads the one attempt of comment while its send is held: it
// stands pending and pinned to session, and the store holds no node of that session, no tuple
// naming it and no comment.delivery receipt of the comment, which is what the outbox states a
// delivered_to tuple from.
func requireAPendingAttemptEarnsNothing(t *testing.T, database *store.Store, comment, session string) {
	t.Helper()
	ctx := context.Background()
	attempts := waitForCommentDeliveries(t, database, comment, "the attempt of the held send",
		func(attempts []model.CommentDelivery) bool { return len(attempts) > 0 })
	if len(attempts) != 1 || attempts[0].State != model.DeliveryStatePending ||
		attempts[0].SessionID == nil || *attempts[0].SessionID != session {
		t.Fatalf("the held mention's attempts = %#v, want one pending attempt pinned to %s", attempts, session)
	}
	if _, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindSession, session); err != nil || found {
		t.Errorf("the store holds the pending attempt's session %s as a node: %t (err %v), want none", session, found, err)
	}
	var naming, receipts int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2)
	`, graphstore.KindSession, session).Scan(&naming); err != nil {
		t.Fatalf("count the tuples naming %s: %v", session, err)
	}
	if naming != 0 {
		t.Errorf("the store holds %d tuple(s) naming the pending attempt's session %s, want none: a pending attempt is no delivery", naming, session)
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from events where type = 'comment.delivery' and payload->>'comment_id' = $1
	`, comment).Scan(&receipts); err != nil {
		t.Fatalf("count the receipts of %s: %v", comment, err)
	}
	if receipts != 0 {
		t.Errorf("the log holds %d comment.delivery receipt(s) of the pending attempt, want none to state a delivered_to from", receipts)
	}
	t.Logf("the attempt of %s stands pending and pinned to %s, which the store holds no node and no tuple of", comment, session)
}

// A message queued for a stopped session and answered by that session when it runs again leaves
// the census at zero. The flip the answer writes states no node of its own (replyMessage), and
// it owes none: the settle that queued the attempt already wrote the session's, a queued
// attempt and a sent one being one thing to every reader that asks whether the listener took
// the message (model.DeliveryTaken). Before that settle wrote it, the flip left the census
// holding the store to a session node nothing had written.
func TestAQueuedMessageAnsweredLaterLeavesTheCensusAtZero(t *testing.T) {
	listener := newQueueingListener(t, map[string]fakeSession{
		"s1": {title: "planner", capabilities: queuedModes, stopped: true},
	})
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Status when you are back?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a stopped session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 ||
		attempts[0].State != model.DeliveryStateQueued || attempts[0].SessionID != "s1" {
		t.Fatalf("the stored attempts = %#v, want one queued attempt pinned to s1", attempts)
	}
	requireStoreCensusZero(t, database, "after a message was queued for a stopped session")

	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Back, and green.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the session's answer to its queued message: status=%d body=%s", reply.Code, reply.Body.String())
	}
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != model.DeliveryStateSent || attempts[0].ReplyID == nil {
		t.Fatalf("the attempts after the answer = %#v, want the one attempt flipped to sent with its reply", attempts)
	}
	requireStoreCensusZero(t, database, "after the queued message was answered")
}

// requireStoreCensusZero takes the store census over database as `envoy-dispatch census --store`
// takes it and fails unless it read no difference in either direction over any kind. It reports
// every kind it read, so a failure says which side of which kind drifted.
func requireStoreCensusZero(t *testing.T, database *store.Store, what string) {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census %s: %v", what, err)
	}
	for _, kind := range report.Kinds {
		t.Logf("%s: kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			what, kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
	}
	if report.Differences() == 0 {
		return
	}
	var printed strings.Builder
	report.Write(&printed)
	t.Fatalf("the store census %s reads %d difference(s):\n%s", what, report.Differences(), printed.String())
}
