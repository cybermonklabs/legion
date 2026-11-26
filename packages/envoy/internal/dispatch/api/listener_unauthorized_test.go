package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A listener that refuses Dispatch's credential (a bad or rotated token) answers 401 to every
// request. That is the listener's answer, not a take-over: no successor will accept the same
// token a few seconds later, so the send settles failed on its first try instead of retrying the
// registry read for the whole take-over window. Dispatch here presents its identity token, which
// its client reads once more and presents again after a 401, as the kubelet may have rotated it
// (envoy.Client.do): one try, two requests. The server runs on a fake clock this test never moves,
// so a send that waited out any part of a take-over would never answer at all.
func TestAListenerRefusingTheTokenSettlesTheSendAtOnce(t *testing.T) {
	t.Parallel()
	listener := startTakeOverListener(t)
	handler, database := newTargetedMessageHandlerOn(t, listener.server.URL, clocktest.New(time.Now()))

	unauthorized := takeOverAnswer{takeOverRegistry, answerJSON(http.StatusUnauthorized, `{"error":"unauthorized"}`)}
	script := make([]takeOverAnswer, 64)
	for i := range script {
		script[i] = unauthorized
	}
	listener.play(script...)

	sent := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		sent <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": "Token refused", "delivery": "steer"}, "alice")
	}()
	response := testwait.Receive(t, "the send to settle on the listener's 401", sent)
	if response.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
	}
	message := decodeBody[model.Message](t, response)
	if len(message.Deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want attempt 1 alone", message.Deliveries)
	}
	attempt := message.Deliveries[0]
	if attempt.State != "failed" || attempt.Error == nil || !strings.Contains(*attempt.Error, "unauthorized") {
		t.Fatalf("attempt = %#v, want failed with the listener's unauthorized answer", attempt)
	}
	if tries := listener.requestsTo(takeOverRegistry); tries != 2 {
		t.Fatalf("a listener answering 401 unauthorized was asked for the registry %d times, want twice (the "+
			"identity token, then the same file read again): a refused token is the listener's answer, not a "+
			"take-over to wait out (envoy Client.Session maps the 401 to ErrUnavailable, which the take-over "+
			"wait retries)", tries)
	}
	if states := attemptStates(t, database, "message_deliveries", "message_id", message.ID); !slices.Equal(states, []string{"1:failed"}) {
		t.Fatalf("attempts = %v, want attempt 1 alone, failed", states)
	}
}
