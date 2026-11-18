package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// failingSendListener is an Envoy listener whose registry holds each of sessions live and whose
// send falls over: a resolution finds the session, the attempt is committed pending and pinned
// to it, and the send then leaves it `failed`. That is the attempt the answer below flips to
// sent, and it is the one shape in which an attempt carries a session the store holds no node
// of.
func failingSendListener(t *testing.T, sessions ...string) string {
	t.Helper()
	rows := make([]map[string]any, 0, len(sessions))
	known := map[string]map[string]any{}
	for _, id := range sessions {
		row := liveRow(id, fakeSession{title: id, capabilities: queuedModes})
		rows = append(rows, row)
		known[id] = row
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			row, holds := known[strings.TrimPrefix(r.URL.Path, "/v1/registry/")]
			if !holds {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session","state":"gone"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(row)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"the listener fell over before it took the message"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// An attempt recorded failed and then answered by the session it was pinned to is a delivery
// from that answer on: the reply flips the row to sent (replyComment, replyMessage), and an
// attempt that reads sent is one the listener took, which the store owes that session a node
// for (R14, graphcensus's recordedSessions). Both replies record it the one way
// (recordAnsweredDelivery): the node under the event that records the answer, and, where this
// flip is what makes the attempt a delivery, the delivered_to tuple of what it carried - a
// comment, the one thing this child gives a node, and nothing for a message, which
// dispatch://INFRA-1152 owns.
//
// Without that record the flip left the census holding the store to a node nobody wrote, with
// no later write that could ever close it.
func TestAFailedAttemptTheSessionAnswersRecordsTheSessionItReached(t *testing.T) {
	ctx := context.Background()
	const mentioned, targeted = "answers-a-failed-mention", "answers-a-failed-message"
	handler, database := newTargetedMessageHandler(t, failingSendListener(t, mentioned, targeted))
	issue := createInteractionIssue(t, handler, "ANS", "An answer after a failed attempt", "before")

	// The mention: its send falls over, so the attempt is left failed, pinned to the session
	// the resolution found, and no settle has recorded that session.
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Look at this when you can.",
		"mentions": []map[string]string{{"target": "session:" + mentioned}},
	}))
	attempts := settledCommentDeliveries(t, database, comment.ID)
	if len(attempts) != 1 || attempts[0].State != model.DeliveryStateFailed ||
		attempts[0].SessionID == nil || *attempts[0].SessionID != mentioned {
		t.Fatalf("the mention's settled attempts = %#v, want one failed attempt pinned to %s", attempts, mentioned)
	}
	requireNoSessionNode(t, database, mentioned, "an attempt the listener never took")

	// The targeted message: the same, and a session this child gives no message node to deliver.
	created := postAgentMessage(t, handler, targeted, map[string]any{
		"body": "Status?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to %s: status=%d body=%s", targeted, created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if sent := messageAttempts(t, database, message.ID); len(sent) != 1 ||
		sent[0].State != model.DeliveryStateFailed || sent[0].SessionID != targeted {
		t.Fatalf("the message's attempts = %#v, want one failed attempt pinned to %s", sent, targeted)
	}
	requireNoSessionNode(t, database, targeted, "an attempt the listener never took")

	// Both sessions answer when they are back.
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": mentioned}, "attempt": 1, "body": "Looked, and it holds.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the session's answer to its failed mention: status=%d body=%s", reply.Code, reply.Body.String())
	}
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": targeted}, "attempt": 1, "body": "Back, and green.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session's answer to its failed message: status=%d body=%s", answered.Code, answered.Body.String())
	}

	// Each attempt now reads sent, each session is a node, and the comment the mention carried
	// reached its session. The message carries no such tuple: this child writes no message node.
	if settled := settledCommentDeliveries(t, database, comment.ID); len(settled) != 1 || settled[0].State != model.DeliveryStateSent {
		t.Fatalf("the mention's attempts after the answer = %#v, want the one attempt flipped to sent", settled)
	}
	if settled := messageAttempts(t, database, message.ID); len(settled) != 1 || settled[0].State != model.DeliveryStateSent {
		t.Fatalf("the message's attempts after the answer = %#v, want the one attempt flipped to sent", settled)
	}
	for _, session := range []string{mentioned, targeted} {
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindSession, session); err != nil || !held {
			t.Errorf("the store holds a node of %s: %t (err %v), want one from the answer on", session, held, err)
		}
	}
	delivered, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, comment.ID,
		[]string{graphstore.TupleDeliveredTo})
	if err != nil {
		t.Fatalf("read what the mentioned comment was delivered to: %v", err)
	}
	if len(delivered) != 1 || delivered[0].ToKind != graphstore.KindSession || delivered[0].ToID != mentioned {
		t.Errorf("the comment states %+v as delivered, want one delivered_to the session that answered it", delivered)
	}
	requireStoreCensusZero(t, database, "after a failed attempt was answered by its session")
}

// requireNoSessionNode fails unless the store holds no node of session, which is what makes the
// answer below the write that records it.
func requireNoSessionNode(t *testing.T, database *store.Store, session, why string) {
	t.Helper()
	if _, held, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindSession, session); err != nil || held {
		t.Fatalf("the store holds a node of %s before the answer: %t (err %v), want none for %s", session, held, err, why)
	}
}
