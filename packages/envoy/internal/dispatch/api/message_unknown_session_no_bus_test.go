package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A deployment with no bus and no test hooks sets no AgentPublisher, so Dispatch has nowhere to
// queue a targeted message for a session no store of the listener's holds. The message is still
// stored, and its attempt settles failed with the listener's "no live session <id>". The Inbox's
// compose dialog reads this outcome back from GET /api/v1/messages/{id} after a send to one
// session, and draws it as failed with that error.
func TestATargetedMessageToAnUnknownSessionWithNoBusSettlesFailed(t *testing.T) {
	// The listener holds no record of s1: its registry read answers 404 "no live session s1".
	live := false
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, noAgentPublisher: true})

	const body = "Pick up the release notes when you are back."
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": body, "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("a direct message to a session no store holds: status=%d body=%s", created.Code, created.Body.String())
	}
	id := decodeBody[model.Message](t, created).ID

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/messages/%s: status=%d body=%s", id, read.Code, read.Body.String())
	}
	thread := decodeBody[messageRead](t, read)
	if thread.Message.ID != id || thread.Message.Body != body {
		t.Fatalf("the read holds message %s with body %q, want the stored message %s with body %q",
			thread.Message.ID, thread.Message.Body, id, body)
	}
	attempts := thread.Message.Deliveries
	if len(attempts) != 1 {
		t.Fatalf("the read holds %d attempts (%#v), want one", len(attempts), attempts)
	}
	attempt := attempts[0]
	errorText := "<none>"
	if attempt.Error != nil {
		errorText = *attempt.Error
	}
	if attempt.State != "failed" {
		t.Fatalf("the attempt's state is %q with error %q, want failed", attempt.State, errorText)
	}
	if !strings.HasPrefix(errorText, "no live session") || !strings.Contains(errorText, "s1") {
		t.Fatalf("the failed attempt's error is %q, want one that starts with \"no live session\" and names s1", errorText)
	}
}
