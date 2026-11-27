package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestMentionsAreTuplesAndEveryAttemptIsANode is c-mention-tuples. Each of its parts is a subtest
// of its own, so a part that breaks is named by the subtest that fails. A mention is a tuple to the
// identity it points at (mentionsAreTuples, mention_tuples_test.go).
//
// Every attempt is one delivery_attempt node: each writer of a comment_deliveries or a
// message_deliveries row is driven once - through the real handlers where a route makes the
// write, and called on the server directly where the delivery machinery makes it on its own - and
// the attempt's node is read after each transition, not only at the end, so a writer that left
// the node behind its row fails the case that drove it. A transition another later in the same
// case would state again is read before that later one runs: a held lookup, a stalled send or a
// step the case calls itself parts them. The opening write is held to its own facts by their time
// and cause, which no later transition can give them.
//
// The session half of delivery: events.published_destinations is the topics of the delivered_to
// tuples the outbox appended for each event (outbox.deliveredGraph), driven with
// runOutboxUntilSettled as comment_delivery_fallback_copy_test.go drives it.
func TestMentionsAreTuplesAndEveryAttemptIsANode(t *testing.T) {
	t.Parallel()
	t.Run("a mention is a tuple to the person, session or role it points at", mentionsAreTuples)
	t.Run("every attempt is one delivery_attempt node", func(t *testing.T) {
		t.Run("a mention is opened, given back, taken, resolved, sent, reported failed and answered", attemptOfAMentionFromOpenToAnswer)
		t.Run("a lane stands down for a Retry in flight, whose send then supersedes the lane's attempt", attemptsOfAStandDownAndASupersede)
		t.Run("a keyed repeat claims an owed mention and the sender writes its claim again and gives it back", attemptOfAKeyedClaim)
		t.Run("a keyed repeat that finds another sender's claim writes nothing, its attempt facts included", attemptOfALostKeyedClaim)
		t.Run("a refused send and a mention nobody holds settle failed with their errors", attemptsThatFail)
		t.Run("the delivery sweep settles a mention its comment no longer holds", attemptTheSweepSettles)
		t.Run("the outbox settles an abandoned mention before it publishes the session's copy", attemptTheOutboxAbandons)
		t.Run("a targeted message is opened, claimed, sent, reported failed and answered", attemptOfATargetedMessage)
		t.Run("a send in another mode settles a stranded attempt and opens one that is renewed, queued and answered", attemptsOfAModeChange)
		t.Run("a session accepts a direct message as its turn", attemptAccepted)
		t.Run("a broadcast opens one attempt for each copy", attemptsOfABroadcast)
		t.Run("an older build's update of a marked attempt row leaves it unmarked, and this build passes it over", attemptAnOlderBuildUpdates)
	})
	t.Run("published_destinations is the topics of the delivered_to tuples the outbox appended", publishedDestinationsProjectTheOutboxsTuples)
}

// A comment's mention, from the write that opens its attempt to the reply that answers it. The
// process is stopping when the comment commits, so the mention sender refuses the send and gives
// the attempt's claim back, and every step after that is one this case takes itself: the send the
// sweep or a lane makes, bound to the attempt, held at its resolution and then at the listener,
// then the session's report that the frame failed, then its answer.
func attemptOfAMentionFromOpenToAnswer(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A mention's attempt", "before")
	rig.stop()
	created := rig.mention(issue.Key, "Are you there?", "session:s1")
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	rig.requireOpened(key, "comment.created", graphstore.AttemptClaimedAt)
	rig.requireProjects(key, "the stopped sender gave the attempt's claim back")
	if claims := rig.history(key, graphstore.AttemptClaimedAt); len(claims) != 2 || !claims[0].Asserted || claims[1].Asserted {
		t.Fatalf("the claimed_at history of %s is %+v, want the claim the comment committed and its release", key.ID(), claims)
	}

	ctx := context.Background()
	comment, createdEvent := rig.comment(created.ID)
	releaseLookup := rig.listener.holdLookups("/v1/registry/s1")
	defer releaseLookup()
	reached, releaseSend := rig.listener.stall("s1")
	defer releaseSend()
	sent := make(chan error, 1)
	go func() {
		_, _, err := rig.direct.deliverResolvedCommentMention(ctx, comment, createdEvent,
			ResolvedMention{Target: "session:s1", Delivery: rig.mentionMode(created.ID, "session:s1")},
			comment.Author, attemptClaim{attempt: 1}, rig.direct.newTakeoverRetry())
		sent <- err
	}()
	rig.awaitAttempt(key, "taken by the send", func(row attemptRowFacts) bool {
		_, claimed := row.values[graphstore.AttemptClaimedAt]
		return claimed
	})
	rig.requireProjects(key, "the send took the attempt")
	releaseLookup()
	awaitClosed(t, reached, "the mention's send reaching the listener")
	rig.requireProjects(key, "the send recorded its resolution on the attempt")
	releaseSend()
	if err := awaitResult(t, sent, "the mention's send"); err != nil {
		t.Fatalf("send the mention: %v", err)
	}
	if row := rig.requireProjects(key, "the send settled the attempt"); row.state != model.DeliveryStateSent {
		t.Fatalf("the mention's attempt settled %s, want sent", row.state)
	}

	rig.reply("/api/v1/comments/"+created.ID+"/reply", map[string]any{"error": "the frame could not be acted on"}, http.StatusOK)
	if row := rig.requireProjects(key, "the session reported the frame failed"); row.state != model.DeliveryStateFailed {
		t.Fatalf("the attempt the session reported failed is %s, want failed", row.state)
	}
	answer := rig.reply("/api/v1/comments/"+created.ID+"/reply", map[string]any{"body": "Here."}, http.StatusCreated)
	rig.requireProjects(key, "the session answered the mention")
	requireSentAndAnswered(t, rig.database, attemptExpectation{
		key: key, table: "comment_deliveries",
		opener: "comment.created", settler: "comment.delivery", answerer: "comment.answered",
		replyKind: graphstore.KindComment, reply: replyID(t, answer.Body.Bytes()),
	})
	rig.requireEveryAttemptRowIsANode()
}

