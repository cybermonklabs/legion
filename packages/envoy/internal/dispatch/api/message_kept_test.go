package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// keptBy answers what the test server's own recorder kept: newRegisteredTestServer gives every
// server one unless its test names another publisher or none.
func keptBy(t *testing.T, deps Deps) []contracts.Envelope {
	t.Helper()
	recorder, ok := deps.AgentPublisher.(*AgentPublishRecorder)
	if !ok {
		t.Fatalf("the test server keeps through %T, not a recorder", deps.AgentPublisher)
	}
	return recorder.Kept()
}

// failingAgentPublisher refuses every publish, as a bus the server cannot reach does.
type failingAgentPublisher struct{ err error }

func (p failingAgentPublisher) PublishReportingDuplicateWithin(context.Context, contracts.Envelope) (bool, error) {
	return false, p.err
}

// assertKeptEnvelope fails t unless kept is the envelope a send of attempt `attempt` of message in
// mode would have been stored as: on session's own subject, under the send's dedupe key, from
// Dispatch, carrying that attempt's frame.
func assertKeptEnvelope(t *testing.T, kept contracts.Envelope, session string, message model.Message, attempt int, mode string) {
	t.Helper()
	if kept.Topic != contracts.AgentSubject(session) ||
		kept.DedupeKey != contracts.AgentSendDedupeKey(session, message.ID+":"+mode) || kept.Source != "dispatch" {
		t.Fatalf("kept envelope on %q under %q from %q, want %q under %q from dispatch", kept.Topic, kept.DedupeKey, kept.Source,
			contracts.AgentSubject(session), contracts.AgentSendDedupeKey(session, message.ID+":"+mode))
	}
	frame := decodeDeliveryFrame(t, map[string]any{"payload": kept.Payload})
	if frame.Event.Type != "message.created" || frame.Event.Payload.ID != message.ID ||
		frame.Delivery.Attempt != attempt || frame.Delivery.Mode != mode {
		t.Fatalf("kept frame = %s, want message %s's attempt %d in %s", kept.Payload, message.ID, attempt, mode)
	}
}

// listenerSendBody is the body of a send Dispatch made to the listener, as its route reads it.
type listenerSendBody struct {
	TargetSession  string          `json:"target_session"`
	Source         string          `json:"source"`
	Message        string          `json:"message"`
	Payload        *string         `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	Urgency        string          `json:"urgency"`
	ExpectsReply   json.RawMessage `json:"expects_reply"`
}

// storedBy is the envelope the listener's send route stores for body (cmd/listener sendHandler),
// with the ids and the time it mints fresh taken from got, so the two compare field by field.
func (body listenerSendBody) storedBy(t *testing.T, got contracts.Envelope) contracts.Envelope {
	t.Helper()
	expectsReply, err := contracts.ParseExpectsReply(body.ExpectsReply)
	if err != nil {
		t.Fatalf("the send's expects_reply %s: %v", body.ExpectsReply, err)
	}
	want := contracts.AgentSendEnvelope(body.TargetSession, body.IdempotencyKey, contracts.Message{
		Source: body.Source, Text: body.Message, Payload: body.Payload, Urgency: body.Urgency, ExpectsReply: expectsReply,
	})
	want.EventID, want.SourceEventID, want.IssuedAt, want.TraceID = got.EventID, got.SourceEventID, got.IssuedAt, got.TraceID
	return want
}

// sendFindsNoSessionListener is a listener whose registry holds s1 live, taking every mode, and
// whose send answers 404 "no live session s1": the session's record went between the two reads.
// It records the body of every send it was asked.
func sendFindsNoSessionListener(t *testing.T) (*httptest.Server, func() []listenerSendBody) {
	t.Helper()
	var mu sync.Mutex
	var sends []listenerSendBody
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(`{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var body listenerSendBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the listener send: %v", err)
			}
			mu.Lock()
			sends = append(sends, body)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(listener.Close)
	return listener, func() []listenerSendBody {
		mu.Lock()
		defer mu.Unlock()
		return append([]listenerSendBody(nil), sends...)
	}
}

