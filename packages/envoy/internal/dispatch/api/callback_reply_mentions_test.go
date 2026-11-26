package api

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// R5 covers a callback reply: a session's reply to the mention that reached it (POST
// /api/v1/comments/{id}/reply, comment.answered) whose text names another session delivers that
// reply to the session once, even where nothing else reaches it. The reply opens no attempt, so the
// outbox's copy to each session the reply's mentions tuples name (outbox's readsMentions,
// mentionDeliveries.mentioned) is that delivery, and the outbox's record states it as the reply's
// delivered_to under its comment.answered (deliveredGraph). The replier itself is the event's
// actor and is sent nothing. Once only: a reply that names s2 twice, in a thread s2 takes part in
// already, still reaches it by one copy.
func TestACallbackReplyReachesEachSessionItsTextNamesOnce(t *testing.T) {
	t.Parallel()
	listener, _ := newMentionListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	issue := createInteractionIssue(t, handler, "CBRM", "A callback reply names a session", "before")
	for _, row := range []struct {
		name, body   string
		participates bool
	}{
		{"a reply that names s2, which nothing else reaches", "Over to [@Reviewer](dispatch://agent/s2).", false},
		{"a reply that names s2 twice, in a thread s2 takes part in", "Over to [@Reviewer](dispatch://agent/s2); [@Reviewer](dispatch://agent/s2), this is yours.", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			asked := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
				"body": "Does this ship, s1?", "mentions": []map[string]any{{"target": "session:s1"}},
			}))
			if settled := settledCommentDeliveries(t, database, asked.ID); len(settled) != 1 || settled[0].State != model.DeliveryStateSent {
				t.Fatalf("the mention of s1 = %#v, want its one attempt sent", settled)
			}
			if row.participates {
				if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "I am watching this.", "reply_to": asked.ID, "actor": map[string]any{"kind": "session", "id": "s2"},
				}); response.Code != http.StatusCreated {
					t.Fatalf("s2's reply in the thread: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			replied := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+asked.ID+"/reply", map[string]any{
				"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": row.body,
			})
			if replied.Code != http.StatusCreated {
				t.Fatalf("s1's callback reply: status=%d body=%s", replied.Code, replied.Body.String())
			}
			reply := decodeBody[model.Comment](t, replied).ID
			runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue.Key, asked.ID)
			if causes := meInboxSessionDeliveries(t, database, reply, "s2"); !slices.Equal(causes, []string{"comment.answered"}) {
				t.Errorf("s1's reply reached s2 under %v, want its comment.answered once", causes)
			}
			destinations := publishedDestinationsOf(t, database, "comment.answered", reply)
			sent := 0
			for _, destination := range destinations {
				if destination == contracts.LegacyAgentSubject("s2") {
					sent++
				}
			}
			if sent != 1 {
				t.Errorf("s1's comment.answered was published to %v, holding s2's own topic %d times, want once", destinations, sent)
			}
			if causes := meInboxSessionDeliveries(t, database, reply, "s1"); len(causes) != 0 {
				t.Errorf("s1's own reply reached s1 under %v, want nothing", causes)
			}
		})
	}
}