// A lane bound to a mention's attempt stands down while a Retry of the same mention is in flight,
// and the Retry's send, once it lands, settles the lane's attempt superseded. The Retry opens an
// attempt of its own because the lane holds the first one claimed, and its resolution is held
// until the lane has stood down.
func attemptsOfAStandDownAndASupersede(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A Retry overtakes a lane", "before")
	rig.stop()
	created := rig.mention(issue.Key, "Who has this?", "session:s1")
	first := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	second := graphstore.CommentAttempt(created.ID, "session:s1", 2)
	ctx := context.Background()
	comment, _ := rig.comment(created.ID)
	mode := rig.mentionMode(created.ID, "session:s1")
	target := ResolvedMention{Target: "session:s1", Delivery: mode}

	lane, err := rig.direct.claimCommentDeliveryAttempt(ctx, comment, target, attemptClaim{attempt: 1})
	if err != nil || lane.settled != nil {
		t.Fatalf("the lane's claim of attempt 1 = %+v, %v; want the attempt taken", lane.settled, err)
	}
	rig.requireProjects(first, "a lane took the attempt")

	releaseLookup := rig.listener.holdLookups("/v1/registry/s1")
	defer releaseLookup()
	retried := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		retried <- dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/comments/"+created.ID+"/deliveries",
			map[string]any{"delivery": mode}, "alice")
	}()
	rig.awaitAttempt(second, "opened by the Retry", nil)
	rig.requireOpened(second, "")
	rig.requireProjects(second, "a Retry opened an attempt of its own beside the lane's")

	stood, err := rig.direct.claimCommentDeliveryAttempt(ctx, comment, target, lane.attemptClaim)
	if err != nil || stood.settled == nil || stood.superseded {
		t.Fatalf("the lane's bound claim beside the Retry in flight = %+v, %v; want it standing down", stood.settled, err)
	}
	if row := rig.requireProjects(first, "the lane stood down for the Retry in flight"); row.state != model.DeliveryStatePending {
		t.Fatalf("the attempt the lane stood down from is %s, want pending", row.state)
	}
	releaseLookup()
	if response := awaitRecorder(t, retried, "the Retry"); response.Code != http.StatusCreated {
		t.Fatalf("the Retry: status=%d body=%s", response.Code, response.Body.String())
	}
	if row := rig.requireProjects(second, "the Retry's send settled its attempt"); row.state != model.DeliveryStateSent {
		t.Fatalf("the Retry's attempt settled %s, want sent", row.state)
	}
	row := rig.requireProjects(first, "the Retry's send superseded the attempt the lane stood down from")
	if row.state != model.DeliveryStateFailed || row.values[graphstore.AttemptError] != mentionSupersededError {
		t.Fatalf("the overtaken attempt is %s with error %q, want it failed as superseded", row.state, row.values[graphstore.AttemptError])
	}
	rig.requireEveryAttemptRowIsANode()
}

// A keyed repeat claims an owed mention before it puts it back on a lane, the mention sender
// writes that claim again while the send waits, and gives it back when the send never starts.
func attemptOfAKeyedClaim(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A keyed repeat's claim", "before")
	rig.stop()
	created := rig.mention(issue.Key, "Still there?", "session:s1")
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	ctx := context.Background()
	queued := mentionKey{comment: created.ID, target: "session:s1", attempt: 1}

	claimed, ok, err := rig.direct.claimQueuedMention(ctx, queued)
	if err != nil || !ok {
		t.Fatalf("the keyed repeat's claim = %v, %v; want it taken", ok, err)
	}
	rig.requireProjects(key, "a keyed repeat claimed the owed attempt")
	refreshed, err := rig.direct.refreshMentionClaims(ctx, []mentionClaim{{key: queued, claimedAt: claimed}})
	if err != nil || len(refreshed) != 1 || !refreshed[0].claimedAt.After(claimed) {
		t.Fatalf("the sender's refresh = %+v, %v; want the claim written again, later", refreshed, err)
	}
	rig.requireProjects(key, "the sender wrote the claim again")
	if err := rig.direct.releaseMentionClaims(ctx, refreshed); err != nil {
		t.Fatalf("give the claim back: %v", err)
	}
	if row := rig.requireProjects(key, "the sender gave the claim back"); row.values[graphstore.AttemptClaimedAt] != "" {
		t.Fatalf("the released attempt carries claimed_at %q, want none", row.values[graphstore.AttemptClaimedAt])
	}
	rig.requireEveryAttemptRowIsANode()
}

// A keyed repeat that finds its owed mention claimed by another sender takes nothing: the claim's
// batch commits at its end whatever it found (claimQueuedMention), so what holds the row as the
// other sender left it is that its update matches no row and the statement in the store finds
// nothing to state - the row holds what its node already says.
func attemptOfALostKeyedClaim(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A keyed repeat's lost claim", "before")
	rig.stop()
	created := rig.mention(issue.Key, "Who has it?", "session:s1")
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	ctx := context.Background()
	queued := mentionKey{comment: created.ID, target: "session:s1", attempt: 1}

	held, ok, err := rig.direct.claimQueuedMention(ctx, queued)
	if err != nil || !ok {
		t.Fatalf("the first sender's claim = %v, %v; want it taken", ok, err)
	}
	rig.requireProjects(key, "the first sender claimed the owed attempt")
	before := rig.tupleCount(key)
	claimed, ok, err := rig.direct.claimQueuedMention(ctx, queued)
	if err != nil || ok || !claimed.IsZero() {
		t.Fatalf("the second claim = %v, %v, %v; want it lost, with no claim and no error", claimed, ok, err)
	}
	if after := rig.tupleCount(key); after != before {
		t.Fatalf("the lost claim appended %d fact(s) to the attempt's node, want none", after-before)
	}
	var stands time.Time
	if err := rig.database.Pool.QueryRow(ctx, `
		select claimed_at from comment_deliveries where comment_id = $1 and target = $2 and attempt = 1
	`, created.ID, "session:s1").Scan(&stands); err != nil || !stands.Equal(held) {
		t.Fatalf("the attempt carries claimed_at %v (%v), want the first sender's %v", stands, err, held)
	}
	rig.requireProjects(key, "the lost claim left the first sender's")
	rig.requireEveryAttemptRowIsANode()
}

// A send the recipient's host refuses settles its attempt failed with the refusal, and a mention
// of a session the listener holds no record of opens its attempt with that resolve error and
// settles it failed. Neither attempt ever delivered anything.
func attemptsThatFail(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Failed mentions", "before")
	rig.listener.refuseSends("s2")
	for _, mention := range []struct{ target, body, step string }{
		{"session:s2", "Can you take this?", "the refused send settled the attempt"},
		{"session:ghost", "Is anyone there?", "the send of a mention nobody holds settled the attempt"},
	} {
		created := rig.mention(issue.Key, mention.body, mention.target)
		settledCommentDeliveries(t, rig.database, created.ID)
		key := graphstore.CommentAttempt(created.ID, mention.target, 1)
		var stated []string
		if mention.target == "session:ghost" {
			stated = append(stated, graphstore.AttemptResolveError)
		}
		rig.requireOpened(key, "comment.created", stated...)
		row := rig.requireProjects(key, mention.step)
		if row.state != model.DeliveryStateFailed || row.values[graphstore.AttemptError] == "" {
			t.Fatalf("%s: the attempt is %s with error %q, want it failed with its cause", mention.step, row.state, row.values[graphstore.AttemptError])
		}
		if delivered := rig.history(key, graphstore.TupleDeliveredTo); len(delivered) != 0 {
			t.Fatalf("%s: the failed attempt states deliveries %+v, want none", mention.step, delivered)
		}
	}
	rig.requireEveryAttemptRowIsANode()
}

// The delivery sweep settles failed an owed mention attempt whose comment no longer holds the
// mention, with that cause and the receipt its row owes.
func attemptTheSweepSettles(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A mention taken back", "before")
	rig.stop()
	created := rig.mention(issue.Key, "Please look.", "session:s1")
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	// The comment no longer holds its mention, and the attempt is a lease old: the sweep owes it
	// and nothing can send it.
	rig.exec(`delete from comment_mentions where comment_id = $1`, created.ID)
	rig.exec(`update comment_deliveries set created_at = created_at - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where comment_id = $1`, created.ID)
	if err := rig.direct.settleUndeliverable(context.Background()); err != nil {
		t.Fatalf("settle the undeliverable attempts: %v", err)
	}
	row := rig.requireProjects(key, "the sweep settled the attempt its comment no longer mentions")
	if row.state != model.DeliveryStateFailed || row.values[graphstore.AttemptError] != undeliverableMentionGone {
		t.Fatalf("the swept attempt is %s with error %q, want it failed as %q", row.state, row.values[graphstore.AttemptError], undeliverableMentionGone)
	}
	rig.requireCausedBy(key, graphstore.AttemptState, "comment.delivery")
	rig.requireEveryAttemptRowIsANode()
}