// A send the registry let through and the listener answered 404 is the same fact as the registry's
// own 404: the listener holds no record of the session. So it is kept too, as the very envelope the
// listener's route would have stored had it held the record, and a repeat under the same key is the
// duplicate the stream reports rather than a second copy. A publish that fails keeps nothing: the
// attempt fails with the publish's own error after the listener's, so the person can retry it.
func TestATargetedMessageWhoseSendFindsNoLiveSessionIsKept(t *testing.T) {
	t.Run("kept as the envelope the send would have stored", func(t *testing.T) {
		listener, sends := sendFindsNoSessionListener(t)
		handler, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
		issue := createInteractionIssue(t, handler, "TEST", "A send that finds no session", "before")

		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can this ship?\nThe canary is green.", "target": "session:s1", "delivery": "btw", "urgency": "high",
		}, "alice")
		attempt := message.Deliveries
		if len(attempt) != 1 || attempt[0].State != "kept" || attempt[0].Error != nil || attempt[0].SessionID != "s1" {
			t.Fatalf("delivery whose send was answered 404 = %#v, want it kept for s1 with no error", attempt)
		}
		kept := keptBy(t, deps)
		asked := sends()
		if len(kept) != 1 || len(asked) == 0 {
			t.Fatalf("kept %d envelopes after %d sends, want one kept after the sends the listener refused", len(kept), len(asked))
		}
		assertKeptEnvelope(t, kept[0], "s1", message, 1, "btw")
		if want := asked[len(asked)-1].storedBy(t, kept[0]); !reflect.DeepEqual(kept[0], want) {
			t.Fatalf("kept envelope = %#v, want the one the listener stores for Dispatch's own send %#v", kept[0], want)
		}
		if attempt[0].EnvelopeID == nil || *attempt[0].EnvelopeID != kept[0].EventID || attempt[0].Duplicate {
			t.Fatalf("kept attempt names envelope %v (duplicate %v), want the kept envelope %s", attempt[0].EnvelopeID, attempt[0].Duplicate, kept[0].EventID)
		}

		// The same mode again is the same key, so the stream already holds that copy.
		retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
			map[string]any{"delivery": "btw"}, "alice")
		if retry.Code != http.StatusCreated {
			t.Fatalf("same-mode retry: status=%d body=%s", retry.Code, retry.Body.String())
		}
		repeated := decodeBody[model.MessageDelivery](t, retry)
		if repeated.Attempt != 2 || repeated.State != "kept" || !repeated.Duplicate || repeated.EnvelopeID != nil {
			t.Fatalf("same-mode retry = %#v, want attempt 2 kept as a duplicate naming no envelope", repeated)
		}
		if got := len(keptBy(t, deps)); got != 1 {
			t.Fatalf("kept %d envelopes after a repeat, want the one", got)
		}
	})

	t.Run("a publish that fails keeps nothing", func(t *testing.T) {
		listener, _ := sendFindsNoSessionListener(t)
		refusal := errors.New("nats: no responders available for request")
		handler, _, _ := newTestServer(t, testServerOptions{
			envoyURL: listener.URL, agentPublisher: failingAgentPublisher{err: refusal},
		})
		issue := createInteractionIssue(t, handler, "TEST", "A keep that fails", "before")

		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
		}, "alice")
		attempt := message.Deliveries
		if len(attempt) != 1 || attempt[0].State != "failed" || attempt[0].Error == nil ||
			!strings.HasPrefix(*attempt[0].Error, "no live session s1") || !strings.Contains(*attempt[0].Error, refusal.Error()) ||
			attempt[0].EnvelopeID != nil {
			t.Fatalf("delivery whose keep failed = %#v, want it failed naming no live session and the publish's error", attempt)
		}
	})
}

// A deployment with no bus to keep a message on - NATS disabled, no test hooks - records a
// targeted message to a session the listener holds no record of failed, as it did before Dispatch
// kept any, from the registry's 404 and from the send's alike.
func TestATargetedMessageWithNowhereToKeepItFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listener func(t *testing.T) *httptest.Server
	}{
		{name: "the registry holds no record", listener: func(t *testing.T) *httptest.Server {
			live := false
			sent := []map[string]any{}
			listener := sessionListener(t, &live, &sent)
			t.Cleanup(listener.Close)
			return listener
		}},
		{name: "the send finds no record", listener: func(t *testing.T) *httptest.Server {
			listener, _ := sendFindsNoSessionListener(t)
			return listener
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, _ := newTestServer(t, testServerOptions{envoyURL: tc.listener(t).URL, noAgentPublisher: true})
			created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Restart the deploy.", "delivery": "steer"}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
			}
			attempt := decodeBody[model.Message](t, created).Deliveries
			if len(attempt) != 1 || attempt[0].State != "failed" || attempt[0].Error == nil || *attempt[0].Error != "no live session s1" {
				t.Fatalf("delivery with nowhere to keep it = %#v, want it failed with no live session s1", attempt)
			}
		})
	}
}

