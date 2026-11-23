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
	"github.com/sjawhar/envoy/listenerwire"
)

// INFRA-3740: a targeted message to a session that neither the listener's registry nor its
// known-session store holds - its registry read answers 404 "no live session <id>", or its send
// does - is queued by Dispatch itself. Dispatch publishes on that session's own subject the
// envelope the listener's send route would have stored and records the attempt queued, as the
// listener's own queueing of a stopped session's message is recorded (delivery_queued_test.go).

// publishedBy answers what the test server's own recorder holds: newRegisteredTestServer gives
// every server one unless its test names another publisher or none.
func publishedBy(t *testing.T, deps Deps) []contracts.Envelope {
	t.Helper()
	recorder, ok := deps.AgentPublisher.(*AgentPublishRecorder)
	if !ok {
		t.Fatalf("the test server publishes through %T, not a recorder", deps.AgentPublisher)
	}
	return recorder.Published()
}

// failingAgentPublisher refuses every publish, as a bus the server cannot reach does.
type failingAgentPublisher struct{ err error }

func (p failingAgentPublisher) PublishReportingDuplicateWithin(context.Context, contracts.Envelope) (bool, error) {
	return false, p.err
}

// assertQueuedEnvelope fails t unless envelope is the one a send of attempt `attempt` of message
// in mode would have been stored as: on session's own subject, under the send's dedupe key, from
// Dispatch, carrying that attempt's frame.
func assertQueuedEnvelope(
	t *testing.T, envelope contracts.Envelope, session string, message model.Message, attempt int, mode string,
) {
	t.Helper()
	wantKey := contracts.AgentSendDedupeKey(session, message.ID+":"+mode)
	if envelope.Topic != contracts.LegacyAgentSubject(session) || envelope.DedupeKey != wantKey || envelope.Source != "dispatch" {
		t.Fatalf("queued envelope on %q under %q from %q, want %q under %q from dispatch",
			envelope.Topic, envelope.DedupeKey, envelope.Source, contracts.LegacyAgentSubject(session), wantKey)
	}
	frame := decodeDeliveryFrame(t, map[string]any{"payload": envelope.Payload})
	if frame.Event.Type != "message.created" || frame.Event.Payload.ID != message.ID ||
		frame.Delivery.Attempt != attempt || frame.Delivery.Mode != mode {
		t.Fatalf("queued frame = %s, want message %s's attempt %d in %s", envelope.Payload, message.ID, attempt, mode)
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
	expectsReply, err := listenerwire.ParseExpectsReply(body.ExpectsReply)
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
			_, _ = w.Write([]byte(`{"error":"no live session s1","state":"gone"}`))
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

// A person's direct message to a session the listener's registry answers 404 for, and its
// known-session store does not hold either, is queued: Dispatch makes no send, publishes the
// message on the session's own subject once, and records the attempt queued with that envelope,
// in its row and in the receipt the conversation renders.
func TestATargetedMessageToASessionNoStoreHoldsIsQueuedByDispatch(t *testing.T) {
	t.Parallel()
	listener := stoppedAndLive(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.url})

	created := postAgentMessage(t, handler, "ghost", map[string]any{
		"body": "Anyone there?\nThe deploy waits on you.", "delivery": "steer",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to an unknown session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "queued" ||
		message.Deliveries[0].Error != nil || message.Deliveries[0].SessionID != "ghost" {
		t.Fatalf("the answer's deliveries = %#v, want one queued attempt pinned to ghost", message.Deliveries)
	}
	published := publishedBy(t, deps)
	if len(published) != 1 {
		t.Fatalf("Dispatch published %d envelopes, want exactly one", len(published))
	}
	assertQueuedEnvelope(t, published[0], "ghost", message, 1, "steer")
	if published[0].PayloadSummary != "Anyone there?" || published[0].ExpectsReply != contracts.ExpectsReplyOptional {
		t.Fatalf("queued envelope summary %q, expects_reply %q, want the message's first line and optional",
			published[0].PayloadSummary, published[0].ExpectsReply)
	}
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "queued" || attempts[0].Duplicate ||
		attempts[0].EnvelopeID == nil || *attempts[0].EnvelopeID != published[0].EventID {
		t.Fatalf("the stored attempts = %#v, want one queued attempt naming the envelope %s", attempts, published[0].EventID)
	}
	if sends := listener.sentTo("ghost"); len(sends) != 0 {
		t.Fatalf("the listener took %d sends for a session it does not know, want none", len(sends))
	}
	if receipts := messageDeliveryReceiptRows(t, database, message.ID); len(receipts) != 1 ||
		receipts[0].State != "queued" {
		t.Fatalf("the delivery receipts = %#v, want one carrying queued", receipts)
	}
}

// A send the registry let through and the listener answered 404 is the same fact as the registry's
// own 404, so it is queued too, as the very envelope the listener's route would have stored had
// it held the record, and a repeat under the same key is the duplicate the stream reports rather
// than a second copy. A publish that fails queues nothing: the attempt fails with the publish's
// own error after the listener's, so the person can retry it.
func TestATargetedMessageWhoseSendFindsNoLiveSessionIsQueuedByDispatch(t *testing.T) {
	t.Parallel()
	t.Run("queued as the envelope the send would have stored", func(t *testing.T) {
		listener, sends := sendFindsNoSessionListener(t)
		handler, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
		issue := createInteractionIssue(t, handler, "TEST", "A send that finds no session", "before")

		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can this ship?\nThe canary is green.", "target": "session:s1", "delivery": "btw", "urgency": "high",
		}, "alice")
		attempt := message.Deliveries
		if len(attempt) != 1 || attempt[0].State != "queued" || attempt[0].Error != nil || attempt[0].SessionID != "s1" {
			t.Fatalf("delivery whose send was answered 404 = %#v, want it queued for s1 with no error", attempt)
		}
		published := publishedBy(t, deps)
		asked := sends()
		if len(published) != 1 || len(asked) == 0 {
			t.Fatalf("published %d envelopes after %d sends, want one after the sends the listener refused",
				len(published), len(asked))
		}
		assertQueuedEnvelope(t, published[0], "s1", message, 1, "btw")
		if want := asked[len(asked)-1].storedBy(t, published[0]); !reflect.DeepEqual(published[0], want) {
			t.Fatalf("queued envelope = %#v, want the one the listener stores for Dispatch's own send %#v", published[0], want)
		}
		if attempt[0].EnvelopeID == nil || *attempt[0].EnvelopeID != published[0].EventID || attempt[0].Duplicate {
			t.Fatalf("queued attempt names envelope %v (duplicate %v), want the published envelope %s",
				attempt[0].EnvelopeID, attempt[0].Duplicate, published[0].EventID)
		}

		// The same mode again is the same key, so the stream already holds that copy.
		retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
			map[string]any{"delivery": "btw"}, "alice")
		if retry.Code != http.StatusCreated {
			t.Fatalf("same-mode retry: status=%d body=%s", retry.Code, retry.Body.String())
		}
		repeated := decodeBody[model.MessageDelivery](t, retry)
		if repeated.Attempt != 2 || repeated.State != "queued" || !repeated.Duplicate || repeated.EnvelopeID != nil {
			t.Fatalf("same-mode retry = %#v, want attempt 2 queued as a duplicate naming no envelope", repeated)
		}
		if got := len(publishedBy(t, deps)); got != 1 {
			t.Fatalf("the recorder holds %d envelopes after a repeat, want the one", got)
		}
	})

	t.Run("a publish that fails settles failed with both reasons", func(t *testing.T) {
		listener, _ := sendFindsNoSessionListener(t)
		refusal := errors.New("nats: no responders available for request")
		handler, _, _ := newTestServer(t, testServerOptions{
			envoyURL: listener.URL, agentPublisher: failingAgentPublisher{err: refusal},
		})
		issue := createInteractionIssue(t, handler, "TEST", "A queue that fails", "before")

		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
		}, "alice")
		attempt := message.Deliveries
		if len(attempt) != 1 || attempt[0].State != "failed" || attempt[0].Error == nil ||
			!strings.HasPrefix(*attempt[0].Error, "no live session s1") ||
			!strings.Contains(*attempt[0].Error, refusal.Error()) || attempt[0].EnvelopeID != nil {
			t.Fatalf("delivery whose publish failed = %#v, want it failed naming no live session and the publish's error", attempt)
		}
	})
}