// The outbox settles failed a mention attempt whose send was abandoned - past the sweep's window
// and held by no sender - before it publishes the comment's copy to the session the issue is
// routed to.
func attemptTheOutboxAbandons(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "An abandoned mention", "before")
	rig.route(issue.Key, "session:s1")
	rig.stop()
	created := rig.mention(issue.Key, "For the route's session.", "session:s1")
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	rig.exec(`update comment_deliveries set created_at = now() - `+model.MentionSweepWindowInterval+` - interval '1 minute'
		where comment_id = $1`, created.ID)
	runOutboxUntilSettled(t, rig.deps, &recordingOutboxPublisher{}, issue.Key, created.ID)
	row := rig.requireProjects(key, "the outbox settled the abandoned attempt")
	if row.state != model.DeliveryStateFailed || !strings.Contains(row.values[graphstore.AttemptError], "abandoned") {
		t.Fatalf("the abandoned attempt is %s with error %q, want it failed as abandoned", row.state, row.values[graphstore.AttemptError])
	}
	rig.requireCausedBy(key, graphstore.AttemptState, "comment.delivery")
	rig.requireEveryAttemptRowIsANode()
}

// A targeted message's attempt 1, opened in the transaction that stores the message, then claimed
// and pinned by the request's own send, held at the listener, settled sent, reported failed by the
// session and answered.
func attemptOfATargetedMessage(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A targeted message", "before")
	reached, releaseSend := rig.listener.stall("s1")
	defer releaseSend()
	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
			"body": "Ping.", "target": "session:s1", "delivery": "steer",
		}, "alice")
	}()
	awaitClosed(t, reached, "the message's send reaching the listener")
	messageID := rig.messageID(issue.Key, "Ping.")
	key := graphstore.MessageAttempt(messageID, 1)
	rig.requireOpened(key, "message.created")
	rig.requireProjects(key, "the send claimed the attempt and pinned its session")
	releaseSend()
	if response := awaitRecorder(t, posted, "the targeted message"); response.Code != http.StatusCreated {
		t.Fatalf("the targeted message: status=%d body=%s", response.Code, response.Body.String())
	}
	if row := rig.requireProjects(key, "the send settled the attempt"); row.state != model.DeliveryStateSent {
		t.Fatalf("the message's attempt settled %s, want sent", row.state)
	}
	rig.reply("/api/v1/messages/"+messageID+"/reply", map[string]any{"error": "the frame could not be acted on"}, http.StatusOK)
	if row := rig.requireProjects(key, "the session reported the frame failed"); row.state != model.DeliveryStateFailed {
		t.Fatalf("the attempt the session reported failed is %s, want failed", row.state)
	}
	answer := rig.reply("/api/v1/messages/"+messageID+"/reply", map[string]any{"body": "Pong."}, http.StatusCreated)
	rig.requireProjects(key, "the session answered the message")
	requireSentAndAnswered(t, rig.database, attemptExpectation{
		key: key, table: "message_deliveries",
		opener: "message.created", settler: "message.delivery", answerer: "message.answered",
		replyKind: graphstore.KindMessage, reply: replyID(t, answer.Body.Bytes()),
	})
	rig.requireEveryAttemptRowIsANode()
}

// A targeted message whose sender never came back strands its attempt: pending, pinned to its
// session, the claim a lease old. A send in another mode settles it failed with its receipt and
// opens an attempt of its own, which renews its claim after a take-over, settles queued for a
// session that is not running, and is answered later: the answer appends answered_by and no
// second delivered_to, since a queued attempt already delivered.
func attemptsOfAModeChange(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A mode change", "before")
	releaseAnswer := rig.listener.hold("s1")
	defer releaseAnswer()
	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
			"body": "Status?", "target": "session:s1", "delivery": "steer",
		}, "alice")
	}()
	if session := rig.listener.awaitSend(t); session != "s1" {
		t.Fatalf("the message's send went to %s, want s1", session)
	}
	messageID := rig.messageID(issue.Key, "Status?")
	stranded := graphstore.MessageAttempt(messageID, 1)
	rig.requireProjects(stranded, "the send claimed the attempt")
	// The sender never answers: a minute passes and its claim lapses.
	rig.exec(`update message_deliveries set claimed_at = claimed_at - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where message_id = $1 and attempt = 1`, messageID)

	ctx := context.Background()
	message, err := rig.direct.loadMessage(ctx, rig.database.Pool, "", messageID)
	if err != nil {
		t.Fatalf("load message %s: %v", messageID, err)
	}
	author := message.Author
	resolved := rig.direct.resolveMentionTarget(ctx, "session:s1", "btw", 0)
	pending, err := rig.direct.recordPendingMessageDelivery(ctx, message, author, resolved, nil, 0)
	if err != nil || pending.attempt != 2 {
		t.Fatalf("the send in another mode claimed attempt %d, %v; want attempt 2 of its own", pending.attempt, err)
	}
	row := rig.requireProjects(stranded, "a send in another mode settled the stranded attempt")
	if row.state != model.DeliveryStateFailed || row.values[graphstore.AttemptError] != supersededByModeChangeText {
		t.Fatalf("the stranded attempt is %s with error %q, want it failed as superseded by the mode change", row.state, row.values[graphstore.AttemptError])
	}
	rig.requireCausedBy(stranded, graphstore.AttemptState, "message.delivery")
	key := graphstore.MessageAttempt(messageID, 2)
	rig.requireOpened(key, "")
	rig.requireProjects(key, "the send opened an attempt of its own")

	renewed, held, err := rig.direct.renewMessageDeliveryClaim(ctx, message, pending)
	if err != nil || !held || !renewed.claimedAt.After(pending.claimedAt) {
		t.Fatalf("the send's renewal = held %v at %v, %v; want its claim written again after %v", held, renewed.claimedAt, err, pending.claimedAt)
	}
	rig.requireProjects(key, "the send renewed its claim after a take-over")
	envelope := "queued-envelope"
	if _, err := rig.direct.completeMessageDelivery(ctx, message, author, renewed, sendOutcome{envelopeID: &envelope, queued: true}); err != nil {
		t.Fatalf("settle the send queued: %v", err)
	}
	if row := rig.requireProjects(key, "the send settled the attempt queued"); row.state != model.DeliveryStateQueued || row.deliveredTo == "" {
		t.Fatalf("the queued attempt is %s delivered to %q, want queued and delivered to s1", row.state, row.deliveredTo)
	}
	rig.reply("/api/v1/messages/"+messageID+"/reply", map[string]any{"attempt": 2, "body": "Back now."}, http.StatusCreated)
	rig.requireProjects(key, "the session answered the queued attempt")
	if delivered := rig.history(key, graphstore.TupleDeliveredTo); len(delivered) != 1 || !delivered[0].Asserted {
		t.Fatalf("the delivered_to history of the queued attempt answered later is %+v, want its one delivery", delivered)
	}
	if answered := rig.history(key, graphstore.TupleAnsweredBy); len(answered) != 1 || !answered[0].Asserted {
		t.Fatalf("the answered_by history of the queued attempt answered later is %+v, want the one answer", answered)
	}
	releaseAnswer()
	awaitRecorder(t, posted, "the stranded send")
	rig.requireEveryAttemptRowIsANode()
}