// A sender keeps a message only while it still holds the attempt. Here the session answers the
// attempt while the sender waits between its tries, so the sender's renew finds the attempt no
// longer its own: it keeps nothing, since the session already has the message, and the attempt
// reads as the session's answer made it.
func TestATargetedMessageAnsweredBetweenItsTriesIsNotKept(t *testing.T) {
	var handler http.Handler
	var database *store.Store
	var mu sync.Mutex
	var reads int
	var replied *httptest.ResponseRecorder
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/registry/s1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		// The second read is the renew's, before it takes the claim again: the session answers
		// the attempt it was handed then.
		if reads++; reads == 2 {
			var messageID string
			if err := database.Pool.QueryRow(context.Background(), `
				select message_id::text from message_deliveries where session_id = 's1' and state = 'pending'
			`).Scan(&messageID); err != nil {
				t.Errorf("read the attempt the session was handed: %v", err)
			} else {
				replied = bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+messageID+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Already on it.",
				})
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
	}))
	t.Cleanup(listener.Close)
	var deps Deps
	handler, database, deps = newTestServer(t, testServerOptions{envoyURL: listener.URL})

	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Restart the deploy.", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	mu.Lock()
	answered := replied
	mu.Unlock()
	if answered == nil || answered.Code != http.StatusCreated {
		t.Fatalf("the session's answer between the tries = %v, want it posted", answered)
	}
	attempt := decodeBody[model.Message](t, created).Deliveries
	if len(attempt) != 1 || attempt[0].State != "sent" || attempt[0].ReplyID == nil {
		t.Fatalf("delivery the session answered between its tries = %#v, want it sent with the answer", attempt)
	}
	if kept := keptBy(t, deps); len(kept) != 0 {
		t.Fatalf("kept %d envelopes for an attempt its session already answered, want none", len(kept))
	}
}

// The browser suite runs Dispatch with no NATS and reads what that process kept from the test
// hook, which lists each kept envelope as its session's subject would hold it. A process that keeps
// messages on the bus holds none to list and says so, and one without test hooks serves no such
// route.
func TestTheKeptTestHookListsWhatThisProcessKept(t *testing.T) {
	live := false
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, testHooks: true})
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Restart the deploy.", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/_test/kept", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("list kept messages: status=%d body=%s", read.Code, read.Body.String())
	}
	listed := decodeBody[keptMessages](t, read).Kept
	if len(listed) != 1 || listed[0].Subject != contracts.AgentSubject("s1") ||
		listed[0].DedupeKey != contracts.AgentSendDedupeKey("s1", message.ID+":steer") || listed[0].Source != "dispatch" ||
		listed[0].PayloadSummary != "Restart the deploy." {
		t.Fatalf("kept messages = %#v, want the one Send kept on s1's subject under its send's key", listed)
	}
	if frame := decodeDeliveryFrame(t, map[string]any{"payload": listed[0].Payload}); frame.Event.Payload.ID != message.ID ||
		frame.Delivery.Attempt != 1 || frame.Delivery.Mode != "steer" {
		t.Fatalf("kept frame = %s, want attempt 1 of message %s in steer", listed[0].Payload, message.ID)
	}

	onTheBus, _, _ := newTestServer(t, testServerOptions{
		testHooks: true, agentPublisher: failingAgentPublisher{err: errors.New("unused")},
	})
	if refused := dispatchRequest(t, onTheBus, http.MethodGet, "/api/v1/messages/_test/kept", nil, "alice"); refused.Code != http.StatusNotFound ||
		responseCode(t, refused) != "TEST_HOOKS_OFF" {
		t.Fatalf("list on a process that keeps on the bus: status=%d body=%s, want 404 TEST_HOOKS_OFF", refused.Code, refused.Body.String())
	}
	withoutHooks, _, _ := newTestServer(t, testServerOptions{})
	if unmounted := dispatchRequest(t, withoutHooks, http.MethodGet, "/api/v1/messages/_test/kept", nil, "alice"); unmounted.Code != http.StatusNotFound {
		t.Fatalf("list without test hooks: status=%d body=%s, want 404", unmounted.Code, unmounted.Body.String())
	}
}