// A deployment with no bus to publish on - NATS disabled, no test hooks - records a targeted
// message to a session no store of the listener's holds failed, as it did before Dispatch queued
// any, from the registry's 404 and from the send's alike.
func TestATargetedMessageWithNoPublisherFails(t *testing.T) {
	t.Parallel()
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
			if len(attempt) != 1 || attempt[0].State != "failed" || attempt[0].Error == nil ||
				*attempt[0].Error != "no live session s1" {
				t.Fatalf("delivery with no publisher = %#v, want it failed with no live session s1", attempt)
			}
		})
	}
}

// Dispatch queues a message only on the subject of the session its target names. A listener that
// answers the read of s1 with another session's record puts that session on the attempt, whether
// the record reads gone or the send to it then finds none; queued, the message would go onto that
// other session's subject. So it is not queued, and the attempt settles failed with the answer.
func TestATargetedMessageWhoseAttemptNamesAnotherSessionIsNotQueued(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		registry string
	}{
		{name: "the read answers another session gone",
			registry: `{"session_id":"s2","title":"planner","state":"gone","capabilities":["aside","btw","steer"]}`},
		{name: "the read answers another session whose send finds none",
			registry: `{"session_id":"s2","title":"planner","capabilities":["aside","btw","steer"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
					_, _ = w.Write([]byte(tc.registry))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"no live session s2","state":"gone"}`))
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"not found"}`))
				}
			}))
			t.Cleanup(listener.Close)
			handler, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})

			created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Restart the deploy.", "delivery": "steer"}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
			}
			attempt := decodeBody[model.Message](t, created).Deliveries
			if len(attempt) != 1 || attempt[0].State != "failed" || attempt[0].Error == nil ||
				*attempt[0].Error != "no live session s2" {
				t.Fatalf("delivery whose attempt names s2 = %#v, want it failed with the listener's answer", attempt)
			}
			if published := publishedBy(t, deps); len(published) != 0 {
				t.Fatalf("published %#v for a message to s1, want nothing: the attempt names another session", published)
			}
		})
	}
}

// A sender queues a message only while it still holds the attempt. Here the session answers the
// attempt while the sender waits between its tries, so the sender's renew finds the attempt no
// longer its own: it publishes nothing, since the session already has the message, and the
// attempt reads as the session's answer made it.
func TestATargetedMessageAnsweredBetweenItsTriesIsNotQueued(t *testing.T) {
	t.Parallel()
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
	if published := publishedBy(t, deps); len(published) != 0 {
		t.Fatalf("published %d envelopes for an attempt its session already answered, want none", len(published))
	}
}

// A message to a role keeps its failure when the role's holder is gone: the role names no session
// to queue for, and a session that held the role once is not where a role's message goes. That
// holds when the role read refuses for a holder that is gone, and when it named a holder whose
// send then found no live session.
func TestAMessageToARoleWhoseHolderIsGoneStillFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		role func(w http.ResponseWriter)
	}{
		{name: "the role read finds its holder gone", role: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"role legion-planner holder s1 lapsed; claim retained",` +
				`"reason":"holder_lapsed","holder":"s1","holder_state":"gone","claim_released":false}`))
		}},
		{name: "the holder's send finds no live session", role: func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"role":"legion-planner","holder":"s1","title":"planner","capabilities":["aside","btw","steer"]}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/legion-planner":
					tc.role(w)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"no live session s1","state":"gone"}`))
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
				}
			}))
			t.Cleanup(listener.Close)
			handler, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
			issue := createInteractionIssue(t, handler, "TEST", "A role whose holder is gone", "before")

			message := createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Who is planning?", "target": "role:legion-planner", "delivery": "steer",
			}, "alice")
			if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" || got[0].Error == nil {
				t.Fatalf("delivery to a role whose holder is gone = %#v, want it failed", got)
			}
			if published := publishedBy(t, deps); len(published) != 0 {
				t.Fatalf("published %d envelopes for a role's message, want none", len(published))
			}
		})
	}
}

// A model or thinking switch asks for a session running now on every send of it, so it is never
// stored for a session that is not running: one no store of the listener's holds keeps its
// failure, and Dispatch publishes nothing for it.
func TestASwitchToASessionNoStoreHoldsIsNotQueued(t *testing.T) {
	t.Parallel()
	listener := stoppedAndLive(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.url})

	created := postAgentMessage(t, handler, "ghost", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the switch: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "failed" || attempts[0].Error == nil ||
		*attempts[0].Error != "no live session ghost" {
		t.Fatalf("the switch's attempts = %#v, want one failed attempt reading \"no live session ghost\"", attempts)
	}
	if published := publishedBy(t, deps); len(published) != 0 {
		t.Fatalf("published %d envelopes for a switch, want none", len(published))
	}
}

// The browser suite runs Dispatch with no NATS and reads what that process queued from the test
// hook, which lists each envelope as its session's subject would hold it. A process that
// publishes to the bus holds none to list and says so, and one without test hooks serves no such
// route.
func TestTheQueuedTestHookListsWhatThisProcessQueued(t *testing.T) {
	t.Parallel()
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

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/_test/queued", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("list queued messages: status=%d body=%s", read.Code, read.Body.String())
	}
	listed := decodeBody[queuedMessages](t, read).Queued
	if len(listed) != 1 || listed[0].Subject != contracts.LegacyAgentSubject("s1") ||
		listed[0].DedupeKey != contracts.AgentSendDedupeKey("s1", message.ID+":steer") || listed[0].Source != "dispatch" ||
		listed[0].PayloadSummary != "Restart the deploy." {
		t.Fatalf("queued messages = %#v, want the one Send queued on s1's subject under its send's key", listed)
	}
	if frame := decodeDeliveryFrame(t, map[string]any{"payload": listed[0].Payload}); frame.Event.Payload.ID != message.ID ||
		frame.Delivery.Attempt != 1 || frame.Delivery.Mode != "steer" {
		t.Fatalf("queued frame = %s, want attempt 1 of message %s in steer", listed[0].Payload, message.ID)
	}

	onTheBus, _, _ := newTestServer(t, testServerOptions{
		testHooks: true, agentPublisher: failingAgentPublisher{err: errors.New("unused")},
	})
	if refused := dispatchRequest(t, onTheBus, http.MethodGet, "/api/v1/messages/_test/queued", nil, "alice"); refused.Code != http.StatusNotFound ||
		responseCode(t, refused) != "QUEUED_ON_BUS" {
		t.Fatalf("list on a process that publishes to the bus: status=%d body=%s, want 404 QUEUED_ON_BUS",
			refused.Code, refused.Body.String())
	}
	withoutHooks, _, _ := newTestServer(t, testServerOptions{})
	if unmounted := dispatchRequest(t, withoutHooks, http.MethodGet, "/api/v1/messages/_test/queued", nil, "alice"); unmounted.Code != http.StatusNotFound {
		t.Fatalf("list without test hooks: status=%d body=%s, want 404", unmounted.Code, unmounted.Body.String())
	}
}