// A person's direct message, sent in a mode a session takes as its turn, is accepted by that
// session within the minute: the attempt carries the moment and what it was accepted as.
func attemptAccepted(t *testing.T) {
	rig := newAttemptRig(t)
	created := postAgentMessage(t, rig.handler, "s1", map[string]any{"body": "Take this as your turn.", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	messageID := replyID(t, created.Body.Bytes())
	key := graphstore.MessageAttempt(messageID, 1)
	rig.requireOpened(key, "message.created")
	if row := rig.requireProjects(key, "the direct message's send settled"); row.state != model.DeliveryStateSent {
		t.Fatalf("the direct message's attempt settled %s, want sent", row.state)
	}
	accepted := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+messageID+"/deliveries/1/accept", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if accepted.Code != http.StatusOK {
		t.Fatalf("the session's accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	row := rig.requireProjects(key, "the session accepted the attempt as its turn")
	if row.values[graphstore.AttemptAcceptedAs] != acceptedAsUserTurn || row.values[graphstore.AttemptAcceptedAt] == "" {
		t.Fatalf("the accepted attempt carries accepted_as %q at %q, want %q at a moment", row.values[graphstore.AttemptAcceptedAs],
			row.values[graphstore.AttemptAcceptedAt], acceptedAsUserTurn)
	}
	rig.requireCausedBy(key, graphstore.AttemptAcceptedAt, "message.accepted")
	rig.requireEveryAttemptRowIsANode()
}

// A broadcast opens each copy's attempt 1 in the transaction that writes the copies, and its
// workers send each.
func attemptsOfABroadcast(t *testing.T) {
	rig := newAttemptRig(t)
	response := postBroadcast(t, rig.handler, map[string]any{
		"body": "Report status.", "delivery": "steer", "session_ids": []string{"s2", "s3"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("the broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if len(created.Recipients) != 2 {
		t.Fatalf("the broadcast's recipients = %#v, want s2 and s3", created.Recipients)
	}
	for _, recipient := range created.Recipients {
		key := graphstore.MessageAttempt(recipient.Message.ID, 1)
		rig.requireOpened(key, "message.created")
		rig.awaitAttempt(key, "settled by its worker", func(row attemptRowFacts) bool {
			return row.state != model.DeliveryStatePending
		})
		if row := rig.requireProjects(key, "the copy's send settled"); row.state != model.DeliveryStateSent {
			t.Fatalf("the copy to %s settled %s, want sent", recipient.SessionID, row.state)
		}
	}
	rig.requireEveryAttemptRowIsANode()
}

// An older build's update of an attempt row this build marked - through a connection that lacks
// this build's marker writer setting (store.StoreMarkerWriterSetting) - leaves the row naming no
// node (migration 20261116150000), and a transition this build makes after it passes the row
// over: the node keeps what it held, and the row is answered by its old arm until the backfill
// takes it.
func attemptAnOlderBuildUpdates(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "A roll", "before")
	created := rig.mention(issue.Key, "Mid-roll.", "session:s1")
	settledCommentDeliveries(t, rig.database, created.ID)
	key := graphstore.CommentAttempt(created.ID, "session:s1", 1)
	rig.requireProjects(key, "the send settled the attempt")
	before := rig.tupleCount(key)

	older := olderBuild(t, rig.database)
	if _, err := older.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() where comment_id = $1 and target = $2 and attempt = 1
	`, created.ID, "session:s1"); err != nil {
		t.Fatalf("update the attempt as an older build: %v", err)
	}
	if row, found := rig.readAttempt(key); !found || row.nodeKind != nil || row.nodeAddress != nil {
		t.Fatalf("the attempt an older build updated is found %t and names node %v/%v, want it found naming none", found, row.nodeKind, row.nodeAddress)
	}
	rig.reply("/api/v1/comments/"+created.ID+"/reply", map[string]any{"body": "Answered mid-roll."}, http.StatusCreated)
	if _, held, err := graphstore.ReadNode(context.Background(), rig.database.Pool, graphstore.KindDeliveryAttempt, key.ID()); err != nil || !held {
		t.Fatalf("the node of the unmarked attempt: held %v, %v; want it kept, a node being history", held, err)
	}
	if after := rig.tupleCount(key); after != before {
		t.Fatalf("this build stated %d more facts of an attempt row an older build unmarked, want none", after-before)
	}
	rig.requireEveryAttemptRowIsANode(key.ID())
}

// published_destinations is the projection of the delivered_to tuples the outbox appends to a
// session or a role (outbox.deliveredGraph). The events cover every shape that projection names:
// a person's comment and a person's message, each reaching the session or the role the issue is
// routed to, which are tuples from the comment's and the message's node; the issue's own events,
// which reach those routes with no tuple since the outbox names no item of theirs; a mention's
// receipt, published to its owner topic alone, whose tuple is its delivery and no topic; a
// targeted message and its receipt, published to their owner topic alone; and every event's
// owner topic, which names no node.
func publishedDestinationsProjectTheOutboxsTuples(t *testing.T) {
	rig := newAttemptRig(t)
	issue := createInteractionIssue(t, rig.handler, "TEST", "The session half", "before")
	rig.route(issue.Key, "session:s2")
	posted := postMentionedComment(t, rig.handler, issue.Key, map[string]any{"body": "For whoever has the issue."})
	if posted.Code != http.StatusCreated {
		t.Fatalf("the routed comment: status=%d body=%s", posted.Code, posted.Body.String())
	}
	routed := replyID(t, posted.Body.Bytes())
	mentioned := rig.mention(issue.Key, "And for s1 by name.", "session:s1")
	settledCommentDeliveries(t, rig.database, mentioned.ID)
	runOutboxUntilSettled(t, rig.deps, &recordingOutboxPublisher{}, issue.Key, mentioned.ID)
	rig.route(issue.Key, "role:reviewer")
	if posted := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "For whoever holds the role.",
	}, "alice"); posted.Code != http.StatusCreated {
		t.Fatalf("the routed message: status=%d body=%s", posted.Code, posted.Body.String())
	}
	createIssueMessage(t, rig.handler, issue.Key, map[string]any{"body": "For s1 alone.", "target": "session:s1", "delivery": "steer"}, "alice")
	runOutboxUntilSettled(t, rig.deps, &recordingOutboxPublisher{}, issue.Key, mentioned.ID)
	// Each delivery written out: the routed comment and the mentioned one each reach the session
	// the issue is routed to, the mention's receipt the session its attempt reached, and the
	// message routed to the role that role; the targeted message and its receipt, and the
	// issue's own events, reach no node the outbox records.
	requirePublishedDestinationsProjectTheOutboxsTuples(t, rig.database, issue.Key,
		"comment.created comment/"+routed+" > session/s2",
		"comment.created comment/"+mentioned.ID+" > session/s2",
		"comment.delivery comment/"+mentioned.ID+" > session/s1",
		"message.created message/"+rig.messageID(issue.Key, "For whoever holds the role.")+" > role/reviewer")
}

// attemptRig is the server one attempt case drives: its handler, store and dependencies, the
// stand-in listener its sends reach, a server the case calls a delivery step on directly
// (directServer), and stop, which ends the server's lifetime so its mention sender starts
// nothing more and gives the claims of the sends it refuses back.
type attemptRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	deps     Deps
	listener *mentionListener
	direct   *server
	stop     context.CancelFunc
}

// attemptListenerWindow is the listener client's window on an attempt rig: longer than any case
// holds a lookup or a send while it reads the attempt, so a held call is answered as the case
// releases it rather than failed by the client's own timeout.
const attemptListenerWindow = 30 * time.Second

func newAttemptRig(t *testing.T) *attemptRig {
	t.Helper()
	listener, state := newMentionListener(t)
	lifetime, stop := context.WithCancel(t.Context())
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: attemptListenerWindow, lifetime: lifetime})
	return &attemptRig{t: t, handler: handler, database: database, deps: deps, listener: state, direct: directServer(deps), stop: stop}
}

// mention posts alice's comment on issue with body, mentioning target.
func (r *attemptRig) mention(issue, body, target string) mentionedCommentRead {
	r.t.Helper()
	return decodeMentionedComment(r.t, postMentionedComment(r.t, r.handler, issue, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": target}},
	}))
}

// route routes issue to target, as alice.
func (r *attemptRig) route(issue, target string) {
	r.t.Helper()
	if patched := dispatchRequest(r.t, r.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"route": target}, "alice"); patched.Code != http.StatusOK {
		r.t.Fatalf("route %s to %s: status=%d body=%s", issue, target, patched.Code, patched.Body.String())
	}
}

// reply posts s1's reply callback to path - an answer or an error report of attempt 1 unless body
// names another - and answers the response, failing unless its status is want.
func (r *attemptRig) reply(path string, body map[string]any, want int) *httptest.ResponseRecorder {
	r.t.Helper()
	input := map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1}
	for name, value := range body {
		input[name] = value
	}
	response := sessionRequest(r.t, r.handler, http.MethodPost, path, input)
	if response.Code != want {
		r.t.Fatalf("s1's reply callback to %s: status=%d body=%s, want %d", path, response.Code, response.Body.String(), want)
	}
	return response
}

// comment loads the comment id names and its comment.created event, as a sender holds them.
func (r *attemptRig) comment(id string) (model.Comment, model.Event) {
	r.t.Helper()
	ctx := context.Background()
	comment, err := r.direct.loadComment(ctx, r.database.Pool, id)
	if err != nil {
		r.t.Fatalf("load comment %s: %v", id, err)
	}
	created, err := r.direct.loadCommentCreatedEvent(ctx, r.database.Pool, comment)
	if err != nil {
		r.t.Fatalf("load the comment.created event of %s: %v", id, err)
	}
	return comment, created
}

// mentionMode is the mode comment's mention of target recorded.
func (r *attemptRig) mentionMode(comment, target string) string {
	r.t.Helper()
	var mode string
	if err := r.database.Pool.QueryRow(context.Background(),
		`select delivery from comment_mentions where comment_id = $1 and target = $2`, comment, target).Scan(&mode); err != nil {
		r.t.Fatalf("read the mode of %s's mention of %s: %v", comment, target, err)
	}
	return mode
}

// messageID is the id of the message on issue whose body is body.
func (r *attemptRig) messageID(issue, body string) string {
	r.t.Helper()
	var id string
	if err := r.database.Pool.QueryRow(context.Background(),
		`select id::text from messages where issue_key = $1 and body = $2`, issue, body).Scan(&id); err != nil {
		r.t.Fatalf("read the message %q on %s: %v", body, issue, err)
	}
	return id
}

func (r *attemptRig) exec(statement string, args ...any) {
	r.t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), statement, args...); err != nil {
		r.t.Fatalf("exec %s: %v", statement, err)
	}
}

// history is every tuple of kind out of key's node, in the order they were written.
func (r *attemptRig) history(key graphstore.AttemptKey, kind string) []graphstore.Tuple {
	r.t.Helper()
	history, err := graphstore.History(context.Background(), r.database.Pool, graphstore.KindDeliveryAttempt, key.ID(), kind)
	if err != nil {
		r.t.Fatalf("read the %s history of %s: %v", kind, key.ID(), err)
	}
	return history
}

// tupleCount is how many tuples out of key's node the store holds.
func (r *attemptRig) tupleCount(key graphstore.AttemptKey) int {
	r.t.Helper()
	var count int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select count(*) from graph_tuples where from_kind = $1 and from_id = $2`, graphstore.KindDeliveryAttempt, key.ID()).Scan(&count); err != nil {
		r.t.Fatalf("count the facts of %s: %v", key.ID(), err)
	}
	return count
}

// requireCausedBy fails unless the newest tuple of kind out of key's node is caused by an event of
// type eventType: the transition that stated it appended that event beside the row.
func (r *attemptRig) requireCausedBy(key graphstore.AttemptKey, kind, eventType string) {
	r.t.Helper()
	history := r.history(key, kind)
	if len(history) == 0 {
		r.t.Fatalf("%s states no %s", key.ID(), kind)
	}
	newest := history[len(history)-1]
	var got string
	if err := r.database.Pool.QueryRow(context.Background(), `select coalesce((select type from events where id = $1), '')`, newest.SourceEvent).Scan(&got); err != nil || got != eventType {
		r.t.Fatalf("the newest %s of %s is caused by event %d (%q, %v), want the %s its transition appended", kind, key.ID(), newest.SourceEvent, got, err, eventType)
	}
}

// attemptValues is every value an attempt's node carries, by the store name of its attribute.
var attemptValues = []string{
	graphstore.AttemptNumber, graphstore.AttemptState, graphstore.AttemptEnvelopeID, graphstore.AttemptError,
	graphstore.AttemptResolveError, graphstore.AttemptDuplicate, graphstore.AttemptClaimedAt,
	graphstore.AttemptAcceptedAt, graphstore.AttemptAcceptedAs,
}

// attemptRowFacts is what one attempt row says its node holds, read by this test from the row's
// own columns as spec R5 words it: the values; owed_to the target while the send is undecided;
// delivered_to the session once the listener took the attempt, sent or queued; answered_by the
// reply it drew. Each relation is written kind/id.
type attemptRowFacts struct {
	nodeKind, nodeAddress *string
	state                 string
	values                map[string]string
	owedTo, answeredBy    []string
	deliveredTo, session  string
}

// readAttempt reads key's row, and whether it exists.
func (r *attemptRig) readAttempt(key graphstore.AttemptKey) (attemptRowFacts, bool) {
	r.t.Helper()
	return readAttemptRow(r.t, r.database, key)
}

func readAttemptRow(t *testing.T, database *store.Store, key graphstore.AttemptKey) (attemptRowFacts, bool) {
	t.Helper()
	var row attemptRowFacts
	var number int
	var envelope, failure, resolveError, acceptedAs, session, reply *string
	var duplicate bool
	var claimed, accepted *time.Time
	var target, replyKind string
	var err error
	if key.Message != "" {
		replyKind = graphstore.KindMessage
		err = database.Pool.QueryRow(context.Background(), `
			select d.node_kind, d.node_address, d.attempt, d.state, d.envelope_id, d.error, d.duplicate, d.claimed_at,
			       d.accepted_at, d.accepted_as, coalesce(m.target, ''), nullif(d.session_id, ''), d.reply_id::text
			from message_deliveries d join messages m on m.id = d.message_id
			where d.message_id = $1::uuid and d.attempt = $2
		`, key.Message, key.Attempt).Scan(&row.nodeKind, &row.nodeAddress, &number, &row.state, &envelope, &failure,
			&duplicate, &claimed, &accepted, &acceptedAs, &target, &session, &reply)
	} else {
		replyKind = graphstore.KindComment
		target = key.Target
		err = database.Pool.QueryRow(context.Background(), `
			select node_kind, node_address, attempt, state, envelope_id, error, resolve_error, duplicate, claimed_at,
			       nullif(session_id, ''), reply_id::text
			from comment_deliveries
			where comment_id = $1::uuid and target = $2 and attempt = $3
		`, key.Comment, key.Target, key.Attempt).Scan(&row.nodeKind, &row.nodeAddress, &number, &row.state, &envelope,
			&failure, &resolveError, &duplicate, &claimed, &session, &reply)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return attemptRowFacts{}, false
	}
	if err != nil {
		t.Fatalf("read the attempt row of %s: %v", key.ID(), err)
	}
	row.values = map[string]string{
		graphstore.AttemptNumber: strconv.Itoa(number), graphstore.AttemptState: row.state,
		graphstore.AttemptDuplicate: strconv.FormatBool(duplicate),
	}
	for kind, value := range map[string]*string{
		graphstore.AttemptEnvelopeID: envelope, graphstore.AttemptError: failure,
		graphstore.AttemptResolveError: resolveError, graphstore.AttemptAcceptedAs: acceptedAs,
	} {
		if value != nil {
			row.values[kind] = *value
		}
	}
	for kind, moment := range map[string]*time.Time{graphstore.AttemptClaimedAt: claimed, graphstore.AttemptAcceptedAt: accepted} {
		if moment != nil {
			row.values[kind] = moment.UTC().Format(time.RFC3339Nano)
		}
	}
	if row.state == model.DeliveryStatePending {
		if route, err := model.ParseRoute(target); err == nil {
			row.owedTo = []string{route.Kind + "/" + route.ID}
		}
	}
	if session != nil {
		row.session = *session
		if model.DeliveryTaken(row.state) {
			row.deliveredTo = graphstore.KindSession + "/" + *session
		}
	}
	if reply != nil {
		row.answeredBy = []string{replyKind + "/" + *reply}
	}
	return row, true
}

// attemptDifferences is every way the node of key differs from what row says it holds: empty
// where the node is the row's projection. A delivery the node states stays stated whatever the
// row says later - an attempt the session reports failed after it was sent delivered all the same
// - so a row the listener has not taken may still have its node delivered to its own session.
func attemptDifferences(t *testing.T, database *store.Store, key graphstore.AttemptKey, row attemptRowFacts) []string {
	t.Helper()
	ctx := context.Background()
	var differences []string
	for _, kind := range attemptValues {
		want, carried := row.values[kind]
		got, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindDeliveryAttempt, key.ID(), kind)
		if err != nil {
			t.Fatalf("read %s of %s: %v", kind, key.ID(), err)
		}
		switch {
		case carried && (!held || got != want):
			differences = append(differences, fmt.Sprintf("its %s is %q (held %v), and its row's %q", kind, got, held, want))
		case !carried && held:
			differences = append(differences, fmt.Sprintf("its %s is %q, and its row holds none", kind, got))
		}
	}
	current, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindDeliveryAttempt, key.ID(),
		[]string{graphstore.TupleOwedTo, graphstore.TupleDeliveredTo, graphstore.TupleAnsweredBy})
	if err != nil {
		t.Fatalf("read the relations of %s: %v", key.ID(), err)
	}
	relations := map[string][]string{}
	for _, tuple := range current {
		relations[tuple.Kind] = append(relations[tuple.Kind], tuple.ToKind+"/"+tuple.ToID)
	}
	compare := func(kind string, want []string) {
		if got := relations[kind]; !slices.Equal(got, want) {
			differences = append(differences, fmt.Sprintf("it is %s %v, and its row says %v", kind, got, want))
		}
	}
	compare(graphstore.TupleOwedTo, row.owedTo)
	compare(graphstore.TupleAnsweredBy, row.answeredBy)
	delivered := relations[graphstore.TupleDeliveredTo]
	switch {
	case row.deliveredTo != "":
		compare(graphstore.TupleDeliveredTo, []string{row.deliveredTo})
	case len(delivered) > 1 || (len(delivered) == 1 && delivered[0] != graphstore.KindSession+"/"+row.session):
		differences = append(differences, fmt.Sprintf("it is delivered_to %v, and its row names session %q", delivered, row.session))
	}
	return differences
}

// requireProjects fails unless key's row names its node and that node holds what the row says,
// read after step; it answers the row.
func (r *attemptRig) requireProjects(key graphstore.AttemptKey, step string) attemptRowFacts {
	r.t.Helper()
	row, found := r.readAttempt(key)
	if !found {
		r.t.Fatalf("after %s, attempt %s has no row", step, key.ID())
	}
	if row.nodeKind == nil || *row.nodeKind != graphstore.KindDeliveryAttempt || row.nodeAddress == nil || *row.nodeAddress != key.ID() {
		r.t.Fatalf("after %s, the row of %s names node %v/%v, want %s/%s", step, key.ID(), row.nodeKind, row.nodeAddress, graphstore.KindDeliveryAttempt, key.ID())
	}
	if differences := attemptDifferences(r.t, r.database, key, row); len(differences) > 0 {
		r.t.Fatalf("after %s, the node of %s is not its row's projection:\n  %s", step, key.ID(), strings.Join(differences, "\n  "))
	}
	return row
}

// requireOpened holds the node of key to the write that opened its attempt: one delivery_attempt
// node, project-visible and live, which the row names, caused by the event of type opener that
// write appended ("" for an opening the delivery machinery makes, which appends none and writes
// as graphstore.AttemptWriter), and whose first number, state, duplicate, owed_to and each kind
// of stated were written by that write - by its writer, at the moment the node was made and under
// its cause - and not by a transition after it.
func (r *attemptRig) requireOpened(key graphstore.AttemptKey, opener string, stated ...string) {
	r.t.Helper()
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, r.database.Pool, graphstore.KindDeliveryAttempt, key.ID())
	if err != nil || !held {
		r.t.Fatalf("read the node of %s: held %v, %v", key.ID(), held, err)
	}
	if node.Supertag != graphstore.KindDeliveryAttempt || node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive {
		r.t.Fatalf("the node of %s is supertag %s, %s and %s, want delivery_attempt, project and live", key.ID(), node.Supertag, node.Visibility, node.State)
	}
	if opener == "" {
		if node.SourceEvent != graphstore.NoEvent || !node.MadeBy.SameAs(graphstore.AttemptWriter) {
			r.t.Fatalf("the node of %s is made by %+v under event %d, want the delivery machinery's, under no event", key.ID(), node.MadeBy, node.SourceEvent)
		}
	} else {
		var got string
		if err := r.database.Pool.QueryRow(ctx, `select type from events where id = $1`, node.SourceEvent).Scan(&got); err != nil || got != opener {
			r.t.Fatalf("the node of %s is caused by event %d (%s, %v), want the %s event of the write that opened it", key.ID(), node.SourceEvent, got, err, opener)
		}
	}
	for _, kind := range append([]string{graphstore.AttemptNumber, graphstore.AttemptState, graphstore.AttemptDuplicate, graphstore.TupleOwedTo}, stated...) {
		history := r.history(key, kind)
		if len(history) == 0 {
			r.t.Fatalf("the node of %s states no %s", key.ID(), kind)
		}
		first := history[0]
		if !first.Asserted || !first.At.Equal(node.MadeAt) || !first.Who.SameAs(node.MadeBy) || first.SourceEvent != node.SourceEvent {
			r.t.Fatalf("the first %s of %s is %+v, want it stated by the write that opened the attempt: %+v at %v under event %d",
				kind, key.ID(), first, node.MadeBy, node.MadeAt, node.SourceEvent)
		}
	}
}

// awaitAttempt reads key's row until it exists and ready, when given, accepts it, for at most ten
// seconds; what says what it waits for.
func (r *attemptRig) awaitAttempt(key graphstore.AttemptKey, what string, ready func(attemptRowFacts) bool) attemptRowFacts {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, found := r.readAttempt(key)
		if found && (ready == nil || ready(row)) {
			return row
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("attempt %s is %+v (row found %v), want it %s", key.ID(), row, found, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireEveryAttemptRowIsANode holds the two tables to the attempt nodes row for row: every row
// names its node, but the rows unmarked names, which an older build's update left naming none;
// every node of kind delivery_attempt is one row's, a node an unmarked row once named among them;
// each row's node is its projection; and every value an attempt's node states is under the store
// name an attribute node of the delivery_attempt supertag carries.
func (r *attemptRig) requireEveryAttemptRowIsANode(unmarked ...string) {
	r.t.Helper()
	ctx := context.Background()
	rows, err := r.database.Pool.Query(ctx, `
		select comment_id::text, target, '', attempt from comment_deliveries
		union all
		select '', '', message_id::text, attempt from message_deliveries
	`)
	if err != nil {
		r.t.Fatalf("read the attempt rows: %v", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.AttemptKey, error) {
		var key graphstore.AttemptKey
		return key, row.Scan(&key.Comment, &key.Target, &key.Message, &key.Attempt)
	})
	if err != nil {
		r.t.Fatalf("read the attempt rows: %v", err)
	}
	nodes, err := graphstore.NodesOfKind(ctx, r.database.Pool, graphstore.KindDeliveryAttempt)
	if err != nil {
		r.t.Fatalf("read the delivery_attempt nodes: %v", err)
	}
	ids := map[string]bool{}
	for _, node := range nodes {
		ids[node.ID] = true
	}
	for _, key := range keys {
		if slices.Contains(unmarked, key.ID()) {
			if row, found := r.readAttempt(key); !found || row.nodeKind != nil {
				r.t.Fatalf("the attempt row of %s an older build updated is found %t and names node %v, want it found naming none", key.ID(), found, row.nodeKind)
			}
			continue
		}
		r.requireProjects(key, "every write of the case")
		if !ids[key.ID()] {
			r.t.Fatalf("no delivery_attempt node %s answers its row", key.ID())
		}
	}
	if len(nodes) != len(keys) {
		r.t.Fatalf("the store holds %d delivery_attempt nodes for %d attempt rows, want one each", len(nodes), len(keys))
	}

	storeNames := map[string]bool{}
	attributes, err := graphstore.NodesOfKind(ctx, r.database.Pool, schemanodes.KindAttribute)
	if err != nil {
		r.t.Fatalf("read the attribute nodes: %v", err)
	}
	for _, attribute := range attributes {
		if name, _ := attribute.Attributes["store_name"].(string); name != "" && strings.HasPrefix(attribute.ID, graphstore.KindDeliveryAttempt+"/") {
			storeNames[name] = true
		}
	}
	kinds, err := readStrings(ctx, r.database.Pool, `
		select distinct kind from graph_tuples
		where from_kind = $1 and to_kind = $2 and kind <> all($3)
	`, graphstore.KindDeliveryAttempt, graphstore.KindValue, graphstoretest.BaseAttributes())
	if err != nil {
		r.t.Fatalf("read the value kinds of the attempts: %v", err)
	}
	for _, kind := range kinds {
		if !storeNames[kind] {
			r.t.Errorf("an attempt states a value of kind %q, which no attribute node of delivery_attempt stores its values under", kind)
		}
	}
}

// attemptExpectation is one attempt the test reads back: its key, its table, the event types of
// the write that opened it, of the settle that recorded it sent and of the reply that answered
// it, and the reply node that answered it.
type attemptExpectation struct {
	key                       graphstore.AttemptKey
	table                     string
	opener, settler, answerer string
	replyKind, reply          string
}

// replyID is the id of the record a route answered with.
func replyID(t *testing.T, body []byte) string {
	t.Helper()
	var read struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &read); err != nil || read.ID == "" {
		t.Fatalf("read the id the route answered: %v: %s", err, body)
	}
	return read.ID
}

// requireSentAndAnswered holds one sent and answered attempt to its node and its tuples: it was
// owed to the session while pending and no longer, delivered to it once though the settle, the
// error report and the reply each recorded the row again, and answered by the reply; the delivery
// is caused by the settle's receipt and the answer by the reply's own event, each about the
// attempt's item.
func requireSentAndAnswered(t *testing.T, database *store.Store, want attemptExpectation) {
	t.Helper()
	ctx := context.Background()
	id := want.key.ID()
	var state string
	where, args := `message_id = $1::uuid and attempt = $2`, []any{want.key.Message, want.key.Attempt}
	if want.key.Message == "" {
		where, args = `comment_id = $1::uuid and target = $3 and attempt = $2`, []any{want.key.Comment, want.key.Attempt, want.key.Target}
	}
	if err := database.Pool.QueryRow(ctx, `select state from `+want.table+` where `+where, args...).Scan(&state); err != nil || state != model.DeliveryStateSent {
		t.Fatalf("the attempt row of %s is %q (%v), want sent", id, state, err)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindDeliveryAttempt, id)
	if err != nil || !held {
		t.Fatalf("read the node of %s: held %v, %v", id, held, err)
	}
	var opener string
	if err := database.Pool.QueryRow(ctx, `select type from events where id = $1`, node.SourceEvent).Scan(&opener); err != nil || opener != want.opener {
		t.Errorf("the node of %s is caused by event %d (%s, %v), want the %s event of the write that opened it", id, node.SourceEvent, opener, err, want.opener)
	}
	owed, err := graphstore.History(ctx, database.Pool, graphstore.KindDeliveryAttempt, id, graphstore.TupleOwedTo)
	if err != nil {
		t.Fatalf("read the owed_to history of %s: %v", id, err)
	}
	if len(owed) != 2 || !owed[0].Asserted || owed[1].Asserted || owed[0].ToKind != graphstore.KindSession || owed[0].ToID != "s1" {
		t.Errorf("the owed_to history of %s is %+v, want owed to session s1 and then retracted", id, owed)
	}
	if len(owed) > 0 && owed[0].SourceEvent != node.SourceEvent {
		t.Errorf("the first owed_to of %s is caused by event %d, want the opening write's %d", id, owed[0].SourceEvent, node.SourceEvent)
	}
	delivered, err := graphstore.History(ctx, database.Pool, graphstore.KindDeliveryAttempt, id, graphstore.TupleDeliveredTo)
	if err != nil {
		t.Fatalf("read the delivered_to history of %s: %v", id, err)
	}
	if len(delivered) != 1 || !delivered[0].Asserted || delivered[0].ToKind != graphstore.KindSession || delivered[0].ToID != "s1" {
		t.Errorf("the delivered_to history of %s is %+v, want one delivery to session s1", id, delivered)
	}
	answered, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindDeliveryAttempt, id, []string{graphstore.TupleAnsweredBy})
	if err != nil {
		t.Fatalf("read what answered %s: %v", id, err)
	}
	if len(answered) != 1 || answered[0].ToKind != want.replyKind || answered[0].ToID != want.reply {
		t.Errorf("%s is answered by %+v, want the reply %s/%s alone", id, answered, want.replyKind, want.reply)
	}
	item := want.key.Comment
	if want.key.Message != "" {
		item = want.key.Message
	}
	for kind, eventType := range map[string]string{graphstore.TupleDeliveredTo: want.settler, graphstore.TupleAnsweredBy: want.answerer} {
		var source int64
		var got, payload string
		if err := database.Pool.QueryRow(ctx, `
			select t.source_event, coalesce(e.type, ''), coalesce(e.payload::text, '')
			from graph_tuples t left join events e on e.id = t.source_event
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.asserted
		`, graphstore.KindDeliveryAttempt, id, kind).Scan(&source, &got, &payload); err != nil {
			t.Fatalf("read the cause of the %s of %s: %v", kind, id, err)
		}
		if got != eventType || !strings.Contains(payload, item) {
			t.Errorf("the %s of %s is caused by event %d (%q, payload %s), want the %s event its transition appended for %s",
				kind, id, source, got, payload, eventType, item)
		}
	}
}

// outboxWriter is who the outbox's record of a delivery is written by (outbox's graphWriter).
const outboxWriter = "dispatch-outbox"

// requirePublishedDestinationsProjectTheOutboxsTuples holds every published event of issueKey to
// the projection outbox.deliveredGraph states. The delivered_to tuples the outbox appended under
// the issue's events are exactly want, each "<event type> <from kind>/<from id> > <to kind>/<to id>",
// written out by the caller. Of every event about a comment or a message but a comment.delivery
// receipt, the session and role topics of its published_destinations are exactly the far ends of
// the tuples under it, each once; a receipt is published to its owner topic alone, its one tuple
// being its attempt's delivery, which comment_deliveries projects; an event about anything else
// appends no tuple (want names none), whatever sessions and roles it reached, and the run must
// hold one that reached a route, so it proves something of them. Each event is published to its
// one owner topic, which no tuple projects.
func requirePublishedDestinationsProjectTheOutboxsTuples(t *testing.T, database *store.Store, issueKey string, want ...string) {
	t.Helper()
	ctx := context.Background()
	reached := map[int64][]string{}
	var got []string
	rows, err := database.Pool.Query(ctx, `
		select t.source_event, e.type, t.from_kind || '/' || t.from_id, t.to_kind || '/' || t.to_id
		from graph_tuples t join events e on e.id = t.source_event
		where t.kind = $1 and t.who ->> 'id' = $2 and e.issue_key = $3
	`, graphstore.TupleDeliveredTo, outboxWriter, issueKey)
	if err != nil {
		t.Fatalf("read the outbox's deliveries: %v", err)
	}
	type delivery struct {
		event               int64
		eventType, from, to string
	}
	deliveries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
		var read delivery
		return read, row.Scan(&read.event, &read.eventType, &read.from, &read.to)
	})
	if err != nil {
		t.Fatalf("read the outbox's deliveries: %v", err)
	}
	for _, read := range deliveries {
		got = append(got, read.eventType+" "+read.from+" > "+read.to)
		reached[read.event] = append(reached[read.event], topicOfNode(t, read.to))
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the outbox's deliveries under %s's events are\n  %s\nwant\n  %s", issueKey, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	var fallbacks int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from comment_fallback_copies f join comments c on c.id = f.comment_id where c.issue_key = $1
	`, issueKey).Scan(&fallbacks); err != nil || fallbacks != 0 {
		t.Fatalf("the issue holds %d fallback copies (%v), want none: a fallback copy's delivery is its own row's, and this run makes none", fallbacks, err)
	}

	events, err := database.Pool.Query(ctx, `
		select id, type, published_destinations, published_at is not null
		from events where issue_key = $1 order by id
	`, issueKey)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issueKey, err)
	}
	defer events.Close()
	routed := 0
	for events.Next() {
		var id int64
		var eventType string
		var destinations []string
		var published bool
		if err := events.Scan(&id, &eventType, &destinations, &published); err != nil {
			t.Fatalf("read an event of %s: %v", issueKey, err)
		}
		if !published {
			t.Fatalf("event %d (%s) is unpublished after the outbox settled", id, eventType)
		}
		var owners, topics []string
		for _, destination := range destinations {
			if strings.HasPrefix(destination, contracts.AgentTopicPrefix) || strings.HasPrefix(destination, contracts.RoleTopicPrefix) {
				topics = append(topics, destination)
				continue
			}
			owners = append(owners, destination)
		}
		if len(owners) != 1 {
			t.Errorf("event %d (%s) was published to owner topics %v, want its one", id, eventType, owners)
		}
		delivered := reached[id]
		slices.Sort(delivered)
		slices.Sort(topics)
		switch {
		case eventType == "comment.delivery":
			if len(topics) != 0 {
				t.Errorf("receipt %d was published to %v, want its owner topic alone", id, topics)
			}
		case strings.HasPrefix(eventType, "comment.") || strings.HasPrefix(eventType, "message."):
			if !slices.Equal(delivered, topics) {
				t.Errorf("event %d (%s) was published to %v, and the outbox's deliveries under it reach %v: published_destinations is not their projection",
					id, eventType, topics, delivered)
			}
		case len(topics) > 0:
			routed++
		}
	}
	if err := events.Err(); err != nil {
		t.Fatalf("read the events of %s: %v", issueKey, err)
	}
	if routed == 0 {
		t.Errorf("the run held no event about something other than a comment or a message that reached a route, so it proves nothing of one")
	}
}

// topicOfNode is the Envoy topic the session or role node kind/id is reached on.
func topicOfNode(t *testing.T, node string) string {
	t.Helper()
	kind, id, _ := strings.Cut(node, "/")
	switch kind {
	case graphstore.KindSession:
		return contracts.LegacyAgentSubject(id)
	case graphstore.KindRole:
		return contracts.RoleTopicPrefix + id
	}
	t.Errorf("a delivery the outbox recorded reaches %s, which is no session and no role", node)
	return node
}

// awaitClosed waits up to ten seconds for done to close; what names it in the failure.
func awaitClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen within 10 s", what)
	}
}

// awaitResult waits up to ten seconds for what's result.
func awaitResult(t *testing.T, results <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return within 10 s", what)
		return nil
	}
}

// awaitRecorder waits up to ten seconds for what's response.
func awaitRecorder(t *testing.T, responses <-chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not answer within 10 s", what)
		return nil
	}
}
